package quotafollow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"sub2api-enhance/internal/sub2api"
	"time"
)

// CacheInvalidator 与只读复核分开，只有已确认数据库动作的恢复流程使用它。
type CacheInvalidator interface {
	Invalidate(context.Context, int64) error
}

func weekStart(now time.Time, location *time.Location) time.Time {
	local := now.In(location)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	return day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
}
func (s *Service) carryoverPrerequisite() error {
	if !s.flusherDisabled {
		return errors.New("周一结转及缓存修复暂停：须核实原版 Flusher 关闭并设置 SUB2API_USER_PLATFORM_QUOTA_FLUSHER_ENABLED=false")
	}
	if s.location == nil {
		return errors.New("周一结转暂停：未配置有效的原版服务器时区")
	}
	if _, ok := s.cache.(CacheInvalidator); !ok {
		return errors.New("周一结转及缓存修复暂停：未配置可清理额度缓存的 Redis")
	}
	return nil
}
func carryoverHealth(cfg SavedConfig, runtime Runtime, now time.Time) error {
	if cfg.Epoch != runtime.Epoch || len(runtime.States) == 0 || len(runtime.States) != len(runtime.Accounts) {
		return errors.New("周一结转等待当前账号基线")
	}
	var first, last time.Time
	for _, state := range runtime.States {
		if state.Error != "" || state.ObservedAt == nil || now.Sub(*state.ObservedAt) > time.Duration(cfg.MaxInterval)*time.Minute || state.ObservedAt.After(now) || state.NextResetAt == nil || !state.NextResetAt.After(now) {
			return errors.New("周一结转暂停：账号证据失效、过旧或官方重置边界已到达")
		}
		if first.IsZero() || state.NextResetAt.Before(first) {
			first = *state.NextResetAt
		}
		if last.IsZero() || state.NextResetAt.After(last) {
			last = *state.NextResetAt
		}
	}
	if last.Sub(first) > consensusTolerance {
		return errors.New("周一结转暂停：账号预期边界相差超过五分钟")
	}
	return nil
}

