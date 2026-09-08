package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"sub2api-enhance/internal/pkg/logger"

	"github.com/google/uuid"
	"sub2api-enhance/internal/notify"
	"sub2api-enhance/internal/sub2api"
)

// persistenceTimeout 限定请求退出后的数据库收尾时间，不用于追加模型调用。
const persistenceTimeout = 5 * time.Second

type Service struct {
	accounts   *sub2api.Client
	repo       *Repository
	config     *ConfigManager
	evaluator  *Evaluator
	client     *ModelClient
	email      notify.Sender
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	closing    bool
	wg         sync.WaitGroup
	foreground sync.WaitGroup
	wake       chan struct{}
	active     atomic.Int64
	running    atomic.Bool
	metrics    *RuntimeMetrics
}

func NewService(repo *Repository, config *ConfigManager, evaluator *Evaluator, client *ModelClient, email notify.Sender) *Service {
	return &Service{repo: repo, config: config, evaluator: evaluator, client: client, email: email, wake: make(chan struct{}, 1), metrics: NewRuntimeMetrics()}
}

func (s *Service) Start(parent context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return nil
	}
	s.ctx, s.cancel = context.WithCancel(parent)
	s.closing = false
	err := s.config.Reload(s.ctx)
	if err != nil {
		s.noteError("config_load_failed", err)
	}
	s.wg.Add(1)
	s.running.Store(true)
	go s.run()
	return err
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.foreground.Wait(); s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) Check(parent context.Context, request IntakeRequest) *IntakeDecision {
	mode := s.config.EffectiveMode()
	if request.Background && mode == "blocking" {
		mode = "async"
	}
	if mode == "off" {
		return nil
	}
	snapshot, configErr := s.config.Active()
	if configErr == nil && (slices.Contains(snapshot.ExcludedUserIDs, request.UserID) ||
		(!snapshot.AllGroups && (request.GroupID == nil || !slices.Contains(snapshot.GroupIDs, *request.GroupID))) ||
		(len(snapshot.Platforms) > 0 && !slices.Contains(snapshot.Platforms, request.Provider))) {
		return nil
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		kind := IngressDecisionUnavailable
		if mode == "async" {
			kind = IngressDecisionUnavailable
		}
		s.metrics.IntakeFailures.Add(1)
		return &IntakeDecision{Mode: mode, Kind: kind, ErrorCode: "third_party_audit_unavailable"}
	}
	s.foreground.Add(1)
	base := s.ctx
	s.mu.Unlock()
	defer s.foreground.Done()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if base != nil {
		stop := context.AfterFunc(base, cancel)
		defer stop()
	}
	if configErr != nil {
		snapshot = ConfigSnapshot{Config: Config{Mode: mode, AuditScope: "full_request", AllGroups: true}}
	}
	if request.Manual {
		request.Stage = "manual_capture_reprocess"
	}
	input, inputErr := CaptureInput(request.Protocol, request.Body)
	capturedCompletely := inputErr == nil
	key := request.CaptureKey
	if key == "" {
		key = uuid.NewString()
	}
	apiKeyID := request.APIKeyID
	job := &Job{CapturedAt: request.CapturedAt, CaptureID: request.CaptureID, CaptureKey: key, RunKind: "request", UserID: request.UserID, APIKeyID: &apiKeyID, GroupID: request.GroupID, RequestID: request.RequestID,
		Identity: Identity{Username: request.Username, UserEmail: request.UserEmail, APIKeyName: request.APIKeyName, GroupName: request.GroupName, Endpoint: request.Endpoint},
		Platform: request.Provider, Protocol: request.Protocol, IngressStage: request.Stage, RequestedModel: request.Model, ExecutionMode: mode,
		Config: snapshot, FullInput: input, SnapshotStatus: "complete", Status: "queued"}
	if mode == "blocking" {
		job.Status = "processing"
	}
	if inputErr == nil {
		_, inputErr = prepareTarget(job)
	}
	if inputErr != nil {
		if !capturedCompletely {
			job.SnapshotStatus = "incomplete"
		}
		job.Status = "failed"
		job.FailureStage = "input_parse"
		job.LastErrorCode = "input_parse_failed"
		job.LastErrorMessage = inputErr.Error()
		if errors.Is(inputErr, ErrNoText) {
			job.Status = "skipped"
			job.SnapshotStatus = "complete"
			job.LastErrorCode = "no_text"
			job.FailureStage = ""
		}
	}
	if configErr != nil {
		job.Status = "failed"
		job.FailureStage = "config"
		job.LastErrorCode = "config_unavailable"
		job.LastErrorMessage = configErr.Error()
	}
	logger.LegacyPrintf("third_party_prompt_audit", "开始保存审核输入 request_id=%s user_id=%d mode=%s", request.RequestID, request.UserID, mode)
	captureCtx, captureCancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	stored, created, err := s.repo.CreateJob(captureCtx, job)
	captureCancel()
	if err != nil {
		s.metrics.IntakeFailures.Add(1)
		s.noteError("input_persist_failed", err)
		kind := IngressDecisionUnavailable
		if mode == "blocking" {
			kind = IngressDecisionUnavailable
		}
		return &IntakeDecision{Mode: mode, Kind: kind, ErrorCode: "third_party_audit_unavailable"}
	}
	job = stored
	result := &IntakeDecision{JobID: job.ID, Mode: mode, Kind: IngressDecisionAllow}
	logger.LegacyPrintf("third_party_prompt_audit", "审核输入已保存 job_id=%d request_id=%s status=%s", job.ID, job.RequestID, job.Status)
	if mode == "async" {
		s.notify()
		return result
	}
	if job.Status == "skipped" {
		return result
	}
	if !created {
		if job.Status == "done" {
			outcome, err := s.repo.GetOutcome(ctx, job.ID)
			if err == nil && outcome != nil {
				result.Kind = gatewayKind(outcome.Decision)
				return result
			}
		}
		result.Kind, result.ErrorCode = IngressDecisionUnavailable, "third_party_audit_unavailable"
		return result
	}
	if job.Status != "processing" {
		result.Kind, result.ErrorCode = IngressDecisionUnavailable, "third_party_audit_unavailable"
		return result
	}
	outcome, failure := s.processJob(ctx, job, true)
	if failure != nil {
		result.Kind, result.ErrorCode = IngressDecisionUnavailable, "third_party_audit_unavailable"
		return result
	}
	result.Kind = gatewayKind(outcome.Decision)
	if result.Kind == IngressDecisionBlock {
		result.ErrorCode = "third_party_audit_blocked"
	}
	return result
}

