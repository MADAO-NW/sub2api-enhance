package quotafollow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"
)

// cacheRepairInterval 只控制已提交动作的缓存恢复重试，不重放归零或结转金额。
const cacheRepairInterval = time.Minute

// repairPendingCaches 即使模块被关闭也收尾已提交动作；金额结果未知时不擅自删除缓存。
func (s *Service) repairPendingCaches(ctx context.Context) error {
	if !s.flusherDisabled {
		return nil
	}
	invalidator, ok := s.cache.(CacheInvalidator)
	if !ok {
		return nil
	}
	rows, err := s.repo.db.QueryContext(ctx, `SELECT 'carryover',id,user_id,(config_snapshot->>'group_id')::bigint,'weekly' FROM sub2api_enhance.quota_follow_weekly_carryovers WHERE status='succeeded' AND cache_status<>'repaired' AND (cache_retry_at IS NULL OR cache_retry_at<=clock_timestamp())
 UNION ALL SELECT 'delivery',d.id,d.user_id,(e.config_snapshot->>'group_id')::bigint,d."window" FROM sub2api_enhance.quota_follow_reset_deliveries d JOIN sub2api_enhance.quota_follow_reset_events e ON e.id=d.event_id WHERE d.status='succeeded' AND d.cache_status NOT IN ('repaired','absent','window_matches') AND (d.cache_retry_at IS NULL OR d.cache_retry_at<=clock_timestamp())`)
	if err != nil {
		return err
	}
	type item struct {
		window              string
		kind                string
		id, userID, groupID int64
	}
	items := []item{}
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.kind, &v.id, &v.userID, &v.groupID, &v.window); err != nil {
			rows.Close()
			return err
		}
		items = append(items, v)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	var failures error
	for _, v := range items {
		repair, cancel := context.WithTimeout(ctx, 5*time.Second)
		snapshot, callErr := s.source.Snapshot(repair, v.userID, v.groupID)
		if callErr == nil && snapshot == nil {
			callErr = errors.New("用户或原分组配额已失效，未操作缓存")
		}
		var before, after map[string]string
		if callErr == nil {
			before, callErr = s.cache.Read(repair, v.userID)
		}
		if callErr == nil {
			callErr = invalidator.Invalidate(repair, v.userID)
		}
		if callErr == nil {
			after, callErr = s.cache.Read(repair, v.userID)
		}
		if callErr == nil && len(after) > 0 {
			_, start := snapshot.Window(v.window)
			if start == nil || after[v.window+"_window_start"] != strconv.FormatInt(start.Unix(), 10) {
				callErr = errors.New("缓存清理后窗口仍与数据库不一致，继续只重试缓存清理")
			}
		}
		cancel()
		status := "repaired"
		message := ""
		if callErr != nil {
			status = "retry_pending"
			message = callErr.Error()
			failures = errors.Join(failures, fmt.Errorf("用户 %d 缓存恢复失败：%w", v.userID, callErr))
		}
		beforeRaw, _ := json.Marshal(before)
		afterRaw, _ := json.Marshal(after)
		evidence, _ := json.Marshal(map[string]any{"at": time.Now().UTC(), "status": status, "before": before, "after": after, "database_observation": snapshot, "error": message})
		// 清理可能已成功但本地提交未知；重试只删除缓存，不再次修改原配额。
		tx, err := s.repo.db.BeginTx(ctx, nil)
		if err != nil {
			return errors.Join(failures, err)
		}
		if v.kind == "carryover" {
			_, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_weekly_carryovers SET cache_status=$2,cache_before=$3,cache_after=$4,error_message=$5,cache_retry_at=$6 WHERE id=$1 AND status='succeeded'`, v.id, status, string(beforeRaw), string(afterRaw), message, time.Now().Add(cacheRepairInterval))
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_reset_deliveries SET cache_status=$2,cache_snapshot=$3,cache_retry_at=$4 WHERE id=$1 AND status='succeeded'`, v.id, status, string(afterRaw), time.Now().Add(cacheRepairInterval))
		}
		if err == nil {
			column := "delivery_id"
			if v.kind == "carryover" {
				column = "carryover_id"
			}
			_, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records SET evidence=jsonb_set(evidence,'{cache_repairs}',COALESCE(evidence->'cache_repairs','[]'::jsonb)||jsonb_build_array($2::jsonb)) WHERE `+column+`=$1`, v.id, string(evidence))
		}
		if err != nil {
			tx.Rollback()
			return errors.Join(failures, err)
		}
		if err := tx.Commit(); err != nil {
			return errors.Join(failures, err)
		}
		log.Printf("额度缓存恢复结束 user_id=%d kind=%s id=%d 清理已确认=%t before=%s after=%s error=%s", v.userID, v.kind, v.id, callErr == nil, beforeRaw, afterRaw, message)
	}
	return failures
}

// RequestCacheRepair 只将指定已成功动作的缓存清理重新排期，调度仍不会重放原金额变更。
func (s *Service) RequestCacheRepair(ctx context.Context, id, actor int64) error {
	if !s.flusherDisabled {
		return errors.New("原版 Flusher 状态未确认关闭，不能修复缓存")
	}
	if _, ok := s.cache.(CacheInvalidator); !ok {
		return errors.New("未配置额度 Redis 清理能力")
	}
	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var delivery, carryover *int64
	if err := tx.QueryRowContext(ctx, `SELECT delivery_id,carryover_id FROM sub2api_enhance.quota_reset_records WHERE id=$1 AND source='enhance' AND status='succeeded'`, id).Scan(&delivery, &carryover); err != nil {
		return err
	}
	var result sql.Result
	if carryover != nil {
		result, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_weekly_carryovers SET cache_status='retry_pending',cache_retry_at=clock_timestamp() WHERE id=$1 AND status='succeeded'`, *carryover)
	} else if delivery != nil {
		result, err = tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_reset_deliveries SET cache_status='retry_pending',cache_retry_at=clock_timestamp() WHERE id=$1 AND status='succeeded'`, *delivery)
	} else {
		return errors.New("该记录没有可恢复的增强动作")
	}
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("动作状态已改变，请刷新记录")
	}
	evidence, _ := json.Marshal(map[string]any{"at": time.Now().UTC(), "actor_user_id": actor, "action": "重新排期缓存清理，不重放归零或结转"})
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_reset_records SET evidence=jsonb_set(evidence,'{cache_repair_requests}',COALESCE(evidence->'cache_repair_requests','[]'::jsonb)||jsonb_build_array($2::jsonb)) WHERE id=$1`, id, string(evidence)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("管理员已重新排期额度缓存清理 record_id=%d actor_user_id=%d evidence=%s", id, actor, evidence)
	return nil
}