// maintainCarryovers 与归零交付共用发送锁；每秒检查周一边界，快照按配置间隔并在边界前最后一秒补采。
func (s *Service) maintainCarryovers(ctx context.Context, cfg SavedConfig, runtime Runtime) (resultErr error) {
	if !cfg.ResetWeekly {
		return nil
	}
	defer func() {
		message := ""
		if resultErr != nil {
			message = resultErr.Error()
		}
		_, err := s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_runtime SET carryover_error=$1 WHERE singleton=true AND carryover_error IS DISTINCT FROM $1`, message)
		if resultErr == nil {
			resultErr = err
		}
	}()
	if err := s.carryoverPrerequisite(); err != nil {
		return err
	}
	var now time.Time
	var lastSnapshot, checkedWeek *time.Time
	if err := s.repo.db.QueryRowContext(ctx, `SELECT clock_timestamp(),carryover_snapshot_at,carryover_checked_week FROM sub2api_enhance.quota_follow_runtime WHERE singleton=true`).Scan(&now, &lastSnapshot, &checkedWeek); err != nil {
		return err
	}
	week := weekStart(now, s.location)
	next := week.AddDate(0, 0, 7)
	due := lastSnapshot == nil || now.Sub(*lastSnapshot) >= time.Duration(cfg.MinInterval)*time.Minute || (now.Before(next) && !now.Before(next.Add(-time.Second)) && lastSnapshot.Before(next.Add(-time.Second)))
	boundaryDue := checkedWeek == nil || !checkedWeek.Equal(week)
	if !due && !boundaryDue {
		return nil
	}
	if err := carryoverHealth(cfg, runtime, now); err != nil {
		return err
	}
	discovery, err := s.source.Discover(ctx, *cfg.GroupID)
	if err != nil {
		return err
	}
	if accountSetHash(discovery.Accounts) != runtime.AccountSetHash {
		return errors.New("周一结转暂停：分组账号集合已经变化")
	}
	// 先处理当前边界再更新下一周快照，旧快照按 week_start 保留，不会被新窗口覆盖。
	if boundaryDue && cfg.EnabledAt != nil && cfg.EnabledAt.Before(week) {
		for _, user := range discovery.Users {
			if err := s.applyCarryover(ctx, cfg, runtime, week, user); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("用户 %d 结转失败：%w", user.ID, err))
				log.Printf("周一用量结转失败 user_id=%d week_start=%s error=%v", user.ID, week.Format(time.RFC3339), err)
			}
		}
	}
	if boundaryDue && resultErr == nil {
		if _, err := s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_runtime SET carryover_checked_week=$1 WHERE singleton=true`, week); err != nil {
			return err
		}
	}
	if !due {
		return resultErr
	}
	for _, user := range discovery.Users {
		snapshot, err := s.source.Snapshot(ctx, user.ID, *cfg.GroupID)
		if err != nil {
			return errors.Join(resultErr, err)
		}
		if snapshot == nil || snapshot.WeeklyStart == nil || snapshot.WeeklyStart.Before(week) || !snapshot.WeeklyStart.Before(next) || !snapshot.ObservedAt.Before(next) {
			continue
		}
		cache, err := s.cache.Read(ctx, user.ID)
		if err != nil {
			return errors.Join(resultErr, err)
		}
		raw, _ := json.Marshal(snapshot)
		cacheRaw, _ := json.Marshal(cache)
		// Flusher 关闭时只采纳 DB 已提交用量，Redis 无法提供跨介质提交顺序，不能取 max 或补差。
		_, err = s.repo.db.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_weekly_snapshots(user_id,week_start,epoch,account_set_hash,snapshot,cache_snapshot,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(user_id,week_start) DO UPDATE SET epoch=$3,account_set_hash=$4,snapshot=$5,cache_snapshot=$6,observed_at=$7 WHERE quota_follow_weekly_snapshots.observed_at<$7`, user.ID, next, cfg.Epoch, runtime.AccountSetHash, string(raw), string(cacheRaw), snapshot.ObservedAt)
		if err != nil {
			return errors.Join(resultErr, err)
		}
		log.Printf("周一结转快照已保存 user_id=%d week_start=%s source=PostgreSQL snapshot=%s cache_evidence=%s", user.ID, next.Format(time.RFC3339), raw, cacheRaw)
	}
	_, err = s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_runtime SET carryover_snapshot_at=$1 WHERE singleton=true`, now)
	return errors.Join(resultErr, err)
}

// carryoverAmount 区分尚未懒重置与已经自然重置，金额使用十进制有理数，不经 float64。
func carryoverAmount(snapshot, current sub2api.QuotaSnapshot, week, now time.Time, maxAge time.Duration) (string, error) {
	if now.Before(week) || !now.Before(week.Add(maxAge)) {
		return "", errors.New("已错过周一安全处理窗口，不补偿历史边界")
	}
	if !snapshot.ObservedAt.Before(week) || week.Sub(snapshot.ObservedAt) > maxAge {
		return "", errors.New("周日前快照缺失或过旧，不猜测旧用量")
	}
	if snapshot.WeeklyStart == nil || !snapshot.WeeklyStart.Before(week) || snapshot.WeeklyStart.Before(week.AddDate(0, 0, -7)) {
		return "", errors.New("周日前快照不是上一自然周的有效窗口")
	}
	old, ok := new(big.Rat).SetString(snapshot.WeeklyUsage)
	if !ok || old.Sign() < 0 {
		return "", errors.New("周日前金额无效")
	}
	value, ok := new(big.Rat).SetString(current.WeeklyUsage)
	if !ok || value.Sign() < 0 {
		return "", errors.New("当前周金额无效")
	}
	if current.WeeklyStart == nil {
		return "", errors.New("当前窗口缺失，不能排除配额被重建")
	}
	if current.WeeklyStart.After(week) {
		return "", errors.New("周一后已经发生其他重置，不再加回旧用量")
	}
	if current.WeeklyStart.Before(week) {
		if !current.WeeklyStart.Equal(*snapshot.WeeklyStart) {
			return "", errors.New("快照后窗口发生改变，不覆盖人工或其他重置")
		}
		if value.Cmp(old) < 0 {
			return "", errors.New("快照后旧窗口用量下降，不覆盖可能的人工调整")
		}
		return current.WeeklyUsage, nil
	}
	return new(big.Rat).Add(old, value).FloatString(10), nil
}