func gatewayKind(decision Decision) IngressDecisionKind {
	switch decision {
	case DecisionBlock:
		return IngressDecisionBlock
	case DecisionReview:
		return IngressDecisionFlag
	default:
		return IngressDecisionAllow
	}
}

func (s *Service) ObserveGateway(ctx context.Context, own *IntakeDecision, final IngressDecision, duration time.Duration) {
	if own == nil || own.JobID == 0 {
		return
	}
	result := "continued"
	if !final.AllowNextStage {
		switch {
		case final.Kind == IngressDecisionBlock && final.ErrorCode == "third_party_audit_blocked":
			result = "blocked_here"
		case final.Kind == IngressDecisionBlock:
			result = "blocked_here"
		default:
			result = "unavailable"
		}
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	defer cancel()
	if err := s.repo.RecordGateway(persistCtx, own.JobID, result, duration); err != nil {
		s.noteError("gateway_result_persist_failed", err)
	}
}

func (s *Service) processJob(parent context.Context, job *Job, foreground bool) (*Outcome, *AuditError) {
	ctx, cancel := context.WithCancelCause(parent)
	done := s.heartbeat(ctx, cancel, func(ctx context.Context) error { return s.repo.Renew(ctx, job) })
	defer func() { cancel(nil); <-done }()
	// 同步请求先持久化并续租，再等待与后台共享的执行槽位。
	if foreground {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for !s.tryAcquireSlot() {
			select {
			case <-ctx.Done():
				return nil, s.finishFailure(ctx, job, requestFailure(ctx.Err()))
			case <-ticker.C:
			}
			if s.config.EffectiveMode() == "off" {
				return nil, s.finishFailure(ctx, job, persistenceFailure(ErrAuditPaused, "audit_paused"))
			}
		}
		defer s.releaseSlot()
		if err := s.repo.BeginForegroundEvaluation(ctx, job); err != nil {
			return nil, s.finishFailure(ctx, job, persistenceFailure(err, "evaluation_start_failed"))
		}
	}
	evaluation := job.Checkpoint
	if evaluation == nil {
		if job.ExecutionMode == "blocking" && !foreground {
			return nil, &AuditError{Code: "blocking_request_ended", Stage: "worker", Message: "同步请求已经结束，不能在后台重新调用模型"}
		}
		started := time.Now()
		_, err := prepareTarget(job)
		var failure *AuditError
		if err != nil {
			failure = &AuditError{Code: "input_parse_failed", Stage: "input_parse", Message: err.Error()}
		} else if err = s.repo.SaveTarget(ctx, job); err != nil {
			failure = persistenceFailure(err, "target_persist_failed")
		} else {
			evaluation, failure = s.evaluator.Evaluate(ctx, job)
		}
		s.metrics.ObserveEvaluation(started, time.Now())
		if failure != nil {
			return nil, s.finishFailure(ctx, job, failure)
		}
		persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
		err = s.repo.SaveCheckpoint(persistCtx, job, evaluation)
		persistCancel()
		if err != nil {
			failure := persistenceFailure(err, "result_checkpoint_failed")
			s.noteError(failure.Code, failure)
			persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
			if err := s.repo.Fail(persistCtx, job, failure, failure.Retryable && job.ExecutionMode == "async" && job.Attempts < job.MaxAttempts); err != nil {
				s.noteError("job_failure_persist_failed", err)
			}
			persistCancel()
			return nil, failure
		}
	}
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	outcome, err := s.repo.Complete(persistCtx, job, evaluation, foreground && ctx.Err() == nil)
	persistCancel()
	if err != nil {
		failure := persistenceFailure(err, "result_persist_failed")
		s.noteError(failure.Code, failure)
		persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
		if err := s.repo.Fail(persistCtx, job, failure, failure.Retryable); err != nil {
			s.noteError("job_failure_persist_failed", err)
		}
		persistCancel()
		return nil, failure
	}
	s.metrics.Success(time.Now())
	s.notify()
	return outcome, nil
}

func (s *Service) tryAcquireSlot() bool {
	for {
		active := s.active.Load()
		if active >= int64(s.config.WorkerCapacity()) {
			return false
		}
		if s.active.CompareAndSwap(active, active+1) {
			return true
		}
	}
}

func (s *Service) releaseSlot() { s.active.Add(-1); s.notify() }

// finishFailure 区分服务暂停、请求结束和业务重试，防止停机耗尽异步任务的评估预算。
func (s *Service) finishFailure(ctx context.Context, job *Job, failure *AuditError) *AuditError {
	if cause := context.Cause(ctx); cause != nil && ctx.Err() == context.Canceled && !errors.Is(cause, context.Canceled) {
		failure = persistenceFailure(cause, "lease_renew_failed")
	}
	if failure.Code == "lease_lost" {
		return failure
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing && job.ExecutionMode == "async" {
		failure = &AuditError{Code: "worker_paused", Stage: "worker", Message: "服务正在停止，任务将在恢复后继续", Retryable: true}
	}
	retry := job.ExecutionMode == "async" && failure.Retryable &&
		(job.Attempts < job.MaxAttempts || failure.Code == "audit_paused" || failure.Code == "worker_paused")
	s.noteError(failure.Code, failure)
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	defer cancel()
	if err := s.repo.Fail(persistCtx, job, failure, retry); err != nil {
		s.noteError("job_failure_persist_failed", err)
	}
	return failure
}

func (s *Service) heartbeat(ctx context.Context, cancel context.CancelCauseFunc, renew func(context.Context) error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(leaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(ctx, persistenceTimeout)
				err := renew(renewCtx)
				renewCancel()
				if err != nil {
					if ctx.Err() == nil {
						s.noteError("lease_renew_failed", err)
					}
					cancel(err)
					return
				}
			}
		}
	}()
	return done
}

