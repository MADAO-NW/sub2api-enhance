package thirdpartypromptaudit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// memoryAuditStore 是真实调用流水的内存实现，只用于不接触数据库的单元测试。
type memoryAuditStore struct {
	mu        sync.Mutex
	attempts  []*ModelAttempt
	startFail error
}

func (s *memoryAuditStore) PrepareModelAttempt(_ context.Context, _ *AuditJob, attempt *ModelAttempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt.ID = int64(len(s.attempts) + 1)
	s.attempts = append(s.attempts, attempt)
	return nil
}

func (s *memoryAuditStore) StartModelAttempt(_ context.Context, _ *AuditJob, attempt *ModelAttempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startFail != nil {
		return s.startFail
	}
	now := time.Now()
	attempt.DispatchStartedAt = &now
	return nil
}

func (s *memoryAuditStore) FinishModelAttempt(context.Context, *AuditJob, *ModelAttempt) error {
	return nil
}

func pointer(value float64) *float64 { return &value }

// 聚合必须用固定启用节点分母，未知票不能被当成通过。
func TestAggregateVotesUsesFixedDenominator(t *testing.T) {
	config := testConfig()
	// 1 block + 1 失败 + 1 pass：多数阻断需要 2 票，阻断与失败合计已达阈值，
	// 只能得出"有效覆盖不足"，不能当成通过。
	config.Aggregation = "majority_block"
	votes := []modelVote{{Decision: DecisionBlock}, {Decision: DecisionPass, Error: &AuditError{Code: "timeout", Message: "超时"}}, {Decision: DecisionPass}}
	decision, partial, failure := aggregateVotes(votes, config)
	require.Empty(t, decision)
	require.True(t, partial)
	require.NotNil(t, failure)

	// all_block 未达阻断阈值时按 review 处理，并标记部分失败。
	config.Aggregation = "all_block"
	decision, partial, failure = aggregateVotes(votes, config)
	require.Equal(t, DecisionReview, decision)
	require.True(t, partial)
	require.Nil(t, failure)

	// any_block 一票阻断即确定结论。
	config.Aggregation = "any_block"
	decision, partial, failure = aggregateVotes(votes, config)
	require.Equal(t, DecisionBlock, decision)
	require.True(t, partial)
	require.Nil(t, failure)

	// 两票阻断满足多数阻断。
	config.Aggregation = "majority_block"
	decision, _, failure = aggregateVotes([]modelVote{{Decision: DecisionBlock}, {Decision: DecisionBlock}, {Decision: DecisionPass}}, config)
	require.Equal(t, DecisionBlock, decision)
	require.Nil(t, failure)
}

func TestAggregateVotesRejectsSkippedNodesBeforeConclusion(t *testing.T) {
	config := testConfig()
	config.Aggregation = "all_block"
	_, _, failure := aggregateVotes([]modelVote{{Decision: DecisionPass}, {Skipped: true}}, config)
	require.NotNil(t, failure)
	require.Equal(t, "invalid_model_result", failure.Code)
}

// 任务裁决取各目标中最严重的一项，已有 block 时可以提前结束。
func TestRoundDecisionTakesMostSevereTarget(t *testing.T) {
	decision, trigger, ready := roundDecision([]JobTarget{
		{Kind: PromptKindInstruction, Resolution: ResolutionModel, Decision: DecisionReview},
		{Kind: PromptKindUser, Resolution: ResolutionModel, Decision: DecisionBlock},
	})
	require.True(t, ready)
	require.Equal(t, DecisionBlock, decision)
	require.Equal(t, PromptKindUser, trigger.Kind)

	// 已有 block 时，其余目标没有结论也可以确定裁决。
	decision, _, ready = roundDecision([]JobTarget{
		{Kind: PromptKindInstruction, Resolution: ResolutionModel, Decision: DecisionBlock},
		{Kind: PromptKindUser},
	})
	require.True(t, ready)
	require.Equal(t, DecisionBlock, decision)

	// 没有 block 且存在未完成目标时不能给出裁决。
	_, _, ready = roundDecision([]JobTarget{{Kind: PromptKindUser}, {Kind: PromptKindInstruction}})
	require.False(t, ready)

	// 同等严重程度下优先标记用户提示词。
	_, trigger, ready = roundDecision([]JobTarget{
		{Kind: PromptKindInstruction, Resolution: ResolutionCache, Decision: DecisionReview},
		{Kind: PromptKindUser, Resolution: ResolutionInflight, Decision: DecisionReview},
	})
	require.True(t, ready)
	require.Equal(t, PromptKindUser, trigger.Kind)
}

