package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNoText 区分纯媒体输入与无法解析的文本，纯媒体不参与文本风险分类。
var ErrNoText = errors.New("没有可审核的文本输入")

type Evaluator struct {
	store          EvaluationStore
	client         *ModelClient
	scheduler      *nodeScheduler
	segmentMu      sync.Mutex
	segmentFlights map[string]*segmentFlight
}

func NewEvaluator(repository *Repository, client *ModelClient) *Evaluator {
	return &Evaluator{store: repository, client: client, scheduler: newNodeScheduler(), segmentFlights: make(map[string]*segmentFlight)}
}

type segmentFlight struct {
	done    chan struct{}
	result  SegmentResult
	failure *AuditError
}

// updateNodeLimits 懒初始化测试构造的 Evaluator，并发布最新节点容量。
func (e *Evaluator) updateNodeLimits(models []ModelConfig) {
	e.nodeScheduler().updateLimits(models)
}

func (e *Evaluator) nodeScheduler() *nodeScheduler {
	e.segmentMu.Lock()
	defer e.segmentMu.Unlock()
	if e.scheduler == nil {
		e.scheduler = newNodeScheduler()
	}
	return e.scheduler
}

type auditTarget struct {
	Protocol string         `json:"protocol"`
	Messages []Segment      `json:"messages"`
	Tools    map[string]any `json:"application_context,omitempty"`
}

type auditEnvelope struct {
	Stage  string `json:"audit_stage"`
	Target any    `json:"target"`
}

// prepareTarget 同时生成审核选择范围和确定性指纹，不修改唯一原文快照。
func prepareTarget(job *Job) (auditTarget, error) {
	segments, err := ExtractSegments(job.FullInput, job.Config.AuditScope)
	if err != nil {
		return auditTarget{}, err
	}
	target := auditTarget{Protocol: job.Protocol, Messages: []Segment{}, Tools: map[string]any{}}
	job.Manifest = make([]SegmentMeta, 0, len(segments))
	for _, segment := range segments {
		job.Manifest = append(job.Manifest, segment.SegmentMeta)
		if segment.Selected {
			// 空串仍留在输入快照中；只有实际文本参与调用和复用。
			blocks := make([]TextBlock, 0, len(segment.Content))
			for _, block := range segment.Content {
				if block.Text != "" {
					blocks = append(blocks, block)
				}
			}
			if len(blocks) == 0 {
				continue
			}
			segment.Content = blocks
			target.Messages = append(target.Messages, segment)
		}
	}
	if len(target.Messages) == 0 {
		return target, ErrNoText
	}
	collectApplicationContext(job.FullInput.Fields, "$", target.Tools)
	job.InputHash, err = fingerprint(job.FullInput)
	if err != nil {
		return target, err
	}
	job.TargetHash, err = fingerprint(auditEnvelope{Stage: "joint", Target: target})
	if err != nil {
		return target, err
	}
	models := make([]any, 0)
	for _, model := range job.Config.Models {
		if model.Enabled {
			models = append(models, modelSemantics(model))
		}
	}
	job.EvaluationHash, err = fingerprint(struct {
		Policy, Contract, Version, Scope string
		Models                           []any
	}{job.Config.AuditPrompt, job.Config.FixedContract, job.Config.ContractVersion, job.Config.AuditScope, models})
	return target, err
}

func collectApplicationContext(root map[string]any, path string, result map[string]any) {
	for _, key := range []string{"tools", "tool_choice", "toolConfig", "tool_config"} {
		if value, exists := root[key]; exists {
			result[path+"."+key] = value
		}
	}
	if response, ok := root["response"].(map[string]any); ok {
		collectApplicationContext(response, path+".response", result)
	}
	if input, ok := root["input"].([]any); ok {
		for i, item := range input {
			entry, _ := item.(map[string]any)
			kind, _ := entry["type"].(string)
			if kind == "additional_tools" {
				result[fmt.Sprintf("%s.input[%d].tools", path, i)] = entry["tools"]
			}
		}
	}
	if requests, ok := root["requests"].([]any); ok {
		for i, item := range requests {
			if request, ok := item.(map[string]any); ok {
				collectApplicationContext(request, fmt.Sprintf("%s.requests[%d]", path, i), result)
			}
		}
	}
}

func modelSemantics(model ModelConfig) any {
	return struct {
		ID, BaseURL, Model string
		Parameters         map[string]any
	}{model.ID, model.BaseURL, model.Model, model.Parameters}
}