// applyCarryover 同一事务锁住幂等记录和原配额行；金额、证据、缓存待办一起提交，未知提交不能再加回。
func (s *Service) applyCarryover(ctx context.Context, cfg SavedConfig, runtime Runtime, week time.Time, user sub2api.QuotaUser) error {
	accounts, err := s.source.Accounts(ctx, *cfg.GroupID)
	if err != nil {
		return err
	}
	if len(accounts) == 0 || accountSetHash(accounts) != runtime.AccountSetHash {
		return errors.New("结转前账号集合已变化")
	}
	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var configRaw []byte
	if err := tx.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1 FOR SHARE`, settingKey).Scan(&configRaw); err != nil {
		return err
	}
	var currentCfg SavedConfig
	if err := json.Unmarshal(configRaw, &currentCfg); err != nil {
		return err
	}
	if !currentCfg.Enabled || currentCfg.ObserveOnly || !currentCfg.ResetWeekly || currentCfg.Revision != cfg.Revision {
		return errors.New("结转期间配置已变化，放弃本次执行")
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_weekly_carryovers(user_id,week_start,epoch,account_set_hash,config_snapshot,status) VALUES($1,$2,$3,$4,$5,'pending') ON CONFLICT(user_id,week_start) DO NOTHING RETURNING id`, user.ID, week, cfg.Epoch, runtime.AccountSetHash, string(configRaw)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var snapshot sub2api.QuotaSnapshot
	var snapshotRaw []byte
	reason := ""
	err = tx.QueryRowContext(ctx, `SELECT snapshot FROM sub2api_enhance.quota_follow_weekly_snapshots WHERE user_id=$1 AND week_start=$2 AND epoch=$3 AND account_set_hash=$4`, user.ID, week, cfg.Epoch, runtime.AccountSetHash).Scan(&snapshotRaw)
	if errors.Is(err, sql.ErrNoRows) {
		reason = "本启用周期或账号集合缺少周日前快照"
	} else if err != nil {
		return err
	} else if err = json.Unmarshal(snapshotRaw, &snapshot); err != nil {
		return err
	}
	var cacheBefore map[string]string
	if reason == "" {
		cacheBefore, err = s.cache.Read(ctx, user.ID)
		if err != nil {
			return err
		}
	}
	var before sub2api.QuotaSnapshot
	// 与原版 IncrementUsageWithReset 使用同一行锁，锁内读取最新消费后再决定保留或相加。
	err = tx.QueryRowContext(ctx, `SELECT u.id,u.username,q.daily_usage_usd::text,q.weekly_usage_usd::text,q.daily_window_start,q.weekly_window_start,clock_timestamp() FROM public.user_platform_quotas q JOIN public.users u ON u.id=q.user_id WHERE u.id=$1 AND u.role='user' AND u.status='active' AND u.deleted_at IS NULL AND q.platform='openai' AND q.deleted_at IS NULL AND EXISTS(SELECT 1 FROM public.user_allowed_groups ug JOIN public.groups g ON g.id=ug.group_id WHERE ug.user_id=u.id AND g.id=$2 AND g.status='active' AND g.deleted_at IS NULL) FOR UPDATE OF q`, user.ID, *cfg.GroupID).Scan(&before.UserID, &before.Username, &before.DailyUsage, &before.WeeklyUsage, &before.DailyStart, &before.WeeklyStart, &before.ObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		reason = "用户、分组或 OpenAI 配额已失效"
	} else if err != nil {
		return err
	}
	var conflicting bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.quota_follow_reset_deliveries WHERE user_id=$1 AND "window"='weekly' AND (status IN ('pending','inflight','uncertain') OR started_at>=$2))`, user.ID, week).Scan(&conflicting); err != nil {
		return err
	}
	if conflicting {
		reason = "已有待处理、不确定或周一后的增强归零，不覆盖该调用"
	}
	if reason == "" {
		// 原版归零后可能被后续计费重新投影成周一窗口；有审计证据时仍须识别为冲突。
		rows, err := tx.QueryContext(ctx, `SELECT request_body FROM public.audit_logs WHERE method='POST' AND path='/api/v1/admin/users/:id/platform-quotas/reset' AND extra#>>'{params,id}'=$1 AND created_at>=$2 AND (status_code BETWEEN 200 AND 299 OR status_code>=500)`, fmt.Sprint(user.ID), snapshot.ObservedAt)
		if err != nil {
			return err
		}
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				rows.Close()
				return err
			}
			var request struct {
				Platform string `json:"platform"`
				Window   string `json:"window"`
			}
			if json.Unmarshal([]byte(body), &request) == nil && request.Platform == "openai" && request.Window == "weekly" {
				reason = "快照后存在原版周额度归零审计，不覆盖该重置"
			}
		}
		readErr := rows.Err()
		closeErr := rows.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	var amount string
	if reason == "" {
		if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&before.ObservedAt); err != nil {
			return err
		}
		amount, err = carryoverAmount(snapshot, before, week, before.ObservedAt, time.Duration(cfg.MaxInterval)*time.Minute)
		if err != nil {
			reason = err.Error()
		}
	}
	beforeRaw, _ := json.Marshal(before)
	if before.UserID == 0 {
		beforeRaw = []byte("null")
	}
	after := before
	status := "skipped"
	cacheStatus := "not_required"
	if reason == "" {
		err = tx.QueryRowContext(ctx, `UPDATE public.user_platform_quotas SET weekly_usage_usd=$2::numeric,weekly_window_start=$3 WHERE user_id=$1 AND platform='openai' AND deleted_at IS NULL RETURNING weekly_usage_usd::text,weekly_window_start,clock_timestamp()`, user.ID, amount, week).Scan(&after.WeeklyUsage, &after.WeeklyStart, &after.ObservedAt)
		if err != nil {
			return err
		}
		status = "succeeded"
		cacheStatus = "pending"
	}
	afterRaw, _ := json.Marshal(after)
	if before.UserID == 0 {
		afterRaw = []byte("null")
	}
	cacheBeforeRaw, _ := json.Marshal(cacheBefore)
	if len(snapshotRaw) == 0 {
		snapshotRaw = []byte("null")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_weekly_carryovers SET snapshot=$2,before_snapshot=$3,after_snapshot=$4,status=$5,cache_status=$6,error_message=$7,applied_at=CASE WHEN $5='succeeded' THEN clock_timestamp() END,cache_before=$8 WHERE id=$1`, id, string(snapshotRaw), string(beforeRaw), string(afterRaw), status, cacheStatus, reason, string(cacheBeforeRaw)); err != nil {
		return err
	}
	evidence, _ := json.Marshal(map[string]any{"snapshot_source": "PostgreSQL", "week_start": week, "snapshot": json.RawMessage(snapshotRaw), "cache_status": cacheStatus, "account_set_hash": runtime.AccountSetHash, "accounts": runtime.States})
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_reset_records(record_key,source,source_detail,evidence_type,action_type,user_id,username,"window",occurred_at,status,before_snapshot,after_snapshot,evidence,carryover_id,error_message) VALUES($1,'enhance','weekly_carryover','confirmed','carryover',$2,$3,'weekly',clock_timestamp(),$4,$5,$6,$7,$8,$9)`, fmt.Sprintf("carryover:%d", id), user.ID, user.Username, status, string(beforeRaw), string(afterRaw), string(evidence), id, reason); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("周一用量结转结束 user_id=%d carryover_id=%d week_start=%s 已应用=%t before=%s after=%s reason=%s", user.ID, id, week.Format(time.RFC3339), status == "succeeded", beforeRaw, afterRaw, reason)
	return nil
}
