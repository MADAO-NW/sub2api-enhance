package thirdpartypromptaudit

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lib/pq"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type probeContextKey int

// probeActorContext 将管理员身份绑定到节点测试调用记录。
const probeActorContext probeContextKey = 0

type ModelClient struct {
	publicOrigin string
	attempts     AttemptStore
	allowAudit   func() bool
}

func NewModelClient(repository *Repository, config *ConfigManager) *ModelClient {
	return &ModelClient{publicOrigin: config.publicOrigin, attempts: repository, allowAudit: func() bool { return config.EffectiveMode() != "off" }}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

func nodeHTTPClient(model ModelConfig) (*http.Client, string, error) {
	client, err := newNodeHTTPClient(ModelConfig{BaseURL: model.BaseURL, TimeoutMS: model.TimeoutMS})
	if err != nil {
		return nil, "", err
	}
	url, err := chatCompletionsURL(model.BaseURL)
	if err != nil {
		client.CloseIdleConnections()
		return nil, "", err
	}
	// 凭据只发给管理员选定的节点，不跟随重定向改变接收方。
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return client, url, nil
}

// EvaluateTarget 在同一个节点总预算内至多修正一次格式，逐次记录真实 HTTP 调用。
func (c *ModelClient) EvaluateTarget(ctx context.Context, job *Job, model ModelConfig, key string, snapshot ConfigSnapshot,
	client *http.Client, url, stage string, target any, order *int) (*Score, int64, *AuditError) {
	var repairOf *int64
	correction := ""
	for number := 0; number < 2; number++ {
		callStage := stage
		if number > 0 {
			callStage = "format_repair"
		}
		result, id, failure := c.call(ctx, job, model, key, snapshot, client, url, stage, callStage, target, order, repairOf, correction)
		if failure == nil {
			return result, id, nil
		}
		if failure.Code != "invalid_response" || number > 0 || ctx.Err() != nil {
			return nil, id, failure
		}
		repairOf = &id
		correction = "【格式修正】上次结果不符合固定 JSON 协议。请对相同目标重新给出完整 JSON 对象，仅包含数值 confidence 和字符串 reason。"
	}
	return nil, 0, &AuditError{Code: "invalid_response", Stage: "response_parse", Message: "节点返回格式无效"}
}

func (c *ModelClient) call(ctx context.Context, job *Job, model ModelConfig, key string, snapshot ConfigSnapshot,
	client *http.Client, url, ruleStage, callStage string, target any, order *int, repairOf *int64, correction string) (*Score, int64, *AuditError) {
	if c.publicOrigin != "" && strings.HasPrefix(url, c.publicOrigin+"/") {
		return nil, 0, &AuditError{Code: "audit_node_recursion", Stage: "model_request", Message: "审核节点不能指向增强公共入口"}
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, requestFailure(err)
	}
	if job != nil && c.allowAudit != nil && !c.allowAudit() {
		return nil, 0, persistenceFailure(ErrAuditPaused, "audit_paused")
	}
	auditStage := ruleStage
	if auditStage == "probe" {
		auditStage = "joint"
	}
	targetJSON, err := json.Marshal(auditEnvelope{Stage: auditStage, Target: target})
	if err != nil {
		return nil, 0, &AuditError{Code: "input_encode_failed", Stage: "input_parse", Message: err.Error()}
	}
	request := chatRequest{Model: model.Model, Stream: false,
		Messages: []chatMessage{{Role: "system", Content: systemPromptSnapshot(snapshot, correction)}, {Role: "user", Content: string(targetJSON)}}}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, 0, &AuditError{Code: "request_encode_failed", Stage: "model_request", Message: err.Error()}
	}
	metadata := map[string]any{"stage": ruleStage, "segment_order": order, "repair_of_attempt_id": repairOf, "model": model, "contract_version": snapshot.ContractVersion}
	metadata["audit_stage"] = auditStage
	attempt := &ModelAttempt{ModelID: model.ID, ModelSnapshot: model, Stage: callStage, SegmentOrder: order, RepairOfAttemptID: repairOf, Status: "prepared"}
	if job == nil {
		attempt.CallKind = "probe"
		metadata["request_body"] = request
		metadata["actor_user_id"] = ctx.Value(probeActorContext)
	} else {
		attempt.JobID = &job.ID
		attempt.EvaluationRound = &job.Attempts
		attempt.CallKind = "audit"
		metadata["job_id"] = job.ID
		metadata["target_hash"] = job.TargetHash
	}
	attempt.RequestMetadata, err = json.Marshal(metadata)
	if err != nil {
		return nil, 0, &AuditError{Code: "request_encode_failed", Stage: "model_request", Message: err.Error()}
	}
	if err := c.attempts.PrepareAttempt(ctx, job, attempt); err != nil {
		return nil, 0, persistenceFailure(err, "attempt_prepare_failed")
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, attempt.ID, &AuditError{Code: "request_invalid", Stage: "model_request", Message: err.Error()}
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	if key != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+key)
	}
	if err := c.attempts.StartAttempt(ctx, job, attempt); err != nil {
		return nil, attempt.ID, persistenceFailure(err, "attempt_start_failed")
	}
	started := time.Now()
	response, callErr := client.Do(httpRequest)
	var failure *AuditError
	var result *Score
	if callErr != nil {
		failure = requestFailure(callErr)
	} else {
		attempt.HTTPStatus = &response.StatusCode
		raw, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		text := string(raw)
		attempt.RawResponse = &text
		if readErr != nil {
			failure = requestFailure(readErr)
		} else if closeErr != nil {
			failure = requestFailure(closeErr)
		} else if response.StatusCode < 200 || response.StatusCode >= 300 {
			failure = &AuditError{Code: "upstream_http_error", Stage: "model_request", Message: fmt.Sprintf("审核节点返回 HTTP %d", response.StatusCode), Retryable: response.StatusCode == 429 || response.StatusCode >= 500}
			if response.StatusCode == 429 {
				failure.Code = "rate_limited"
			}
			if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 && int64(seconds) <= int64((1<<63-1)/int64(time.Second)) {
				failure.RetryAfter = time.Duration(seconds) * time.Second
			} else if retryAt, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil && retryAt.After(time.Now()) {
				failure.RetryAfter = time.Until(retryAt)
			}
		} else {
			var envelope struct {
				Choices []struct {
					Message struct {
						Content json.RawMessage `json:"content"`
					} `json:"message"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
				Usage struct {
					PromptTokens     *int64 `json:"prompt_tokens"`
					CompletionTokens *int64 `json:"completion_tokens"`
				} `json:"usage"`
			}
			parseErr := json.Unmarshal(raw, &envelope)
			if parseErr == nil && len(envelope.Choices) > 0 {
				attempt.InputTokens, attempt.OutputTokens = envelope.Usage.PromptTokens, envelope.Usage.CompletionTokens
				var content string
				parseErr = json.Unmarshal(envelope.Choices[0].Message.Content, &content)
				if envelope.Choices[0].FinishReason == "length" {
					parseErr = errors.New("模型响应因长度限制未完成")
				}
				if parseErr == nil {
					score, err := ParseScore(content)
					parseErr = err
					if err == nil {
						result = &score
					}
				}
			} else if parseErr == nil {
				parseErr = errors.New("响应缺少 choices 消息")
			}
			if parseErr != nil {
				failure = &AuditError{Code: "invalid_response", Stage: "response_parse", Message: parseErr.Error()}
			}
		}
	}
	latency := time.Since(started).Milliseconds()
	attempt.LatencyMS = &latency
	attempt.Status = "succeeded"
	attempt.Result = result
	if failure != nil {
		attempt.Status = "failed"
		attempt.ErrorCode = failure.Code
		attempt.ErrorMessage = failure.Message
	}
	// 仅持久化收尾脱离请求取消，不能借此再次调用第三方。
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := c.attempts.FinishAttempt(persistCtx, job, attempt); err != nil {
		return nil, attempt.ID, persistenceFailure(err, "attempt_result_persist_failed")
	}
	return result, attempt.ID, failure
}

func requestFailure(err error) *AuditError {
	failure := &AuditError{Code: "network_error", Stage: "model_request", Message: err.Error(), Retryable: true}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()) {
		failure.Code = "timeout"
	}
	if errors.Is(err, context.Canceled) {
		failure.Code = "cancelled"
		failure.Retryable = false
	}
	return failure
}

func persistenceFailure(err error, code string) *AuditError {
	if errors.Is(err, ErrLeaseLost) {
		return &AuditError{Code: "lease_lost", Stage: "worker", Message: err.Error()}
	}
	if errors.Is(err, ErrAuditPaused) {
		return &AuditError{Code: "audit_paused", Stage: "worker", Message: err.Error(), Retryable: true}
	}
	retryable := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn)
	var network net.Error
	if errors.As(err, &network) {
		retryable = true
	}
	var postgres *pq.Error
	if errors.As(err, &postgres) {
		class := postgres.Code.Class()
		retryable = class == "08" || class == "40" || class == "53" || class == "57" || class == "58"
	}
	return &AuditError{Code: code, Stage: "result_persist", Message: err.Error(), Retryable: retryable}
}
