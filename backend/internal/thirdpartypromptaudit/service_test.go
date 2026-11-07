package thirdpartypromptaudit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// async 模式下已判定阻断的回合，续跑请求必须被拒绝（决策 11）。
func TestBlockedTurnRejectsContinuationInAsyncMode(t *testing.T) {
	service := &Service{metrics: NewRuntimeMetrics()}
	decision := service.boundDecision(&turnState{JobID: 12, Status: "done", Decision: string(DecisionBlock)}, "async", "key")
	require.Equal(t, IngressDecisionBlock, decision.Kind)
	require.Equal(t, "third_party_audit_blocked", decision.ErrorCode)
	require.Equal(t, int64(12), decision.JobID)

	allow := service.boundDecision(&turnState{JobID: 12, Status: "done", Decision: string(DecisionPass)}, "async", "key")
	require.Equal(t, IngressDecisionAllow, allow.Kind)
	flag := service.boundDecision(&turnState{JobID: 12, Status: "done", Decision: string(DecisionReview)}, "async", "key")
	require.Equal(t, IngressDecisionFlag, flag.Kind)
	// 首轮仍在审核中：async 直接转发，不等待也不落库。
	pending := service.boundDecision(&turnState{JobID: 12, Status: "queued"}, "async", "key")
	require.Equal(t, IngressDecisionAllow, pending.Kind)
}

// Redis 读写失败时 async 放行、blocking 按不可用处理（决策 11）。
func TestRedisFailureSemanticsFollowMode(t *testing.T) {
	service := &Service{metrics: NewRuntimeMetrics()}
	require.Equal(t, IngressDecisionAllow, service.redisFailure("async", "turn_key_unavailable", "turn", errors.New("NOPERM")).Kind)
	require.Equal(t, IngressDecisionUnavailable, service.redisFailure("blocking", "turn_key_unavailable", "turn", errors.New("NOPERM")).Kind)
}

// 没有 Redis 时回合查询必须报错，不能静默当成未命中。
func TestTurnLookupWithoutRedisFails(t *testing.T) {
	service := &Service{metrics: NewRuntimeMetrics()}
	_, err := service.getTurn(context.Background(), "key")
	require.Error(t, err)
	_, err = service.resolveResponseTurn(context.Background(), "resp")
	require.Error(t, err)
}

// Redis 故障时本地兜底仍拦得住已阻断回合。
func TestRedisFailureFallsBackToLocalBlockedTurn(t *testing.T) {
	service := &Service{metrics: NewRuntimeMetrics(), blockedTurns: map[string]time.Time{}}
	service.rememberBlockedTurn("turn-1")
	decision := service.redisFailure("async", "turn_key_unavailable", "turn-1", errors.New("NOPERM"))
	require.Equal(t, IngressDecisionBlock, decision.Kind)
	other := service.redisFailure("async", "turn_key_unavailable", "turn-2", errors.New("NOPERM"))
	require.Equal(t, IngressDecisionAllow, other.Kind)
}

// 续跑被拒绝只打运行日志，不写数据库、不产生任务。
func TestRejectedContinuationStaysInRuntimeLog(t *testing.T) {
	service := &Service{metrics: NewRuntimeMetrics()}
	service.RejectContinuation(nil, 9, "third_party_audit_blocked")
	service.RejectContinuation(&IntakeDecision{JobID: 5, Kind: IngressDecisionBlock}, 9, "third_party_audit_blocked")
}
