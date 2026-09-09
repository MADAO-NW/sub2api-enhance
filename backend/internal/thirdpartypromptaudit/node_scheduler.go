package thirdpartypromptaudit

import (
	"context"
	"errors"
	"sync"
)

type nodeScheduleState struct {
	active     int
	waiting    int
	limit      int
	configured bool
}

type nodeScheduler struct {
	mu      sync.Mutex
	states  map[string]*nodeScheduleState
	changed chan struct{}
	cursor  uint64
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
		state.limit = model.MaxConcurrency
		state.configured = true
	}
	for id, state := range s.states {
		if !state.configured && state.active == 0 && state.waiting == 0 {
			delete(s.states, id)
		}
	}
	s.signal()
}

// acquire 从未使用候选节点中原子选择并预留当前最空闲的节点。
func (s *nodeScheduler) acquire(ctx context.Context, candidates []ModelConfig) (ModelConfig, func(), error) {
	if len(candidates) == 0 {
		return ModelConfig{}, nil, errors.New("没有可调度的审核节点")
	}
	for {
		s.mu.Lock()
		best := -1
		for i, candidate := range candidates {
			state := s.state(candidate.ID, candidate.MaxConcurrency)
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
			}, nil
		}
		wait := s.changed
		for _, candidate := range candidates {
			s.state(candidate.ID, candidate.MaxConcurrency).waiting++
		}
		s.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
		}
		s.mu.Lock()
		for _, candidate := range candidates {
			state := s.states[candidate.ID]
			if state != nil && state.waiting > 0 {
				state.waiting--
				if !state.configured && state.active == 0 && state.waiting == 0 {
					delete(s.states, candidate.ID)
				}
			}
		}
		s.mu.Unlock()
		if ctx.Err() != nil {
			return ModelConfig{}, nil, ctx.Err()
		}
	}
}

func (s *nodeScheduler) state(id string, limit int) *nodeScheduleState {
	state := s.states[id]
	if state == nil {
		if limit < 1 {
			limit = DefaultNodeMaxConcurrency
		}
		state = &nodeScheduleState{limit: limit}
		s.states[id] = state
	}
	return state
}

func (s *nodeScheduler) lessLoaded(left, right *nodeScheduleState, leftIndex, rightIndex, count int) bool {
	leftRatio := left.active * right.limit
	rightRatio := right.active * left.limit
	if leftRatio != rightRatio {
		return leftRatio < rightRatio
	}
	if left.waiting != right.waiting {
		return left.waiting < right.waiting
	}
	start := int(s.cursor % uint64(count))
	return (leftIndex-start+count)%count < (rightIndex-start+count)%count
}

func (s *nodeScheduler) signal() {
	close(s.changed)
	s.changed = make(chan struct{})
}
