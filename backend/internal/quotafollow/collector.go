package quotafollow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sub2api-enhance/internal/sub2api"
	"time"

	"github.com/lib/pq"
)

// ApplyAccountResetSignals 补扫基线后的成功重置卡审计；采用操作开始时间隔离已消费的观测，避免迟到日志重复触发。
func (r *Repository) ApplyAccountResetSignals(ctx context.Context, states []AccountState, previous map[int64]AccountState, baseline time.Time) error {
	ids := []string{}
	for _, v := range states {
		if v.Account.Type == "oauth" && !v.Account.IsShadow {
			ids = append(ids, strconv.FormatInt(v.Account.ID, 10))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT ON (extra#>>'{params,id}') id,(extra#>>'{params,id}')::bigint,created_at,latency_ms FROM public.audit_logs WHERE method='POST' AND path='/api/v1/admin/openai/accounts/:id/reset-quota' AND status_code BETWEEN 200 AND 299 AND created_at>$1 AND extra#>>'{params,id}'=ANY($2::text[]) ORDER BY extra#>>'{params,id}',created_at DESC,id DESC`, baseline, pq.Array(ids))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, accountID, latency int64
		var at time.Time
		if err = rows.Scan(&id, &accountID, &at, &latency); err != nil {
			return err
		}
		for i := range states {
			s := &states[i]
			if s.Account.ID != accountID || s.Error != "" || s.FetchedAt == nil || s.NextResetAt == nil {
				continue
			}
			start := at.Add(-time.Duration(latency) * time.Millisecond)
			if id == s.LastAuditID || !start.After(baseline) || at.After(*s.FetchedAt) || latency < 0 {
				continue
			}
			if s.ConsumedObservedAt != nil && !start.After(*s.ConsumedObservedAt) {
				s.LastAuditID = id
				continue
			}
			before := previous[accountID]
			// 审计没有响应正文，HTTP 200 不能证明具体周窗口已经重置；还需操作前后的真实周用量下降。
			if before.FetchedAt == nil || !start.After(*before.FetchedAt) || !s.SuspectedDrop || !s.NextResetAt.After(*s.FetchedAt) {
				continue
			}
			s.LastAuditID = id
			if s.CandidateResetAt == nil || at.After(*s.CandidateResetAt) {
				s.CandidateResetAt = &at
				s.BaselineRebased = false
			}
		}
	}
	return rows.Err()
}

type sourceAudit struct {
	ID                              int64
	CreatedAt                       time.Time
	RequestID, Body, TargetID, Path string
	HTTPStatus                      int
	LatencyMS                       int64
}

func (s *Service) collect(ctx context.Context) error {
	cfg, err := s.repo.Config(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return nil
	}
	subscriptions := map[int64]sub2api.SubscriptionSnapshot{}
	var since time.Time
	for _, g := range cfg.Groups {
		if !g.Enabled || g.EnabledAt == nil {
			continue
		}
		d, err := s.source.DiscoverSubscriptions(ctx, g.GroupID)
		if err != nil {
			continue
		}
		if since.IsZero() || g.EnabledAt.Before(since) {
			since = *g.EnabledAt
		}
		for _, v := range d.Subscriptions {
			subscriptions[v.ID] = v
		}
	}
	if since.IsZero() {
		return nil
	}
	// 原版审计没有响应正文，批量 HTTP 200 只能证明请求返回，不能据此确认每条订阅成功。
	rows, err := s.repo.db.QueryContext(ctx, `SELECT id,created_at,request_id,request_body,COALESCE(extra#>>'{params,id}',''),path,status_code,latency_ms FROM public.audit_logs a WHERE method='POST' AND path IN ('/api/v1/admin/subscriptions/bulk-action','/api/v1/admin/subscriptions/:id/reset-quota') AND created_at>=$1 AND NOT EXISTS(SELECT 1 FROM sub2api_enhance.quota_reset_records r WHERE r.audit_log_id=a.id) ORDER BY created_at,id`, since)
	if err != nil {
		return err
	}
	audits := []sourceAudit{}
	for rows.Next() {
		var v sourceAudit
		if err = rows.Scan(&v.ID, &v.CreatedAt, &v.RequestID, &v.Body, &v.TargetID, &v.Path, &v.HTTPStatus, &v.LatencyMS); err != nil {
			rows.Close()
			return err
		}
		audits = append(audits, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, a := range audits {
		if err = s.repo.importAudit(ctx, a, subscriptions); err != nil {
			return err
		}
	}
	for _, v := range subscriptions {
		if err = s.repo.observeSubscription(ctx, v); err != nil {
			return err
		}
	}
	return nil
}
func (r *Repository) importAudit(ctx context.Context, a sourceAudit, targets map[int64]sub2api.SubscriptionSnapshot) error {
	var request sub2api.SubscriptionResetRequest
	if json.Unmarshal([]byte(a.Body), &request) != nil {
		return nil
	}
	bulk := a.Path == "/api/v1/admin/subscriptions/bulk-action"
	if bulk {
		if request.Action != "reset_quota" {
			return nil
		}
	} else {
		id, err := strconv.ParseInt(a.TargetID, 10, 64)
		if err != nil {
			return nil
		}
		request.IDs = []int64{id}
	}
	windows := selectedWindows(GroupRule{ResetDaily: request.Daily, ResetWeekly: request.Weekly})
	if len(windows) == 0 {
		return nil
	}
	status := "uncertain"
	if !bulk && a.HTTPStatus >= 200 && a.HTTPStatus < 300 {
		status = "succeeded"
	} else if a.HTTPStatus >= 400 && a.HTTPStatus < 500 && a.HTTPStatus != 409 {
		status = "failed"
	}
	evidence, _ := json.Marshal(map[string]any{"audit_log_id": a.ID, "request_body": a.Body, "http_status": a.HTTPStatus, "request_started_at": a.CreatedAt.Add(-time.Duration(a.LatencyMS) * time.Millisecond), "request_completed_at": a.CreatedAt, "note": "原版审计没有逐项响应；批量请求不能仅凭 HTTP 状态确认订阅结果"})
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range request.IDs {
		target, ok := targets[id]
		if !ok {
			continue
		}
		for _, window := range windows {
			result, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records SET audit_log_id=COALESCE(audit_log_id,$1) WHERE request_id=$2 AND subscription_id=$3 AND "window"=$4 AND source='enhance'`, a.ID, a.RequestID, id, window)
			if err != nil {
				return err
			}
			matched, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if matched > 0 {
				continue
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_reset_records(record_key,source,source_detail,evidence_type,user_id,username,"window",occurred_at,status,request_id,evidence,audit_log_id,group_id,subscription_id) VALUES($1,'sub2api','manual','confirmed',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(record_key) DO NOTHING`, fmt.Sprintf("subscription-audit:%d:%d:%s", a.ID, id, window), target.UserID, target.Username, window, a.CreatedAt, status, a.RequestID, string(evidence), a.ID, target.GroupID, id)
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
func (r *Repository) observeSubscription(ctx context.Context, next sub2api.SubscriptionSnapshot) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldRaw []byte
	err = tx.QueryRowContext(ctx, `SELECT snapshot FROM sub2api_enhance.quota_follow_subscription_snapshots WHERE subscription_id=$1 FOR UPDATE`, next.ID).Scan(&oldRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	nextRaw, _ := json.Marshal(next)
	if len(oldRaw) > 0 {
		var old sub2api.SubscriptionSnapshot
		if err = json.Unmarshal(oldRaw, &old); err != nil {
			return err
		}
		if !next.ObservedAt.After(old.ObservedAt) {
			return nil
		}
		for _, w := range []struct {
			name          string
			before, after *time.Time
		}{{"daily", old.DailyStart, next.DailyStart}, {"weekly", old.WeeklyStart, next.WeeklyStart}} {
			if w.before == nil || w.after == nil || w.before.Equal(*w.after) {
				continue
			}
			evidence, _ := json.Marshal(map[string]any{"note": "订阅窗口发生变化；原始观测不能单独证明自然重置或管理员操作", "previous_observed_at": old.ObservedAt, "observed_at": next.ObservedAt})
			if _, err = tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_reset_records(record_key,source,source_detail,evidence_type,user_id,username,"window",occurred_at,status,before_snapshot,after_snapshot,evidence,group_id,subscription_id) VALUES($1,'sub2api','window_observation','inferred',$2,$3,$4,$5,'uncertain',$6,$7,$8,$9,$10) ON CONFLICT(record_key) DO NOTHING`, fmt.Sprintf("subscription-window:%d:%s:%s", next.ID, w.name, w.after.UTC().Format(time.RFC3339Nano)), next.UserID, next.Username, w.name, *w.after, string(oldRaw), string(nextRaw), string(evidence), next.GroupID, next.ID); err != nil {
				return err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_subscription_snapshots(subscription_id,snapshot) VALUES($1,$2) ON CONFLICT(subscription_id) DO UPDATE SET snapshot=EXCLUDED.snapshot`, next.ID, string(nextRaw)); err != nil {
		return err
	}
	// 只在窗口、订阅和观测时间共同命中唯一成功动作时合并，迟到审计仍可在后续轮次补关联。
	if _, err = tx.ExecContext(ctx, `WITH matches AS (SELECT n.id,min(e.id) AS evidence_id,count(*) AS total FROM sub2api_enhance.quota_reset_records n JOIN sub2api_enhance.quota_reset_records e ON e.subscription_id=n.subscription_id AND e."window"=n."window" AND e.status='succeeded' AND e.evidence_type='confirmed' AND e.occurred_at BETWEEN (n.before_snapshot->>'observed_at')::timestamptz AND (n.after_snapshot->>'observed_at')::timestamptz AND e.after_snapshot->>(n."window"||'_window_start')=n.after_snapshot->>(n."window"||'_window_start') WHERE n.subscription_id=$1 AND n.source_detail='window_observation' AND n.merged_into IS NULL GROUP BY n.id) UPDATE sub2api_enhance.quota_reset_records n SET merged_into=m.evidence_id FROM matches m WHERE n.id=m.id AND m.total=1`, next.ID); err != nil {
		return err
	}
	return tx.Commit()
}
