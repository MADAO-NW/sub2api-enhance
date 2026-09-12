package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrNoText 表示当前任务没有可审核的 user 文本，不能回退到历史内容。
var ErrNoText = errors.New("当前任务没有可审核的 user 文本")

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
	Protocol           string    `json:"protocol"`
	CurrentUser        []Segment `json:"current_user"`
	InstructionContext []Segment `json:"instruction_context"`
	EffectiveBehavior  []Segment `json:"effective_behavior,omitempty"`
}

type messageBundle struct {
	Protocol string    `json:"protocol"`
	Messages []Segment `json:"messages"`
}

type intentBindingTarget struct {
	CurrentUser        messageBundle `json:"current_user"`
	InstructionContext messageBundle `json:"instruction_context"`
}

type auditEnvelope struct {
	Stage  string `json:"audit_stage"`
	Target any    `json:"target"`
}

// prepareTarget 只选择最新 user 消息和生效指令，同时保留完整角色清单供详情解释。
func prepareTarget(job *Job) (auditTarget, error) {
	return prepareTargetForContract(job, ContractVersion)
}

// prepareTargetForContract 按任务冻结的协议语义重建目标，避免历史轮次被新规则错误解释。
func prepareTargetForContract(job *Job, contractVersion string) (auditTarget, error) {
	if contractVersion != ContractVersion && contractVersion != previousEffectiveBehaviorContractVersion && contractVersion != previousLatestUserContractVersion && contractVersion != previousCurrentUserContractVersion {
		return auditTarget{}, errors.New("不支持重建该历史审核协议的分阶段目标")
	}
	latestUserBehavior := contractVersion == ContractVersion || contractVersion == previousEffectiveBehaviorContractVersion
	segments, err := ExtractSegments(job.FullInput, "full_request")
	if err != nil {
		return auditTarget{}, err
	}
	latestMessageKey := ""
	if latestUserBehavior || contractVersion == previousLatestUserContractVersion {
		for i := len(segments) - 1; i >= 0; i-- {
			hasText := false
			for _, block := range segments[i].Content {
				if block.Text != "" {
					hasText = true
					break
				}
			}
			if segments[i].SourceRole == "user" && segments[i].TurnScope == "current" && hasText {
				latestMessageKey = segments[i].logicalMessageKey
				break
			}
		}
	}
	target := auditTarget{Protocol: job.Protocol, CurrentUser: []Segment{}, InstructionContext: []Segment{}, EffectiveBehavior: []Segment{}}
	latestUserOrder := 0
	job.Manifest = make([]SegmentMeta, 0, len(segments))
	for i := range segments {
		segment := segments[i]
		blocks := make([]TextBlock, 0, len(segment.Content))
		for _, block := range segment.Content {
			if block.Text != "" {
				blocks = append(blocks, block)
			}
		}
		segment.Content = blocks
		segment.Selected = false
		switch {
		case segment.SourceRole == "user" && segment.TurnScope == "current" && len(blocks) > 0 && (contractVersion == previousCurrentUserContractVersion || segment.logicalMessageKey == latestMessageKey):
			segment.Selected = true
			segment.SelectionKind = TargetKindCurrentUser
			if latestUserBehavior || contractVersion == previousLatestUserContractVersion {
				segment.SelectionReason = "latest_user"
			} else {
				segment.SelectionReason = "current_user_bundle"
			}
			target.CurrentUser = append(target.CurrentUser, segment)
			if segment.Order > latestUserOrder {
				latestUserOrder = segment.Order
			}
		case (segment.SourceRole == "system" || segment.SourceRole == "developer") && len(blocks) > 0:
			segment.Selected = true
			segment.SelectionKind = TargetKindInstructionContext
			segment.SelectionReason = "active_instruction"
			target.InstructionContext = append(target.InstructionContext, segment)
		case latestUserBehavior && segment.TurnScope == "current" && segment.Order > latestUserOrder && isEffectiveBehaviorCandidate(segment):
			segment.Selected = true
			segment.SelectionKind = TargetKindEffectiveBehavior
			segment.SelectionReason = "effective_behavior_delta"
			target.EffectiveBehavior = append(target.EffectiveBehavior, segment)
		case len(blocks) == 0:
			segment.SelectionKind = "excluded"
			segment.SelectionReason = "empty"
		case segment.SourceRole == "user" && segment.TurnScope == "current":
			segment.SelectionKind = "excluded"
			segment.SelectionReason = "earlier_current_user"
		case segment.SourceRole == "user":
			segment.SelectionKind = "excluded"
			segment.SelectionReason = "historical"
		default:
			segment.SelectionKind = "excluded"
			segment.SelectionReason = "assistant_or_tool"
		}
		segments[i] = segment
		job.Manifest = append(job.Manifest, segment.SegmentMeta)
	}
	if len(target.CurrentUser) == 0 {
		return target, ErrNoText
	}
	job.InputHash, err = fingerprint(job.FullInput)
	if err != nil {
		return target, err
	}
	targetStage, scope := "current_user_behavior_context_guard", "current_user_behavior"
	if latestUserBehavior {
		targetStage, scope = "latest_user_behavior_context_guard", "latest_user_behavior"
	} else if contractVersion == previousLatestUserContractVersion {
		targetStage, scope = "latest_user_context_guard", "latest_user"
	} else {
		targetStage, scope = "current_user_context_guard", "current_user"
	}
	job.TargetHash, err = fingerprint(auditEnvelope{Stage: targetStage, Target: target})
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
	}{job.Config.AuditPrompt, job.Config.FixedContract, job.Config.ContractVersion, scope, models})
	return target, err
}

