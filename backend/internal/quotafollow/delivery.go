package quotafollow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"strconv"
	"sub2api-enhance/internal/sub2api"
	"time"
)

func deliveryStatus(reply sub2api.QuotaResetReply) string {
	if reply.HTTPStatus >= 200 && reply.HTTPStatus < 300 && reply.Error == nil {
		return "succeeded"
	}
	if reply.HTTPStatus == 404 {
		return "skipped"
	}
	if reply.HTTPStatus >= 400 && reply.HTTPStatus < 500 {
		return "failed"
	}
	return "uncertain"
}
func (s *Service) deliver(ctx context.Context) error {
	// 远端调用超时短于此恢复边界；过期 inflight 只转不确定，永远不重新领取发送。
	if _, err := s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_reset_deliveries SET status='uncertain',error_message='上次发送没有持久确认，禁止自动重调',completed_at=clock_timestamp() WHERE status='inflight' AND started_at<clock_timestamp()-interval '1 minute'`); err != nil {
		return err
	}
	if _, err := s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records r SET status=d.status,error_message=d.error_message FROM sub2api_enhance.quota_follow_reset_deliveries d WHERE r.delivery_id=d.id AND r.status='inflight' AND d.status='uncertain'`); err != nil {
		return err
	}
	// 崩溃恢复也推进事件终态，避免最后一条交付未知时事件永久显示处理中。
	if _, err := s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_reset_events e SET status=CASE WHEN EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_reset_deliveries WHERE event_id=e.id AND status='uncertain') THEN 'uncertain' WHEN EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_reset_deliveries WHERE event_id=e.id AND status='failed') THEN 'partial' ELSE 'completed' END,completed_at=clock_timestamp() WHERE e.status='pending' AND NOT EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_reset_deliveries WHERE event_id=e.id AND status IN ('pending','inflight'))`); err != nil {
		return err
	}
	if err := s.repairPendingCaches(ctx); err != nil {
		log.Printf("额度缓存恢复未完成：%v", err)
	}
	cfg, err := s.repo.Config(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled || cfg.ObserveOnly || cfg.GroupID == nil || (!cfg.ResetDaily && !cfg.ResetWeekly) {
		return nil
	}
	runtime, err := s.repo.Runtime(ctx)
	if err != nil {
		return err
	}
	if runtime.PausedRevision != nil && *runtime.PausedRevision == cfg.Revision {
		return nil
	}
	if !s.api.Configured() {
		return nil
	}

	for ctx.Err() == nil {
		if err := s.maintainCarryovers(ctx, cfg, runtime); err != nil {
			log.Printf("周一结转暂未完成：%v", err)
		}
		d, err := s.repo.NextDelivery(ctx)
		if err != nil {
			return err
		}
		if d == nil {
			return nil
		}
		current, err := s.repo.Config(ctx)
		if err != nil {
			return err
		}
		if !current.Enabled || current.ObserveOnly {
			return nil
		}
		if current.Epoch != cfg.Epoch {
			return nil
		}
		if current.Epoch != d.Epoch || d.Config.GroupID == nil || !sameID(d.Config.GroupID, current.GroupID) {
			d.Status = "skipped"
			d.Error = "启用周期、分组或日周范围已经改变"
			if err := s.repo.FinishDelivery(ctx, d); err != nil {
				return err
			}
			continue
		}
		accounts, err := s.source.Accounts(ctx, *cfg.GroupID)
		if err != nil {
			return err
		}
		currentHash := accountSetHash(accounts)
		var eventHash string
		if err := s.repo.db.QueryRowContext(ctx, `SELECT account_set_hash FROM sub2api_enhance.quota_follow_reset_events WHERE id=$1`, d.EventID).Scan(&eventHash); err != nil {
			return err
		}
		if currentHash != eventHash || len(accounts) == 0 {
			d.Status = "skipped"
			d.Error = "原事件的账号集合已变化"
			if err := s.repo.FinishDelivery(ctx, d); err != nil {
				return err
			}
			continue
		}
		before, err := s.source.Snapshot(ctx, d.UserID, *cfg.GroupID)
		if err != nil {
			return err
		}
		if before == nil {
			d.Status = "skipped"
			d.Error = "用户、分组关系或 OpenAI 配额记录已失效"
			if err := s.repo.FinishDelivery(ctx, d); err != nil {
				return err
			}
			continue
		}
		d.Before = before
		if s.cache != nil {
			d.CacheBefore, err = s.cache.Read(ctx, d.UserID)
			if err != nil {
				d.Error = joinMessage(d.Error, "重置前缓存只读观测失败："+err.Error())
			}
		}
		// 在本地提交已发送边界；即使响应或进程丢失，也不能再次调用原版归零。
		sent, err := s.repo.MarkInflight(ctx, d, current)
		if err != nil {
			return err
		}
		if !sent {
			continue
		}
		log.Printf("开始交付额度重置 event_id=%d user_id=%d window=%s request_id=%s", d.EventID, d.UserID, d.Window, d.RequestID)
		reply := s.api.ResetQuota(ctx, d.UserID, d.Window, d.RequestID)
		d.Status = deliveryStatus(reply)
		d.Response = reply.Raw
		if reply.HTTPStatus != 0 {
			d.HTTPStatus = &reply.HTTPStatus
		}
		if reply.Error != nil {
			d.Error = joinMessage(d.Error, reply.Error.Error())
		}
		verify, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		s.verifyDelivery(verify, d, *cfg.GroupID)
		err = s.repo.FinishDelivery(verify, d)
		cancel()
		if err != nil {
			return err
		}
		if reply.HTTPStatus == 401 || reply.HTTPStatus == 403 {
			return s.repo.Schedule(ctx, time.Now().Add(time.Duration(cfg.MaxInterval)*time.Minute), "管理员 API 鉴权失败，自动额度任务已暂停；修复后重新保存配置", &cfg.Revision)
		}
	}
	return ctx.Err()
}
func (s *Service) verifyDelivery(ctx context.Context, d *Delivery, groupID int64) {
	after, err := s.source.Snapshot(ctx, d.UserID, groupID)
	d.DatabaseStatus = "unknown"
	if err != nil {
		d.Error = joinMessage(d.Error, "数据库结果复核失败："+err.Error())
	} else if after == nil {
		d.DatabaseStatus = "missing"
	} else {
		d.After = after
		_, start := after.Window(d.Window)
		if start != nil && d.StartedAt != nil && !start.Before(*d.StartedAt) {
			d.DatabaseStatus = "window_advanced"
		}
	}
	d.CacheStatus = "not_configured"
	if s.cache == nil {
		return
	}
	raw, err := s.cache.Read(ctx, d.UserID)
	if err != nil {
		d.CacheStatus = "unknown"
		d.Error = joinMessage(d.Error, "Redis 只读复核失败："+err.Error())
		return
	}
	d.Cache = raw
	if len(raw) == 0 {
		d.CacheStatus = "absent"
		return
	}
	d.CacheStatus = "unknown"
	if d.After == nil {
		return
	}
	_, dbStart := d.After.Window(d.Window)
	cached := raw[d.Window+"_window_start"]
	unix, err := strconv.ParseInt(cached, 10, 64)
	if err != nil || dbStart == nil {
		return
	}
	if dbStart.Unix() == unix {
		d.CacheStatus = "window_matches"
	} else {
		d.CacheStatus = "stale"
		d.Error = joinMessage(d.Error, "缓存窗口与数据库不一致；只读核对未修改缓存或重复归零")
	}
}
func joinMessage(a, b string) string {
	if a == "" {
		return b
	}
	return a + "；" + b
}

// cacheCompatible 检查原版 Hash 所需的窗口及金额字段，失败时不把空值当 0。
// quotaCacheSchemaVersion 来自已核对的 main 0.2.1 配额 Hash 协议。
const quotaCacheSchemaVersion = "1"

func cacheCompatible(raw map[string]string) error {
	if len(raw) == 0 {
		return nil
	}
	if raw["schema_version"] != quotaCacheSchemaVersion {
		return errors.New("Redis 配额 Hash 版本不兼容")
	}
	for _, window := range []string{"daily", "weekly"} {
		value, ok := raw[window+"_usage"]
		if !ok {
			return errors.New("Redis 配额 Hash 缺少用量字段")
		}
		if _, ok := new(big.Rat).SetString(value); !ok {
			return errors.New("Redis 配额金额格式不兼容")
		}
		if value := raw[window+"_window_start"]; value != "" {
			if _, err := strconv.ParseInt(value, 10, 64); err != nil {
				return errors.New("Redis 配额窗口不是 Unix 秒")
			}
		}
	}
	return nil
}
func (r *Repository) NextDelivery(ctx context.Context) (*Delivery, error) {
	var d Delivery
	var cfg []byte
	err := r.db.QueryRowContext(ctx, `SELECT d.id,d.event_id,d.user_id,d."window",d.request_id,d.status,e.epoch,e.config_snapshot FROM sub2api_enhance.quota_follow_reset_deliveries d JOIN sub2api_enhance.quota_follow_reset_events e ON e.id=d.event_id WHERE d.status='pending' ORDER BY d.id LIMIT 1`).Scan(&d.ID, &d.EventID, &d.UserID, &d.Window, &d.RequestID, &d.Status, &d.Epoch, &cfg)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(cfg, &d.Config); err != nil {
		return nil, err
	}
	return &d, nil
}
func (r *Repository) MarkInflight(ctx context.Context, d *Delivery, cfg SavedConfig) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1 FOR SHARE`, settingKey).Scan(&raw); err != nil {
		return false, err
	}
	var current SavedConfig
	if err := json.Unmarshal(raw, &current); err != nil {
		return false, err
	}
	if !current.Enabled || current.ObserveOnly || current.Epoch != cfg.Epoch {
		return false, nil
	}
	before, err := json.Marshal(d.Before)
	if err != nil {
		return false, err
	}
	cacheBefore, _ := json.Marshal(d.CacheBefore)
	err = tx.QueryRowContext(ctx, `UPDATE sub2api_enhance.quota_follow_reset_deliveries SET status='inflight',started_at=clock_timestamp(),before_snapshot=$2,before_cache_snapshot=$3 WHERE id=$1 AND status='pending' RETURNING started_at`, d.ID, string(before), string(cacheBefore)).Scan(&d.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records SET status='inflight',before_snapshot=$2 WHERE delivery_id=$1`, d.ID, string(before)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	d.Status = "inflight"
	return true, nil
}
func (r *Repository) FinishDelivery(ctx context.Context, d *Delivery) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, _ := json.Marshal(d.Before)
	after, _ := json.Marshal(d.After)
	cache, _ := json.Marshal(d.Cache)
	// 迟到的原版审计可能已经先确认完成；保留更强证据，同时补齐当前调用的响应与快照。
	err = tx.QueryRowContext(ctx, `UPDATE sub2api_enhance.quota_follow_reset_deliveries SET status=CASE WHEN audit_log_id IS NOT NULL AND status IN ('succeeded','failed','skipped') THEN status ELSE $2 END,response_body=$3,before_snapshot=COALESCE(before_snapshot,$4::jsonb),after_snapshot=$5,cache_snapshot=$6,database_status=$7,cache_status=$8,http_status=$9,error_message=$10,completed_at=clock_timestamp() WHERE id=$1 AND (status IN ('pending','inflight','uncertain') OR audit_log_id IS NOT NULL) RETURNING status`, d.ID, d.Status, d.Response, string(before), string(after), string(cache), d.DatabaseStatus, d.CacheStatus, d.HTTPStatus, d.Error).Scan(&d.Status)
	if err != nil {
		return err
	}
	evidence, _ := json.Marshal(map[string]any{"http_status": d.HTTPStatus, "database_status": d.DatabaseStatus, "cache_status": d.CacheStatus, "cache_snapshot": d.Cache, "before_cache_snapshot": d.CacheBefore, "request_started_at": d.StartedAt})
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records SET status=$2,before_snapshot=COALESCE(before_snapshot,$3::jsonb),after_snapshot=$4,error_message=$5,evidence=evidence || $6::jsonb,occurred_at=COALESCE($7,occurred_at) WHERE delivery_id=$1`, d.ID, d.Status, string(before), string(after), d.Error, string(evidence), d.StartedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_reset_events e SET status=CASE WHEN EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_reset_deliveries WHERE event_id=e.id AND status IN ('pending','inflight')) THEN 'pending' WHEN EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_reset_deliveries WHERE event_id=e.id AND status='uncertain') THEN 'uncertain' WHEN EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_reset_deliveries WHERE event_id=e.id AND status='failed') THEN 'partial' ELSE 'completed' END,completed_at=CASE WHEN NOT EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_reset_deliveries WHERE event_id=e.id AND status IN ('pending','inflight')) THEN clock_timestamp() END WHERE id=$1`, d.EventID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	transition, _ := json.Marshal(map[string]any{"before": d.Before, "after": d.After, "cache_before": d.CacheBefore, "cache_after": d.Cache})
	names := map[string]string{"succeeded": "接口重置成功", "skipped": "已跳过", "failed": "明确失败", "uncertain": "结果不确定，未重调"}
	log.Printf("额度重置交付结束：%s event_id=%d user_id=%d window=%s request_id=%s transition=%s reason=%s", names[d.Status], d.EventID, d.UserID, d.Window, d.RequestID, transition, d.Error)
	return nil
}
