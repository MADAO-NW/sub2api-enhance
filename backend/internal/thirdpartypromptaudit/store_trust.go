package thirdpartypromptaudit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"slices"

	infraerrors "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/pkg/logger"
)

// instructionLedger 抽象“只计一次”和熔断所需的事务内操作，SQL 实现与测试实现共用同一判定流程。
type instructionLedger interface {
	lockInstruction(ctx context.Context, hash string) error
	breakerStatus(ctx context.Context, hash string) (string, bool, error)
	markExists(ctx context.Context, userID int64, hash string) (bool, error)
	distinctUsers(ctx context.Context, hash string) (int, error)
	insertMark(ctx context.Context, userID int64, hash string, jobID int64, suppressed bool) error
	tripBreaker(ctx context.Context, hash string, users int, jobID int64) error
	updateBreakerUsers(ctx context.Context, hash string, users int) error
}

// instructionOutcome 是一次任务中违规指令块的计数结果。
type instructionOutcome struct {
	// Countable 是本用户首次计入、且未被熔断暂停的指令块。
	Countable []string
	// Suppressed 是熔断期间新增的暂停记录（不计入累计、不触发停用）。
	Suppressed []string
	// Repeated 是本用户此前已经记录过的指令块。
	Repeated []string
	// Tripped 是本任务触发熔断的指令块。
	Tripped []string
}

// applyInstructionViolations 对每个违规指令块先按哈希加锁，再判断只计一次和熔断（决策 10）。
// 哈希按字典序处理，多个任务同时完成时加锁顺序一致，避免死锁。
func applyInstructionViolations(ctx context.Context, ledger instructionLedger, userID, jobID int64, hashes []string) (instructionOutcome, error) {
	var outcome instructionOutcome
	sorted := append([]string(nil), hashes...)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	for _, hash := range sorted {
		if err := ledger.lockInstruction(ctx, hash); err != nil {
			return outcome, err
		}
		status, breakerExists, err := ledger.breakerStatus(ctx, hash)
		if err != nil {
			return outcome, err
		}
		marked, err := ledger.markExists(ctx, userID, hash)
		if err != nil {
			return outcome, err
		}
		if marked {
			outcome.Repeated = append(outcome.Repeated, hash)
			continue
		}
		users, err := ledger.distinctUsers(ctx, hash)
		if err != nil {
			return outcome, err
		}
		users++
		trip := !breakerExists && users >= instructionBreakerUserThreshold
		suppressed := trip || (breakerExists && (status == BreakerTripped || status == BreakerTrusted))
		if err := ledger.insertMark(ctx, userID, hash, jobID, suppressed); err != nil {
			return outcome, err
		}
		if trip {
			if err := ledger.tripBreaker(ctx, hash, users, jobID); err != nil {
				return outcome, err
			}
			outcome.Tripped = append(outcome.Tripped, hash)
		} else if breakerExists {
			if err := ledger.updateBreakerUsers(ctx, hash, users); err != nil {
				return outcome, err
			}
		}
		if suppressed {
			outcome.Suppressed = append(outcome.Suppressed, hash)
		} else {
			outcome.Countable = append(outcome.Countable, hash)
		}
	}
	return outcome, nil
}

// instructionLockKey 把指令块哈希映射为事务级 advisory lock 键，与熔断行锁配合串行化同一哈希的统计。
func instructionLockKey(hash string) int64 {
	digest := sha256.Sum256([]byte("sub2api_enhance:instruction_breaker:" + hash))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

type sqlInstructionLedger struct{ tx *sql.Tx }

func (l sqlInstructionLedger) lockInstruction(ctx context.Context, hash string) error {
	_, err := l.tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, instructionLockKey(hash))
	return err
}

func (l sqlInstructionLedger) breakerStatus(ctx context.Context, hash string) (string, bool, error) {
	var status string
	err := l.tx.QueryRowContext(ctx, `SELECT status FROM sub2api_enhance.instruction_breakers WHERE hash=$1 FOR UPDATE`, hash).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return status, err == nil, err
}

func (l sqlInstructionLedger) markExists(ctx context.Context, userID int64, hash string) (bool, error) {
	var exists bool
	err := l.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.instruction_violation_marks WHERE user_id=$1 AND hash=$2)`, userID, hash).Scan(&exists)
	return exists, err
}

func (l sqlInstructionLedger) distinctUsers(ctx context.Context, hash string) (int, error) {
	var count int
	err := l.tx.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.instruction_violation_marks WHERE hash=$1`, hash).Scan(&count)
	return count, err
}

func (l sqlInstructionLedger) insertMark(ctx context.Context, userID int64, hash string, jobID int64, suppressed bool) error {
	_, err := l.tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.instruction_violation_marks(user_id,hash,first_job_id,suppressed) VALUES($1,$2,$3,$4)`, userID, hash, jobID, suppressed)
	return err
}

func (l sqlInstructionLedger) tripBreaker(ctx context.Context, hash string, users int, jobID int64) error {
	_, err := l.tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.instruction_breakers(hash,status,distinct_users,tripped_job_id) VALUES($1,'tripped',$2,$3)`, hash, users, jobID)
	return err
}

func (l sqlInstructionLedger) updateBreakerUsers(ctx context.Context, hash string, users int) error {
	_, err := l.tx.ExecContext(ctx, `UPDATE sub2api_enhance.instruction_breakers SET distinct_users=$2,updated_at=clock_timestamp() WHERE hash=$1`, hash, users)
	return err
}