func segmentKey(snapshot ConfigSnapshot, model ModelConfig, segment Segment) (string, string, error) {
	content := make([]struct{ Type, Text string }, 0, len(segment.Content))
	for _, block := range segment.Content {
		content = append(content, struct{ Type, Text string }{block.Type, block.Text})
	}
	contentHash, err := fingerprint(content)
	if err != nil {
		return "", "", err
	}
	// 保留旧指纹字段但固定为空，使无会话历史缓存可继续命中，同时移除会话隔离语义。
	key, err := fingerprint(struct {
		Model                                                                                      any
		Policy, Contract, Version, SourceRole, PolicyRole, TurnScope, ContentHash, ConversationKey string
		Stage                                                                                      string
	}{modelSemantics(model), snapshot.AuditPrompt, snapshot.FixedContract, snapshot.ContractVersion,
		segment.SourceRole, segment.PolicyRole, segment.TurnScope, contentHash, "", "segment"})
	return key, contentHash, err
}

func (e *Evaluator) Evaluate(ctx context.Context, job *Job, keys map[string]string) (*Evaluation, *AuditError) {
	normalizeModelConcurrency(&job.Config.Config)
	if job.Config.historicalJSON != nil {
		return nil, &AuditError{Code: "audit_snapshot_requires_reaudit", Stage: "config", Message: "规则结构已更新，请按当前配置重新审核"}
	}
	if err := validateConfig(job.Config.Config, true); err != nil {
		return nil, &AuditError{Code: "invalid_config", Stage: "config", Message: err.Error()}
	}
	if job.Config.ContractVersion != ContractVersion || job.Config.FixedContract == "" {
		return nil, &AuditError{Code: "unsupported_contract", Stage: "config", Message: "任务审核协议不可用，请按当前配置重新审核"}
	}
	target, err := prepareTarget(job)
	if err != nil {
		return nil, &AuditError{Code: "input_parse_failed", Stage: "input_parse", Message: err.Error()}
	}
	if keys == nil {
		keys = map[string]string{}
	}
	var whole *Outcome
	if job.ReuseMode != ReuseModeForce {
		job.Reuse.WholeLookups++
		// 复用读取失败可回退评估；真实调用仍须先完成调用日志持久化。
		whole, _ = e.store.FindWholeResult(ctx, job)
	}
	cached := make(map[string]ModelResult)
	if whole != nil {
		for _, model := range whole.Models {
			cached[model.ModelID] = model
		}
	}
	result := &Evaluation{Models: []ModelResult{}}
	remaining := make([]ModelConfig, 0)
	allWhole := whole != nil
	enabledCount := 0
	for _, model := range job.Config.Models {
		if model.Enabled {
			enabledCount++
			remaining = append(remaining, model)
		}
	}
	blockThreshold := aggregationBlockThreshold(job.Config.Aggregation, enabledCount)
	blocks := 0
	for len(remaining) > 0 {
		modelIndex := -1
		for i, model := range remaining {
			if prior, exists := cached[model.ID]; exists && reusableModel(prior, job.Config.Config) {
				modelIndex = i
				break
			}
		}
		var model ModelConfig
		var release func()
		if modelIndex >= 0 {
			model = remaining[modelIndex]
		} else {
			var acquireErr error
			model, release, acquireErr = e.nodeScheduler().acquire(ctx, remaining)
			if acquireErr != nil {
				return nil, requestFailure(acquireErr)
			}
			for i := range remaining {
				if remaining[i].ID == model.ID {
					modelIndex = i
					break
				}
			}
		}
		remaining = append(remaining[:modelIndex], remaining[modelIndex+1:]...)
		if prior, exists := cached[model.ID]; exists && reusableModel(prior, job.Config.Config) {
			prior.Segments = append([]SegmentUse(nil), prior.Segments...)
			prior.ModelName = model.Name
			prior.Reused = true
			prior.JointAttemptID = nil
			if prior.Confidence != nil {
				prior.Decision = classifyScore(*prior.Confidence, job.Config.Config)
			}
			for i := range prior.Segments {
				prior.Segments[i].ReuseKind = "full_evaluation"
			}
			result.Models = append(result.Models, prior)
			if prior.Decision == DecisionBlock {
				blocks++
			}
			if blocks >= blockThreshold && job.Config.Aggregation != "all_block" {
				appendAggregationSkips(result, remaining, job)
				break
			}
			continue
		}
		allWhole = false
		nodeCtx, cancel := context.WithTimeout(ctx, time.Duration(model.TimeoutMS)*time.Millisecond)
		node := e.evaluateModel(nodeCtx, job, model, target, keys)
		cancel()
		release()
		if node.Error != nil && node.Error.Code == "audit_paused" {
			return nil, node.Error
		}
		result.Models = append(result.Models, node)
		if node.Decision == DecisionBlock {
			blocks++
		}
		if blocks >= blockThreshold && job.Config.Aggregation != "all_block" {
			appendAggregationSkips(result, remaining, job)
			break
		}
	}
	decision, partial, failure := AggregateResults(result.Models, job.Config.Config)
	if failure != nil {
		return nil, failure
	}
	result.Decision, result.PartialFailure = decision, partial
	if allWhole {
		job.Reuse.WholeHits++
		result.SourceOutcomeID = &whole.ID
	}
	return result, nil
}

