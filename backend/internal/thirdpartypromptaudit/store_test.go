package thirdpartypromptaudit

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// memoryLedger 模拟事务内的只计一次与熔断记录；lockInstruction 持有按哈希的互斥锁直到 commit。
type memoryLedger struct {
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	marks    map[string]map[int64]bool // hash -> user -> suppressed
	breakers map[string]string
	users    map[string]int
}

func newMemoryLedger() *memoryLedger {
	return &memoryLedger{locks: map[string]*sync.Mutex{}, marks: map[string]map[int64]bool{}, breakers: map[string]string{}, users: map[string]int{}}
}

type memoryTx struct {
	ledger *memoryLedger
	held   []*sync.Mutex
}

func (l *memoryLedger) begin() *memoryTx { return &memoryTx{ledger: l} }

func (t *memoryTx) commit() {
	for i := len(t.held) - 1; i >= 0; i-- {
		t.held[i].Unlock()
	}
}

func (t *memoryTx) lockInstruction(_ context.Context, hash string) error {
	t.ledger.mu.Lock()
	lock := t.ledger.locks[hash]
	if lock == nil {
		lock = &sync.Mutex{}
		t.ledger.locks[hash] = lock
	}
	t.ledger.mu.Unlock()
	lock.Lock()
	t.held = append(t.held, lock)
	return nil
}

func (t *memoryTx) breakerStatus(_ context.Context, hash string) (string, bool, error) {
	t.ledger.mu.Lock()
	defer t.ledger.mu.Unlock()
	status, ok := t.ledger.breakers[hash]
	return status, ok, nil
}

func (t *memoryTx) markExists(_ context.Context, userID int64, hash string) (bool, error) {
	t.ledger.mu.Lock()
	defer t.ledger.mu.Unlock()
	_, ok := t.ledger.marks[hash][userID]
	return ok, nil
}

func (t *memoryTx) distinctUsers(_ context.Context, hash string) (int, error) {
	t.ledger.mu.Lock()
	defer t.ledger.mu.Unlock()
	return len(t.ledger.marks[hash]), nil
}

func (t *memoryTx) insertMark(_ context.Context, userID int64, hash string, _ int64, suppressed bool) error {
	t.ledger.mu.Lock()
	defer t.ledger.mu.Unlock()
	if t.ledger.marks[hash] == nil {
		t.ledger.marks[hash] = map[int64]bool{}
	}
	t.ledger.marks[hash][userID] = suppressed
	return nil
}

func (t *memoryTx) tripBreaker(_ context.Context, hash string, users int, _ int64) error {
	t.ledger.mu.Lock()
	defer t.ledger.mu.Unlock()
	t.ledger.breakers[hash], t.ledger.users[hash] = BreakerTripped, users
	return nil
}

func (t *memoryTx) updateBreakerUsers(_ context.Context, hash string, users int) error {
	t.ledger.mu.Lock()
	defer t.ledger.mu.Unlock()
	t.ledger.users[hash] = users
	return nil
}

func apply(t *testing.T, ledger *memoryLedger, userID int64, hashes ...string) instructionOutcome {
	t.Helper()
	tx := ledger.begin()
	defer tx.commit()
	outcome, err := applyInstructionViolations(context.Background(), tx, userID, 1, hashes)
	require.NoError(t, err)
	return outcome
}

func TestInstructionViolationCountsOncePerUserAndHash(t *testing.T) {
	ledger := newMemoryLedger()
	first := apply(t, ledger, 7, "h1")
	require.Equal(t, []string{"h1"}, first.Countable)
	again := apply(t, ledger, 7, "h1")
	require.Empty(t, again.Countable)
	require.Equal(t, []string{"h1"}, again.Repeated)
	// 同一任务里重复的哈希只处理一次。
	both := apply(t, ledger, 8, "h2", "h2")
	require.Equal(t, []string{"h2"}, both.Countable)
}

func TestInstructionBreakerThresholdBoundary(t *testing.T) {
	ledger := newMemoryLedger()
	one := apply(t, ledger, 1, "shared")
	require.Equal(t, []string{"shared"}, one.Countable)
	require.Empty(t, one.Tripped)
	require.NotContains(t, ledger.breakers, "shared")

	two := apply(t, ledger, 2, "shared")
	require.Empty(t, two.Countable)
	require.Equal(t, []string{"shared"}, two.Tripped)
	require.Equal(t, []string{"shared"}, two.Suppressed)
	require.Equal(t, BreakerTripped, ledger.breakers["shared"])
	require.Equal(t, 2, ledger.users["shared"])
	require.False(t, ledger.marks["shared"][1], "熔断前已计入的记录保持不变")
	require.True(t, ledger.marks["shared"][2])

	three := apply(t, ledger, 3, "shared")
	require.Empty(t, three.Countable)
	require.Empty(t, three.Tripped)
	require.Equal(t, []string{"shared"}, three.Suppressed)
	require.Equal(t, 3, ledger.users["shared"])
}