// isEffectiveBehaviorCandidate 只保留可能改变下游实际行为的新增内容，跳过普通分析和重复历史。
func isEffectiveBehaviorCandidate(segment Segment) bool {
	if segment.SourceRole == "user" {
		return false
	}
	for _, block := range segment.Content {
		blockType := strings.ToLower(block.Type)
		if block.Type == "tool_data" || strings.Contains(blockType, "tool") {
			// 已经返回的工具结果只有在包含可执行/外发信号时才进入审核，避免为普通查询结果重复付费。
			if segment.SourceRole != "tool" {
				return true
			}
			text := strings.ToLower(block.Text)
			for _, wrapper := range []string{"custom_tool_call_output", "function_call_output", "tool_result", "tool_call", "function_call"} {
				text = strings.ReplaceAll(text, wrapper, "")
			}
			if !containsBehaviorSignal(text) {
				continue
			}
			return true
		}
		text := strings.ToLower(block.Text)
		if containsBehaviorSignal(text) {
			return true
		}
	}
	return false
}

func containsBehaviorSignal(text string) bool {
	for _, marker := range []string{
		"exec_command", "function_call", "tool_call", "curl ", "wget ", "nmap ", "ssh ", "reverse shell", "command and control", "c2 ",
		"execute", "run ", "upload", "download", "exfiltrat", "credential", "secret", "bypass", "ignore safety",
		"读取", "写入", "修改", "删除", "执行", "发送", "调用", "访问", "绕过", "突破", "外传", "上传", "下载", "凭据", "密钥", "反向连接", "忽略规则", "生成代码", "生成脚本",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return strings.Contains(text, "```") || strings.Contains(text, "http://") || strings.Contains(text, "https://")
}

func modelSemantics(model ModelConfig) any {
	return struct {
		ID, BaseURL, Model string
		Parameters         map[string]any
	}{model.ID, model.BaseURL, model.Model, model.Parameters}
}

func targetKey(snapshot ConfigSnapshot, model ModelConfig, kind string, target any) (string, string, error) {
	contentHash, err := fingerprint(target)
	if err != nil {
		return "", "", err
	}
	key, err := fingerprint(struct {
		Model                                        any
		Policy, Contract, Version, Kind, ContentHash string
	}{modelSemantics(model), snapshot.AuditPrompt, snapshot.FixedContract, snapshot.ContractVersion, kind, contentHash})
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
	targetCacheChecked := make(map[string]bool)
	targetCached := make(map[string]ModelResult)
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
		selection := ""
		for i, model := range remaining {
			if prior, exists := cached[model.ID]; exists && reusableModel(prior, job.Config.Config, len(target.InstructionContext) > 0) {
				modelIndex = i
				selection = "whole"
				break
			}
		}
		if modelIndex < 0 && job.ReuseMode != ReuseModeForce {
			for i, model := range remaining {
				if !targetCacheChecked[model.ID] {
					targetCacheChecked[model.ID] = true
					if prior, ok := e.cachedModel(ctx, job, model, target); ok {
						targetCached[model.ID] = prior
					}
				}
				if _, exists := targetCached[model.ID]; exists {
					modelIndex = i
					selection = "target"
					break
				}
			}
		}
		var model ModelConfig
		var release func()
		var dispatch DispatchSnapshot
		if modelIndex >= 0 {
			model = remaining[modelIndex]
		} else {
			acquireCtx := ctx
			var acquireCancel context.CancelFunc
			waitForCapacity := job.ExecutionMode == "blocking"
			if waitForCapacity {
				acquireCtx, acquireCancel = context.WithTimeout(ctx, 5*time.Second)
			}
			var acquireErr error
			model, release, dispatch, acquireErr = e.nodeScheduler().acquireWithPolicy(acquireCtx, remaining, waitForCapacity)
			if acquireCancel != nil {
				acquireCancel()
			}
			if acquireErr != nil {
				var scheduleErr *nodeScheduleError
				if errors.As(acquireErr, &scheduleErr) {
					return nil, &AuditError{Code: scheduleErr.Code, Stage: "scheduler", Message: "审核节点暂不可调度", Retryable: scheduleErr.Code != "no_healthy_nodes", RetryAfter: scheduleErr.RetryAfter}
				}
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
		if selection == "whole" {
			prior := cached[model.ID]
			prior.Segments = append([]SegmentUse(nil), prior.Segments...)
			prior.TargetUses = append([]SegmentUse(nil), prior.TargetUses...)
			prior.BehaviorUses = append([]SegmentUse(nil), prior.BehaviorUses...)
			prior.ModelName = model.Name
			prior.Reused = true
			prior.JointAttemptID = nil
			prior.Dispatch = nil
			reclassifyReusableModel(&prior, job.Config.Config, len(target.InstructionContext) > 0)
			for i := range prior.Segments {
				prior.Segments[i].ReuseKind = "full_evaluation"
			}
			for i := range prior.TargetUses {
				prior.TargetUses[i].ReuseKind = "full_evaluation"
			}
			for i := range prior.BehaviorUses {
				prior.BehaviorUses[i].ReuseKind = "full_evaluation"
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
		if selection == "target" {
			allWhole = false
			prior := targetCached[model.ID]
			prior.ModelName = model.Name
			prior.Reused = true
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
		dispatch.Order = len(result.Models) + 1
		node.Dispatch = &dispatch
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
	result.BehaviorDecision, result.BehaviorConfidence, result.BehaviorReason, result.BehaviorUnavailable = aggregateBehaviorResults(result.Models)
	if allWhole {
		job.Reuse.WholeHits++
		result.SourceOutcomeID = &whole.ID
	}
	return result, nil
}

func aggregateBehaviorResults(models []ModelResult) (Decision, *float64, string, bool) {
	decision := DecisionPass
	var confidence *float64
	reason := ""
	active, unavailable := false, false
	for _, model := range models {
		if len(model.BehaviorUses) == 0 && model.BehaviorError == nil {
			continue
		}
		active = true
		if model.BehaviorError != nil {
			unavailable = true
		}
		if model.BehaviorDecision == DecisionBlock {
			decision = DecisionBlock
		} else if model.BehaviorDecision == DecisionReview && decision != DecisionBlock {
			decision = DecisionReview
		}
		if model.BehaviorConfidence != nil && (confidence == nil || *model.BehaviorConfidence > *confidence) {
			score := *model.BehaviorConfidence
			confidence = &score
			reason = model.BehaviorReason
		}
	}
	if !active {
		return "", nil, "", false
	}
	return decision, confidence, reason, unavailable
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

func reusableModel(model ModelResult, config Config, hasInstructionContext bool) bool {
	return reclassifyReusableModel(&model, config, hasInstructionContext)
}

// reclassifyReusableModel 仅在缓存已包含当前阈值要求的条件阶段时复用整节点结论。
func reclassifyReusableModel(model *ModelResult, config Config, hasInstructionContext bool) bool {
	if model.Error != nil {
		return false
	}
	uses := model.TargetUses
	if len(uses) == 0 {
		uses = model.Segments
	}
	if len(uses) == 0 {
		return false
	}
	var current, instructions, binding *SegmentUse
	for i := range uses {
		switch uses[i].TargetKind {
		case TargetKindCurrentUser:
			current = &uses[i]
		case TargetKindInstructionContext:
			instructions = &uses[i]
		case TargetKindIntentBinding:
			binding = &uses[i]
		}
	}
	if current == nil {
		return model.Confidence != nil
	}
	selected := current
	requiredUses := []SegmentUse{*current}
	basis := TargetKindCurrentUser
	if current.Result.Confidence < *config.BlockThreshold && hasInstructionContext {
		if instructions == nil {
			return false
		}
		requiredUses = append(requiredUses, *instructions)
		if instructions.Result.Confidence >= *config.ReviewThreshold {
			if binding == nil {
				return false
			}
			requiredUses = append(requiredUses, *binding)
			selected, basis = binding, TargetKindIntentBinding
		}
	}
	confidence := selected.Result.Confidence
	model.Confidence, model.Reason, model.Basis = &confidence, selected.Result.Reason, basis
	model.Decision = classifyScore(confidence, config)
	model.BindingTriggered = basis == TargetKindIntentBinding
	model.TargetUses = append([]SegmentUse(nil), requiredUses...)
	model.Segments = []SegmentUse{}
	if model.BehaviorConfidence != nil {
		model.BehaviorDecision = classifyScore(*model.BehaviorConfidence, config)
	}
	return true
}

// cachedModel 在占用节点容量前确认当前阈值所需的全部目标均已缓存或正在同实例生成。
func (e *Evaluator) cachedModel(ctx context.Context, job *Job, model ModelConfig, target auditTarget) (ModelResult, bool) {
	node := ModelResult{ModelID: model.ID, ModelName: model.Name, Segments: []SegmentUse{}, TargetUses: []SegmentUse{}}
	load := func(kind string, value any, order int) (SegmentUse, bool, *AuditError) {
		auditKey, _, err := targetKey(job.Config, model, kind, value)
		if err != nil {
			return SegmentUse{}, false, &AuditError{Code: "input_hash_failed", Stage: "input_parse", Message: err.Error()}
		}
		if found, err := e.store.FindSegments(ctx, model.ID, []string{auditKey}); err == nil {
			if result, exists := found[auditKey]; exists {
				if result.TargetKind == "" {
					result.TargetKind = kind
				}
				return SegmentUse{Order: order, SourcePath: kind, TargetKind: kind, ReuseKind: "history", Result: result}, true, nil
			}
		}
		e.segmentMu.Lock()
		flight := e.segmentFlights[auditKey]
		e.segmentMu.Unlock()
		if flight == nil {
			return SegmentUse{}, false, nil
		}
		select {
		case <-flight.done:
			if flight.failure != nil {
				failure := *flight.failure
				return SegmentUse{}, true, &failure
			}
			result := flight.result
			if result.TargetKind == "" {
				result.TargetKind = kind
			}
			return SegmentUse{Order: order, SourcePath: kind, TargetKind: kind, ReuseKind: "inflight", Result: result}, true, nil
		case <-ctx.Done():
			return SegmentUse{}, true, requestFailure(ctx.Err())
		}
	}

	currentTarget := messageBundle{Protocol: target.Protocol, Messages: target.CurrentUser}
	current, exists, failure := load(TargetKindCurrentUser, currentTarget, 1)
	if failure != nil {
		node.Error = failure
		return node, true
	}
	if !exists {
		return ModelResult{}, false
	}
	node.TargetUses = append(node.TargetUses, current)
	if current.Result.Confidence >= *job.Config.BlockThreshold {
		node.Confidence, node.Reason, node.Basis = &current.Result.Confidence, current.Result.Reason, TargetKindCurrentUser
		node.Decision = DecisionBlock
	} else if len(target.InstructionContext) == 0 {
		node.Confidence, node.Reason, node.Basis = &current.Result.Confidence, current.Result.Reason, TargetKindCurrentUser
		node.Decision = classifyScore(current.Result.Confidence, job.Config.Config)
	} else {
		instructionTarget := messageBundle{Protocol: target.Protocol, Messages: target.InstructionContext}
		instructions, found, instructionFailure := load(TargetKindInstructionContext, instructionTarget, 2)
		if instructionFailure != nil {
			node.Error = instructionFailure
			return node, true
		}
		if !found {
			return ModelResult{}, false
		}
		node.TargetUses = append(node.TargetUses, instructions)
		if instructions.Result.Confidence < *job.Config.ReviewThreshold {
			node.Confidence, node.Reason, node.Basis = &current.Result.Confidence, current.Result.Reason, TargetKindCurrentUser
			node.Decision = classifyScore(current.Result.Confidence, job.Config.Config)
		} else {
			bindingTarget := intentBindingTarget{CurrentUser: currentTarget, InstructionContext: instructionTarget}
			binding, found, bindingFailure := load(TargetKindIntentBinding, bindingTarget, 3)
			if bindingFailure != nil {
				node.Error = bindingFailure
				return node, true
			}
			if !found {
				return ModelResult{}, false
			}
			node.TargetUses = append(node.TargetUses, binding)
			node.BindingTriggered = true
			node.Confidence, node.Reason, node.Basis = &binding.Result.Confidence, binding.Result.Reason, TargetKindIntentBinding
			node.Decision = classifyScore(binding.Result.Confidence, job.Config.Config)
		}
	}
	for index := range target.EffectiveBehavior {
		behaviorTarget := messageBundle{Protocol: target.Protocol, Messages: []Segment{target.EffectiveBehavior[index]}}
		behavior, found, behaviorFailure := load(TargetKindEffectiveBehavior, behaviorTarget, index+1)
		if behaviorFailure != nil {
			node.BehaviorError = behaviorFailure
			return node, true
		}
		if !found {
			return ModelResult{}, false
		}
		node.BehaviorUses = append(node.BehaviorUses, behavior)
		if node.BehaviorConfidence == nil || behavior.Result.Confidence > *node.BehaviorConfidence {
			score := behavior.Result.Confidence
			node.BehaviorConfidence = &score
			node.BehaviorReason = behavior.Result.Reason
		}
		decision := classifyScore(behavior.Result.Confidence, job.Config.Config)
		if decision == DecisionBlock || (decision == DecisionReview && node.BehaviorDecision != DecisionBlock) {
			node.BehaviorDecision = decision
		}
	}
	if node.BehaviorConfidence != nil && node.BehaviorDecision == "" {
		node.BehaviorDecision = DecisionPass
	}
	job.Reuse.SegmentLookups += len(node.TargetUses)
	for _, use := range node.TargetUses {
		if use.ReuseKind == "history" {
			job.Reuse.SegmentHits++
		} else if use.ReuseKind == "inflight" {
			job.Reuse.InflightHits++
		}
	}
	job.Reuse.SegmentLookups += len(node.BehaviorUses)
	for _, use := range node.BehaviorUses {
		if use.ReuseKind == "history" {
			job.Reuse.SegmentHits++
		} else if use.ReuseKind == "inflight" {
			job.Reuse.InflightHits++
		}
	}
	return node, true
}

func (e *Evaluator) evaluateModel(ctx context.Context, job *Job, model ModelConfig, target auditTarget, credentials map[string]string) ModelResult {
	node := ModelResult{ModelID: model.ID, ModelName: model.Name, Segments: []SegmentUse{}, TargetUses: []SegmentUse{}}
	key := credentials[model.ID]
	client, url, err := nodeHTTPClient(model)
	if err != nil {
		node.Error = &AuditError{Code: "node_unavailable", Stage: "config", Message: err.Error()}
		e.nodeScheduler().observe(model.ID, node.Error, 0)
		return node
	}
	defer client.CloseIdleConnections()
	current := messageBundle{Protocol: target.Protocol, Messages: target.CurrentUser}
	currentUse, failure := e.evaluateAuditTarget(ctx, job, model, key, client, url, TargetKindCurrentUser, current, 1)
	if failure != nil {
		node.Error = failure
		return node
	}
	node.TargetUses = append(node.TargetUses, currentUse)
	if currentUse.Result.Confidence >= *job.Config.BlockThreshold {
		node.Confidence, node.Reason, node.Basis = &currentUse.Result.Confidence, currentUse.Result.Reason, TargetKindCurrentUser
		node.Decision = DecisionBlock
		return node
	}
	if len(target.InstructionContext) == 0 {
		node.Confidence, node.Reason, node.Basis = &currentUse.Result.Confidence, currentUse.Result.Reason, TargetKindCurrentUser
		node.Decision = classifyScore(currentUse.Result.Confidence, job.Config.Config)
		return e.attachBehavior(ctx, job, model, key, client, url, target, node)
	}
	instructions := messageBundle{Protocol: target.Protocol, Messages: target.InstructionContext}
	contextUse, failure := e.evaluateAuditTarget(ctx, job, model, key, client, url, TargetKindInstructionContext, instructions, 2)
	if failure != nil {
		node.Error = failure
		return node
	}
	node.TargetUses = append(node.TargetUses, contextUse)
	if contextUse.Result.Confidence < *job.Config.ReviewThreshold {
		node.Confidence, node.Reason, node.Basis = &currentUse.Result.Confidence, currentUse.Result.Reason, TargetKindCurrentUser
		node.Decision = classifyScore(currentUse.Result.Confidence, job.Config.Config)
		return e.attachBehavior(ctx, job, model, key, client, url, target, node)
	}
	node.BindingTriggered = true
	binding := intentBindingTarget{CurrentUser: current, InstructionContext: instructions}
	bindingUse, failure := e.evaluateAuditTarget(ctx, job, model, key, client, url, TargetKindIntentBinding, binding, 3)
	if failure != nil {
		node.Error = failure
		return node
	}
	node.TargetUses = append(node.TargetUses, bindingUse)
	node.Confidence, node.Reason, node.Basis = &bindingUse.Result.Confidence, bindingUse.Result.Reason, TargetKindIntentBinding
	node.Decision = classifyScore(bindingUse.Result.Confidence, job.Config.Config)
	return e.attachBehavior(ctx, job, model, key, client, url, target, node)
}

// attachBehavior 逐个审核新增的有效行为候选，避免将 Agent/tool 内容并入用户结论。
func (e *Evaluator) attachBehavior(ctx context.Context, job *Job, model ModelConfig, key string, client *http.Client, url string, target auditTarget, node ModelResult) ModelResult {
	if len(target.EffectiveBehavior) == 0 {
		return node
	}
	for index, segment := range target.EffectiveBehavior {
		bundle := messageBundle{Protocol: target.Protocol, Messages: []Segment{segment}}
		use, failure := e.evaluateAuditTarget(ctx, job, model, key, client, url, TargetKindEffectiveBehavior, bundle, index+1)
		if failure != nil {
			node.BehaviorError = failure
			continue
		}
		node.BehaviorUses = append(node.BehaviorUses, use)
		confidence := use.Result.Confidence
		if node.BehaviorConfidence == nil || confidence > *node.BehaviorConfidence {
			node.BehaviorConfidence = &confidence
			node.BehaviorReason = use.Result.Reason
		}
		decision := classifyScore(confidence, job.Config.Config)
		if decision == DecisionBlock || (decision == DecisionReview && node.BehaviorDecision != DecisionBlock) {
			node.BehaviorDecision = decision
		}
	}
	if node.BehaviorConfidence != nil && node.BehaviorDecision == "" {
		node.BehaviorDecision = DecisionPass
	}
	return node
}

// evaluateAuditTarget 对结构化目标统一执行全库复用、并发合并、调用和结果持久化。
func (e *Evaluator) evaluateAuditTarget(ctx context.Context, job *Job, model ModelConfig, credential string, client *http.Client, url, kind string, target any, order int) (SegmentUse, *AuditError) {
	auditKey, contentHash, err := targetKey(job.Config, model, kind, target)
	if err != nil {
		return SegmentUse{}, &AuditError{Code: "input_hash_failed", Stage: "input_parse", Message: err.Error()}
	}
	job.Reuse.SegmentLookups++
	fresh := func() (SegmentResult, *AuditError) {
		started := time.Now()
		score, attemptID, failure := e.client.EvaluateTarget(ctx, job, model, credential, job.Config, client, url, kind, target, &order)
		job.Dispatched = failure == nil || (failure.Code != "audit_paused" && failure.Code != "user_excluded")
		e.nodeScheduler().observe(model.ID, failure, time.Since(started))
		if failure != nil {
			return SegmentResult{}, failure
		}
		sourceRole, policyRole, turnScope := "user", "user", "current"
		if kind == TargetKindInstructionContext {
			sourceRole, policyRole, turnScope = "system/developer", "instruction", "active"
		} else if kind == TargetKindEffectiveBehavior {
			sourceRole, policyRole = "agent/model/tool", "effective_behavior"
		}
		result := SegmentResult{UserID: job.UserID, ModelID: model.ID, AuditKey: auditKey, SourceAttemptID: attemptID,
			SourceRole: sourceRole, PolicyRole: policyRole, TurnScope: turnScope,
			ContentHash: contentHash, TargetKind: kind, Score: *score}
		if err := e.store.SaveSegment(ctx, job, &result); err != nil {
			return SegmentResult{}, persistenceFailure(err, "target_persist_failed")
		}
		return result, nil
	}
	if job.ReuseMode == ReuseModeForce {
		job.Reuse.SegmentLookups--
	}
	result, reuseKind, failure := e.evaluateSegment(ctx, job, model.ID, auditKey, fresh)
	if failure != nil {
		return SegmentUse{}, failure
	}
	if reuseKind == "history" || reuseKind == "force_batch" {
		job.Reuse.SegmentHits++
	} else if reuseKind == "inflight" {
		job.Reuse.InflightHits++
	}
	return SegmentUse{Order: order, SourcePath: kind, TargetKind: kind, ReuseKind: reuseKind, Result: result}, nil
}

// evaluateSegment 合并当前实例内完全相同的首次目标审核，并在领头调用前再次查询全库缓存。
func (e *Evaluator) evaluateSegment(ctx context.Context, job *Job, modelID, auditKey string, fresh func() (SegmentResult, *AuditError)) (SegmentResult, string, *AuditError) {
	if job.ReuseMode == ReuseModeForce {
		if job.ReauditBatchID != "" {
			return e.evaluateForceBatchSegment(ctx, job, modelID, auditKey, fresh)
		}
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

// evaluateForceBatchSegment 为同一强制复核批次协调首个真实调用，并复用其成功的全局片段结果。
func (e *Evaluator) evaluateForceBatchSegment(ctx context.Context, job *Job, modelID, auditKey string, fresh func() (SegmentResult, *AuditError)) (SegmentResult, string, *AuditError) {
	flightKey := "force:" + job.ReauditBatchID + ":" + auditKey
	e.segmentMu.Lock()
	if e.segmentFlights == nil {
		e.segmentFlights = make(map[string]*segmentFlight)
	}
	if current := e.segmentFlights[flightKey]; current != nil {
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
	e.segmentFlights[flightKey] = flight
	e.segmentMu.Unlock()

	coordinator, coordinated := e.store.(forceBatchCoordinator)
	result, reuseKind := SegmentResult{}, "fresh"
	var failure *AuditError
	if !coordinated {
		result, failure = fresh()
	} else {
		for failure == nil {
			claimed, err := coordinator.ClaimForceTarget(ctx, job.ReauditBatchID, auditKey)
			if err != nil {
				failure = &AuditError{Code: "force_batch_coordination_failed", Stage: "worker", Message: err.Error(), Retryable: true}
				break
			}
			if claimed {
				result, failure = fresh()
				if failure == nil {
					// SaveSegment 已先写入全局片段表，再发布批次内成功标记。
					_ = coordinator.CompleteForceTarget(ctx, job.ReauditBatchID, auditKey)
				} else {
					_ = coordinator.ReleaseForceTarget(context.WithoutCancel(ctx), job.ReauditBatchID, auditKey)
				}
				break
			}
			state, err := coordinator.ForceTargetState(ctx, job.ReauditBatchID, auditKey)
			if err != nil {
				failure = &AuditError{Code: "force_batch_coordination_failed", Stage: "worker", Message: err.Error(), Retryable: true}
				break
			}
			if state == "succeeded" {
				found, findErr := e.store.FindSegments(ctx, modelID, []string{auditKey})
				if findErr != nil {
					failure = &AuditError{Code: "force_batch_result_lookup_failed", Stage: "result_persist", Message: findErr.Error(), Retryable: true}
					break
				}
				if cached, ok := found[auditKey]; ok {
					result, reuseKind = cached, "force_batch"
					break
				}
			}
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				failure = requestFailure(ctx.Err())
			case <-timer.C:
			}
		}
	}

	e.segmentMu.Lock()
	delete(e.segmentFlights, flightKey)
	flight.result, flight.failure = result, failure
	close(flight.done)
	e.segmentMu.Unlock()
	if failure != nil {
		return SegmentResult{}, reuseKind, failure
	}
	return result, reuseKind, nil
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