func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) noteError(code string, err error) {
	s.metrics.Error(code, err.Error())
	logger.LegacyPrintf("third_party_prompt_audit", "审核处理出现错误 error_code=%s error=%v", code, err)
}

type ProbeRequest struct {
	ActorUserID int64       `json:"-"`
	Model       ModelConfig `json:"model"`
	KeyAction   string      `json:"key_action" binding:"required,oneof=keep replace clear"`
	APIKey      string      `json:"api_key"`
	AuditPrompt string      `json:"audit_prompt" binding:"required"`
	Protocol    string      `json:"protocol"`
	Input       string      `json:"input"`
	InputKind   string      `json:"input_kind" binding:"required,oneof=text json"`
}

type ProbeResult struct {
	OK        bool        `json:"ok"`
	ModelID   string      `json:"model_id"`
	AttemptID int64       `json:"attempt_id"`
	Result    *Score      `json:"result"`
	Error     *AuditError `json:"error"`
	TestedAt  time.Time   `json:"tested_at"`
	LatencyMS int64       `json:"latency_ms"`
}

func (s *Service) Probe(ctx context.Context, input ProbeRequest) ProbeResult {
	ctx = context.WithValue(ctx, probeActorContext, input.ActorUserID)
	started := time.Now()
	result := ProbeResult{ModelID: input.Model.ID, TestedAt: started.UTC()}
	if err := validateModel(input.Model); err != nil {
		result.Error = &AuditError{Code: "invalid_model", Stage: "config", Message: err.Error()}
		return result
	}
	key := ""
	var err error
	switch input.KeyAction {
	case "replace":
		if input.APIKey == "" {
			err = errors.New("替换凭据时密钥不能为空")
		} else {
			key = input.APIKey
		}
	case "keep":
		key, err = s.config.ResolveKeyByModelID(input.Model.ID)
	case "clear":
	default:
		err = errors.New("凭据操作无效")
	}
	if err != nil {
		result.Error = &AuditError{Code: "credential_binding_unavailable", Stage: "config", Message: err.Error()}
		return result
	}
	value := any("这是一段正常的项目开发说明，请按审核政策评估。")
	if input.Input != "" {
		value = input.Input
		if input.InputKind == "json" {
			value, err = decodeUniqueJSON([]byte(input.Input))
			if err != nil {
				result.Error = &AuditError{Code: "invalid_probe_input", Stage: "input_parse", Message: err.Error()}
				return result
			}
			if _, ok := value.(map[string]any); !ok {
				result.Error = &AuditError{Code: "invalid_probe_input", Stage: "input_parse", Message: "结构化测试输入必须是完整协议请求对象"}
				return result
			}
		}
	}

	protocol := input.Protocol
	if protocol == "" {
		protocol = "openai_responses"
	}
	var body []byte
	if text, ok := value.(string); ok {
		protocol = "openai_responses"
		body, err = json.Marshal(map[string]any{"input": text})
	} else {
		body, err = json.Marshal(value)
	}
	if err != nil {
		result.Error = &AuditError{Code: "invalid_probe_input", Stage: "input_parse", Message: err.Error()}
		return result
	}
	snapshot, err := CaptureInput(protocol, body)
	if err != nil {
		result.Error = &AuditError{Code: "invalid_probe_input", Stage: "input_parse", Message: err.Error()}
		return result
	}
	config := ConfigSnapshot{Config: DefaultConfig(), ContractVersion: ContractVersion, FixedContract: OutputContract}
	config.AuditPrompt = input.AuditPrompt
	job := &Job{Config: config, Protocol: protocol, FullInput: snapshot}
	target, err := prepareTarget(job)
	if err != nil {
		result.Error = &AuditError{Code: "invalid_probe_input", Stage: "input_parse", Message: err.Error()}
		return result
	}
	client, url, err := nodeHTTPClient(input.Model)
	if err != nil {
		result.Error = &AuditError{Code: "invalid_model", Stage: "config", Message: err.Error()}
		return result
	}
	defer client.CloseIdleConnections()
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(input.Model.TimeoutMS)*time.Millisecond)
	defer cancel()
	result.Result, result.AttemptID, result.Error = s.client.EvaluateTarget(probeCtx, nil, input.Model, key, config, client, url, "probe", target, nil)
	result.OK = result.Error == nil
	result.LatencyMS = time.Since(started).Milliseconds()
	s.metrics.Probe(result)
	return result
}

func (s *Service) CreateReaudits(ctx context.Context, request ReauditRequest, actorID int64) (*ReauditResult, error) {
	snapshot, err := s.config.Active()
	if err != nil {
		return nil, err
	}
	if err := validateConfig(snapshot.Config, true); err != nil {
		return nil, fmt.Errorf("当前配置不能执行复核: %w", err)
	}
	result, err := s.repo.CreateReaudits(ctx, request, snapshot, actorID)
	if err == nil {
		s.notify()
	}
	return result, err
}