func TestConfirmedBreakerCountsNormallyAndNeverTripsAgain(t *testing.T) {
	ledger := newMemoryLedger()
	ledger.breakers["h"] = BreakerConfirmed
	ledger.marks["h"] = map[int64]bool{1: false}
	outcome := apply(t, ledger, 2, "h")
	require.Equal(t, []string{"h"}, outcome.Countable)
	require.Empty(t, outcome.Tripped)
	require.Equal(t, BreakerConfirmed, ledger.breakers["h"])

	ledger.breakers["t"] = BreakerTrusted
	require.Equal(t, []string{"t"}, apply(t, ledger, 2, "t").Suppressed)
}

func TestConcurrentCompletionTripsBreakerExactlyOnce(t *testing.T) {
	for round := 0; round < 50; round++ {
		ledger := newMemoryLedger()
		var wg sync.WaitGroup
		results := make([]instructionOutcome, 2)
		start := make(chan struct{})
		for i := range results {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				<-start
				tx := ledger.begin()
				defer tx.commit()
				outcome, err := applyInstructionViolations(context.Background(), tx, int64(index+1), int64(index+1), []string{"shared"})
				require.NoError(t, err)
				results[index] = outcome
			}(i)
		}
		close(start)
		wg.Wait()
		countable, tripped := 0, 0
		for _, result := range results {
			countable += len(result.Countable)
			tripped += len(result.Tripped)
		}
		// 加锁后统计：两个任务同时完成时最多一人被计入，另一个触发熔断。
		require.Equal(t, 1, countable)
		require.Equal(t, 1, tripped)
		require.Equal(t, BreakerTripped, ledger.breakers["shared"])
	}
}

func TestSQLLedgerLocksBeforeCounting(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`SELECT pg_advisory_xact_lock($1)`)).WithArgs(instructionLockKey("h")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT status FROM sub2api_enhance.instruction_breakers WHERE hash=\$1 FOR UPDATE`).WithArgs("h").WillReturnRows(sqlmock.NewRows([]string{"status"}))
	mock.ExpectQuery(`instruction_violation_marks WHERE user_id=\$1 AND hash=\$2`).WithArgs(int64(9), "h").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT count\(\*\) FROM sub2api_enhance.instruction_violation_marks WHERE hash=\$1`).WithArgs("h").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(`INSERT INTO sub2api_enhance.instruction_violation_marks`).WithArgs(int64(9), "h", int64(5), true).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO sub2api_enhance.instruction_breakers`).WithArgs("h", 2, int64(5)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	tx, err := db.Begin()
	require.NoError(t, err)
	outcome, err := applyInstructionViolations(context.Background(), sqlInstructionLedger{tx: tx}, 9, 5, []string{"h"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, []string{"h"}, outcome.Tripped)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRetentionKeepsEnforcementMarksBreakersAndTrust(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	for _, statement := range retentionStatements {
		for _, forbidden := range []string{"enforcement", "instruction_violation_marks WHERE", "DELETE FROM sub2api_enhance.instruction_breakers", "DELETE FROM sub2api_enhance.prompt_trust"} {
			require.NotContains(t, statement.query, forbidden, statement.name)
		}
	}
	require.Contains(t, retentionStatements[0].query, "status NOT IN ('queued','processing','retry')")
	// 任务按批删除：满批时继续下一批。
	mock.ExpectExec(`DELETE FROM sub2api_enhance.audit_jobs`).WithArgs(30, retentionBatchSize).WillReturnResult(sqlmock.NewResult(0, retentionBatchSize))
	mock.ExpectExec(`DELETE FROM sub2api_enhance.audit_jobs`).WithArgs(30, retentionBatchSize).WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(`DELETE FROM sub2api_enhance.model_attempts`).WithArgs(30, retentionBatchSize).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`row_number\(\) OVER \(PARTITION BY r.hash,r.eval_hash`).WithArgs(retentionBatchSize).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM sub2api_enhance.prompt_reviews WHERE id IN \(SELECT r.id`).WithArgs(30, retentionBatchSize).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`DELETE FROM sub2api_enhance.prompt_texts`).WithArgs(30, retentionBatchSize).WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectExec(`DELETE FROM sub2api_enhance.policy_replays`).WithArgs(30, retentionBatchSize).WillReturnResult(sqlmock.NewResult(0, 2))
	repo := NewRepository(db)
	result, err := repo.RunRetention(context.Background(), 30)
	require.Equal(t, RetentionResult{Jobs: retentionBatchSize + 3, ProbeAttempts: 1, ReviewDuplicates: 1, Reviews: 2, Texts: 4, Replays: 2}, result)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRetentionStatementArgsMatchPlaceholders 校验每条清理语句引用的占位符数量与传入参数数量一致，
