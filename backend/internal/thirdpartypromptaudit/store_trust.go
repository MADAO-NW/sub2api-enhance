package thirdpartypromptaudit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/lib/pq"
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

// RecordNodeAuthNotice 记录一次审核节点凭据失效，每个任务只留一条，只通知管理员。
func (r *Repository) RecordNodeAuthNotice(ctx context.Context, job *AuditJob, modelName, message string) error {
	if job == nil {
		return nil
	}
	now := time.Now().UTC()
	action := Action{UserID: job.UserID, JobID: &job.ID, ActionType: "node_auth_notice", NotificationStatus: "pending",
		AuthCacheStatus: "not_required", ExecutionStatus: "succeeded", AppliedAt: &now, CompletedAt: &now,
		RuleSnapshot:     map[string]any{"model": modelName},
		BusinessSnapshot: map[string]any{"model": modelName, "error": message, "request_id": job.RequestID}}
	if job.Config.AdminEmail != "" {
		action.Deliveries = []Delivery{{Recipient: job.Config.AdminEmail, Kind: "admin", Subject: "第三方提示词审计：审核节点凭据失效",
			Body:   "<p>审核节点凭据被上游拒绝，节点已降级。</p><p>节点：" + modelName + "</p><p>错误：" + message + "</p>",
			Status: "pending", Attempts: []DeliveryAttempt{}}}
	} else {
		action.NotificationStatus = "not_required"
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rule, _ := json.Marshal(action.RuleSnapshot)
	business, _ := json.Marshal(action.BusinessSnapshot)
	deliveries, _ := json.Marshal(action.Deliveries)
	_, err = tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_actions
 (user_id,job_id,action_type,rule_snapshot,business_snapshot,notification_status,deliveries,auth_cache_status,execution_status,applied_at,completed_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (job_id,action_type) DO NOTHING`,
		action.UserID, action.JobID, action.ActionType, string(rule), string(business), action.NotificationStatus, string(deliveries),
		action.AuthCacheStatus, action.ExecutionStatus, action.AppliedAt, action.CompletedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// TrustInstructions 批量信任指令块；单个失败不影响其他项，返回成功哈希。
func (r *Repository) TrustInstructions(ctx context.Context, hashes []string, actorID int64, reason string) ([]string, error) {
	trusted := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		if err := r.TrustInstruction(ctx, hash, actorID, reason); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return trusted, err
		}
		trusted = append(trusted, hash)
	}
	return trusted, nil
}

// ListTrust 返回管理员信任的指令块及内容预览。
func (r *Repository) ListTrust(ctx context.Context, query string, page, size int) (*Page[TrustEntry], error) {
	result := &Page[TrustEntry]{Items: []TrustEntry{}, Page: page, PageSize: size}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.prompt_trust t JOIN sub2api_enhance.prompt_texts p ON p.hash=t.hash
 WHERE ($1='' OR t.hash LIKE $1||'%' OR p.text ILIKE '%'||$1||'%')`, query).Scan(&result.Total); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT t.hash,left(p.text,500),p.bytes,t.trusted_by,t.reason,t.created_at
 FROM sub2api_enhance.prompt_trust t JOIN sub2api_enhance.prompt_texts p ON p.hash=t.hash
 WHERE ($3='' OR t.hash LIKE $3||'%' OR p.text ILIKE '%'||$3||'%') ORDER BY t.created_at DESC,t.hash LIMIT $1 OFFSET $2`, size, (page-1)*size, query)
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
func (r *Repository) ListBreakers(ctx context.Context, status, query string, page, size int) (*Page[BreakerEntry], error) {
	result := &Page[BreakerEntry]{Items: []BreakerEntry{}, Page: page, PageSize: size}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.instruction_breakers b JOIN sub2api_enhance.prompt_texts p ON p.hash=b.hash
 WHERE ($1='' OR b.status=$1) AND ($2='' OR b.hash LIKE $2||'%' OR p.text ILIKE '%'||$2||'%')`, status, query).Scan(&result.Total); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT b.hash,COALESCE(left(p.text,500),''),COALESCE(p.bytes,0),b.status,b.distinct_users,b.tripped_at,b.tripped_job_id,b.resolved_by,b.resolved_at,
 COALESCE((SELECT array_agg(m.user_id ORDER BY m.user_id) FROM (SELECT user_id FROM sub2api_enhance.instruction_violation_marks WHERE hash=b.hash ORDER BY user_id LIMIT 20) m),'{}'),
 COALESCE((SELECT count(*) FROM sub2api_enhance.prompt_texts p2
   WHERE p.simhash IS NOT NULL AND p2.simhash IS NOT NULL AND p2.kind='instruction' AND p2.hash<>b.hash
   AND (EXISTS (SELECT 1 FROM sub2api_enhance.instruction_breakers b2 WHERE b2.hash=p2.hash)
        OR EXISTS (SELECT 1 FROM sub2api_enhance.prompt_trust pt2 WHERE pt2.hash=p2.hash))
   AND bit_count((p2.simhash # p.simhash)::bit(64))<=$5),0)
 FROM sub2api_enhance.instruction_breakers b LEFT JOIN sub2api_enhance.prompt_texts p ON p.hash=b.hash
 WHERE ($1='' OR b.status=$1) AND ($4='' OR b.hash LIKE $4||'%' OR COALESCE(p.text,'') ILIKE '%'||$4||'%')
 ORDER BY b.tripped_at DESC,b.hash LIMIT $2 OFFSET $3`, status, size, (page-1)*size, query, similarSimhashDistance)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item BreakerEntry
		if err := rows.Scan(&item.Hash, &item.Preview, &item.Bytes, &item.Status, &item.DistinctUsers, &item.TrippedAt, &item.TrippedJobID, &item.ResolvedBy, &item.ResolvedAt,
			pq.Array(&item.UserIDs), &item.Similar); err != nil {
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
