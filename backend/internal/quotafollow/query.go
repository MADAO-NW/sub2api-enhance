package quotafollow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// recordColumns 列表不加载完整证据或大快照，详情按需读取。
const recordColumns = `id,action_type,source,source_detail,evidence_type,user_id,username,"window",occurred_at,detected_at,status,event_id,delivery_id,audit_log_id,request_id,error_message,before_snapshot->>("window"||'_usage_usd'),after_snapshot->>("window"||'_usage_usd'),
 COALESCE((SELECT jsonb_agg(state->'account') FROM sub2api_enhance.quota_follow_reset_events event CROSS JOIN LATERAL jsonb_array_elements(event.accounts) state WHERE event.id=event_id),(SELECT jsonb_agg(state->'account') FROM jsonb_array_elements(evidence->'accounts') state),'[]')`

func recordFilter(f Filter) (string, []any) {
	clauses := []string{"merged_into IS NULL"}
	args := []any{}
	for _, v := range []struct {
		column  string
		value   any
		present bool
	}{{"occurred_at >=", f.From, f.From != nil}, {"occurred_at <", f.To, f.To != nil}, {"source =", f.Source, f.Source != ""}, {"source_detail =", f.Detail, f.Detail != ""}, {"\"window\" =", f.Window, f.Window != ""}, {"status =", f.Status, f.Status != ""}} {
		if v.present {
			args = append(args, v.value)
			clauses = append(clauses, fmt.Sprintf("%s $%d", v.column, len(args)))
		}
	}
	if f.Keyword != "" {
		args = append(args, "%"+f.Keyword+"%")
		clauses = append(clauses, fmt.Sprintf("(username ILIKE $%d OR user_id::text ILIKE $%d)", len(args), len(args)))
	}
	return strings.Join(clauses, " AND "), args
}
func (r *Repository) Records(ctx context.Context, f Filter) (Page[Record], error) {
	out := Page[Record]{Items: []Record{}, Page: f.Page, PageSize: f.PageSize}
	where, args := recordFilter(f)
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.quota_reset_records WHERE `+where, args...).Scan(&out.Total); err != nil {
		return out, err
	}
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := r.db.QueryContext(ctx, `SELECT `+recordColumns+` FROM sub2api_enhance.quota_reset_records WHERE `+where+fmt.Sprintf(" ORDER BY occurred_at DESC,id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var v Record
		var accounts []byte
		if err := rows.Scan(&v.ID, &v.ActionType, &v.Source, &v.SourceDetail, &v.EvidenceType, &v.UserID, &v.Username, &v.Window, &v.OccurredAt, &v.DetectedAt, &v.Status, &v.EventID, &v.DeliveryID, &v.AuditLogID, &v.RequestID, &v.Error, &v.BeforeUsage, &v.AfterUsage, &accounts); err != nil {
			return out, err
		}
		if err := json.Unmarshal(accounts, &v.Accounts); err != nil {
			return out, err
		}
		out.Items = append(out.Items, v)
	}
	return out, rows.Err()
}
func (r *Repository) Record(ctx context.Context, id int64) (map[string]any, error) {
	var raw []byte
	err := r.db.QueryRowContext(ctx, `SELECT row_to_json(r) FROM sub2api_enhance.quota_reset_records r WHERE id=$1`, id).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var record map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	result := map[string]any{"record": record}
	if eventID, ok := record["event_id"].(json.Number); ok {
		var event json.RawMessage
		if err := r.db.QueryRowContext(ctx, `SELECT row_to_json(e) FROM sub2api_enhance.quota_follow_reset_events e WHERE id=$1`, eventID.String()).Scan(&event); err != nil {
			return nil, err
		}
		result["event"] = event
	}
	if deliveryID, ok := record["delivery_id"].(json.Number); ok {
		var delivery json.RawMessage
		if err := r.db.QueryRowContext(ctx, `SELECT row_to_json(d) FROM sub2api_enhance.quota_follow_reset_deliveries d WHERE id=$1`, deliveryID.String()).Scan(&delivery); err != nil {
			return nil, err
		}
		result["delivery"] = delivery
	}
	if carryoverID, ok := record["carryover_id"].(json.Number); ok {
		var carryover json.RawMessage
		if err := r.db.QueryRowContext(ctx, `SELECT row_to_json(c) FROM sub2api_enhance.quota_follow_weekly_carryovers c WHERE id=$1`, carryoverID.String()).Scan(&carryover); err != nil {
			return nil, err
		}
		result["carryover"] = carryover
	}
	return result, nil
}
func (r *Repository) Delivery(ctx context.Context, id int64) (*Delivery, error) {
	var d Delivery
	var cfg, before, after, cache, cacheBefore []byte
	var response *string
	err := r.db.QueryRowContext(ctx, `SELECT d.id,d.event_id,d.user_id,d."window",d.request_id,d.status,e.epoch,e.config_snapshot,d.before_snapshot,d.after_snapshot,d.cache_snapshot,d.database_status,d.cache_status,d.http_status,d.response_body,d.error_message,d.started_at,d.completed_at,d.audit_log_id,d.before_cache_snapshot FROM sub2api_enhance.quota_follow_reset_deliveries d JOIN sub2api_enhance.quota_follow_reset_events e ON e.id=d.event_id WHERE d.id=$1`, id).Scan(&d.ID, &d.EventID, &d.UserID, &d.Window, &d.RequestID, &d.Status, &d.Epoch, &cfg, &before, &after, &cache, &d.DatabaseStatus, &d.CacheStatus, &d.HTTPStatus, &response, &d.Error, &d.StartedAt, &d.CompletedAt, &d.AuditLogID, &cacheBefore)
	if err != nil {
		return nil, err
	}
	for _, v := range []struct {
		raw    []byte
		target any
	}{{cfg, &d.Config}, {before, &d.Before}, {after, &d.After}, {cache, &d.Cache}, {cacheBefore, &d.CacheBefore}} {
		if len(v.raw) > 0 {
			if err := json.Unmarshal(v.raw, v.target); err != nil {
				return nil, err
			}
		}
	}
	if response != nil {
		d.Response = *response
	}
	return &d, nil
}
func (s *Service) Reconcile(ctx context.Context, id int64) (*Delivery, error) {
	d, err := s.repo.Delivery(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.Status == "pending" || d.Status == "inflight" {
		return nil, errors.New("交付尚未完成发送阶段，不能并发复核")
	}
	if d.Config.GroupID == nil {
		return nil, errors.New("交付缺少原分组快照")
	}
	before, _ := json.Marshal(d)
	d.Error = ""
	s.verifyDelivery(ctx, d, *d.Config.GroupID)
	if d.Status == "uncertain" && d.Error == "" {
		d.Error = "原调用仍未确认；本次只读复核不证明归零来源"
	}
	// 只刷新复核证据，不改变不确定调用的成功归属，也不发送重置请求。
	after, _ := json.Marshal(d.After)
	cache, _ := json.Marshal(d.Cache)
	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_reset_deliveries SET after_snapshot=$2,cache_snapshot=$3,database_status=$4,cache_status=$5,error_message=$6 WHERE id=$1 AND status NOT IN ('pending','inflight')`, d.ID, string(after), string(cache), d.DatabaseStatus, d.CacheStatus, d.Error)
	if err != nil {
		return nil, err
	}
	afterValue, _ := json.Marshal(d)
	evidence, _ := json.Marshal(map[string]any{"at": time.Now().UTC(), "before": json.RawMessage(before), "after": json.RawMessage(afterValue)})
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records SET error_message=$2,evidence=jsonb_set(evidence,'{reconciliations}',COALESCE(evidence->'reconciliations','[]'::jsonb)||jsonb_build_array($3::jsonb)) WHERE delivery_id=$1`, d.ID, d.Error, string(evidence)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	log.Printf("额度交付只读复核完成 delivery_id=%d before=%s after=%s", d.ID, before, afterValue)
	return d, nil
}

func (r *Repository) Events(ctx context.Context, page, size int) (Page[json.RawMessage], error) {
	out := Page[json.RawMessage]{Items: []json.RawMessage{}, Page: page, PageSize: size}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.quota_follow_reset_events`).Scan(&out.Total); err != nil {
		return out, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT row_to_json(e) FROM (SELECT id,reset_at,status,created_at,completed_at FROM sub2api_enhance.quota_follow_reset_events ORDER BY id DESC LIMIT $1 OFFSET $2)e`, size, (page-1)*size)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			return out, err
		}
		out.Items = append(out.Items, raw)
	}
	return out, rows.Err()
}
func (r *Repository) Event(ctx context.Context, id int64) (json.RawMessage, error) {
	var raw json.RawMessage
	err := r.db.QueryRowContext(ctx, `SELECT row_to_json(e) FROM sub2api_enhance.quota_follow_reset_events e WHERE id=$1`, id).Scan(&raw)
	return raw, err
}
