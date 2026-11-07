package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"sub2api-enhance/internal/pkg/logger"
)

// nodeCapacityWait 给同步审核等待节点容量的时间：跟随最慢启用节点的预算，
// 但不短于 5 秒、不超过 30 秒，避免单节点被占满时同步请求立刻 503。
func nodeCapacityWait(models []ModelConfig) time.Duration {
	wait := 5 * time.Second
	for _, model := range models {
		if !model.Enabled {
			continue
		}
		if candidate := time.Duration(model.TimeoutMS) * time.Millisecond; candidate > wait {
			wait = candidate
		}
	}
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	return wait
}

// Evaluator 按“单个目标”调度审核节点：每个目标单独调用模型，投票结果按当前规则聚合。
type Evaluator struct {
	repo   *Repository
	client *ModelClient
	node   *nodeScheduler
}

func NewEvaluator(repository *Repository, client *ModelClient) *Evaluator {
	return &Evaluator{repo: repository, client: client, node: newNodeScheduler()}
}

func (e *Evaluator) nodeScheduler() *nodeScheduler { return e.node }

// updateNodeLimits 把最新节点并发上限发布给进程内调度器。
func (e *Evaluator) updateNodeLimits(models []ModelConfig) { e.node.updateLimits(models) }

// nodeSemantics 是进入缓存键的节点语义，不包含凭据。
func nodeSemantics(model ModelConfig) any {
	parameters, err := json.Marshal(model.Parameters)
	if err != nil {
		parameters = []byte("{}")
	}
	return struct {
		ID, BaseURL, Model, Parameters, TimeoutMS string
	}{model.ID, model.BaseURL, model.Model, string(parameters), itoa(model.TimeoutMS)}
}

func itoa(value int) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// evaluationHash 覆盖政策、固定协议、目标类型和启用节点语义；不含原请求协议，允许跨协议复用（决策 16）。
func evaluationHash(snapshot ConfigSnapshot, kind string) (string, error) {
	nodes := make([]any, 0, len(snapshot.Models))
	for _, model := range snapshot.Models {
		if model.Enabled {
			nodes = append(nodes, nodeSemantics(model))
		}
	}
	return fingerprint(struct {
		Policy, Contract, Version, Kind string
		Nodes                           []any
	}{snapshot.AuditPrompt, snapshot.FixedContract, snapshot.ContractVersion, kind, nodes})
}

// reviewOutcome 是一个目标本次得到的结论及其来源。
type reviewOutcome struct {
	review     *PromptReview
	resolution string
}

