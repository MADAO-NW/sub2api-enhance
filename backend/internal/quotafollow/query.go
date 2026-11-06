package quotafollow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func recordFilter(f Filter) (string, []any) {
	clauses := []string{"merged_into IS NULL"}
	args := []any{}
	for _, v := range []struct {
		column  string
		value   any
		present bool
	}{{"occurred_at >=", f.From, f.From != nil}, {"occurred_at <", f.To, f.To != nil}, {"source =", f.Source, f.Source != ""}, {"source_detail =", f.Detail, f.Detail != ""}, {`"window" =`, f.Window, f.Window != ""}, {"status =", f.Status, f.Status != ""}, {"group_id =", f.GroupID, f.GroupID != nil}} {
		if v.present {
			args = append(args, v.value)
			clauses = append(clauses, fmt.Sprintf("%s $%d", v.column, len(args)))
		}
	}
	if f.Keyword != "" {
		args = append(args, "%"+f.Keyword+"%")
		n := len(args)
		clauses = append(clauses, fmt.Sprintf("(username ILIKE $%d OR user_id::text ILIKE $%d OR subscription_id::text ILIKE $%d)", n, n, n))
	}
	return strings.Join(clauses, " AND "), args
}
func (r *Repository) Records(ctx context.Context, f Filter) (Page[json.RawMessage], error) {
	out := Page[json.RawMessage]{Items: []json.RawMessage{}, Page: f.Page, PageSize: f.PageSize}
	where, args := recordFilter(f)
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.quota_reset_records WHERE `+where, args...).Scan(&out.Total); err != nil {
		return out, err
	}
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := r.db.QueryContext(ctx, `SELECT row_to_json(x) FROM (SELECT id,action_type,source,source_detail,evidence_type,user_id,username,group_id,subscription_id,batch_id,"window",occurred_at,detected_at,status,event_id,delivery_id,request_id,error_message,before_snapshot->>("window"||'_usage_usd') AS before_usage_usd,after_snapshot->>("window"||'_usage_usd') AS after_usage_usd FROM sub2api_enhance.quota_reset_records WHERE `+where+fmt.Sprintf(` ORDER BY occurred_at DESC,id DESC LIMIT $%d OFFSET $%d)x`, len(args)-1, len(args)), args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw json.RawMessage
		if err = rows.Scan(&raw); err != nil {
			return out, err
		}
		out.Items = append(out.Items, raw)
	}
	return out, rows.Err()
}
func (r *Repository) Record(ctx context.Context, id int64) (map[string]any, error) {
	var raw []byte
	if err := r.db.QueryRowContext(ctx, `SELECT row_to_json(r) FROM sub2api_enhance.quota_reset_records r WHERE id=$1`, id).Scan(&raw); err != nil {
		return nil, err
	}
	var record map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	out := map[string]any{"record": record}
	// 表名均为固定内部枚举；旧事件、交付和结转只读保留原证据，禁止重新执行。
	for _, entry := range []struct{ field, name, table string }{{"event_id", "event", "quota_follow_reset_events"}, {"delivery_id", "delivery", "quota_follow_reset_deliveries"}, {"carryover_id", "carryover", "quota_follow_weekly_carryovers"}} {
		if key, ok := record[entry.field].(json.Number); ok {
			var detail json.RawMessage
			if err := r.db.QueryRowContext(ctx, `SELECT row_to_json(x) FROM sub2api_enhance.`+entry.table+` x WHERE id=$1`, key.String()).Scan(&detail); err != nil {
				return nil, err
			}
			out[entry.name] = detail
		}
	}
	if key, ok := record["batch_id"].(json.Number); ok {
		id, err := key.Int64()
		if err != nil {
			return nil, err
		}
		batch, err := r.Batch(ctx, id)
		if err != nil {
			return nil, err
		}
		out["batch"] = batch
	}
	return out, nil
}
func (r *Repository) Events(ctx context.Context, page, size int) (Page[json.RawMessage], error) {
	out := Page[json.RawMessage]{Items: []json.RawMessage{}, Page: page, PageSize: size}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.quota_follow_reset_events`).Scan(&out.Total); err != nil {
		return out, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT row_to_json(e) FROM (SELECT id,group_id,trigger_type,reset_at,status,created_at,completed_at,jsonb_array_length(subscriptions) AS subscription_count FROM sub2api_enhance.quota_follow_reset_events ORDER BY id DESC LIMIT $1 OFFSET $2)e`, size, (page-1)*size)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw json.RawMessage
		if err = rows.Scan(&raw); err != nil {
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
