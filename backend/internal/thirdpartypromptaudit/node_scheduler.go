package thirdpartypromptaudit

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const (
	nodeHealthUnknown       = "unknown"
	nodeHealthHealthy       = "healthy"
	nodeHealthDegraded      = "degraded"
	nodeHealthCooldown      = "cooldown"
	nodeHealthHalfOpen      = "half_open"
	nodeHealthMisconfigured = "misconfigured"
	nodeHealthDisabled      = "disabled"
)

type nodeScheduleState struct {
	active, waiting, limit, consecutiveFailures, openCount int
	configured, probing                                    bool
	health                                                 string
	cooldownUntil                                          time.Time
	latencyEWMAMS                                          *float64
	lastErrorCode                                          string
	lastObservedAt                                         time.Time
}

type nodeScheduler struct {
	mu                 sync.Mutex
	states             map[string]*nodeScheduleState
	changed            chan struct{}
	cursor             uint64
	backpressuredTotal atomic.Uint64
	blockingWaiters    atomic.Int64
}

type nodeScheduleError struct {
	Code       string
	RetryAfter time.Duration
}

func (e *nodeScheduleError) Error() string { return e.Code }

type nodeRuntimeSnapshot struct {
	ModelID             string     `json:"model_id"`
	Health              string     `json:"health"`
	Active              int        `json:"active"`
	MaxConcurrency      int        `json:"max_concurrency"`
	EligibleWaiters     int        `json:"eligible_waiters"`
	LoadRatio           float64    `json:"load_ratio"`
	LatencyEWMAMS       *float64   `json:"latency_ewma_ms"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	CooldownUntil       *time.Time `json:"cooldown_until"`
	NextProbeAt         *time.Time `json:"next_probe_at"`
	LastErrorCode       string     `json:"last_error_code"`
	LastObservedAt      *time.Time `json:"last_observed_at"`
}

type schedulerRuntimeSnapshot struct {
	BackpressuredTotal uint64                `json:"backpressured_total"`
	BlockingWaiters    int64                 `json:"blocking_waiters"`
	Nodes              []nodeRuntimeSnapshot `json:"nodes"`
}

func newNodeScheduler() *nodeScheduler {
	return &nodeScheduler{states: make(map[string]*nodeScheduleState), changed: make(chan struct{})}
}

// updateLimits 应用最新节点容量；缩容不取消已取得的并发位。
func (s *nodeScheduler) updateLimits(models []ModelConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, state := range s.states {
		state.configured = false
	}
	for _, model := range models {
		if !model.Enabled {
			continue
		}
		state := s.state(model.ID, model.MaxConcurrency)
		state.limit, state.configured = model.MaxConcurrency, true
	}
	for id, state := range s.states {
		if !state.configured && state.active == 0 && state.waiting == 0 {
			delete(s.states, id)
		}
	}
	s.signal()
}

// resetHealth 在管理员保存节点配置后重新验证全部启用节点。
func (s *nodeScheduler) resetHealth() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, state := range s.states {
		state.health, state.lastErrorCode = nodeHealthUnknown, ""
		state.consecutiveFailures, state.openCount = 0, 0
		state.cooldownUntil, state.probing = time.Time{}, false
	}
	s.signal()
}

func (s *nodeScheduler) acquire(ctx context.Context, candidates []ModelConfig) (ModelConfig, func(), error) {
	model, release, _, err := s.acquireWithPolicy(ctx, candidates, true)
	return model, release, err
}

// acquireWithPolicy 原子选择健康且最空闲的未使用节点；异步任务可选择立即背压。
func (s *nodeScheduler) acquireWithPolicy(ctx context.Context, candidates []ModelConfig, waitForCapacity bool) (ModelConfig, func(), DispatchSnapshot, error) {
	if len(candidates) == 0 {
		return ModelConfig{}, nil, DispatchSnapshot{}, errors.New("没有可调度的审核节点")
	}
	if waitForCapacity {
		s.blockingWaiters.Add(1)
		defer s.blockingWaiters.Add(-1)
	}
	for {
		s.mu.Lock()
		best, eligible := -1, 0
		soonest := time.Time{}
		temporarilyUnavailable := false
		for i, candidate := range candidates {
			state := s.state(candidate.ID, candidate.MaxConcurrency)
			if state.health == nodeHealthCooldown {
				temporarilyUnavailable = true
				if soonest.IsZero() || state.cooldownUntil.Before(soonest) {
					soonest = state.cooldownUntil
				}
				continue
			}
			if state.health == nodeHealthHalfOpen {
				temporarilyUnavailable = true
				continue
			}
			if state.health == nodeHealthMisconfigured {
				continue
			}
			eligible++
			if state.active >= state.limit {
				continue
			}
			if best < 0 || s.lessLoaded(state, s.states[candidates[best].ID], i, best, len(candidates)) {
				best = i
			}
		}
		if best >= 0 {
			chosen := candidates[best]
			state := s.states[chosen.ID]
			state.active++
			s.cursor++
			dispatch := s.dispatchSnapshot(state)
			dispatch.Reason = "health_then_load"
			s.mu.Unlock()
			var once sync.Once
			return chosen, func() {
				once.Do(func() {
					s.mu.Lock()
					state.active--
					if !state.configured && state.active == 0 && state.waiting == 0 {
						delete(s.states, chosen.ID)
					}
					s.signal()
					s.mu.Unlock()
				})
			}, dispatch, nil
		}
		code, retryAfter := "capacity_saturated", time.Second
		if eligible == 0 {
			if temporarilyUnavailable {
				code, retryAfter = "temporarily_unhealthy", time.Until(soonest)
				if soonest.IsZero() || retryAfter < 0 {
					retryAfter = time.Second
				}
			} else {
				code, retryAfter = "no_healthy_nodes", 0
			}
		}
		if !waitForCapacity || code == "no_healthy_nodes" {
			s.backpressuredTotal.Add(1)
			s.mu.Unlock()
			return ModelConfig{}, nil, DispatchSnapshot{}, &nodeScheduleError{Code: code, RetryAfter: retryAfter}
		}
		wait := s.changed
		for _, candidate := range candidates {
			state := s.state(candidate.ID, candidate.MaxConcurrency)
			if state.health != nodeHealthMisconfigured {
				state.waiting++
			}
		}
		s.mu.Unlock()
		var timer <-chan time.Time
		if !soonest.IsZero() {
			delay := time.Until(soonest)
			if delay < 0 {
				delay = 0
			}
			timer = time.After(delay)
		}
		select {
		case <-wait:
		case <-timer:
		case <-ctx.Done():
		}
		s.mu.Lock()
		for _, candidate := range candidates {
			state := s.states[candidate.ID]
			if state != nil && state.waiting > 0 {
				state.waiting--
			}
		}
		s.mu.Unlock()
		if ctx.Err() != nil {
			return ModelConfig{}, nil, DispatchSnapshot{}, ctx.Err()
		}
	}
}

// observe 用真实调用或节点测试更新健康，不使用审核业务分数。
func (s *nodeScheduler) observe(id string, failure *AuditError, latency time.Duration) {
	if failure != nil && failure.Stage != "config" && failure.Stage != "model_request" && failure.Stage != "response_parse" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state(id, DefaultNodeMaxConcurrency)
	now := time.Now().UTC()
	state.lastObservedAt, state.probing = now, false
	if latency > 0 {
		value := float64(latency) / float64(time.Millisecond)
		if state.latencyEWMAMS == nil {
			state.latencyEWMAMS = &value
		} else {
			next := 0.2*value + 0.8**state.latencyEWMAMS
			state.latencyEWMAMS = &next
		}
	}
	if failure == nil {
		state.health, state.lastErrorCode = nodeHealthHealthy, ""
		state.consecutiveFailures, state.openCount = 0, 0
		state.cooldownUntil = time.Time{}
		s.signal()
		return
	}
	state.lastErrorCode = failure.Code
	if (failure.Code == "upstream_http_error" && !failure.Retryable) ||
		failure.Code == "invalid_model" || failure.Code == "node_unavailable" || failure.Code == "credential_binding_unavailable" || failure.Code == "audit_node_recursion" {
		state.health, state.cooldownUntil = nodeHealthMisconfigured, time.Time{}
		s.signal()
		return
	}
	state.consecutiveFailures++
	if failure.Code == "rate_limited" {
		state.openCount++
		state.health = nodeHealthCooldown
		delay := failure.RetryAfter
		if delay <= 0 {
			delay = 30 * time.Second
		}
		state.cooldownUntil = now.Add(delay)
	} else if state.consecutiveFailures >= 2 {
		state.openCount++
		state.health = nodeHealthCooldown
		delay := 15 * time.Second * time.Duration(math.Pow(2, float64(state.openCount-1)))
		if delay > 5*time.Minute {
			delay = 5 * time.Minute
		}
		state.cooldownUntil = now.Add(delay)
	} else {
		state.health = nodeHealthDegraded
	}
	s.signal()
}

// dueProbe 为到期异常节点预留低优先级恢复探测槽位。
func (s *nodeScheduler) dueProbe(models []ModelConfig) (ModelConfig, func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, model := range models {
		state := s.state(model.ID, model.MaxConcurrency)
		if !model.Enabled || state.health != nodeHealthCooldown || state.cooldownUntil.After(now) || state.probing || state.waiting > 0 || state.active >= state.limit {
			continue
		}
		state.health, state.probing = nodeHealthHalfOpen, true
		state.active++
		var once sync.Once
		return model, func() {
			once.Do(func() {
				s.mu.Lock()
				state.active--
				state.probing = false
				if state.health == nodeHealthHalfOpen {
					state.health = nodeHealthCooldown
					state.cooldownUntil = time.Now().UTC().Add(15 * time.Second)
				}
				s.signal()
				s.mu.Unlock()
			})
		}, true
	}
	return ModelConfig{}, nil, false
}

func (s *nodeScheduler) state(id string, limit int) *nodeScheduleState {
	state := s.states[id]
	if state == nil {
		if limit < 1 {
			limit = DefaultNodeMaxConcurrency
		}
		state = &nodeScheduleState{limit: limit, health: nodeHealthUnknown}
		s.states[id] = state
	}
	return state
}

func (s *nodeScheduler) lessLoaded(left, right *nodeScheduleState, leftIndex, rightIndex, count int) bool {
	leftRank, rightRank := healthRank(left.health), healthRank(right.health)
	if leftRank != rightRank {
		return leftRank < rightRank
	}
	leftRatio, rightRatio := left.active*right.limit, right.active*left.limit
	if leftRatio != rightRatio {
		return leftRatio < rightRatio
	}
	if left.waiting != right.waiting {
		return left.waiting < right.waiting
	}
	if left.latencyEWMAMS != nil && right.latencyEWMAMS != nil && *left.latencyEWMAMS != *right.latencyEWMAMS {
		return *left.latencyEWMAMS < *right.latencyEWMAMS
	}
	start := int(s.cursor % uint64(count))
	return (leftIndex-start+count)%count < (rightIndex-start+count)%count
}

func healthRank(health string) int {
	if health == nodeHealthDegraded {
		return 1
	}
	return 0
}

func (s *nodeScheduler) dispatchSnapshot(state *nodeScheduleState) DispatchSnapshot {
	return DispatchSnapshot{Health: state.health, Active: state.active, MaxConcurrency: state.limit, Waiting: state.waiting,
		LoadRatio: float64(state.active) / float64(state.limit), LatencyEWMAMS: state.latencyEWMAMS}
}

func (s *nodeScheduler) runtime(models []ModelConfig) schedulerRuntimeSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := schedulerRuntimeSnapshot{BackpressuredTotal: s.backpressuredTotal.Load(), BlockingWaiters: s.blockingWaiters.Load(), Nodes: []nodeRuntimeSnapshot{}}
	for _, model := range models {
		state := s.state(model.ID, model.MaxConcurrency)
		health := state.health
		if !model.Enabled {
			health = nodeHealthDisabled
		}
		item := nodeRuntimeSnapshot{ModelID: model.ID, Health: health, Active: state.active, MaxConcurrency: state.limit, EligibleWaiters: state.waiting,
			LoadRatio: float64(state.active) / float64(state.limit), LatencyEWMAMS: state.latencyEWMAMS, ConsecutiveFailures: state.consecutiveFailures, LastErrorCode: state.lastErrorCode}
		if !state.cooldownUntil.IsZero() {
			value := state.cooldownUntil.UTC()
			item.CooldownUntil, item.NextProbeAt = &value, &value
		}
		if !state.lastObservedAt.IsZero() {
			value := state.lastObservedAt.UTC()
			item.LastObservedAt = &value
		}
		result.Nodes = append(result.Nodes, item)
	}
	return result
}

func (s *nodeScheduler) signal() {
	close(s.changed)
	s.changed = make(chan struct{})
}