// Review 审核一个目标：缓存与并发复用不占节点并发位，真实调用按节点并发调度并更新节点健康。
func (e *Evaluator) Review(ctx context.Context, job *AuditJob, target *JobTarget, keys map[string]string) (*reviewOutcome, *AuditError) {
	normalizeModelConcurrency(&job.Config.Config)
	if err := validateConfig(job.Config.Config, true); err != nil {
		return nil, &AuditError{Code: "invalid_config", Stage: "config", Message: err.Error()}
	}
	if job.Config.ContractVersion != ContractVersion || job.Config.FixedContract == "" {
		return nil, &AuditError{Code: "unsupported_contract", Stage: "config", Message: "任务审核协议不可用，请按当前配置重新审核"}
	}
	evalHash, err := evaluationHash(job.Config, target.Kind)
	if err != nil {
		return nil, &AuditError{Code: "evaluation_hash_failed", Stage: "config", Message: err.Error()}
	}
	// 重新审核只复用发起之后产生的结论；业务请求传 nil，行为不变。
	cached, lookupErr := e.repo.FindReusableReview(ctx, target.Hash, evalHash, job.ReuseAfter)
	if lookupErr == nil && cached != nil {
		// 复用时按当前任务冻结的阈值和聚合规则重新计算结论。
		decision, _, aggregateErr := aggregateVotes(votesFrom(cached.Nodes, job.Config.Config), job.Config.Config)
		if aggregateErr == nil {
			cached.Decision = decision
			return &reviewOutcome{review: cached, resolution: ResolutionCache}, nil
		}
	}
	models := make([]ModelConfig, 0, len(job.Config.Models))
	for _, model := range job.Config.Models {
		if model.Enabled {
			models = append(models, model)
		}
	}
	if len(models) == 0 {
		return nil, &AuditError{Code: "no_models", Stage: "config", Message: "没有启用审核节点"}
	}
	blockThreshold := aggregationBlockThreshold(job.Config.Aggregation, len(models))
	remaining := append([]ModelConfig(nil), models...)
	results := make([]NodeResult, 0, len(models))
	blocks := 0

	// 阶段 1：不等待容量地抢占节点；选择与摘除串行执行，保证同一次执行不重复选择同一节点。
	type reserved struct {
		model    ModelConfig
		release  func()
		dispatch DispatchSnapshot
	}
	held := make([]reserved, 0, len(models))
	for len(remaining) > 0 {
		model, release, dispatch, acquireErr := e.node.acquireWithPolicy(ctx, remaining, false)
		if acquireErr != nil {
			break
		}
		held = append(held, reserved{model: model, release: release, dispatch: dispatch})
		remaining = slices.DeleteFunc(remaining, func(candidate ModelConfig) bool { return candidate.ID == model.ID })
	}

	// 阶段 2：已抢到容量的节点并行调用；任一节点判定违规后仍按聚合规则决定是否提前结束。
	if len(held) > 0 {
		records := make([]*NodeResult, len(held))
		failures := make([]*AuditError, len(held))
		var wait sync.WaitGroup
		for index := range held {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				defer held[index].release()
				records[index], failures[index] = e.runNode(ctx, job, target, keys, held[index].model, held[index].dispatch)
			}(index)
		}
		wait.Wait()
		for index := range held {
			if failures[index] != nil {
				return nil, failures[index]
			}
		}
		for index := range held {
			record := records[index]
			results = append(results, *record)
			if record.Error == nil && record.Confidence != nil && classifyScore(*record.Confidence, job.Config.Config) == DecisionBlock {
				blocks++
			}
		}
	}

	// 阶段 3：容量不足的节点按原有语义（同步模式等待 5 秒）逐个补足。
	for len(remaining) > 0 && !(blocks >= blockThreshold && job.Config.Aggregation != "all_block") {
		if ctx.Err() != nil {
			return nil, requestFailure(ctx.Err())
		}
		record, stop := e.callNode(ctx, job, target, keys, &remaining)
		if record == nil {
			return nil, stop
		}
		results = append(results, *record)
		if record.Error == nil && record.Confidence != nil && classifyScore(*record.Confidence, job.Config.Config) == DecisionBlock {
			blocks++
		}
	}
	if blocks >= blockThreshold && job.Config.Aggregation != "all_block" {
		for _, model := range remaining {
			results = append(results, NodeResult{ModelID: model.ID, ModelName: model.Name, Skipped: true, SkipReason: "aggregation_reached"})
		}
	}
	votes := votesFrom(results, job.Config.Config)
	decision, partial, failure := aggregateVotes(votes, job.Config.Config)
	if failure != nil {
		return nil, failure
	}
	review := &PromptReview{Hash: target.Hash, EvalHash: evalHash, RunKind: job.runKind(), Decision: decision,
		Score: reviewScore(results, decision), Reason: reviewReason(results, decision), Nodes: results,
		Reusable: !partial, RequestedBy: job.CurrentRequestedBy}
	if err := e.repo.SaveReview(ctx, job, review); err != nil {
		return nil, persistenceFailure(err, "review_persist_failed")
	}
	return &reviewOutcome{review: review, resolution: ResolutionModel}, nil
}

// callNode 调度一个节点并完成一次真实调用；调度失败返回 nil 和致命错误。
func (e *Evaluator) callNode(ctx context.Context, job *AuditJob, target *JobTarget, keys map[string]string, remaining *[]ModelConfig) (*NodeResult, *AuditError) {
	waitForCapacity := job.Mode == "blocking"
	acquireCtx := ctx
	var cancel context.CancelFunc
	if waitForCapacity {
		acquireCtx, cancel = context.WithTimeout(ctx, nodeCapacityWait(job.Config.Models))
	}
	model, release, dispatch, acquireErr := e.node.acquireWithPolicy(acquireCtx, *remaining, waitForCapacity)
	if cancel != nil {
		cancel()
	}
	if acquireErr != nil {
		var scheduleErr *nodeScheduleError
		if errors.As(acquireErr, &scheduleErr) {
			return nil, &AuditError{Code: scheduleErr.Code, Stage: "scheduler", Message: "审核节点暂不可调度", Retryable: scheduleErr.Code != "no_healthy_nodes", RetryAfter: scheduleErr.RetryAfter}
		}
		return nil, requestFailure(acquireErr)
	}
	defer release()
	for position := range *remaining {
		if (*remaining)[position].ID == model.ID {
			*remaining = append((*remaining)[:position:position], (*remaining)[position+1:]...)
			break
		}
	}
	return e.runNode(ctx, job, target, keys, model, dispatch)
}