// 避免多传未引用参数导致 PostgreSQL 无法推断参数类型。
func TestRetentionStatementArgsMatchPlaceholders(t *testing.T) {
	placeholder := regexp.MustCompile(`\$(\d+)`)
	for _, statement := range retentionStatements {
		largest := 0
		for _, match := range placeholder.FindAllStringSubmatch(statement.query, -1) {
			number, err := strconv.Atoi(match[1])
			require.NoError(t, err, statement.name)
			if number > largest {
				largest = number
			}
		}
		require.Equal(t, largest, len(statement.args(30)), statement.name)
	}
}

func TestUntrustRestoresBreakerToTripped(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM sub2api_enhance.prompt_trust WHERE hash=\$1`).WithArgs("h").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE sub2api_enhance.instruction_breakers SET status='tripped'.*WHERE hash=\$1 AND status='trusted'`).WithArgs("h").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, NewRepository(db).UntrustInstruction(context.Background(), "h", 1))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTrustRejectsUserPromptAndConfirmDeletesSuppressedMarks(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT kind FROM sub2api_enhance.prompt_texts`).WithArgs("u").WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("user"))
	mock.ExpectRollback()
	err = NewRepository(db).TrustInstruction(context.Background(), "u", 1, "")
	require.ErrorContains(t, err, "只允许信任指令块")

	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT status FROM sub2api_enhance.instruction_breakers`).WithArgs("h").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("tripped"))
	mock.ExpectExec(`DELETE FROM sub2api_enhance.instruction_violation_marks WHERE hash=\$1 AND suppressed`).WithArgs("h").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE sub2api_enhance.instruction_breakers SET status='confirmed'`).WithArgs("h", int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, NewRepository(db).ConfirmBreaker(context.Background(), "h", 1))

	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT status FROM sub2api_enhance.instruction_breakers`).WithArgs("h").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("trusted"))
	mock.ExpectRollback()
	require.ErrorContains(t, NewRepository(db).ConfirmBreaker(context.Background(), "h", 1), "只能确认熔断中")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRoundDecisionTakesMostSevereAndAllowsEarlyBlock(t *testing.T) {
	decision, trigger, ready := roundDecision([]JobTarget{{Order: 1, Kind: "instruction", Resolution: "cache", Decision: DecisionReview}, {Order: 2, Kind: "user", Resolution: "model", Decision: DecisionPass}})
	require.True(t, ready)
	require.Equal(t, DecisionReview, decision)
	require.Equal(t, 1, trigger.Order)
	// 已有 block 时，其余目标未完成也能确定结论。
	decision, trigger, ready = roundDecision([]JobTarget{{Order: 1, Kind: "instruction", Resolution: "model", Decision: DecisionBlock}, {Order: 2, Kind: "user"}})
	require.True(t, ready)
	require.Equal(t, DecisionBlock, decision)
	require.Equal(t, "instruction", trigger.Kind)
	// 同等严重时优先标记用户提示词。
	_, trigger, _ = roundDecision([]JobTarget{{Order: 1, Kind: "instruction", Resolution: "model", Decision: DecisionBlock}, {Order: 2, Kind: "user", Resolution: "model", Decision: DecisionBlock}})
	require.Equal(t, "user", trigger.Kind)
	// 无 block 且仍有失败目标：没有结论。
	_, _, ready = roundDecision([]JobTarget{{Order: 1, Kind: "instruction", Resolution: "trust", Decision: DecisionPass}, {Order: 2, Kind: "user"}})
	require.False(t, ready)
}

func TestCreateAuditJobStoresTextsJobAndTargetsInOneTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	targets := []PromptText{newPromptText(PromptKindInstruction, "S"), newPromptText(PromptKindUser, "U")}
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO sub2api_enhance.prompt_texts\(hash,kind,text,bytes,simhash\).*ON CONFLICT \(hash\) DO NOTHING`).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(`INSERT INTO sub2api_enhance.audit_jobs`).WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at", "lease_until", "started_at", "next_attempt_at"}).
		AddRow(int64(11), nowForTest(), nowForTest(), nil, nil, nowForTest()))
	mock.ExpectExec(`INSERT INTO sub2api_enhance.audit_job_targets`).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	job := &AuditJob{UserID: 1, APIKeyID: 2, RequestID: "r", Protocol: "chat_completions", Mode: "async", TurnFingerprint: "f", Model: "m\x00"}
	require.NoError(t, NewRepository(db).CreateAuditJob(context.Background(), job, targets))
	// 相同内容只插入一次：冲突时不更新已有行，避免共享指令块形成热点写入。
	require.Equal(t, int64(11), job.ID)
	require.Equal(t, 2, job.TargetCount)
	require.Equal(t, "queued", job.Status)
	require.False(t, strings.Contains(sanitizeStoredText(job.Model), "\x00"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func nowForTest() time.Time { return time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC) }
