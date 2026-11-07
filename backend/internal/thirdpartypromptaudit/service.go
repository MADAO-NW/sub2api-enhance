package thirdpartypromptaudit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	infraerrors "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/pkg/logger"

	"sub2api-enhance/internal/notify"
	"sub2api-enhance/internal/sub2api"
)

// persistenceTimeout 限定请求退出后的数据库收尾时间，不用于追加模型调用。
const persistenceTimeout = 5 * time.Second

// blockingTurnWait 是 blocking 模式等待同一回合已有任务结论的上限（既定 5 秒边界）。
const blockingTurnWait = 5 * time.Second

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
	// syncActive 是同步审核的独立并发计数，不占用后台 Worker 槽位。
	syncActive atomic.Int64
	running    atomic.Bool
	metrics    *RuntimeMetrics
	redis      *RedisStore
	flightMu   sync.Mutex
	flights    map[string]*reviewFlight
	leaders    map[int64][]string
	// retention 保存最近一次清理结果，供运行概览展示。
	retentionMu sync.Mutex
	retention   *RetentionRecord
	// turnStates 是 Redis 故障时使用的进程内回合裁决缓存（30 分钟）。
	turnMu     sync.Mutex
	turnStates map[string]cachedTurn
	// metricsMu/metricsCache 缓存 Prometheus 文本，避免高频抓取压库。
	metricsMu    sync.Mutex
	metricsCache string
	metricsAt    time.Time
}

// reviewFlight 合并同一 (内容哈希, 评估语义) 的并发审核：后到的任务等待并共享结论。
type reviewFlight struct {
	done    chan struct{}
	outcome *reviewOutcome
	failure *AuditError
	// started 是发起方开始真实调用的时间，重新审核只加入发起之后的 flight。
	started time.Time
}

// SetRedisStore 在服务对外启动前装配回合键与响应映射。
func (s *Service) SetRedisStore(store *RedisStore) {
	s.redis = store
	s.repo.SetRedisStore(store)
}

func NewService(repo *Repository, config *ConfigManager, evaluator *Evaluator, client *ModelClient, email notify.Sender) *Service {
	return &Service{repo: repo, config: config, evaluator: evaluator, client: client, email: email, wake: make(chan struct{}, 1),
		metrics: NewRuntimeMetrics(), flights: make(map[string]*reviewFlight), leaders: make(map[int64][]string)}
}