// runNode 在已占用的容量上完成一次真实节点调用并更新节点健康。
func (e *Evaluator) runNode(ctx context.Context, job *AuditJob, target *JobTarget, keys map[string]string, model ModelConfig, dispatch DispatchSnapshot) (*NodeResult, *AuditError) {
	key := keys[model.ID]
	if key == "" {
		return nil, &AuditError{Code: "credential_binding_unavailable", Stage: "config", Message: "审核节点凭据不可用"}
	}
	client, url, err := nodeHTTPClient(model)
	if err != nil {
		e.node.observe(model.ID, &AuditError{Code: "node_unavailable", Stage: "config", Message: err.Error()}, 0)
		return nil, &AuditError{Code: "node_unavailable", Stage: "config", Message: err.Error()}
	}
	defer client.CloseIdleConnections()
	order := target.Order
	started := time.Now()
	score, attemptID, failure := e.client.EvaluateTarget(ctx, job, model, key, job.Config, client, url, target.Kind, target.Text, target.Hash, &order)
	// 只有真实节点调用才更新节点健康。
	e.node.observe(model.ID, failure, time.Since(started))
	if failure != nil && authFailure(failure) {
		// 凭据失效单独通知管理员；每任务只记一次，通知失败不影响审核结论。
		if err := e.repo.RecordNodeAuthNotice(ctx, job, model.Name, failure.Message); err != nil {
			logger.LegacyPrintf("third_party_prompt_audit", "审核节点凭据失效通知写入失败 job_id=%d model=%s error=%v", job.ID, model.Name, err)
		}
	}
	record := &NodeResult{ModelID: model.ID, ModelName: model.Name, AttemptID: &attemptID, Dispatch: &dispatch}
	if failure != nil {
		record.Error = failure
		return record, nil
	}
	record.Confidence = &score.Confidence
	record.Reason = score.Reason
	return record, nil
}

// authFailure 判断节点失败是否属于凭据被上游拒绝。
func authFailure(failure *AuditError) bool {
	if failure == nil {
		return false
	}
	message := strings.ToUpper(failure.Message)
	return strings.Contains(message, "HTTP 401") || strings.Contains(message, "HTTP 403") ||
		strings.Contains(message, "INVALID_API_KEY") || strings.Contains(failure.Code, "unauthorized")
}

// votesFrom 把节点投票还原为聚合输入；失败与跳过保持原样，未知票不会被当成通过。
func votesFrom(results []NodeResult, config Config) []modelVote {
	votes := make([]modelVote, 0, len(results))
	for _, result := range results {
		vote := modelVote{ModelID: result.ModelID, Skipped: result.Skipped, Error: result.Error}
		if result.Error == nil && !result.Skipped && result.Confidence != nil {
			vote.Decision = classifyScore(*result.Confidence, config)
		}
		votes = append(votes, vote)
	}
	return votes
}

// modelVote 是单个节点对一个目标的投票。
type modelVote struct {
	ModelID  string
	Decision Decision
	Error    *AuditError
	Skipped  bool
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

// aggregationBlockThreshold 按聚合规则计算判定违规所需的节点数。
func aggregationBlockThreshold(aggregation string, enabled int) int {
	switch aggregation {
	case "all_block":
		return enabled
	case "majority_block":
		return enabled/2 + 1
	default:
		return 1
	}
}

// aggregateVotes 使用固定启用节点分母，未知票不能被静默当成通过或缩小分母。
func aggregateVotes(votes []modelVote, config Config) (Decision, bool, *AuditError) {
	if len(votes) == 0 {
		return "", false, &AuditError{Code: "no_models", Stage: "config", Message: "没有启用审核节点"}
	}
	threshold := aggregationBlockThreshold(config.Aggregation, len(votes))
	switch config.Aggregation {
	case "majority_block", "all_block", "any_block":
	default:
		return "", false, &AuditError{Code: "invalid_aggregation", Stage: "config", Message: "聚合规则无效"}
	}
	blocks, reviews, failures, skipped := 0, 0, 0, 0
	var firstFailure *AuditError
	var retryAfter time.Duration
	for _, vote := range votes {
		if vote.Skipped {
			skipped++
			continue
		}
		if vote.Error != nil {
			if vote.Error.RetryAfter > retryAfter {
				retryAfter = vote.Error.RetryAfter
			}
			failures++
			if firstFailure == nil {
				copied := *vote.Error
				firstFailure = &copied
			}
			continue
		}
		switch vote.Decision {
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

// reviewScore 取与最终结论一致的节点中最高的置信度，用于页面展示。
func reviewScore(results []NodeResult, decision Decision) *float64 {
	var selected *float64
	for _, result := range results {
		if result.Confidence == nil || result.Skipped || result.Error != nil {
			continue
		}
		if selected == nil || *result.Confidence > *selected {
			value := *result.Confidence
			selected = &value
		}
	}
	return selected
}

// reviewReason 汇总与最终结论一致的节点理由，避免把未命中节点的理由当作依据。
func reviewReason(results []NodeResult, decision Decision) string {
	reasons := make([]string, 0, 2)
	for _, result := range results {
		if result.Confidence == nil || result.Skipped || result.Error != nil || strings.TrimSpace(result.Reason) == "" {
			continue
		}
		reasons = append(reasons, strings.TrimSpace(result.Reason))
		if len(reasons) == 2 {
			break
		}
	}
	return strings.Join(reasons, "；")
}