func aggregationBlockThreshold(aggregation string, enabled int) int {
	switch aggregation {
	case "majority_block":
		return enabled/2 + 1
	case "all_block":
		return enabled
	default:
		return 1
	}
}

func appendAggregationSkips(result *Evaluation, models []ModelConfig, job *Job) {
	for _, model := range models {
		if !model.Enabled {
			continue
		}
		result.Models = append(result.Models, ModelResult{ModelID: model.ID, ModelName: model.Name, Basis: "aggregation_decided", Skipped: true, SkipReason: "aggregation_decided", Segments: []SegmentUse{}})
		job.Reuse.ShortCircuited++
	}
}

func reusableModel(model ModelResult, config Config) bool {
	if model.Error != nil {
		return false
	}
	if model.Confidence != nil {
		return true
	}
	if len(model.Segments) == 0 || model.Decision != DecisionPass {
		return false
	}
	for _, segment := range model.Segments {
		if segment.Result.Confidence >= *config.ReviewThreshold {
			return false
		}
	}
	return true
}

func (e *Evaluator) evaluateModel(ctx context.Context, job *Job, model ModelConfig, target auditTarget, credentials map[string]string) ModelResult {
	node := ModelResult{ModelID: model.ID, ModelName: model.Name, Segments: []SegmentUse{}}
	keys := make([]string, len(target.Messages))
	hashes := make([]string, len(target.Messages))
	for i, segment := range target.Messages {
		key, hash, err := segmentKey(job.Config, model, segment)
		if err != nil {
			node.Error = &AuditError{Code: "input_hash_failed", Stage: "input_parse", Message: err.Error()}
			return node
		}
		keys[i], hashes[i] = key, hash
	}
	stored := make(map[string]SegmentResult)
	if job.ReuseMode != ReuseModeForce {
		job.Reuse.SegmentLookups += len(keys)
		if found, err := e.store.FindSegments(ctx, model.ID, keys); err == nil {
			stored = found
		}
	}
	key := credentials[model.ID]
	client, url, err := nodeHTTPClient(model)
	if err != nil {
		node.Error = &AuditError{Code: "node_unavailable", Stage: "config", Message: err.Error()}
		return node
	}
	defer client.CloseIdleConnections()
	within := make(map[string]SegmentResult)
	jointRequired := false
	for i, segment := range target.Messages {
		var raw SegmentResult
		reuseKind := "fresh"
		if previous, ok := within[keys[i]]; ok {
			raw, reuseKind = previous, "within_job"
			job.Reuse.WithinJobHits++
		} else if previous, ok := stored[keys[i]]; ok {
			raw, reuseKind = previous, "history"
			job.Reuse.SegmentHits++
		} else {
			var failure *AuditError
			fresh := func() (SegmentResult, *AuditError) {
				order := segment.Order
				score, attemptID, failure := e.client.EvaluateTarget(ctx, job, model, key, job.Config, client, url, "segment", segment, &order)
				if failure != nil {
					return SegmentResult{}, failure
				}
				result := SegmentResult{UserID: job.UserID, ModelID: model.ID, AuditKey: keys[i], SourceAttemptID: attemptID,
					SourceRole: segment.SourceRole, PolicyRole: segment.PolicyRole, TurnScope: segment.TurnScope, ContentHash: hashes[i], Score: *score}
				if err := e.store.SaveSegment(ctx, job, &result); err != nil {
					return SegmentResult{}, persistenceFailure(err, "segment_persist_failed")
				}
				return result, nil
			}
			raw, reuseKind, failure = e.evaluateSegment(ctx, job, model.ID, keys[i], fresh)
			if failure != nil {
				node.Error = failure
				return node
			}
			if reuseKind == "history" {
				job.Reuse.SegmentHits++
			} else if reuseKind == "inflight" {
				job.Reuse.InflightHits++
			}
		}
		within[keys[i]] = raw
		node.Segments = append(node.Segments, SegmentUse{Order: segment.Order, SourcePath: segment.SourcePath, ReuseKind: reuseKind, Result: raw})
		if raw.Confidence >= *job.Config.ReviewThreshold {
			jointRequired = true
		}
	}
	if !jointRequired {
		node.Decision, node.Basis = DecisionPass, "segments_all_pass"
		return node
	}
	// 联合裁决仅接收完整目标，不携带片段高分来暗示模型维持原判。
	score, attemptID, failure := e.client.EvaluateTarget(ctx, job, model, key, job.Config, client, url, "joint", target, nil)
	if failure != nil {
		node.Error = failure
		return node
	}
	node.Confidence = &score.Confidence
	node.Reason = score.Reason
	node.JointAttemptID = &attemptID
	node.Basis = "joint"
	node.Decision = classifyScore(score.Confidence, job.Config.Config)
	return node
}