func (s *Service) TestAdminEmail(ctx context.Context) error {
	public, err := s.config.Public()
	if err != nil {
		return err
	}
	if public.AdminEmail == "" {
		return infraerrors.BadRequest("third_party_audit_admin_email_missing", "管理员通知邮箱未配置")
	}
	if s.email == nil {
		return infraerrors.ServiceUnavailable("third_party_audit_smtp_unavailable", "邮件通知服务不可用")
	}
	if err := s.email.SendEmail(ctx, public.AdminEmail, "第三方提示词审计管理员邮箱测试", fmt.Sprintf("<p>第三方提示词审计管理员邮箱测试成功。</p><p>发送时间：%s</p>", time.Now().UTC().Format(time.RFC3339))); err != nil {
		if errors.Is(err, notify.ErrNotConfigured) {
			return infraerrors.ServiceUnavailable("third_party_audit_smtp_unavailable", "SMTP 通知未配置")
		}
		return infraerrors.ServiceUnavailable("third_party_audit_email_failed", "测试邮件发送失败").WithCause(err)
	}
	return nil
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
	s.refreshNodeLimits()
	if err != nil {
		s.noteError("config_load_failed", err)
	}
	s.wg.Add(3)
	s.running.Store(true)
	if err := s.config.LoadEscalation(s.ctx); err != nil {
		s.noteError("escalation_load_failed", err)
	}
	if err := s.repo.InterruptRunningReplays(s.ctx); err != nil {
		s.noteError("replay_interrupt_failed", err)
	}
	go s.run()
	go s.runHealthProbes()
	go s.runRetention()
	// 启动即写一次指标快照，避免重启后趋势表要等一小时。
	go s.collectHourlyMetrics()
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

// Check 判定一次入口请求：续跑只绑定已审回合，首次请求与孤儿续跑才建任务。
func (s *Service) Check(parent context.Context, request IntakeRequest) *IntakeDecision {
	parsed := request.Parsed
	if parsed == nil {
		return nil
	}
	mode, snapshot, modeErr := s.config.EffectiveMode(), ConfigSnapshot{}, error(nil)
	if s.config.EffectiveMode() == "off" {
		return nil
	}
	snapshot, modeErr = s.config.Active()
	if modeErr == nil {
		if excludedByScope(snapshot, request) {
			return nil
		}
		snapshot.Config = effectiveConfigForUser(snapshot.Config, request.UserID)
		// 已自动加严的异步用户在入口按同步审核，任务冻结的处理方式随之为 blocking。
		snapshot.Mode = s.config.escalatedMode(snapshot.Mode, request.UserID)
		mode = snapshot.Mode
	}
	if request.Background && mode == "blocking" {
		mode = "async"
		snapshot.Mode = mode
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return &IntakeDecision{Mode: mode, Kind: IngressDecisionUnavailable, ErrorCode: "third_party_audit_unavailable"}
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
	if modeErr != nil {
		if mode == "blocking" {
			return &IntakeDecision{Mode: mode, Kind: IngressDecisionUnavailable, ErrorCode: "third_party_audit_unavailable"}
		}
		return nil
	}
	if len(parsed.Targets()) == 0 {
		return &IntakeDecision{Mode: mode, Kind: IngressDecisionAllow, ErrorCode: "current_user_not_found"}
	}
	turnKey := turnKeyFor(request.APIKeyID, parsed.TurnFingerprint)
	// 续跑请求不建任务，只查询已审回合；绑不上的孤儿续跑按首次请求处理。
	if parsed.Continuation {
		bound := turnKey
		if parsed.PreviousResponseID != "" {
			resolved, err := s.resolveResponseTurn(ctx, parsed.PreviousResponseID)
			if err != nil {
				return s.redisFailure(mode, "response_mapping_unavailable", turnKey, err)
			}
			if resolved != "" {
				bound = resolved
			}
		}
		state, err := s.getTurn(ctx, bound)
		if err != nil {
			return s.redisFailure(mode, "turn_key_unavailable", bound, err)
		}
		if state != nil {
			return s.boundDecision(state, mode, turnKey)
		}
	}
	state, err := s.getTurn(ctx, turnKey)
	if err != nil {
		return s.redisFailure(mode, "turn_key_unavailable", turnKey, err)
	}
	if state != nil {
		return s.boundDecision(state, mode, turnKey)
	}
	job := &AuditJob{UserID: request.UserID, APIKeyID: request.APIKeyID, GroupID: request.GroupID, Identity: request.Identity,
		Platform: request.Provider, RequestID: request.RequestID, Model: request.Model, Protocol: parsed.Protocol,
		ClientFingerprint: parsed.ClientRules, Mode: mode, TurnFingerprint: parsed.TurnFingerprint,
		OrphanContinuation: parsed.Continuation, Status: "queued", CurrentRound: 1, CurrentRunKind: RunKindRequest,
		ReuseMode: ReuseModeAllow, Config: snapshot, MaxAttempts: MaxEvaluationAttempts}
	if request.ActorUserID > 0 {
		job.CurrentRequestedBy = &request.ActorUserID
	}
	if mode == "blocking" {
		job.Status = "processing"
	}
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	err = s.repo.CreateAuditJob(persistCtx, job, parsed.Targets())
	persistCancel()
	if err != nil {
		s.metrics.IntakeFailures.Add(1)
		s.noteError("input_persist_failed", err)
		return &IntakeDecision{Mode: mode, Kind: IngressDecisionUnavailable, ErrorCode: "third_party_audit_unavailable"}
	}
	logger.LegacyPrintf("third_party_prompt_audit", "审核任务已创建 job_id=%d request_id=%s user_id=%d mode=%s protocol=%s targets=%d orphan_continuation=%t",
		job.ID, job.RequestID, job.UserID, mode, job.Protocol, len(parsed.Targets()), parsed.Continuation)
	s.rememberTurn(ctx, turnKey, job.ID, job.Status, "")
	result := &IntakeDecision{JobID: job.ID, Mode: mode, Kind: IngressDecisionAllow, TurnKey: turnKey}
	if mode == "async" {
		s.notify()
		return result
	}
	completion, failure := s.processJob(ctx, job, true)
	if failure != nil {
		// 失败回合也要更新回合键，避免后续请求按"仍在审核"白等到超时。
		s.rememberTurn(ctx, turnKey, job.ID, "failed", "")
		if failure.Code == "user_excluded" {
			result.Kind, result.ErrorCode = IngressDecisionAllow, "user_excluded"
			return result
		}
		result.Kind, result.ErrorCode = IngressDecisionUnavailable, "third_party_audit_unavailable"
		return result
	}
	s.rememberTurn(ctx, turnKey, job.ID, "done", string(completion.Decision))
	s.rememberTurnState(turnKey, string(completion.Decision))
	result.Kind = gatewayKind(completion.Decision)
	if result.Kind == IngressDecisionBlock {
		result.ErrorCode = "third_party_audit_blocked"
	}
	return result
}

// excludedByScope 判断请求是否不属于自动审核范围：不审核用户、分组和平台筛选都跳过。
func excludedByScope(snapshot ConfigSnapshot, request IntakeRequest) bool {
	if slices.Contains(snapshot.ExcludedUserIDs, request.UserID) {
		return true
	}
	if len(snapshot.Platforms) > 0 && !slices.Contains(snapshot.Platforms, request.Provider) {
		return true
	}
	if snapshot.AllGroups {
		return false
	}
	if request.GroupID == nil {
		return true
	}
	for _, groupID := range snapshot.GroupIDs {
		if groupID == *request.GroupID {
			return false
		}
	}
	return true
}

// redisFailure 处理回合键读写失败：async 放行并记运行日志，blocking 沿用不可用语义（决策 11）。
func (s *Service) redisFailure(mode, code, turnKey string, err error) *IntakeDecision {
	s.noteError(code, err)
	// Redis 不可用时按进程内缓存沿用真实裁决，避免降级放行已判定违规的对话。
	switch s.cachedTurnDecision(turnKey) {
	case string(DecisionBlock):
		return &IntakeDecision{Mode: mode, Kind: IngressDecisionBlock, ErrorCode: "third_party_audit_blocked", TurnKey: turnKey}
	case string(DecisionReview):
		return &IntakeDecision{Mode: mode, Kind: IngressDecisionFlag, TurnKey: turnKey}
	case string(DecisionPass):
		return &IntakeDecision{Mode: mode, Kind: IngressDecisionAllow, TurnKey: turnKey}
	}
	if mode == "async" {
		return &IntakeDecision{Mode: mode, Kind: IngressDecisionAllow, ErrorCode: code}
	}
	return &IntakeDecision{Mode: mode, Kind: IngressDecisionUnavailable, ErrorCode: "third_party_audit_unavailable"}
}

// boundDecision 按已绑定回合的状态与裁决返回结果：阻断继续拒绝，其余直接转发且不落库。
func (s *Service) boundDecision(state *turnState, mode, turnKey string) *IntakeDecision {
	switch {
	case state.Status == "done" && state.Decision == string(DecisionBlock):
		return &IntakeDecision{JobID: state.JobID, Mode: mode, Kind: IngressDecisionBlock, ErrorCode: "third_party_audit_blocked", TurnKey: turnKey}
	case state.Status == "done" && state.Decision == string(DecisionReview):
		return &IntakeDecision{JobID: state.JobID, Mode: mode, Kind: IngressDecisionFlag, TurnKey: turnKey}
	case state.Status == "done":
		return &IntakeDecision{JobID: state.JobID, Mode: mode, Kind: IngressDecisionAllow, TurnKey: turnKey}
	case mode == "async":
		// 首轮审核仍在进行或已失败：异步模式不阻断原请求。
		return &IntakeDecision{JobID: state.JobID, Mode: mode, Kind: IngressDecisionAllow, TurnKey: turnKey}
	default:
		return s.waitForTurn(state.JobID, mode, turnKey)
	}
}

// waitForTurn 在 blocking 模式下等待同一回合已有任务的结论，超时按不可用处理。
func (s *Service) waitForTurn(jobID int64, mode, turnKey string) *IntakeDecision {
	deadline := time.Now().Add(blockingTurnWait)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), persistenceTimeout)
		job, err := s.repo.GetAuditJob(ctx, jobID)
		cancel()
		if err == nil {
			switch job.Status {
			case "done":
				result := &IntakeDecision{JobID: jobID, Mode: mode, Kind: gatewayKind(job.Decision), TurnKey: turnKey}
				if job.Decision == DecisionBlock {
					result.ErrorCode = "third_party_audit_blocked"
				}
				return result
			case "failed", "skipped":
				return &IntakeDecision{JobID: jobID, Mode: mode, Kind: IngressDecisionUnavailable, ErrorCode: "third_party_audit_unavailable"}
			}
		}
		if time.Now().After(deadline) {
			return &IntakeDecision{JobID: jobID, Mode: mode, Kind: IngressDecisionUnavailable, ErrorCode: "third_party_audit_unavailable"}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// turnKeyFor 由 API key 与回合指纹组成回合键，用于去重和续跑绑定。
func turnKeyFor(apiKeyID int64, turnFingerprint string) string {
	return strconv.FormatInt(apiKeyID, 10) + ":" + turnFingerprint
}

func (s *Service) getTurn(ctx context.Context, key string) (*turnState, error) {
	if s.redis == nil {
		return nil, errors.New("增强 Redis 未装配")
	}
	return s.redis.GetTurn(ctx, key)
}

func (s *Service) resolveResponseTurn(ctx context.Context, responseID string) (string, error) {
	if s.redis == nil {
		return "", errors.New("增强 Redis 未装配")
	}
	return s.redis.GetResponseTurn(ctx, responseID)
}

// rememberTurn 写入回合键状态；Redis 故障只记运行日志，不影响已确定的业务结论。
func (s *Service) rememberTurn(ctx context.Context, key string, jobID int64, status, decision string) {
	if s.redis == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	defer cancel()
	if err := s.redis.SaveTurn(writeCtx, key, turnState{JobID: jobID, Status: status, Decision: decision}); err != nil {
		s.noteError("turn_key_persist_failed", err)
	}
}

// RegisterResponseID 登记上游响应 ID 到回合键的映射；续跑请求按 previous_response_id 查回该回合。
func (s *Service) RegisterResponseID(ctx context.Context, responseID, turnKey string) {
	if responseID == "" || turnKey == "" || s.redis == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	defer cancel()
	if err := s.redis.SaveResponseTurn(writeCtx, responseID, turnKey); err != nil {
		s.noteError("response_mapping_persist_failed", err)
	}
}

// RejectContinuation 记录被拒绝的续跑：只打一行运行日志，不落库、不展示（决策 15）。
func (s *Service) RejectContinuation(decision *IntakeDecision, apiKeyID int64, reason string) {
	if decision == nil {
		return
	}
	logger.LegacyPrintf("third_party_prompt_audit", "续跑请求被拒绝 api_key_id=%d job_id=%d reason=%s", apiKeyID, decision.JobID, reason)
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

// processJob 执行一轮审核：逐目标取得结论后统一提交轮次与处置。
func (s *Service) processJob(parent context.Context, job *AuditJob, foreground bool) (*CompletionResult, *AuditError) {
	defer s.finishFlights(job.ID)
	ctx, cancel := context.WithCancelCause(parent)
	done := s.heartbeat(ctx, cancel, func(ctx context.Context) error { return s.repo.RenewAuditJob(ctx, job) })
	defer func() { cancel(nil); <-done }()
	if foreground {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for !s.tryAcquireSyncSlot() {
			select {
			case <-ctx.Done():
				return nil, s.finishFailure(ctx, job, requestFailure(ctx.Err()))
			case <-ticker.C:
			}
			if s.config.EffectiveMode() == "off" {
				return nil, s.finishFailure(ctx, job, persistenceFailure(ErrAuditPaused, "audit_paused"))
			}
		}
		defer s.releaseSyncSlot()
		if err := s.repo.BeginForegroundAuditJob(ctx, job); err != nil {
			return nil, s.finishFailure(ctx, job, persistenceFailure(err, "evaluation_start_failed"))
		}
		// 同步等待耗时在建任务到得出结论之间统计，收尾时写入任务记录。
	}
	started := time.Now()
	var binding ConfigSnapshot
	var keys map[string]string
	var err error
	if job.CurrentRequestedBy != nil {
		binding, keys, err = s.config.EvaluationBindingAllowExcluded(job.UserID)
	} else {
		binding, keys, err = s.config.EvaluationBinding(job.UserID)
	}
	var failure *AuditError
	switch {
	case errors.Is(err, ErrAuditPaused):
		failure = persistenceFailure(err, "audit_paused")
	case errors.Is(err, ErrUserExcluded):
		failure = persistenceFailure(err, "user_excluded")
	case err != nil:
		failure = &AuditError{Code: "evaluation_binding_unavailable", Stage: "config", Message: err.Error(), Retryable: true}
	}
	if failure != nil {
		return nil, s.finishFailure(ctx, job, failure)
	}
	s.evaluator.updateNodeLimits(binding.Models)
	// 请求的同步/异步语义在入口确定；重新绑定只更新本轮审核配置。
	binding.Mode = job.Mode
	if err := s.repo.BindAuditJobConfig(ctx, job, binding); err != nil {
		return nil, s.finishFailure(ctx, job, persistenceFailure(err, "evaluation_binding_persist_failed"))
	}
	targets, err := s.repo.LoadRoundTargets(ctx, job.ID, job.CurrentRound)
	if err != nil {
		return nil, s.finishFailure(ctx, job, persistenceFailure(err, "target_load_failed"))
	}
	hashes := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.Kind == PromptKindInstruction {
			hashes = append(hashes, target.Hash)
		}
	}
	trusted, err := s.repo.TrustedHashes(ctx, hashes)
	if err != nil {
		return nil, s.finishFailure(ctx, job, persistenceFailure(err, "trust_load_failed"))
	}
	for index := range targets {
		target := &targets[index]
		if target.Resolution != "" {
			// 已成功的目标保留结论，重试只补未完成的目标。
			continue
		}
		if trusted[target.Hash] {
			// 信任优先：已信任的指令块不调用模型（决策 23）。
			target.Resolution, target.Decision = ResolutionTrust, DecisionPass
			if err := s.repo.ResolveTarget(ctx, job, target); err != nil {
				return nil, s.finishFailure(ctx, job, persistenceFailure(err, "target_resolve_failed"))
			}
			continue
		}
		outcome, reviewFailure := s.reviewWithInflightReuse(ctx, job, target, keys)
		if reviewFailure != nil {
			persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
			_ = s.repo.FailTarget(persistCtx, job, target, reviewFailure)
			persistCancel()
			return nil, s.finishFailure(ctx, job, reviewFailure)
		}
		target.Resolution, target.Decision = outcome.resolution, outcome.review.Decision
		// 目标类别按本任务冻结的阈值从节点结果重新计算，缓存复用同样适用。
		target.Categories = targetCategories(outcome.review.Nodes, job.Config.Config, target.Decision)
		reviewID := outcome.review.ID
		target.ReviewID = &reviewID
		if err := s.repo.ResolveTarget(ctx, job, target); err != nil {
			return nil, s.finishFailure(ctx, job, persistenceFailure(err, "target_resolve_failed"))
		}
		if target.Decision == DecisionBlock {
			// 已有 block 即可确定任务裁决，剩余目标不再消耗模型调用。
			break
		}
	}
	if foreground && job.Mode == "blocking" {
		wait := time.Since(job.CreatedAt).Milliseconds()
		job.BlockingWaitMS = &wait
	}
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	completion, err := s.repo.CompleteAuditJob(persistCtx, job, foreground && ctx.Err() == nil, s.config.PublicOrigin())
	persistCancel()
	s.metrics.ObserveEvaluation(started, time.Now())
	if err != nil {
		failure := persistenceFailure(err, "result_persist_failed")
		s.noteError(failure.Code, failure)
		persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
		if err := s.repo.FailAuditJob(persistCtx, job, failure, failure.Retryable); err != nil {
			s.noteError("job_failure_persist_failed", err)
		}
		persistCancel()
		return nil, failure
	}
	s.metrics.Success(time.Now())
	if completion.Escalated {
		// 加严集合只驻留内存；数据库状态已在同一事务内提交。
		s.config.SetEscalation(job.UserID, true)
	}
	s.notify()
	logger.LegacyPrintf("third_party_prompt_audit", "审核任务完成 job_id=%d round=%d decision=%s trigger_kind=%s violation_counted=%t escalated=%t tripped_breakers=%v",
		job.ID, completion.Round, completion.Decision, completion.TriggerKind, completion.ViolationCounted, completion.Escalated, completion.TrippedBreakers)
	return completion, nil
}

// reviewWithInflightReuse 合并同一 (内容哈希, 评估语义) 的并发审核；等待方不占节点并发位。
func (s *Service) reviewWithInflightReuse(ctx context.Context, job *AuditJob, target *JobTarget, keys map[string]string) (*reviewOutcome, *AuditError) {
	evalHash, err := evaluationHash(job.Config, target.Kind)
	if err != nil {
		return nil, &AuditError{Code: "evaluation_hash_failed", Stage: "config", Message: err.Error()}
	}
	key := target.Hash + "\x00" + evalHash
	s.flightMu.Lock()
	// 重新审核只加入本次重审发起之后开始的 flight，否则自己作为发起方真实调用。
	if flight := s.flights[key]; flight != nil && reusableFlight(job, flight) {
		s.flightMu.Unlock()
		select {
		case <-flight.done:
			if flight.failure != nil {
				copied := *flight.failure
				return nil, &copied
			}
			return &reviewOutcome{review: flight.outcome.review, resolution: ResolutionInflight}, nil
		case <-ctx.Done():
			return nil, requestFailure(ctx.Err())
		}
	}
	flight := &reviewFlight{done: make(chan struct{}), started: time.Now()}
	s.flights[key] = flight
	s.leaders[job.ID] = append(s.leaders[job.ID], key)
	s.flightMu.Unlock()
	outcome, failure := s.evaluator.Review(ctx, job, target, keys)
	s.flightMu.Lock()
	flight.outcome, flight.failure = outcome, failure
	close(flight.done)
	s.flightMu.Unlock()
	return outcome, failure
}

// finishFlights 移除本任务发起的并发审核合并键，避免结果长期驻留内存。
func (s *Service) finishFlights(jobID int64) {
	s.flightMu.Lock()
	defer s.flightMu.Unlock()
	for _, key := range s.leaders[jobID] {
		delete(s.flights, key)
	}
	delete(s.leaders, jobID)
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

// syncCapacity 是同步审核的并发上限：按启用节点的总并发推导（2 倍），
// 既不会超过节点真实处理能力，也不会与后台 Worker 互锁。
func (s *Service) syncCapacity() int64 {
	capacity := int64(s.config.NodeConcurrencySum() * 2)
	if capacity < 8 {
		capacity = 8
	}
	if capacity > 128 {
		capacity = 128
	}
	return capacity
}

func (s *Service) tryAcquireSyncSlot() bool {
	for {
		active := s.syncActive.Load()
		if active >= s.syncCapacity() {
			return false
		}
		if s.syncActive.CompareAndSwap(active, active+1) {
			return true
		}
	}
}

func (s *Service) releaseSyncSlot() { s.syncActive.Add(-1) }

// refreshNodeLimits 将最新已应用节点容量发布给进程内调度器。
func (s *Service) refreshNodeLimits() {
	if s == nil || s.config == nil || s.evaluator == nil {
		return
	}
	snapshot, err := s.config.Active()
	if err == nil {
		s.evaluator.updateNodeLimits(snapshot.Models)
	}
}

// runHealthProbes 只在异常节点冷却到期后进行低优先级恢复探测。
func (s *Service) runHealthProbes() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		if s.config.EffectiveMode() == "off" {
			continue
		}
		snapshot, err := s.config.Active()
		if err != nil {
			continue
		}
		model, release, ok := s.evaluator.nodeScheduler().dueProbe(snapshot.Models)
		if !ok {
			continue
		}
		s.runHealthProbe(snapshot, model)
		release()
	}
}

// runHealthProbe 验证节点连通性和固定 JSON 返回协议，不创建业务任务、审核轮次或缓存。
func (s *Service) runHealthProbe(snapshot ConfigSnapshot, model ModelConfig) {
	started := time.Now()
	key, err := s.config.ResolveKeyByModelID(model.ID)
	if err != nil {
		s.evaluator.nodeScheduler().observe(model.ID, &AuditError{Code: "credential_binding_unavailable", Stage: "config", Message: err.Error()}, time.Since(started))
		return
	}
	client, url, err := nodeHTTPClient(model)
	if err != nil {
		s.evaluator.nodeScheduler().observe(model.ID, &AuditError{Code: "node_unavailable", Stage: "config", Message: err.Error()}, time.Since(started))
		return
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.WithValue(s.ctx, healthProbeContext, true), time.Duration(model.TimeoutMS)*time.Millisecond)
	defer cancel()
	_, _, failure := s.client.EvaluateTarget(ctx, nil, model, key, snapshot, client, url, "health_probe", "节点健康检查，请按固定协议返回评分。", "", nil)
	s.evaluator.nodeScheduler().observe(model.ID, failure, time.Since(started))
}

// finishFailure 区分服务暂停、请求结束和业务重试，防止停机耗尽异步任务的评估预算。
func (s *Service) finishFailure(ctx context.Context, job *AuditJob, failure *AuditError) *AuditError {
	if cause := context.Cause(ctx); cause != nil && ctx.Err() == context.Canceled && !errors.Is(cause, context.Canceled) {
		failure = persistenceFailure(cause, "lease_renew_failed")
	}
	if failure.Code == "lease_lost" {
		return failure
	}
	if !job.Dispatched && failure.Code == "user_excluded" {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
		defer cancel()
		if err := s.repo.SkipExcludedAuditJob(persistCtx, job); err != nil {
			s.noteError("excluded_skip_failed", err)
		}
		s.notify()
		return failure
	}
	if !job.Dispatched && job.Mode == "async" && (failure.Code == "capacity_saturated" || failure.Code == "temporarily_unhealthy" || failure.Code == "audit_paused") {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
		defer cancel()
		if err := s.repo.DeferAuditJob(persistCtx, job, failure); err != nil {
			s.noteError("capacity_defer_failed", err)
		}
		s.notify()
		return failure
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing && (job.Mode == "async" || job.runKind() == RunKindReaudit) {
		failure = &AuditError{Code: "worker_paused", Stage: "worker", Message: "服务正在停止，任务将在恢复后继续", Retryable: true}
	}
	retry := (job.Mode == "async" || job.runKind() == RunKindReaudit) && failure.Retryable &&
		(job.Attempts < job.MaxAttempts || failure.Code == "audit_paused" || failure.Code == "worker_paused")
	s.noteError(failure.Code, failure)
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	defer cancel()
	if err := s.repo.FailAuditJob(persistCtx, job, failure, retry); err != nil {
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

// recordRetention 记录最近一次保留期清理结果。
func (s *Service) recordRetention(record *RetentionRecord) {
	s.retentionMu.Lock()
	defer s.retentionMu.Unlock()
	s.retention = record
}

// retentionSnapshot 读取最近一次清理结果。
func (s *Service) retentionSnapshot() *RetentionRecord {
	s.retentionMu.Lock()
	defer s.retentionMu.Unlock()
	if s.retention == nil {
		return nil
	}
	copied := *s.retention
	return &copied
}

// cachedTurn 是进程内的回合裁决快照。
type cachedTurn struct {
	Decision string
	At       time.Time
}

// rememberTurnState 在内存里记录回合裁决，Redis 不可用时作为降级兜底。
func (s *Service) rememberTurnState(turnKey, decision string) {
	if turnKey == "" {
		return
	}
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.turnStates == nil {
		s.turnStates = make(map[string]cachedTurn)
	}
	now := time.Now()
	for key, item := range s.turnStates {
		if now.Sub(item.At) > turnKeyTTL {
			delete(s.turnStates, key)
		}
	}
	if len(s.turnStates) > 8192 {
		for key := range s.turnStates {
			delete(s.turnStates, key)
			if len(s.turnStates) <= 4096 {
				break
			}
		}
	}
	s.turnStates[turnKey] = cachedTurn{Decision: decision, At: now}
}

// cachedTurnDecision 读取本地缓存的回合裁决；过期视为未命中。
func (s *Service) cachedTurnDecision(turnKey string) string {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	item, ok := s.turnStates[turnKey]
	if !ok {
		return ""
	}
	if time.Since(item.At) > turnKeyTTL {
		delete(s.turnStates, turnKey)
		return ""
	}
	return item.Decision
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
	ActorUserID int64       `json:"-" binding:"-"`
	Model       ModelConfig `json:"model"`
	KeyAction   string      `json:"key_action" binding:"required,oneof=keep replace clear"`
	APIKey      string      `json:"api_key"`
	AuditPrompt string      `json:"audit_prompt" binding:"required"`
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

// Probe 只测试单个节点：使用管理员提供的政策与一段待审目标，不创建任务或复用结果。
func (s *Service) Probe(ctx context.Context, input ProbeRequest) (result ProbeResult) {
	ctx = context.WithValue(ctx, probeActorContext, input.ActorUserID)
	started := time.Now()
	result = ProbeResult{ModelID: input.Model.ID, TestedAt: started.UTC()}
	defer func() {
		if s.evaluator != nil && result.AttemptID > 0 {
			s.evaluator.nodeScheduler().observe(input.Model.ID, result.Error, time.Since(started))
		}
	}()
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
	kind, text := PromptKindUser, "这是一段正常的项目开发说明，请按审核政策评估。"
	if input.Input != "" {
		if input.InputKind == "json" {
			value, decodeErr := decodeUniqueJSON([]byte(input.Input))
			if decodeErr != nil {
				result.Error = &AuditError{Code: "invalid_probe_input", Stage: "input_parse", Message: decodeErr.Error()}
				return result
			}
			object, ok := value.(map[string]any)
			if !ok {
				result.Error = &AuditError{Code: "invalid_probe_input", Stage: "input_parse", Message: "结构化测试输入必须是待审对象"}
				return result
			}
			if value, ok := object["target_kind"].(string); ok && (value == PromptKindInstruction || value == PromptKindUser) {
				kind = value
			}
			body, ok := object["target"].(string)
			if !ok || body == "" {
				result.Error = &AuditError{Code: "invalid_probe_input", Stage: "input_parse", Message: "结构化测试输入必须包含字符串 target"}
				return result
			}
			text = body
		} else {
			text = input.Input
		}
	}
	config := ConfigSnapshot{Config: DefaultConfig(), ContractVersion: ContractVersion, FixedContract: OutputContract}
	config.AuditPrompt = input.AuditPrompt
	client, url, err := nodeHTTPClient(input.Model)
	if err != nil {
		result.Error = &AuditError{Code: "invalid_model", Stage: "config", Message: err.Error()}
		return result
	}
	defer client.CloseIdleConnections()
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(input.Model.TimeoutMS)*time.Millisecond)
	defer cancel()
	result.Result, result.AttemptID, result.Error = s.client.EvaluateTarget(probeCtx, nil, input.Model, key, config, client, url, kind, text, "", nil)
	result.OK = result.Error == nil
	result.LatencyMS = time.Since(started).Milliseconds()
	s.metrics.Probe(result)
	return result
}

// CreateReaudits 在同一任务上追加一轮审核；重新审核本身不触发新处置。
func (s *Service) CreateReaudits(ctx context.Context, request ReauditRequest, actorID int64) (*ReauditResult, error) {
	// 重新审核统一使用 fresh 语义：只复用本次发起之后产生的结论。
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

// reusableFlight 判断当前重审是否可以加入已有并发调用：业务请求始终可以，
// fresh 轮次只能加入重审发起之后开始的调用。
func reusableFlight(job *AuditJob, flight *reviewFlight) bool {
	return job.ReuseAfter == nil || !flight.started.Before(*job.ReuseAfter)
}
