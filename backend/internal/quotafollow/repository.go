package quotafollow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	infra "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/sub2api"
	"time"

	"github.com/google/uuid"
)

// configLock 串行化配置首次创建与修订，避免多管理员覆盖。
const configLock int64 = 748239516309715

type Repository struct{ db *sql.DB }

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }
func (r *Repository) Config(ctx context.Context) (SavedConfig, error) {
	out := DefaultConfig()
	var raw []byte
	err := r.db.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1`, settingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}
func (r *Repository) SaveConfig(ctx context.Context, input ConfigUpdate, actor int64) (SavedConfig, error) {
	if err := input.Config.Validate(); err != nil {
		return SavedConfig{}, infra.BadRequest("quota_follow_config_invalid", err.Error())
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return SavedConfig{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, configLock); err != nil {
		return SavedConfig{}, err
	}
	old := DefaultConfig()
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1 FOR UPDATE`, settingKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return old, err
	}
	if err == nil {
		if err = json.Unmarshal(raw, &old); err != nil {
			return old, err
		}
	}
	if old.Revision != input.ExpectedRevision {
		return old, infra.Conflict("quota_follow_config_conflict", "配置已改变，请刷新后重新保存")
	}
	next := SavedConfig{Config: input.Config, Revision: old.Revision + 1, UpdatedBy: actor, UpdatedAt: time.Now().UTC()}
	for i := range next.Groups {
		g := &next.Groups[i]
		before, exists := old.Group(g.GroupID)
		// 客户端不能指定轮次或启用时间；只有真实业务范围变化才重建该组基线。
		g.Epoch = before.Epoch
		g.EnabledAt = before.EnabledAt
		if !exists || newEpochRequired(before, *g) || old.Enabled != next.Enabled || g.Epoch == "" {
			g.Epoch = uuid.NewString()
			at := next.UpdatedAt
			g.EnabledAt = &at
		}
	}
	raw, err = json.Marshal(next)
	if err != nil {
		return next, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.settings(key,value,updated_at) VALUES($1,$2,clock_timestamp()) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at`, settingKey, string(raw)); err != nil {
		return next, err
	}
	if err = tx.Commit(); err != nil {
		return next, err
	}
	transition, _ := json.Marshal(map[string]any{"before": old, "after": next})
	log.Printf("订阅跟随配置已保存 actor_user_id=%d transition=%s", actor, transition)
	return next, nil
}
func (r *Repository) Runtime(ctx context.Context, groupID int64) (Runtime, error) {
	out := Runtime{GroupID: groupID, States: []AccountState{}}
	var raw []byte
	err := r.db.QueryRowContext(ctx, `SELECT snapshot FROM sub2api_enhance.quota_follow_group_runtime WHERE group_id=$1`, groupID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

// SaveObservation 将账号进度、分组事件、固定订阅清单与批次原子提交；远程调用不占数据库事务。
func (r *Repository) SaveObservation(ctx context.Context, cfg SavedConfig, rule GroupRule, discovery sub2api.SubscriptionDiscovery, next Runtime, previous Runtime, boundary *time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkConfig(ctx, tx, cfg.Revision); err != nil {
		return err
	}
	// 为首次观测也建立行锁，随后比较上次成功提交时间，防止失去会话锁的旧进程覆盖新证据。
	initial, _ := json.Marshal(Runtime{GroupID: rule.GroupID, States: []AccountState{}})
	if _, err = tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_group_runtime(group_id,snapshot) VALUES($1,$2) ON CONFLICT(group_id) DO NOTHING`, rule.GroupID, string(initial)); err != nil {
		return err
	}
	var raw []byte
	var actual Runtime
	if err = tx.QueryRowContext(ctx, `SELECT snapshot FROM sub2api_enhance.quota_follow_group_runtime WHERE group_id=$1 FOR UPDATE`, rule.GroupID).Scan(&raw); err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &actual); err != nil {
		return err
	}
	if !sameTime(actual.LastCheckedAt, previous.LastCheckedAt) || actual.Epoch != previous.Epoch {
		return errors.New("已有更新的分组状态，本轮旧检测不再提交")
	}
	if boundary != nil {
		evidence, _ := json.Marshal(next.States)
		key := fmt.Sprintf("subscription:%d:%s:%s:%s", rule.GroupID, rule.Epoch, next.AccountSetHash, boundary.UTC().Format(time.RFC3339Nano))
		if _, err = r.createEvent(ctx, tx, key, "automatic", rule, next.AccountSetHash, discovery, evidence, *boundary, cfg.Revision); err != nil {
			return err
		}
		consume(next.States)
		next.LastEventAt = boundary
	}
	raw, err = json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_group_runtime SET snapshot=$2,updated_at=clock_timestamp() WHERE group_id=$1`, rule.GroupID, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}
func sameTime(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}
func checkConfig(ctx context.Context, tx *sql.Tx, revision int64) error {
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1 FOR SHARE`, settingKey).Scan(&raw); err != nil {
		return err
	}
	var cfg SavedConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if cfg.Revision != revision {
		return infra.Conflict("quota_follow_config_conflict", "操作期间配置已改变，请刷新后重试")
	}
	return nil
}