// evaluateSegment 合并当前实例内完全相同的首次片段审核，并在领头调用前再次查询全库缓存。
func (e *Evaluator) evaluateSegment(ctx context.Context, job *Job, modelID, auditKey string, fresh func() (SegmentResult, *AuditError)) (SegmentResult, string, *AuditError) {
	if job.ReuseMode == ReuseModeForce {
		result, failure := fresh()
		return result, "fresh", failure
	}
	e.segmentMu.Lock()
	if e.segmentFlights == nil {
		e.segmentFlights = make(map[string]*segmentFlight)
	}
	if current := e.segmentFlights[auditKey]; current != nil {
		e.segmentMu.Unlock()
		select {
		case <-current.done:
			if current.failure != nil {
				failure := *current.failure
				return SegmentResult{}, "inflight", &failure
			}
			return current.result, "inflight", nil
		case <-ctx.Done():
			return SegmentResult{}, "inflight", requestFailure(ctx.Err())
		}
	}
	flight := &segmentFlight{done: make(chan struct{})}
	e.segmentFlights[auditKey] = flight
	e.segmentMu.Unlock()

	reuseKind := "fresh"
	if found, err := e.store.FindSegments(ctx, modelID, []string{auditKey}); err == nil {
		if cached, ok := found[auditKey]; ok {
			flight.result = cached
			reuseKind = "history"
		} else {
			flight.result, flight.failure = fresh()
		}
	} else {
		flight.result, flight.failure = fresh()
	}
	e.segmentMu.Lock()
	delete(e.segmentFlights, auditKey)
	close(flight.done)
	e.segmentMu.Unlock()
	if flight.failure != nil {
		return SegmentResult{}, reuseKind, flight.failure
	}
	return flight.result, reuseKind, nil
}

func classifyScore(score float64, config Config) Decision {
	if score >= *config.BlockThreshold {
		return DecisionBlock
	}
	if score >= *config.ReviewThreshold {
		return DecisionReview
	}
	return DecisionPass
}

// AggregateResults 使用固定启用节点分母，未知票不能被静默当成通过或缩小分母。
func AggregateResults(models []ModelResult, config Config) (Decision, bool, *AuditError) {
	if len(models) == 0 {
		return "", false, &AuditError{Code: "no_models", Stage: "config", Message: "没有启用审核节点"}
	}
	threshold := aggregationBlockThreshold(config.Aggregation, len(models))
	switch config.Aggregation {
	case "majority_block", "all_block", "any_block":
	default:
		return "", false, &AuditError{Code: "invalid_aggregation", Stage: "config", Message: "聚合规则无效"}
	}
	blocks, reviews, failures, skipped := 0, 0, 0, 0
	var firstFailure *AuditError
	var retryAfter time.Duration
	for _, model := range models {
		if model.Skipped {
			skipped++
			continue
		}
		if model.Error != nil {
			if model.Error.RetryAfter > retryAfter {
				retryAfter = model.Error.RetryAfter
			}
			failures++
			if firstFailure == nil {
				copied := *model.Error
				firstFailure = &copied
			}
			continue
		}
		switch model.Decision {
		case DecisionBlock:
			blocks++
		case DecisionReview:
			reviews++
		case DecisionPass:
		default:
			return "", false, &AuditError{Code: "invalid_model_result", Stage: "aggregate", Message: "节点缺少完整有效的最终分类"}
		}
	}
	if blocks >= threshold {
		return DecisionBlock, failures > 0, nil
	}
	if skipped > 0 {
		return "", failures > 0, &AuditError{Code: "invalid_model_result", Stage: "aggregate", Message: "聚合尚未确定时不能跳过审核节点"}
	}
	if blocks+failures >= threshold {
		if firstFailure == nil {
			firstFailure = &AuditError{Code: "audit_unavailable", Stage: "aggregate", Message: "有效审核覆盖不足"}
		}
		firstFailure.RetryAfter = retryAfter
		return "", true, firstFailure
	}
	if blocks+reviews+failures > 0 {
		return DecisionReview, failures > 0, nil
	}
	return DecisionPass, false, nil
}

func cloneEvaluation(evaluation *Evaluation) (*Evaluation, error) {
	raw, err := json.Marshal(evaluation)
	if err != nil {
		return nil, err
	}
	var cloned Evaluation
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return nil, err
	}
	return &cloned, nil
}
