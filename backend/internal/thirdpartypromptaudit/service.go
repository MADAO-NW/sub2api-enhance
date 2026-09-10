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
	accounts      *sub2api.Client
	repo          *Repository
	config        *ConfigManager
	evaluator     *Evaluator
	client        *ModelClient
	email         notify.Sender
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	closing       bool
	wg            sync.WaitGroup
	foreground    sync.WaitGroup
	wake          chan struct{}
	active        atomic.Int64
	running       atomic.Bool
	metrics       *RuntimeMetrics
	flightMu      sync.Mutex
	flights       map[string]*evaluationFlight
	flightLeaders map[int64]string
}

type evaluationFlight struct {
	done       chan struct{}
	evaluation *Evaluation
	failure    *AuditError
}

func NewService(repo *Repository, config *ConfigManager, evaluator *Evaluator, client *ModelClient, email notify.Sender) *Service {
	return &Service{repo: repo, config: config, evaluator: evaluator, client: client, email: email, wake: make(chan struct{}, 1), metrics: NewRuntimeMetrics(), flights: make(map[string]*evaluationFlight), flightLeaders: make(map[int64]string)}
}

func (s *Service) Start(parent context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return nil
	}
	s.ctx, s.cancel = context.WithCancel(parent)
	s.closing = false
	err := s.config.upgradeLegacyDefaultPolicy(s.ctx)
	if err == nil {
		err = s.config.Reload(s.ctx)
	}
	s.refreshNodeLimits()
	if err != nil {
		s.noteError("config_load_failed", err)
	}
	s.wg.Add(2)
	s.running.Store(true)
	go s.run()
	go s.runHealthProbes()
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
	if mode == "off" && !request.Manual {
		return nil
	}
	snapshot, configErr := s.config.Active()
	if configErr == nil && !request.Manual && (slices.Contains(snapshot.ExcludedUserIDs, request.UserID) ||
		(!snapshot.AllGroups && (request.GroupID == nil || !slices.Contains(snapshot.GroupIDs, *request.GroupID))) ||
		(len(snapshot.Platforms) > 0 && !slices.Contains(snapshot.Platforms, request.Provider))) {
		return nil
	}
	if configErr == nil {
		snapshot.Config = effectiveConfigForUser(snapshot.Config, request.UserID)
		mode = snapshot.Mode
	}
	if request.Manual {
		mode = "async"
		snapshot.Mode = mode
	}
	if request.Background && mode == "blocking" {
		mode = "async"
		if configErr == nil {
			snapshot.Mode = mode
		}
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
		snapshot = ConfigSnapshot{Config: Config{Mode: mode, AuditScope: "current_user", AllGroups: true}}
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
	job := &Job{CapturedAt: request.CapturedAt, CaptureID: request.CaptureID, CaptureKey: key, ReuseMode: ReuseModeAllow, UserID: request.UserID, APIKeyID: &apiKeyID, GroupID: request.GroupID, RequestID: request.RequestID,
		ConversationKey: request.ConversationKey, CurrentRunKind: "request", AuditRound: 1,
		Identity: Identity{Username: request.Username, UserEmail: request.UserEmail, APIKeyName: request.APIKeyName, GroupName: request.GroupName, Endpoint: request.Endpoint},
		Platform: request.Provider, Protocol: request.Protocol, IngressStage: request.Stage, RequestedModel: request.Model, ExecutionMode: mode,
		Config: snapshot, FullInput: input, SnapshotStatus: "complete", Status: "queued"}
	if request.Manual && request.ActorUserID > 0 {
		job.CurrentRequestedBy = &request.ActorUserID
	}
	if mode == "blocking" {
		job.Status = "processing"
	}
	if inputErr == nil {
		_, inputErr = prepareTarget(job)
	}
	if !request.Manual && errors.Is(inputErr, ErrNoText) {
		return &IntakeDecision{Mode: mode, Kind: IngressDecisionAllow, ErrorCode: "current_user_not_found"}
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
			job.LastErrorCode = "current_user_not_found"
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
	defer s.finishEvaluationFlight(job.ID)
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
		if job.ExecutionMode == "blocking" && job.auditRunKind() != "reaudit" && !foreground {
			return nil, &AuditError{Code: "blocking_request_ended", Stage: "worker", Message: "同步请求已经结束，不能在后台重新调用模型"}
		}
		started := time.Now()
		var failure *AuditError
		binding, keys, err := s.config.EvaluationBinding(job.UserID)
		if errors.Is(err, ErrAuditPaused) {
			failure = persistenceFailure(err, "audit_paused")
		} else if err != nil {
			failure = &AuditError{Code: "evaluation_binding_unavailable", Stage: "config", Message: err.Error(), Retryable: true}
		} else {
			s.evaluator.updateNodeLimits(binding.Models)
			// 请求的同步/异步语义在入口确定；重新绑定只更新本轮审核配置。
			binding.Mode = job.ExecutionMode
			if err = s.repo.BindEvaluationConfig(ctx, job, binding); err != nil {
				failure = persistenceFailure(err, "evaluation_binding_persist_failed")
			}
		}
		if failure == nil {
			_, err = prepareTarget(job)
			if err != nil {
				failure = &AuditError{Code: "input_parse_failed", Stage: "input_parse", Message: err.Error()}
			}
		}
		if failure == nil {
			if err = s.repo.SaveTarget(ctx, job); err != nil {
				failure = persistenceFailure(err, "target_persist_failed")
			}
		}
		if failure == nil {
			evaluation, failure = s.evaluateWithInflightReuse(ctx, job, keys)
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
			if err := s.repo.Fail(persistCtx, job, failure, failure.Retryable && (job.ExecutionMode == "async" || job.auditRunKind() == "reaudit") && job.Attempts < job.MaxAttempts); err != nil {
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

// evaluateWithInflightReuse 在相同裁决配置下合并全库完全一致的完整目标。
func (s *Service) evaluateWithInflightReuse(ctx context.Context, job *Job, boundKeys ...map[string]string) (*Evaluation, *AuditError) {
	keys := map[string]string{}
	if len(boundKeys) > 0 {
		keys = boundKeys[0]
	}
	if job.ReuseMode == ReuseModeForce {
		return s.evaluator.Evaluate(ctx, job, keys)
	}
	key, err := fingerprint(struct {
		TargetHash, EvaluationHash, Aggregation string
		ReviewThreshold, BlockThreshold         *float64
	}{job.TargetHash, job.EvaluationHash, job.Config.Aggregation, job.Config.ReviewThreshold, job.Config.BlockThreshold})
	if err != nil {
		return nil, &AuditError{Code: "inflight_key_failed", Stage: "worker", Message: err.Error()}
	}
	s.flightMu.Lock()
	if s.flights == nil {
		s.flights = make(map[string]*evaluationFlight)
	}
	if current := s.flights[key]; current != nil {
		s.flightMu.Unlock()
		select {
		case <-current.done:
			if current.failure != nil {
				failure := *current.failure
				return nil, &failure
			}
			cloned, err := cloneEvaluation(current.evaluation)
			if err != nil {
				return nil, &AuditError{Code: "inflight_result_clone_failed", Stage: "worker", Message: err.Error()}
			}
			job.Reuse.InflightHits++
			for i := range cloned.Models {
				if cloned.Models[i].Skipped {
					continue
				}
				cloned.Models[i].Reused = true
				cloned.Models[i].JointAttemptID = nil
				cloned.Models[i].Dispatch = nil
				for j := range cloned.Models[i].Segments {
					cloned.Models[i].Segments[j].ReuseKind = "inflight"
				}
				for j := range cloned.Models[i].TargetUses {
					cloned.Models[i].TargetUses[j].ReuseKind = "inflight"
				}
			}
			return cloned, nil
		case <-ctx.Done():
			return nil, requestFailure(ctx.Err())
		}
	}
	flight := &evaluationFlight{done: make(chan struct{})}
	s.flights[key] = flight
	if s.flightLeaders == nil {
		s.flightLeaders = make(map[int64]string)
	}
	s.flightLeaders[job.ID] = key
	s.flightMu.Unlock()
	evaluation, failure := s.evaluator.Evaluate(ctx, job, keys)
	s.flightMu.Lock()
	flight.evaluation, flight.failure = evaluation, failure
	close(flight.done)
	s.flightMu.Unlock()
	return evaluation, failure
}

func (s *Service) finishEvaluationFlight(jobID int64) {
	s.flightMu.Lock()
	defer s.flightMu.Unlock()
	if key := s.flightLeaders[jobID]; key != "" {
		delete(s.flightLeaders, jobID)
		delete(s.flights, key)
	}
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

// runHealthProbe 验证节点连通性和固定 JSON 返回协议，不创建业务任务或缓存。
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
	target := map[string]any{"protocol": "health_probe", "messages": []map[string]any{{"source_role": "user", "content": "节点健康检查，请按固定协议返回评分。"}}}
	_, _, failure := s.client.EvaluateTarget(ctx, nil, model, key, snapshot, client, url, "health_probe", target, nil)
	s.evaluator.nodeScheduler().observe(model.ID, failure, time.Since(started))
}

// finishFailure 区分服务暂停、请求结束和业务重试，防止停机耗尽异步任务的评估预算。
func (s *Service) finishFailure(ctx context.Context, job *Job, failure *AuditError) *AuditError {
	if cause := context.Cause(ctx); cause != nil && ctx.Err() == context.Canceled && !errors.Is(cause, context.Canceled) {
		failure = persistenceFailure(cause, "lease_renew_failed")
	}
	if failure.Code == "lease_lost" {
		return failure
	}
	if !job.Dispatched && job.ExecutionMode == "async" && (failure.Code == "capacity_saturated" || failure.Code == "temporarily_unhealthy" || failure.Code == "audit_paused") {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
		defer cancel()
		if err := s.repo.DeferCapacity(persistCtx, job, failure); err != nil {
			s.noteError("capacity_defer_failed", err)
		}
		s.notify()
		return failure
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing && (job.ExecutionMode == "async" || job.auditRunKind() == "reaudit") {
		failure = &AuditError{Code: "worker_paused", Stage: "worker", Message: "服务正在停止，任务将在恢复后继续", Retryable: true}
	}
	retry := (job.ExecutionMode == "async" || job.auditRunKind() == "reaudit") && failure.Retryable &&
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
	if request.ReuseMode == "" {
		request.ReuseMode = ReuseModeAllow
	}
	if request.ReuseMode != ReuseModeAllow && request.ReuseMode != ReuseModeForce {
		return nil, errors.New("复核复用方式无效")
	}
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