// createEvent 的目标名单在事件产生时冻结，观察模式同样消费账号轮次，但不创建交付批次。
func (r *Repository) createEvent(ctx context.Context, tx *sql.Tx, key, trigger string, rule GroupRule, hash string, d sub2api.SubscriptionDiscovery, evidence []byte, at time.Time, revision int64) (int64, error) {
	status := "pending"
	if trigger == "automatic" && rule.ObserveOnly {
		status = "observed"
	} else if len(d.Subscriptions) == 0 {
		status = "completed"
	}
	configRaw, _ := json.Marshal(struct {
		GroupRule
		Revision int64 `json:"revision"`
	}{rule, revision})
	targets, _ := json.Marshal(d.Subscriptions)
	recordEvidence, _ := json.Marshal(map[string]any{"accounts": json.RawMessage(evidence), "trigger": trigger})
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_reset_events(event_key,epoch,account_set_hash,reset_at,accounts,users,config_snapshot,status,group_id,trigger_type,subscriptions) VALUES($1,$2,$3,$4,$5,'[]',$6,$7,$8,$9,$10) RETURNING id`, key, rule.Epoch, hash, at, string(evidence), string(configRaw), status, rule.GroupID, trigger, string(targets)).Scan(&id)
	if err != nil {
		return 0, err
	}
	if status != "pending" {
		return id, nil
	}
	if trigger == "automatic" {
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_batches b SET status='skipped',error_message='已有更新的账号重置轮次，旧待发送事件不再补发',completed_at=clock_timestamp() FROM sub2api_enhance.quota_follow_reset_events e WHERE b.event_id=e.id AND b.group_id=$1 AND b.event_id<>$2 AND e.trigger_type='automatic' AND b.status='pending'`, rule.GroupID, id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_items i SET status='skipped',error_message=b.error_message FROM sub2api_enhance.quota_follow_subscription_batches b WHERE i.batch_id=b.id AND b.group_id=$1 AND b.status='skipped' AND i.status='pending'`, rule.GroupID); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records r SET status='skipped',error_message=i.error_message FROM sub2api_enhance.quota_follow_subscription_items i WHERE r.subscription_item_id=i.id AND i.group_id=$1 AND i.status='skipped' AND r.status='pending'`, rule.GroupID); err != nil {
			return 0, err
		}
	}
	for offset := 0; offset < len(d.Subscriptions); offset += sub2api.SubscriptionBatchLimit {
		end := min(offset+sub2api.SubscriptionBatchLimit, len(d.Subscriptions))
		items := d.Subscriptions[offset:end]
		req := sub2api.SubscriptionResetRequest{IDs: []int64{}, Action: "reset_quota", Daily: rule.ResetDaily, Weekly: rule.ResetWeekly}
		for _, v := range items {
			req.IDs = append(req.IDs, v.ID)
		}
		body, _ := json.Marshal(req)
		requestID := uuid.NewString()
		var batchID int64
		if err = tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_subscription_batches(event_id,group_id,request_id,request_body) VALUES($1,$2,$3,$4) RETURNING id`, id, rule.GroupID, requestID, string(body)).Scan(&batchID); err != nil {
			return 0, err
		}
		for _, v := range items {
			var itemID int64
			if err = tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_subscription_items(event_id,batch_id,subscription_id,user_id,group_id,username) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, id, batchID, v.ID, v.UserID, rule.GroupID, v.Username).Scan(&itemID); err != nil {
				return 0, err
			}
			for _, window := range selectedWindows(rule) {
				if _, err = tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_reset_records(record_key,source,source_detail,evidence_type,user_id,username,"window",occurred_at,status,event_id,request_id,evidence,group_id,subscription_id,subscription_item_id,batch_id) VALUES($1,'enhance',$2,'confirmed',$3,$4,$5,$6,'pending',$7,$8,$9,$10,$11,$12,$13)`, fmt.Sprintf("subscription:%d:%s", itemID, window), trigger, v.UserID, v.Username, window, at, id, requestID, string(recordEvidence), rule.GroupID, v.ID, itemID, batchID); err != nil {
					return 0, err
				}
			}
		}
	}
	return id, nil
}
func selectedWindows(g GroupRule) []string {
	out := []string{}
	if g.ResetDaily {
		out = append(out, "daily")
	}
	if g.ResetWeekly {
		out = append(out, "weekly")
	}
	return out
}