// fakeLedger 模拟 instruction_violation_marks 与 instruction_breakers 的判定依据。
type fakeLedger struct {
	users      map[string]int
	breakers   map[string]string
	marks      map[string]bool
	suppressed map[string]bool
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{users: map[string]int{}, breakers: map[string]string{}, marks: map[string]bool{}, suppressed: map[string]bool{}}
}

func (l *fakeLedger) lockInstruction(context.Context, string) error { return nil }
func (l *fakeLedger) breakerStatus(_ context.Context, hash string) (string, bool, error) {
	status, ok := l.breakers[hash]
	return status, ok, nil
}
func (l *fakeLedger) markExists(_ context.Context, userID int64, hash string) (bool, error) {
	return l.marks[itoa(int(userID))+":"+hash], nil
}
func (l *fakeLedger) distinctUsers(_ context.Context, hash string) (int, error) {
	return l.users[hash], nil
}
func (l *fakeLedger) insertMark(_ context.Context, userID int64, hash string, _ int64, suppressed bool) error {
	l.marks[itoa(int(userID))+":"+hash] = true
	l.suppressed[itoa(int(userID))+":"+hash] = suppressed
	l.users[hash]++
	return nil
}
func (l *fakeLedger) tripBreaker(_ context.Context, hash string, _ int, _ int64) error {
	l.breakers[hash] = BreakerTripped
	return nil
}
func (l *fakeLedger) updateBreakerUsers(context.Context, string, int) error { return nil }

// 同一用户的同一指令块只计一次；第二个用户触发熔断后不再计入累计。
func TestInstructionViolationsCountOnceAndTripAtTwoUsers(t *testing.T) {
	ledger := newFakeLedger()
	hash := "hash-1"
	first, err := applyInstructionViolations(context.Background(), ledger, 1, 10, []string{hash})
	require.NoError(t, err)
	require.Equal(t, []string{hash}, first.Countable)
	require.Empty(t, first.Tripped)

	repeated, err := applyInstructionViolations(context.Background(), ledger, 1, 11, []string{hash})
	require.NoError(t, err)
	require.Empty(t, repeated.Countable)
	require.Equal(t, []string{hash}, repeated.Repeated)

	second, err := applyInstructionViolations(context.Background(), ledger, 2, 12, []string{hash})
	require.NoError(t, err)
	require.Equal(t, []string{hash}, second.Tripped)
	require.Empty(t, second.Countable)
	require.Equal(t, []string{hash}, second.Suppressed)

	paused, err := applyInstructionViolations(context.Background(), ledger, 3, 13, []string{hash})
	require.NoError(t, err)
	require.Empty(t, paused.Countable)
	require.Equal(t, []string{hash}, paused.Suppressed)
	require.Empty(t, paused.Tripped)
}

// 已确认违规的哈希不再熔断，违规按正常规则计一次。
func TestConfirmedBreakerNeverTripsAgain(t *testing.T) {
	ledger := newFakeLedger()
	hash := "hash-confirmed"
	ledger.breakers[hash] = BreakerConfirmed
	outcome, err := applyInstructionViolations(context.Background(), ledger, 1, 20, []string{hash})
	require.NoError(t, err)
	require.Equal(t, []string{hash}, outcome.Countable)
	require.Empty(t, outcome.Tripped)
}

func TestClassifyScoreUsesFrozenThresholds(t *testing.T) {
	config := testConfig()
	require.Equal(t, DecisionPass, classifyScore(*pointer(0.1), config))
	require.Equal(t, DecisionReview, classifyScore(*pointer(0.6), config))
	require.Equal(t, DecisionBlock, classifyScore(*pointer(0.9), config))
}

func TestEvaluationHashIgnoresRequestProtocol(t *testing.T) {
	config := testConfig()
	snapshot := ConfigSnapshot{Config: config, ContractVersion: ContractVersion, FixedContract: OutputContract}
	snapshot.AuditPrompt = "政策"
	instructions, err := evaluationHash(snapshot, PromptKindInstruction)
	require.NoError(t, err)
	users, err := evaluationHash(snapshot, PromptKindUser)
	require.NoError(t, err)
	require.NotEqual(t, instructions, users)
	snapshot.Models[0].Parameters = map[string]any{"temperature": 0.2}
	changed, err := evaluationHash(snapshot, PromptKindUser)
	require.NoError(t, err)
	require.NotEqual(t, users, changed)
	// 禁用节点不进入节点语义。
	snapshot.Models[0].Enabled = false
	disabled, err := evaluationHash(snapshot, PromptKindUser)
	require.NoError(t, err)
	require.NotEqual(t, changed, disabled)
}
