package thirdpartypromptaudit

import (
	"context"
	"time"

	"sub2api-enhance/internal/pkg/logger"
)

// retentionBatchSize 限定每条清理语句删除的行数，避免长事务。
const retentionBatchSize = 500

// retentionInterval 是保留期清理的执行间隔（决策 24）。
const retentionInterval = time.Hour

// failureCheckInterval 是指标快照与失败率检查的执行间隔。
const failureCheckInterval = 5 * time.Minute

// retentionStatements 按依赖顺序清理过期数据。任务删除会级联删除轮次、目标与模型调用，
// 处置表中的来源外键置空；处置状态、处置动作、信任记录、只计一次记录和熔断记录都不在清理范围内。
var retentionStatements = []struct {
	name  string
	query string
}{
	{"jobs", `DELETE FROM sub2api_enhance.audit_jobs WHERE id IN (SELECT id FROM sub2api_enhance.audit_jobs
 WHERE created_at<clock_timestamp()-make_interval(days=>$1) AND status NOT IN ('queued','processing','retry') ORDER BY id LIMIT $2)`},
	{"probe_attempts", `DELETE FROM sub2api_enhance.model_attempts WHERE id IN (SELECT id FROM sub2api_enhance.model_attempts
 WHERE job_id IS NULL AND created_at<clock_timestamp()-make_interval(days=>$1) ORDER BY id LIMIT $2)`},
	{"review_duplicates", `DELETE FROM sub2api_enhance.prompt_reviews WHERE id IN (
 SELECT id FROM (SELECT r.id, row_number() OVER (PARTITION BY r.hash,r.eval_hash ORDER BY r.created_at DESC,r.id DESC) AS rn
  FROM sub2api_enhance.prompt_reviews r
  WHERE NOT EXISTS (SELECT 1 FROM sub2api_enhance.audit_job_targets t WHERE t.review_id=r.id)) ranked
 WHERE ranked.rn>5 LIMIT $2)`},
	{"reviews", `DELETE FROM sub2api_enhance.prompt_reviews WHERE id IN (SELECT r.id FROM sub2api_enhance.prompt_reviews r
 WHERE r.created_at<clock_timestamp()-make_interval(days=>$1)
 AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.audit_job_targets t WHERE t.review_id=r.id) ORDER BY r.id LIMIT $2)`},
	{"texts", `DELETE FROM sub2api_enhance.prompt_texts WHERE hash IN (SELECT p.hash FROM sub2api_enhance.prompt_texts p
WHERE p.first_seen_at<clock_timestamp()-make_interval(days=>$1)
 AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.audit_job_targets t WHERE t.hash=p.hash)
 AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.prompt_reviews r WHERE r.hash=p.hash)
 AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.prompt_trust pt WHERE pt.hash=p.hash)
 AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.instruction_breakers b WHERE b.hash=p.hash)
 AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.policy_samples s WHERE s.hash=p.hash)
 ORDER BY p.hash LIMIT $2)`},
	{"replays", `DELETE FROM sub2api_enhance.policy_replays WHERE id IN (SELECT id FROM sub2api_enhance.policy_replays
 WHERE created_at<clock_timestamp()-make_interval(days=>$1) AND status<>'running' ORDER BY id LIMIT $2)`},
}

// RunRetention 按保留天数分批清理过期审计记录，每批独立提交；ctx 取消时停止。
func (r *Repository) RunRetention(ctx context.Context, days int) (RetentionResult, error) {
	var result RetentionResult
	counters := map[string]*int64{"jobs": &result.Jobs, "probe_attempts": &result.ProbeAttempts, "review_duplicates": &result.ReviewDuplicates, "reviews": &result.Reviews, "texts": &result.Texts, "replays": &result.Replays}
	for _, statement := range retentionStatements {
		for {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			deleted, err := r.db.ExecContext(ctx, statement.query, days, retentionBatchSize)
			if err != nil {
				return result, err
			}
			count, err := deleted.RowsAffected()
			if err != nil {
				return result, err
			}
			*counters[statement.name] += count
			if count < retentionBatchSize {
				break
			}
		}
	}
	return result, nil
}

// RetentionRecord 记录最近一次保留期清理的结果，供运行概览展示。
type RetentionRecord struct {
	FinishedAt    time.Time `json:"finished_at"`
	Days          int       `json:"days"`
	Jobs          int64     `json:"jobs"`
	ProbeAttempts int64     `json:"probe_attempts"`
	Reviews       int64     `json:"reviews"`
	Texts         int64     `json:"texts"`
	DurationMS    int64     `json:"duration_ms"`
	Error         string    `json:"error"`
}

// collectHourlyMetrics 每小时写一次指标快照并检查失败率。
func (s *Service) collectHourlyMetrics() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), persistenceTimeout)
	defer cancel()
	if err := s.repo.SnapshotMetrics(ctx); err != nil {
		if s.ctx.Err() == nil {
			s.noteError("metrics_snapshot_failed", err)
		}
		return
	}
	adminEmail := ""
	if snapshot, err := s.config.Active(); err == nil {
		adminEmail = snapshot.AdminEmail
	}
	if err := s.repo.CheckFailureRate(ctx, adminEmail); err != nil && s.ctx.Err() == nil {
		s.noteError("failure_alert_failed", err)
	}
}

// runRetention 每小时按保留天数清理一次；失败率与指标快照每 5 分钟检查一次，
// 让失败率告警的延迟从 1 小时降到分钟级。
func (s *Service) runRetention() {
	defer s.wg.Done()
	ticker := time.NewTicker(failureCheckInterval)
	defer ticker.Stop()
	lastCleanup := time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		s.collectHourlyMetrics()
		if time.Since(lastCleanup) < retentionInterval {
			continue
		}
		days := s.config.RetentionDays()
		started := time.Now()
		result, err := s.repo.RunRetention(s.ctx, days)
		record := &RetentionRecord{FinishedAt: time.Now().UTC(), Days: days, Jobs: result.Jobs,
			ProbeAttempts: result.ProbeAttempts, Reviews: result.Reviews, Texts: result.Texts,
			DurationMS: time.Since(started).Milliseconds()}
		if err != nil {
			record.Error = err.Error()
			s.recordRetention(record)
			if s.ctx.Err() == nil {
				s.noteError("retention_cleanup_failed", err)
			}
			continue
		}
		s.recordRetention(record)
		lastCleanup = time.Now()
		logger.LegacyPrintf("third_party_prompt_audit", "审计记录保留期清理完成 retention_days=%d jobs=%d probe_attempts=%d reviews=%d texts=%d",
			days, result.Jobs, result.ProbeAttempts, result.Reviews, result.Texts)
	}
}
