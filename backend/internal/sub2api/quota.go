package sub2api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

type AccountUsage struct {
	Utilization *json.Number
	ResetsAt    *time.Time
	UpdatedAt   *time.Time
	Error       string
}
type QuotaData struct{ db *sql.DB }

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
	rows, err = tx.QueryContext(ctx, `SELECT DISTINCT u.id,u.username,u.email FROM public.users u JOIN public.user_allowed_groups ug ON ug.user_id=u.id JOIN public.user_platform_quotas q ON q.user_id=u.id AND q.platform='openai' AND q.deleted_at IS NULL WHERE ug.group_id=$1 AND u.role='user' AND u.status='active' AND u.deleted_at IS NULL ORDER BY u.id`, groupID)
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
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.username,q.daily_usage_usd::text,q.weekly_usage_usd::text,q.daily_window_start,q.weekly_window_start,clock_timestamp() FROM public.user_platform_quotas q JOIN public.users u ON u.id=q.user_id WHERE u.id=$1 AND u.role='user' AND u.status='active' AND u.deleted_at IS NULL AND q.platform='openai' AND q.deleted_at IS NULL AND EXISTS(SELECT 1 FROM public.user_allowed_groups ug JOIN public.groups g ON g.id=ug.group_id WHERE ug.user_id=u.id AND g.id=$2 AND g.status='active' AND g.deleted_at IS NULL)`, userID, groupID).Scan(&q.UserID, &q.Username, &q.DailyUsage, &q.WeeklyUsage, &q.DailyStart, &q.WeeklyStart, &q.ObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &q, nil
}
func (c *Client) AccountUsageBatch(ctx context.Context, ids []int64) (map[int64]AccountUsage, error) {
	body, err := json.Marshal(map[string]any{"account_ids": ids, "force": true})
	if err != nil {
		return nil, err
	}
	data, _, err := c.request(ctx, http.MethodPost, "/api/v1/admin/accounts/usage/batch", body, "", "", "")
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Usage  map[string]json.RawMessage `json:"usage"`
		Errors map[string]string          `json:"errors"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}
	result := make(map[int64]AccountUsage, len(ids))
	for _, id := range ids {
		key := strconv.FormatInt(id, 10)
		value := AccountUsage{Error: envelope.Errors[key]}
		if value.Error == "" {
			var parsed struct {
				SevenDay *struct {
					Utilization *json.Number `json:"utilization"`
					ResetsAt    *time.Time   `json:"resets_at"`
				} `json:"seven_day"`
				UpdatedAt *time.Time `json:"updated_at"`
			}
			if err := json.Unmarshal(envelope.Usage[key], &parsed); err != nil || parsed.SevenDay == nil {
				value.Error = "账号用量响应缺少可解析的 seven_day"
			} else {
				value.Utilization = parsed.SevenDay.Utilization
				value.ResetsAt = parsed.SevenDay.ResetsAt
				value.UpdatedAt = parsed.UpdatedAt
			}
		}
		result[id] = value
	}
	return result, nil
}

type QuotaResetReply struct {
	HTTPStatus int
	Raw        string
	Error      error
}

// ResetQuota 发起一次非幂等归零；此处没有自动重试或跳转，调用前由交付台账记录 requestID。
func (c *Client) ResetQuota(ctx context.Context, userID int64, window, requestID string) QuotaResetReply {
	if userID <= 0 || (window != "daily" && window != "weekly") || requestID == "" {
		return QuotaResetReply{Error: errors.New("额度重置参数无效")}
	}
	if c.key == "" {
		return QuotaResetReply{Error: errors.New("原版管理员 API Key 未配置")}
	}
	body, _ := json.Marshal(map[string]string{"platform": "openai", "window": window})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/admin/users/"+strconv.FormatInt(userID, 10)+"/platform-quotas/reset", bytes.NewReader(body))
	if err != nil {
		return QuotaResetReply{Error: err}
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", requestID)
	res, err := c.http.Do(req)
	if err != nil {
		return QuotaResetReply{Error: errors.New("额度重置请求未完成确认")}
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	reply := QuotaResetReply{HTTPStatus: res.StatusCode, Raw: string(raw)}
	if err != nil {
		reply.Error = errors.New("额度重置响应未完整读取")
		return reply
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		reply.Error = fmt.Errorf("原版额度重置返回 HTTP %d", res.StatusCode)
		return reply
	}
	var envelope struct {
		Code *int `json:"code"`
		Data struct {
			Quotas json.RawMessage `json:"platform_quotas"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Code == nil || *envelope.Code != 0 || len(envelope.Data.Quotas) == 0 {
		reply.Error = errors.New("额度重置响应合同未确认")
	}
	return reply
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