func (r *Repository) Batch(ctx context.Context, id int64) (*Batch, error) {
	b := &Batch{Items: []BatchItem{}}
	var raw []byte
	err := r.db.QueryRowContext(ctx, `SELECT b.id,b.event_id,b.group_id,b.request_id,b.request_body,b.status,b.response_body,b.error_message,b.http_status,b.started_at,b.completed_at,e.config_snapshot,e.account_set_hash,e.trigger_type FROM sub2api_enhance.quota_follow_subscription_batches b JOIN sub2api_enhance.quota_follow_reset_events e ON e.id=b.event_id WHERE b.id=$1`, id).Scan(&b.ID, &b.EventID, &b.GroupID, &b.RequestID, &b.Body, &b.Status, &b.Response, &b.Error, &b.HTTPStatus, &b.StartedAt, &b.CompletedAt, &raw, &b.AccountSetHash, &b.Trigger)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &b.Rule); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id,subscription_id,user_id,group_id,username,status,error_message,before_snapshot,after_snapshot FROM sub2api_enhance.quota_follow_subscription_items WHERE batch_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v BatchItem
		var before, after []byte
		if err = rows.Scan(&v.ID, &v.SubscriptionID, &v.UserID, &v.GroupID, &v.Username, &v.Status, &v.Error, &before, &after); err != nil {
			return nil, err
		}
		if len(before) > 0 {
			if err = json.Unmarshal(before, &v.Before); err != nil {
				return nil, err
			}
		}
		if len(after) > 0 {
			if err = json.Unmarshal(after, &v.After); err != nil {
				return nil, err
			}
		}
		b.Items = append(b.Items, v)
	}
	return b, rows.Err()
}
func (r *Repository) PendingBatches(ctx context.Context) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM sub2api_enhance.quota_follow_subscription_batches WHERE status='pending' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ClaimBatch 在发送前持久化固定正文与逐项资格结果，已发出的正文永不重组。
func (r *Repository) ClaimBatch(ctx context.Context, b *Batch, revision int64) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = checkConfig(ctx, tx, revision); err != nil {
		return false, err
	}
	// 会话锁断连后仍通过分组行锁串行领取；持久化 inflight 后才允许释放行锁和发出请求。
	initial, _ := json.Marshal(Runtime{GroupID: b.GroupID, States: []AccountState{}})
	if _, err = tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_group_runtime(group_id,snapshot) VALUES($1,$2) ON CONFLICT(group_id) DO NOTHING`, b.GroupID, string(initial)); err != nil {
		return false, err
	}
	var groupID int64
	if err = tx.QueryRowContext(ctx, `SELECT group_id FROM sub2api_enhance.quota_follow_group_runtime WHERE group_id=$1 FOR UPDATE`, b.GroupID).Scan(&groupID); err != nil {
		return false, err
	}
	var blocked bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_subscription_batches WHERE group_id=$1 AND id<>$2 AND status IN ('inflight','uncertain'))`, b.GroupID, b.ID).Scan(&blocked); err != nil {
		return false, err
	}
	if blocked {
		return false, nil
	}
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM sub2api_enhance.quota_follow_subscription_batches WHERE id=$1 FOR UPDATE`, b.ID).Scan(&status); err != nil {
		return false, err
	}
	if status != "pending" {
		return false, nil
	}
	ids := []int64{}
	for _, v := range b.Items {
		if v.Status == "pending" {
			ids = append(ids, v.SubscriptionID)
		}
	}
	req := sub2api.SubscriptionResetRequest{IDs: ids, Action: "reset_quota", Daily: b.Rule.ResetDaily, Weekly: b.Rule.ResetWeekly}
	body, _ := json.Marshal(req)
	b.Body = string(body)
	b.Status = "inflight"
	if len(ids) == 0 {
		b.Status = "skipped"
	}
	for i := range b.Items {
		v := &b.Items[i]
		if v.Status == "pending" {
			v.Status = "inflight"
		}
		raw, _ := json.Marshal(v.Before)
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_items SET status=$2,before_snapshot=$3,error_message=$4 WHERE id=$1`, v.ID, v.Status, string(raw), v.Error); err != nil {
			return false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records SET status=$2,before_snapshot=$3,error_message=$4 WHERE subscription_item_id=$1`, v.ID, v.Status, string(raw), v.Error); err != nil {
			return false, err
		}
	}
	if err = tx.QueryRowContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_batches SET status=$2,request_body=$3,started_at=clock_timestamp(),completed_at=CASE WHEN $2='skipped' THEN clock_timestamp() END WHERE id=$1 RETURNING started_at`, b.ID, b.Status, b.Body).Scan(&b.StartedAt); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return len(ids) > 0, nil
}
func (r *Repository) FinishBatch(ctx context.Context, b *Batch) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, v := range b.Items {
		raw, _ := json.Marshal(v.After)
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_items SET status=$2,after_snapshot=$3,error_message=$4 WHERE id=$1`, v.ID, v.Status, string(raw), v.Error); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records SET status=$2,after_snapshot=$3,error_message=$4 WHERE subscription_item_id=$1`, v.ID, v.Status, string(raw), v.Error); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_batches SET status=$2,response_body=$3,http_status=$4,error_message=$5,completed_at=clock_timestamp() WHERE id=$1`, b.ID, b.Status, b.Response, b.HTTPStatus, b.Error); err != nil {
		return err
	}
	if err = finishEvents(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
func finishEvents(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_reset_events e SET status=CASE WHEN EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_subscription_batches b WHERE b.event_id=e.id AND b.status IN ('uncertain','reviewed')) THEN 'uncertain' WHEN EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_subscription_batches b WHERE b.event_id=e.id AND b.status IN ('partial','failed')) THEN 'partial' ELSE 'completed' END,completed_at=clock_timestamp() WHERE e.trigger_type<>'legacy' AND e.status='pending' AND NOT EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_subscription_batches b WHERE b.event_id=e.id AND b.status IN ('pending','inflight'))`)
	return err
}

// RecoverInflight 只收拢超过原版执行时限的未知调用；不能把租约到期当成允许再次清零。
func (r *Repository) RecoverInflight(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_batches SET status='uncertain',error_message='上次调用结果未持久确认，请只读核对；禁止换键重发',completed_at=clock_timestamp() WHERE status='inflight' AND started_at<clock_timestamp()-interval '3 minutes'`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_items i SET status='uncertain',error_message=b.error_message FROM sub2api_enhance.quota_follow_subscription_batches b WHERE i.batch_id=b.id AND b.status='uncertain' AND i.status='inflight'`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records r SET status=i.status,error_message=i.error_message FROM sub2api_enhance.quota_follow_subscription_items i WHERE r.subscription_item_id=i.id AND r.status='inflight' AND i.status='uncertain'`); err != nil {
		return err
	}
	if err = finishEvents(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