// ListTrust 返回管理员信任的指令块及内容预览。
func (r *Repository) ListTrust(ctx context.Context, page, size int) (*Page[TrustEntry], error) {
	result := &Page[TrustEntry]{Items: []TrustEntry{}, Page: page, PageSize: size}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.prompt_trust`).Scan(&result.Total); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT t.hash,left(p.text,500),p.bytes,t.trusted_by,t.reason,t.created_at
 FROM sub2api_enhance.prompt_trust t JOIN sub2api_enhance.prompt_texts p ON p.hash=t.hash ORDER BY t.created_at DESC,t.hash LIMIT $1 OFFSET $2`, size, (page-1)*size)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item TrustEntry
		if err := rows.Scan(&item.Hash, &item.Preview, &item.Bytes, &item.TrustedBy, &item.Reason, &item.CreatedAt); err != nil {
			return nil, err
		}
		result.Items = append(result.Items, item)
	}
	return result, rows.Err()
}

// TrustInstruction 信任一个指令块；熔断中的哈希同时结束熔断（与熔断列表的“信任”效果相同）。
func (r *Repository) TrustInstruction(ctx context.Context, hash string, actorID int64, reason string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, instructionLockKey(hash)); err != nil {
		return err
	}
	var kind string
	err = tx.QueryRowContext(ctx, `SELECT kind FROM sub2api_enhance.prompt_texts WHERE hash=$1`, hash).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if kind != PromptKindInstruction {
		return infraerrors.BadRequest("third_party_audit_trust_user_prompt", "只允许信任指令块，不允许信任用户提示词")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.prompt_trust(hash,trusted_by,reason) VALUES($1,$2,$3) ON CONFLICT (hash) DO NOTHING`, hash, actorID, reason); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.instruction_breakers SET status='trusted',resolved_by=$2,resolved_at=clock_timestamp(),updated_at=clock_timestamp()
 WHERE hash=$1 AND status='tripped'`, hash, actorID)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	logger.LegacyPrintf("third_party_prompt_audit", "指令块已信任 hash=%s actor_user_id=%d breaker_resolved=%t reason=%s", hash, actorID, changed > 0, reason)
	return nil
}

// UntrustInstruction 取消信任；从熔断列表信任过的哈希恢复为熔断中，继续暂停累计（决策 22）。
func (r *Repository) UntrustInstruction(ctx context.Context, hash string, actorID int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, instructionLockKey(hash)); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM sub2api_enhance.prompt_trust WHERE hash=$1`, hash)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	restored, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.instruction_breakers SET status='tripped',resolved_by=NULL,resolved_at=NULL,updated_at=clock_timestamp()
 WHERE hash=$1 AND status='trusted'`, hash)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	count, _ := restored.RowsAffected()
	logger.LegacyPrintf("third_party_prompt_audit", "指令块已取消信任 hash=%s actor_user_id=%d breaker_restored=%t", hash, actorID, count > 0)
	return nil
}

// ListBreakers 返回熔断记录；status 为空时返回全部。
func (r *Repository) ListBreakers(ctx context.Context, status string, page, size int) (*Page[BreakerEntry], error) {
	result := &Page[BreakerEntry]{Items: []BreakerEntry{}, Page: page, PageSize: size}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.instruction_breakers WHERE ($1='' OR status=$1)`, status).Scan(&result.Total); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT b.hash,COALESCE(left(p.text,500),''),COALESCE(p.bytes,0),b.status,b.distinct_users,b.tripped_at,b.tripped_job_id,b.resolved_by,b.resolved_at
 FROM sub2api_enhance.instruction_breakers b LEFT JOIN sub2api_enhance.prompt_texts p ON p.hash=b.hash
 WHERE ($1='' OR b.status=$1) ORDER BY b.tripped_at DESC,b.hash LIMIT $2 OFFSET $3`, status, size, (page-1)*size)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item BreakerEntry
		if err := rows.Scan(&item.Hash, &item.Preview, &item.Bytes, &item.Status, &item.DistinctUsers, &item.TrippedAt, &item.TrippedJobID, &item.ResolvedBy, &item.ResolvedAt); err != nil {
			return nil, err
		}
		result.Items = append(result.Items, item)
	}
	return result, rows.Err()
}

// ConfirmBreaker 确认违规：结束熔断并删除熔断期间的暂停记录，这些用户下次按正常规则计一次；确认后不再熔断。
func (r *Repository) ConfirmBreaker(ctx context.Context, hash string, actorID int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, instructionLockKey(hash)); err != nil {
		return err
	}
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM sub2api_enhance.instruction_breakers WHERE hash=$1 FOR UPDATE`, hash).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != BreakerTripped {
		return infraerrors.Conflict("third_party_audit_breaker_state_conflict", "只能确认熔断中的指令块；已信任的指令块请先取消信任")
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM sub2api_enhance.instruction_violation_marks WHERE hash=$1 AND suppressed`, hash)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.instruction_breakers SET status='confirmed',resolved_by=$2,resolved_at=clock_timestamp(),
 distinct_users=(SELECT count(*) FROM sub2api_enhance.instruction_violation_marks WHERE hash=$1),updated_at=clock_timestamp() WHERE hash=$1`, hash, actorID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	removed, _ := deleted.RowsAffected()
	logger.LegacyPrintf("third_party_prompt_audit", "指令块熔断已确认违规 hash=%s actor_user_id=%d removed_suppressed_marks=%d", hash, actorID, removed)
	return nil
}
