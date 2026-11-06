package sub2api

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type QuotaAccount struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}
type QuotaUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}
type QuotaDiscovery struct {
	GroupID   int64          `json:"group_id"`
	GroupName string         `json:"group_name"`
	Accounts  []QuotaAccount `json:"accounts"`
	Users     []QuotaUser    `json:"users"`
}
type QuotaSnapshot struct {
	UserID      int64      `json:"user_id"`
	Username    string     `json:"username"`
	DailyUsage  string     `json:"daily_usage_usd"`
	WeeklyUsage string     `json:"weekly_usage_usd"`
	DailyStart  *time.Time `json:"daily_window_start"`
	WeeklyStart *time.Time `json:"weekly_window_start"`
	ObservedAt  time.Time  `json:"observed_at"`
}

func (q QuotaSnapshot) Window(window string) (string, *time.Time) {
	if window == "daily" {
		return q.DailyUsage, q.DailyStart
	}
	return q.WeeklyUsage, q.WeeklyStart
}

type QuotaData struct{ db *sql.DB }

// QuotaUserGroupAccessSQL 统一额度跟随的公开分组与显式授权判断，调用方使用用户 u、分组 g 别名并校验其有效性。
const QuotaUserGroupAccessSQL = `((NOT g.is_exclusive AND NOT u.restrict_public_groups) OR EXISTS(SELECT 1 FROM public.user_allowed_groups ug WHERE ug.user_id=u.id AND ug.group_id=g.id))`

func NewQuotaData(db *sql.DB) *QuotaData { return &QuotaData{db: db} }

// Discover 在同一只读快照内确认分组、有效 OpenAI 账号和参加跟随的普通用户。
func (s *QuotaData) Discover(ctx context.Context, groupID int64) (QuotaDiscovery, error) {
	out := QuotaDiscovery{GroupID: groupID, Accounts: []QuotaAccount{}, Users: []QuotaUser{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT name FROM public.groups WHERE id=$1 AND deleted_at IS NULL AND status='active'`, groupID).Scan(&out.GroupName)
	if errors.Is(err, sql.ErrNoRows) {
		return out, errors.New("目标分组不存在或已失效")
	}
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT a.id,a.name,a.type FROM public.accounts a JOIN public.account_groups ag ON ag.account_id=a.id WHERE ag.group_id=$1 AND a.platform='openai' AND a.status='active' AND a.deleted_at IS NULL ORDER BY a.id`, groupID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var a QuotaAccount
		if err := rows.Scan(&a.ID, &a.Name, &a.Type); err != nil {
			_ = rows.Close()
			return out, err
		}
		out.Accounts = append(out.Accounts, a)
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil {
		return out, readErr
	}
	if closeErr != nil {
		return out, closeErr
	}
	rows, err = tx.QueryContext(ctx, `SELECT DISTINCT u.id,u.username,u.email FROM public.users u JOIN public.user_platform_quotas q ON q.user_id=u.id AND q.platform='openai' AND q.deleted_at IS NULL JOIN public.groups g ON g.id=$1 AND g.status='active' AND g.deleted_at IS NULL WHERE u.role='user' AND u.status='active' AND u.deleted_at IS NULL AND `+QuotaUserGroupAccessSQL+` ORDER BY u.id`, groupID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var u QuotaUser
		if err := rows.Scan(&u.ID, &u.Username, &u.Email); err != nil {
			_ = rows.Close()
			return out, err
		}
		out.Users = append(out.Users, u)
	}
	readErr = rows.Err()
	closeErr = rows.Close()
	if readErr != nil {
		return out, readErr
	}
	if closeErr != nil {
		return out, closeErr
	}
	return out, tx.Commit()
}

// Snapshot 只读原版配额；数据库 NUMERIC 以原文本输出，避免金额经过 float64。
func (s *QuotaData) Snapshot(ctx context.Context, userID, groupID int64) (*QuotaSnapshot, error) {
	var q QuotaSnapshot
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.username,q.daily_usage_usd::text,q.weekly_usage_usd::text,q.daily_window_start,q.weekly_window_start,clock_timestamp() FROM public.user_platform_quotas q JOIN public.users u ON u.id=q.user_id WHERE u.id=$1 AND u.role='user' AND u.status='active' AND u.deleted_at IS NULL AND q.platform='openai' AND q.deleted_at IS NULL AND EXISTS(SELECT 1 FROM public.groups g WHERE g.id=$2 AND g.status='active' AND g.deleted_at IS NULL AND `+QuotaUserGroupAccessSQL+`)`, userID, groupID).Scan(&q.UserID, &q.Username, &q.DailyUsage, &q.WeeklyUsage, &q.DailyStart, &q.WeeklyStart, &q.ObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// Accounts 在逐项发送前重新确认原事件账号集合，避免长批次沿用过期绑定。
func (s *QuotaData) Accounts(ctx context.Context, groupID int64) ([]QuotaAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT a.id,a.name,a.type FROM public.accounts a JOIN public.account_groups ag ON ag.account_id=a.id JOIN public.groups g ON g.id=ag.group_id WHERE g.id=$1 AND g.deleted_at IS NULL AND g.status='active' AND a.platform='openai' AND a.status='active' AND a.deleted_at IS NULL ORDER BY a.id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QuotaAccount{}
	for rows.Next() {
		var a QuotaAccount
		if err := rows.Scan(&a.ID, &a.Name, &a.Type); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
