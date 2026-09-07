package quotafollow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"log"
	infra "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/sub2api"
	"time"
)

// configLock 串行首次保存与配置修订，不与提示词审计共用业务锁。
const configLock int64 = 579147893221901942

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
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, configLock); err != nil {
		return SavedConfig{}, err
	}
	old := DefaultConfig()
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1 FOR UPDATE`, settingKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return old, err
	}
	if err == nil {
		if err := json.Unmarshal(raw, &old); err != nil {
			return old, err
		}
	}
	if old.Revision != input.ExpectedRevision {
		return old, infra.Conflict("quota_follow_config_conflict", "配置已改变，请刷新后重新保存")
	}
	next := old
	next.Config = input.Config
	next.Revision++
	next.UpdatedBy = actor
	next.UpdatedAt = time.Now().UTC()
	if newEpochRequired(old.Config, next.Config) || next.Epoch == "" {
		next.Epoch = uuid.NewString()
		if next.Enabled {
			now := next.UpdatedAt
			next.EnabledAt = &now
		}
	}
	raw, err = json.Marshal(next)
	if err != nil {
		return next, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.settings(key,value,updated_at) VALUES($1,$2,clock_timestamp()) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at`, settingKey, string(raw)); err != nil {
		return next, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_runtime(singleton) VALUES(true) ON CONFLICT(singleton) DO UPDATE SET next_check_at=clock_timestamp(),paused_revision=NULL,last_error='',carryover_snapshot_at=CASE WHEN $1 THEN NULL ELSE quota_follow_runtime.carryover_snapshot_at END,carryover_checked_week=CASE WHEN $1 THEN NULL ELSE quota_follow_runtime.carryover_checked_week END`, old.Epoch != next.Epoch); err != nil {
		return next, err
	}
	if err := tx.Commit(); err != nil {
		return next, err
	}
	transition, _ := json.Marshal(map[string]any{"before": old, "after": next})
	log.Printf("额度跟随配置已保存 actor_user_id=%d transition=%s", actor, transition)
	return next, nil
}
func (r *Repository) Runtime(ctx context.Context) (Runtime, error) {
	out := Runtime{Accounts: []sub2api.QuotaAccount{}, States: []AccountState{}}
	var raw []byte
	err := r.db.QueryRowContext(ctx, `SELECT epoch,account_set_hash,accounts,last_event_at,last_checked_at,next_check_at,last_error,paused_revision FROM sub2api_enhance.quota_follow_runtime WHERE singleton=true`).Scan(&out.Epoch, &out.AccountSetHash, &raw, &out.LastEventAt, &out.LastCheckedAt, &out.NextCheckAt, &out.LastError, &out.PausedRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out.Accounts); err != nil {
		return out, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT snapshot FROM sub2api_enhance.quota_follow_account_states WHERE epoch=$1 AND account_set_hash=$2 ORDER BY account_id`, out.Epoch, out.AccountSetHash)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var state AccountState
		if err := rows.Scan(&raw); err != nil {
			return out, err
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return out, err
		}
		out.States = append(out.States, state)
	}
	return out, rows.Err()
}
func (r *Repository) Schedule(ctx context.Context, next time.Time, message string, paused *int64) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_runtime(singleton,next_check_at,last_checked_at,last_error,paused_revision) VALUES(true,$1,clock_timestamp(),$2,$3) ON CONFLICT(singleton) DO UPDATE SET next_check_at=$1,last_checked_at=clock_timestamp(),last_error=$2,paused_revision=$3`, next, message, paused)
	return err
}

// SaveObservation 提交账号证据与唯一分组事件；远端重置在事务外由交付循环执行。
func (r *Repository) SaveObservation(ctx context.Context, cfg SavedConfig, discovery sub2api.QuotaDiscovery, hash string, states []AccountState, boundary *time.Time, next time.Time, message string, expectedCheckedAt *time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentRaw []byte
	if err := tx.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1 FOR SHARE`, settingKey).Scan(&currentRaw); err != nil {
		return err
	}
	var current SavedConfig
	if err := json.Unmarshal(currentRaw, &current); err != nil {
		return err
	}
	if current.Revision != cfg.Revision {
		return errors.New("检测期间配置已变化，放弃本轮提交")
	}
	// 即使会话锁因断连丢失，旧检测也不能覆盖另一实例已经提交的新证据。
	var actualCheckedAt *time.Time
	err = tx.QueryRowContext(ctx, `SELECT last_checked_at FROM sub2api_enhance.quota_follow_runtime WHERE singleton=true FOR UPDATE`).Scan(&actualCheckedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if (actualCheckedAt == nil) != (expectedCheckedAt == nil) || (actualCheckedAt != nil && !actualCheckedAt.Equal(*expectedCheckedAt)) {
		return errors.New("已有更新的检测结果，本轮旧证据不再提交")
	}
	accounts, _ := json.Marshal(discovery.Accounts)
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_runtime(singleton,epoch,account_set_hash,accounts,last_event_at,next_check_at,last_checked_at,last_error) VALUES(true,$1,$2,$3,$4,$5,clock_timestamp(),$6)
 ON CONFLICT(singleton) DO UPDATE SET last_event_at=CASE WHEN sub2api_enhance.quota_follow_runtime.epoch<>$1 OR sub2api_enhance.quota_follow_runtime.account_set_hash<>$2 THEN $4 ELSE COALESCE($4,sub2api_enhance.quota_follow_runtime.last_event_at) END,epoch=$1,account_set_hash=$2,accounts=$3,next_check_at=$5,last_checked_at=clock_timestamp(),last_error=$6,paused_revision=NULL`, cfg.Epoch, hash, string(accounts), boundary, next, message); err != nil {
		return err
	}
	for _, state := range states {
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_account_states(account_id,epoch,account_set_hash,snapshot) VALUES($1,$2,$3,$4) ON CONFLICT(account_id) DO UPDATE SET epoch=$2,account_set_hash=$3,snapshot=$4,updated_at=clock_timestamp()`, state.Account.ID, cfg.Epoch, hash, string(raw)); err != nil {
			return err
		}
	}
	if boundary != nil {
		key := cfg.Epoch + ":" + hash + ":" + boundary.UTC().Format(time.RFC3339Nano)
		accountEvidence, _ := json.Marshal(states)
		users, _ := json.Marshal(discovery.Users)
		configRaw, _ := json.Marshal(cfg)
		status := "pending"
		if cfg.ObserveOnly {
			status = "observed"
		}
		var eventID int64
		err := tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_reset_events(event_key,epoch,account_set_hash,reset_at,accounts,users,config_snapshot,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(event_key) DO NOTHING RETURNING id`, key, cfg.Epoch, hash, boundary, string(accountEvidence), string(users), string(configRaw), status).Scan(&eventID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && !cfg.ObserveOnly {
			windows := []string{}
			if cfg.ResetWeekly {
				windows = append(windows, "weekly")
			}
			if cfg.ResetDaily {
				windows = append(windows, "daily")
			}
			for _, user := range discovery.Users {
				for _, window := range windows {
					requestID := uuid.NewString()
					body, _ := json.Marshal(map[string]string{"platform": "openai", "window": window})
					var id int64
					if err := tx.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.quota_follow_reset_deliveries(event_id,user_id,"window",request_id,request_body) VALUES($1,$2,$3,$4,$5) RETURNING id`, eventID, user.ID, window, requestID, string(body)).Scan(&id); err != nil {
						return err
					}
					evidence, _ := json.Marshal(map[string]any{"trigger": "OpenAI 账号一致重置", "account_set_hash": hash})
					if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.quota_reset_records(record_key,source,source_detail,evidence_type,user_id,username,"window",occurred_at,status,event_id,delivery_id,request_id,evidence) VALUES($1,'enhance','openai_consensus','confirmed',$2,$3,$4,$5,'pending',$6,$7,$8,$9)`, fmt.Sprintf("delivery:%d", id), user.ID, user.Username, window, boundary, eventID, id, requestID, string(evidence)); err != nil {
						return err
					}
				}
			}
			if len(discovery.Users) == 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.quota_follow_reset_events SET status='completed',completed_at=clock_timestamp() WHERE id=$1`, eventID); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}
