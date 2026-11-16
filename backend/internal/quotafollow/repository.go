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

// SaveConfig 只在启用状态由关变开或监控账号变化时生成新周期，其余保存只递增修订号。
func (r *Repository) SaveConfig(ctx context.Context, input ConfigUpdate, actor int64) (SavedConfig, error) {
	config := Config{Enabled: input.Enabled, MonitorAccountID: input.MonitorAccountID}
	if err := config.Validate(); err != nil {
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
	next := SavedConfig{Config: config, Epoch: old.Epoch, EnabledAt: old.EnabledAt, Revision: old.Revision + 1, UpdatedBy: actor, UpdatedAt: time.Now().UTC()}
	// 客户端不能指定轮次或启用时间；只有真实业务范围变化才重建基线。
	if next.Enabled && (next.Epoch == "" || !old.Enabled || old.MonitorAccountID != next.MonitorAccountID) {
		next.Epoch = uuid.NewString()
		at := next.UpdatedAt
		next.EnabledAt = &at
	}
	if raw, err = json.Marshal(next); err != nil {
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

func (r *Repository) Runtime(ctx context.Context) (Runtime, error) {
	out := Runtime{}
	var raw []byte
	err := r.db.QueryRowContext(ctx, `SELECT snapshot FROM sub2api_enhance.quota_follow_monitor_runtime WHERE singleton`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

// SaveObservation 将检测状态、事件、冻结的订阅名单与待发送批次在一个事务内提交；远程调用不占事务。
func (r *Repository) SaveObservation(ctx context.Context, cfg SavedConfig, previous, next Runtime, event *pendingEvent) (Runtime, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return next, err
	}
	defer tx.Rollback()
	if err = checkConfig(ctx, tx, cfg.Revision); err != nil {
		return next, err
	}
	// 为首次观测也建立行锁，随后比较上次成功提交时间，防止失去会话锁的旧进程覆盖新证据。
	if _, err = tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_monitor_runtime(singleton,snapshot) VALUES(TRUE,'{}') ON CONFLICT(singleton) DO NOTHING`); err != nil {
		return next, err
	}
	var raw []byte
	var actual Runtime
	if err = tx.QueryRowContext(ctx, `SELECT snapshot FROM sub2api_enhance.quota_follow_monitor_runtime WHERE singleton FOR UPDATE`).Scan(&raw); err != nil {
		return next, err
	}
	if err = json.Unmarshal(raw, &actual); err != nil {
		return next, err
	}
	if actual.Epoch != previous.Epoch || !sameTime(actual.LastCheckedAt, previous.LastCheckedAt) {
		return next, errors.New("已有更新的检测状态，本轮旧检测不再提交")
	}
	if event != nil {
		id, created, createErr := r.createEvent(ctx, tx, cfg, event)
		if createErr != nil {
			return next, createErr
		}
		// 事件键冲突说明同一窗口已经处理过，照常保存检测状态，避免检测卡死。
		if created {
			next.LastResetAt = &event.ResetAt
			next.LastEventID = &id
		}
	}
	if raw, err = json.Marshal(next); err != nil {
		return next, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_monitor_runtime SET snapshot=$1,updated_at=clock_timestamp() WHERE singleton`, string(raw)); err != nil {
		return next, err
	}
	return next, tx.Commit()
}

func sameTime(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

// readConfigTx 按默认值处理从未保存过的配置，所有调用点共享同一口径。
func readConfigTx(ctx context.Context, tx *sql.Tx) (SavedConfig, error) {
	out := DefaultConfig()
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1 FOR SHARE`, settingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

func checkConfig(ctx context.Context, tx *sql.Tx, revision int64) error {
	cfg, err := readConfigTx(ctx, tx)
	if err != nil {
		return err
	}
	if cfg.Revision != revision {
		return infra.Conflict("quota_follow_config_conflict", "操作期间配置已改变，请刷新后重试")
	}
	return nil
}

// createEvent 的目标名单在事件产生时冻结；固定正文重置日、周、月三个窗口。
func (r *Repository) createEvent(ctx context.Context, tx *sql.Tx, cfg SavedConfig, e *pendingEvent) (int64, bool, error) {
	status := "pending"
	if len(e.Subscriptions) == 0 {
		status = "completed"
	}
	evidence := e.Evidence
	if len(evidence) == 0 {
		evidence = json.RawMessage(`{}`)
	}
	configRaw, _ := json.Marshal(map[string]any{"epoch": cfg.Epoch, "monitor_account_id": cfg.MonitorAccountID, "revision": cfg.Revision})
	targets, _ := json.Marshal(e.Subscriptions)
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_reset_events(event_key,epoch,account_set_hash,reset_at,accounts,users,config_snapshot,status,group_id,trigger_type,subscriptions) VALUES($1,$2,'',$3,$4,'[]',$5,$6,NULL,$7,$8) ON CONFLICT(event_key) DO NOTHING RETURNING id`, e.Key, cfg.Epoch, e.ResetAt, string(evidence), string(configRaw), status, e.Trigger, string(targets)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if e.Trigger == "automatic" {
		// 新的自动事件把更早的待发送自动批次标为跳过，避免恢复后连续补发。
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_batches b SET status='skipped',error_message='已有更新的账号重置轮次，旧待发送事件不再补发',completed_at=clock_timestamp() FROM sub2api_enhance.quota_follow_reset_events e WHERE b.event_id=e.id AND b.event_id<>$1 AND e.trigger_type='automatic' AND b.status='pending'`, id); err != nil {
			return 0, false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_items i SET status='skipped',error_message=b.error_message FROM sub2api_enhance.quota_follow_subscription_batches b WHERE i.batch_id=b.id AND b.status='skipped' AND i.status='pending'`); err != nil {
			return 0, false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records r SET status='skipped',error_message=i.error_message FROM sub2api_enhance.quota_follow_subscription_items i WHERE r.subscription_item_id=i.id AND i.status='skipped' AND r.status='pending'`); err != nil {
			return 0, false, err
		}
	}
	if status != "pending" {
		return id, true, nil
	}
	recordEvidence, _ := json.Marshal(map[string]any{"accounts": json.RawMessage(evidence), "trigger": e.Trigger})
	for offset := 0; offset < len(e.Subscriptions); offset += sub2api.SubscriptionBatchLimit {
		end := min(offset+sub2api.SubscriptionBatchLimit, len(e.Subscriptions))
		items := e.Subscriptions[offset:end]
		req := sub2api.SubscriptionResetRequest{IDs: []int64{}, Action: "reset_quota", Daily: true, Weekly: true, Monthly: true}
		for _, v := range items {
			req.IDs = append(req.IDs, v.ID)
		}
		body, _ := json.Marshal(req)
		requestID := uuid.NewString()
		var batchID int64
		if err = tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_subscription_batches(event_id,group_id,request_id,request_body) VALUES($1,NULL,$2,$3) RETURNING id`, id, requestID, string(body)).Scan(&batchID); err != nil {
			return 0, false, err
		}
		for _, v := range items {
			var itemID int64
			if err = tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_subscription_items(event_id,batch_id,subscription_id,user_id,group_id,username) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, id, batchID, v.ID, v.UserID, v.GroupID, v.Username).Scan(&itemID); err != nil {
				return 0, false, err
			}
			for _, window := range resetWindows {
				if _, err = tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_reset_records(record_key,source,source_detail,evidence_type,user_id,username,"window",occurred_at,status,event_id,request_id,evidence,group_id,subscription_id,subscription_item_id,batch_id) VALUES($1,'enhance',$2,'confirmed',$3,$4,$5,$6,'pending',$7,$8,$9,$10,$11,$12,$13)`, fmt.Sprintf("subscription:%d:%s", itemID, window), e.Trigger, v.UserID, v.Username, window, e.ResetAt, id, requestID, string(recordEvidence), v.GroupID, v.ID, itemID, batchID); err != nil {
					return 0, false, err
				}
			}
		}
	}
	return id, true, nil
}

func (r *Repository) Batch(ctx context.Context, id int64) (*Batch, error) {
	b := &Batch{Items: []BatchItem{}}
	err := r.db.QueryRowContext(ctx, `SELECT b.id,b.event_id,b.request_id,b.request_body,b.status,b.response_body,b.error_message,b.http_status,b.started_at,b.completed_at,e.trigger_type,e.epoch FROM sub2api_enhance.quota_follow_subscription_batches b JOIN sub2api_enhance.quota_follow_reset_events e ON e.id=b.event_id WHERE b.id=$1`, id).Scan(&b.ID, &b.EventID, &b.RequestID, &b.Body, &b.Status, &b.Response, &b.Error, &b.HTTPStatus, &b.StartedAt, &b.CompletedAt, &b.Trigger, &b.Epoch)
	if err != nil {
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

// ClaimBatch 在全局运行行锁内复核配置与阻塞，持久化 inflight 后才允许发出请求。
func (r *Repository) ClaimBatch(ctx context.Context, b *Batch) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// 全局运行行既是互斥锁，也是检测与交付共享的串行锚点。
	if _, err = tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_monitor_runtime(singleton,snapshot) VALUES(TRUE,'{}') ON CONFLICT(singleton) DO NOTHING`); err != nil {
		return false, err
	}
	var locked []byte
	if err = tx.QueryRowContext(ctx, `SELECT snapshot FROM sub2api_enhance.quota_follow_monitor_runtime WHERE singleton FOR UPDATE`).Scan(&locked); err != nil {
		return false, err
	}
	// 任一未结束的批次（含旧分组批次）阻塞全局，直到管理员核对确认。
	var blocked bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_subscription_batches WHERE id<>$1 AND status IN ('inflight','uncertain'))`, b.ID).Scan(&blocked); err != nil {
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
	if b.Trigger == "automatic" {
		cfg, readErr := readConfigTx(ctx, tx)
		if readErr != nil {
			return false, readErr
		}
		// 总开关关闭时暂缓发送；启用周期变化时整批跳过。
		if !cfg.Enabled {
			return false, nil
		}
		if cfg.Epoch != b.Epoch {
			if err = skipBatch(ctx, tx, b, "监控账号或启用周期已经改变"); err != nil {
				return false, err
			}
			return false, tx.Commit()
		}
	}
	ids := []int64{}
	for _, v := range b.Items {
		if v.Status == "pending" {
			ids = append(ids, v.SubscriptionID)
		}
	}
	req := sub2api.SubscriptionResetRequest{IDs: ids, Action: "reset_quota", Daily: true, Weekly: true, Monthly: true}
	body, _ := json.Marshal(req)
	b.Body = string(body)
	b.Status = "inflight"
	if len(ids) == 0 {
		b.Status = "skipped"
		if b.Error == "" {
			b.Error = "发送前复核后没有仍然有效的订阅"
		}
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
	if err = tx.QueryRowContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_batches SET status=$2,request_body=$3,error_message=$4,started_at=clock_timestamp(),completed_at=CASE WHEN $2='skipped' THEN clock_timestamp() END WHERE id=$1 RETURNING started_at`, b.ID, b.Status, b.Body, b.Error).Scan(&b.StartedAt); err != nil {
		return false, err
	}
	if b.Status == "skipped" {
		if err = finishEvents(ctx, tx); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return len(ids) > 0, nil
}

// skipBatch 在事务内把整批标为跳过，item 与重置记录同步收尾。
func skipBatch(ctx context.Context, tx *sql.Tx, b *Batch, reason string) error {
	for i := range b.Items {
		v := &b.Items[i]
		// 已发出批次的逐项结果不可改写；发送前标记的跳过项仍需把内存状态落到库。
		if v.Status != "pending" && v.Status != "inflight" && v.Status != "skipped" {
			continue
		}
		v.Status = "skipped"
		if v.Error == "" {
			v.Error = reason
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_items SET status=$2,error_message=$3 WHERE id=$1`, v.ID, v.Status, v.Error); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records SET status=$2,error_message=$3 WHERE subscription_item_id=$1`, v.ID, v.Status, v.Error); err != nil {
			return err
		}
	}
	b.Status = "skipped"
	b.Error = reason
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_batches SET status='skipped',error_message=$2,completed_at=clock_timestamp() WHERE id=$1`, b.ID, reason); err != nil {
		return err
	}
	return finishEvents(ctx, tx)
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

// SaveAfterSnapshots 在逐项结果提交后补写只读观测，不改变任何结果状态与错误信息。
func (r *Repository) SaveAfterSnapshots(ctx context.Context, items []BatchItem) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	written := false
	for _, v := range items {
		if v.After == nil {
			continue
		}
		raw, _ := json.Marshal(v.After)
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_subscription_items SET after_snapshot=$2 WHERE id=$1`, v.ID, string(raw)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records SET after_snapshot=$2 WHERE subscription_item_id=$1`, v.ID, string(raw)); err != nil {
			return err
		}
		written = true
	}
	if !written {
		return nil
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
