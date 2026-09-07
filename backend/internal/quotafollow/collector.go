package quotafollow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lib/pq"
	"log"
	"strconv"
	"sub2api-enhance/internal/sub2api"
	"time"
)

type sourceAudit struct {
	ID                      int64
	CreatedAt               time.Time
	RequestID, Body, UserID string
	HTTPStatus              int
	LatencyMS               int64
}

// collect 只采集目标用户的配额证据。使用已处理 audit ID 去重补扫，避免异步日志迟到越过时间水位。
func (s *Service) collect(ctx context.Context) (resultErr error) {
	cfg, err := s.repo.Config(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled || cfg.GroupID == nil {
		return nil
	}
	if _, err := s.repo.db.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_collector_state(singleton,started_at) VALUES(true,COALESCE($1,clock_timestamp())) ON CONFLICT DO NOTHING`, cfg.EnabledAt); err != nil {
		return err
	}
	defer func() {
		message := ""
		if resultErr != nil {
			message = resultErr.Error()
		}
		save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = s.repo.db.ExecContext(save, `UPDATE sub2api_enhance.quota_follow_collector_state SET last_error=$1,last_completed_at=CASE WHEN $1='' THEN clock_timestamp() ELSE last_completed_at END WHERE singleton=true`, message)
	}()
	discovery, err := s.source.Discover(ctx, *cfg.GroupID)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(discovery.Users))
	names := map[int64]string{}
	for _, u := range discovery.Users {
		ids = append(ids, strconv.FormatInt(u.ID, 10))
		names[u.ID] = u.Username
	}
	if len(ids) == 0 {
		return nil
	}
	if s.cache != nil {
		_, _ = s.cache.Read(ctx, discovery.Users[0].ID)
	}
	rows, err := s.repo.db.QueryContext(ctx, `SELECT a.id,a.created_at,a.request_id,a.request_body,a.extra#>>'{params,id}',a.status_code,a.latency_ms FROM public.audit_logs a WHERE a.method='POST' AND a.path='/api/v1/admin/users/:id/platform-quotas/reset' AND a.extra#>>'{params,id}'=ANY($1::text[]) AND a.created_at>=(SELECT started_at FROM sub2api_enhance.quota_follow_collector_state WHERE singleton=true) AND NOT EXISTS(SELECT 1 FROM sub2api_enhance.quota_reset_records r WHERE r.audit_log_id=a.id) ORDER BY a.created_at,a.id`, pq.Array(ids))
	if err != nil {
		return err
	}
	audits := []sourceAudit{}
	for rows.Next() {
		var a sourceAudit
		if err := rows.Scan(&a.ID, &a.CreatedAt, &a.RequestID, &a.Body, &a.UserID, &a.HTTPStatus, &a.LatencyMS); err != nil {
			_ = rows.Close()
			return err
		}
		audits = append(audits, a)
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, a := range audits {
		var body struct {
			Platform string `json:"platform"`
			Window   string `json:"window"`
		}
		if json.Unmarshal([]byte(a.Body), &body) != nil || body.Platform != "openai" || (body.Window != "daily" && body.Window != "weekly") {
			continue
		}
		id, err := strconv.ParseInt(a.UserID, 10, 64)
		if err != nil {
			return err
		}
		if err := s.repo.importAudit(ctx, a, id, names[id], body.Window); err != nil {
			return err
		}
	}
	// 新快照只与本服务实际保存的上次观测比较，不猜测停机期间的中间重置。
	for _, u := range discovery.Users {
		snapshot, err := s.source.Snapshot(ctx, u.ID, *cfg.GroupID)
		if err != nil {
			return err
		}
		if snapshot == nil {
			continue
		}
		if err := s.repo.observeQuota(ctx, *snapshot, s.location); err != nil {
			return err
		}
	}
	// 上一轮的窗口变更在完成本轮日志补扫后再归因；以后迟到的证据仍可纠正推断。
	return s.repo.attributeWindows(ctx, s.location)
}
func (r *Repository) importAudit(ctx context.Context, a sourceAudit, userID int64, username, window string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	status := "uncertain"
	if a.HTTPStatus >= 200 && a.HTTPStatus < 300 {
		status = "succeeded"
	} else if a.HTTPStatus == 404 {
		status = "skipped"
	} else if a.HTTPStatus >= 400 && a.HTTPStatus < 500 {
		status = "failed"
	}
	evidence, _ := json.Marshal(map[string]any{"audit_log_id": a.ID, "request_body": a.Body, "http_status": a.HTTPStatus, "request_started_at": a.CreatedAt.Add(-time.Duration(a.LatencyMS) * time.Millisecond), "request_completed_at": a.CreatedAt, "evidence_source": "原版管理审计"})
	var deliveryID, eventID int64
	err = tx.QueryRowContext(ctx, `SELECT id,event_id FROM sub2api_enhance.quota_follow_reset_deliveries WHERE request_id=$1 AND user_id=$2 AND "window"=$3 FOR UPDATE`, a.RequestID, userID, window).Scan(&deliveryID, &eventID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		// 原版成功审计可以确认请求已应用，窗口读回一致本身不能证明来源。
		if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_reset_deliveries SET audit_log_id=$2,status=CASE WHEN status IN ('inflight','uncertain') AND $3<>'uncertain' THEN $3 ELSE status END,completed_at=COALESCE(completed_at,$4) WHERE id=$1`, deliveryID, a.ID, status, a.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records r SET audit_log_id=$2,evidence=evidence||$3::jsonb,status=d.status FROM sub2api_enhance.quota_follow_reset_deliveries d WHERE r.delivery_id=$1 AND d.id=$1`, deliveryID, a.ID, string(evidence)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_reset_events e SET status=CASE WHEN EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_reset_deliveries WHERE event_id=e.id AND status IN ('pending','inflight')) THEN 'pending' WHEN EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_reset_deliveries WHERE event_id=e.id AND status='uncertain') THEN 'uncertain' WHEN EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_reset_deliveries WHERE event_id=e.id AND status='failed') THEN 'partial' ELSE 'completed' END WHERE id=$1`, eventID); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_reset_records(record_key,source,source_detail,evidence_type,user_id,username,"window",occurred_at,status,evidence,audit_log_id,request_id) VALUES($1,'sub2api','manual','confirmed',$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(record_key) DO NOTHING`, fmt.Sprintf("audit:%d", a.ID), userID, username, window, a.CreatedAt, status, string(evidence), a.ID, a.RequestID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_collector_state SET audit_watermark_at=$1,audit_watermark_id=$2 WHERE singleton=true AND (audit_watermark_at IS NULL OR (audit_watermark_at,audit_watermark_id)<($1,$2))`, a.CreatedAt, a.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("原版额度审计证据已关联 audit_log_id=%d request_id=%s user_id=%d window=%s delivery_id=%d http_status=%d request_body=%s", a.ID, a.RequestID, userID, window, deliveryID, a.HTTPStatus, a.Body)
	return nil
}
func (r *Repository) observeQuota(ctx context.Context, next sub2api.QuotaSnapshot, location *time.Location) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldRaw []byte
	err = tx.QueryRowContext(ctx, `SELECT snapshot FROM sub2api_enhance.quota_follow_user_snapshots WHERE user_id=$1 FOR UPDATE`, next.UserID).Scan(&oldRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	nextRaw, errJSON := json.Marshal(next)
	if errJSON != nil {
		return errJSON
	}
	if err == nil {
		var old sub2api.QuotaSnapshot
		if err := json.Unmarshal(oldRaw, &old); err != nil {
			return err
		}
		for _, window := range []string{"daily", "weekly"} {
			_, before := old.Window(window)
			_, after := next.Window(window)
			if before == nil || after == nil || before.Equal(*after) {
				continue
			}
			key := fmt.Sprintf("window:%d:%s:%s", next.UserID, window, after.UTC().Format(time.RFC3339Nano))
			evidence, _ := json.Marshal(map[string]any{"previous_observed_at": old.ObservedAt, "observed_at": next.ObservedAt, "new_window_start": after, "natural_candidate": after.After(*before) && naturalBoundary(*after, next.ObservedAt, window, location), "snapshot_basis": "实际观测值，不等于操作瞬间的原子前后值"})
			if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_reset_records(record_key,source,source_detail,evidence_type,user_id,username,"window",occurred_at,status,before_snapshot,after_snapshot,evidence) VALUES($1,'sub2api','pending_attribution','inferred',$2,$3,$4,$5,'pending_attribution',$6,$7,$8) ON CONFLICT(record_key) DO NOTHING`, key, next.UserID, next.Username, window, after, string(oldRaw), string(nextRaw), string(evidence)); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_user_snapshots(user_id,snapshot,observed_at) VALUES($1,$2,$3) ON CONFLICT(user_id) DO UPDATE SET snapshot=$2,observed_at=$3`, next.UserID, string(nextRaw), next.ObservedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("额度窗口观测已保存 user_id=%d before=%s after=%s", next.UserID, oldRaw, nextRaw)
	return nil
}
func naturalBoundary(start, observed time.Time, window string, location *time.Location) bool {
	if location == nil {
		return false
	}
	local := observed.In(location)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	if window == "weekly" {
		weekday := (int(day.Weekday()) + 6) % 7
		day = day.AddDate(0, 0, -weekday)
	}
	return start.Equal(day)
}

// windowEvidenceMatch 同时约束用户、窗口及可验证的时间/快照来源，供合并和保留待归因共同使用。
const windowEvidenceMatch = `e.user_id=n.user_id AND e."window"=n."window" AND e.id<>n.id AND e.evidence_type='confirmed' AND (
 (e.source='enhance' AND (e.action_type='reset' OR e.before_snapshot->>(n."window"||'_window_start') IS DISTINCT FROM e.after_snapshot->>(n."window"||'_window_start')) AND e.after_snapshot->>(n."window"||'_window_start')=n.after_snapshot->>(n."window"||'_window_start')) OR
 (e.source='sub2api' AND e.source_detail='manual' AND (n.after_snapshot->>(n."window"||'_window_start'))::timestamptz BETWEEN (e.evidence->>'request_started_at')::timestamptz AND (e.evidence->>'request_completed_at')::timestamptz) OR
 (e.source='enhance' AND e.status IN ('inflight','uncertain') AND EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_reset_deliveries d WHERE d.id=e.delivery_id AND d.started_at<=(n.after_snapshot->>(n."window"||'_window_start'))::timestamptz AND (d.completed_at IS NULL OR d.completed_at>=(n.after_snapshot->>(n."window"||'_window_start'))::timestamptz))))`

func (r *Repository) attributeWindows(ctx context.Context, location *time.Location) error {
	// 唯一且成功的证据才能合并；多份匹配或不确定调用保持待归因。
	_, err := r.db.ExecContext(ctx, `WITH matches AS (SELECT n.id,array_agg(e.id) ids,bool_and(e.status='succeeded') all_succeeded FROM sub2api_enhance.quota_reset_records n JOIN sub2api_enhance.quota_reset_records e ON `+windowEvidenceMatch+` WHERE n.merged_into IS NULL AND n.evidence_type='inferred' AND e.status IN ('succeeded','uncertain','inflight') GROUP BY n.id)
 UPDATE sub2api_enhance.quota_reset_records n SET merged_into=matches.ids[1] FROM matches WHERE n.id=matches.id AND cardinality(matches.ids)=1 AND matches.all_succeeded`)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records n SET source_detail=CASE WHEN n.evidence->>'natural_candidate'='true' THEN 'natural_'||n."window" ELSE 'unattributed' END,status=CASE WHEN n.evidence->>'natural_candidate'='true' THEN 'succeeded' ELSE 'uncertain' END
 WHERE n.merged_into IS NULL AND n.source_detail='pending_attribution' AND n.detected_at<(SELECT last_completed_at FROM sub2api_enhance.quota_follow_collector_state WHERE singleton=true) AND NOT EXISTS(SELECT 1 FROM sub2api_enhance.quota_reset_records e WHERE `+windowEvidenceMatch+` AND e.status IN ('succeeded','uncertain','inflight'))`)
	return err
}
