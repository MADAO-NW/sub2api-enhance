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

	"github.com/lib/pq"
)

// SubscriptionBatchLimit 与原版订阅批量接口的上限保持一致。
const SubscriptionBatchLimit = 100

type SubscriptionSnapshot struct {
	ID           int64      `json:"subscription_id"`
	UserID       int64      `json:"user_id"`
	GroupID      int64      `json:"group_id"`
	Username     string     `json:"username"`
	DailyUsage   string     `json:"daily_usage_usd"`
	WeeklyUsage  string     `json:"weekly_usage_usd"`
	MonthlyUsage string     `json:"monthly_usage_usd"`
	DailyStart   *time.Time `json:"daily_window_start"`
	WeeklyStart  *time.Time `json:"weekly_window_start"`
	MonthlyStart *time.Time `json:"monthly_window_start"`
	ExpiresAt    time.Time  `json:"expires_at"`
	ObservedAt   time.Time  `json:"observed_at"`
}

type SubscriptionGroupCount struct {
	GroupID   int64  `json:"group_id"`
	GroupName string `json:"group_name"`
	Count     int    `json:"count"`
}

// MonitorAccount 是原版账号表中与监控资格相关的只读字段，不含凭据与邮箱。
type MonitorAccount struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	Type        string `json:"type"`
	ParentID    *int64 `json:"parent_account_id"`
	Status      string `json:"status"`
	Schedulable bool   `json:"schedulable"`
	Deleted     bool   `json:"deleted"`
	GroupCount  int    `json:"group_count"`
}

// SubscriptionColumns 保留数据库金额原文，快照只表示采样值而非原子操作前后值。
const SubscriptionColumns = `s.id,s.user_id,s.group_id,u.username,s.daily_usage_usd::text,s.weekly_usage_usd::text,s.monthly_usage_usd::text,s.daily_window_start,s.weekly_window_start,s.monthly_window_start,s.expires_at,clock_timestamp()`

// SubscriptionEligibility 以真实订阅关系确定目标；跟随作用于全库有效订阅，不套用分组访问授权。
const SubscriptionEligibility = `s.deleted_at IS NULL AND s.status='active' AND s.expires_at>clock_timestamp() AND u.deleted_at IS NULL AND u.status='active' AND u.role='user' AND g.deleted_at IS NULL AND g.status='active' AND g.platform='openai' AND g.subscription_type='subscription'`

// ValidSubscriptions 在一个只读快照内读取全部有效订阅，仅用于事件成立时冻结名单。
func (s *QuotaData) ValidSubscriptions(ctx context.Context) ([]SubscriptionSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+SubscriptionColumns+` FROM public.user_subscriptions s JOIN public.users u ON u.id=s.user_id JOIN public.groups g ON g.id=s.group_id WHERE `+SubscriptionEligibility+` ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	subscriptions := []SubscriptionSnapshot{}
	for rows.Next() {
		var v SubscriptionSnapshot
		if err = scanSubscription(rows, &v); err != nil {
			rows.Close()
			return nil, err
		}
		subscriptions = append(subscriptions, v)
	}
	readErr, closeErr := rows.Err(), rows.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return subscriptions, tx.Commit()
}

// ValidSubscriptionGroups 返回有效订阅的全局总数与按分组计数，供页面在确认前只读预览。
func (s *QuotaData) ValidSubscriptionGroups(ctx context.Context) ([]SubscriptionGroupCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id,g.name,count(*)::int FROM public.user_subscriptions s JOIN public.users u ON u.id=s.user_id JOIN public.groups g ON g.id=s.group_id WHERE `+SubscriptionEligibility+` GROUP BY g.id,g.name ORDER BY count(*) DESC,g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubscriptionGroupCount{}
	for rows.Next() {
		var v SubscriptionGroupCount
		if err = rows.Scan(&v.GroupID, &v.GroupName, &v.Count); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SubscriptionSnapshots 按订阅 ID 重新确认当前资格，不限定分组。
func (s *QuotaData) SubscriptionSnapshots(ctx context.Context, ids []int64) (map[int64]SubscriptionSnapshot, error) {
	out := map[int64]SubscriptionSnapshot{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+SubscriptionColumns+` FROM public.user_subscriptions s JOIN public.users u ON u.id=s.user_id JOIN public.groups g ON g.id=s.group_id WHERE s.id=ANY($1) AND `+SubscriptionEligibility, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v SubscriptionSnapshot
		if err = scanSubscription(rows, &v); err != nil {
			return nil, err
		}
		out[v.ID] = v
	}
	return out, rows.Err()
}

func scanSubscription(row interface{ Scan(...any) error }, v *SubscriptionSnapshot) error {
	return row.Scan(&v.ID, &v.UserID, &v.GroupID, &v.Username, &v.DailyUsage, &v.WeeklyUsage, &v.MonthlyUsage, &v.DailyStart, &v.WeeklyStart, &v.MonthlyStart, &v.ExpiresAt, &v.ObservedAt)
}

// MonitorAccount 读取单个账号的监控资格字段；账号不存在时返回 sql.ErrNoRows。
func (s *QuotaData) MonitorAccount(ctx context.Context, id int64) (MonitorAccount, error) {
	var v MonitorAccount
	err := s.db.QueryRowContext(ctx, monitorAccountSQL+` WHERE a.id=$1`, id).Scan(&v.ID, &v.Name, &v.Platform, &v.Type, &v.ParentID, &v.Status, &v.Schedulable, &v.Deleted, &v.GroupCount)
	return v, err
}

// MonitorCandidates 只列出 OpenAI OAuth 非影子未删除账号，资格决定权仍在调用方。
func (s *QuotaData) MonitorCandidates(ctx context.Context) ([]MonitorAccount, error) {
	rows, err := s.db.QueryContext(ctx, monitorAccountSQL+` WHERE a.platform='openai' AND a.type='oauth' AND a.parent_account_id IS NULL AND a.deleted_at IS NULL ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MonitorAccount{}
	for rows.Next() {
		var v MonitorAccount
		if err = rows.Scan(&v.ID, &v.Name, &v.Platform, &v.Type, &v.ParentID, &v.Status, &v.Schedulable, &v.Deleted, &v.GroupCount); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

const monitorAccountSQL = `SELECT a.id,a.name,a.platform,a.type,a.parent_account_id,a.status,a.schedulable,a.deleted_at IS NOT NULL,(SELECT count(*)::int FROM public.account_groups ag WHERE ag.account_id=a.id) FROM public.accounts a`

type QuotaWindow struct {
	UsedPercent *json.Number `json:"used_percent"`
	Seconds     int64        `json:"limit_window_seconds"`
	ResetAfter  int64        `json:"reset_after_seconds"`
	ResetAt     int64        `json:"reset_at"`
}
type QuotaWindows struct {
	Primary   *QuotaWindow `json:"primary_window"`
	Secondary *QuotaWindow `json:"secondary_window"`
}

// AccountQuota 只解析监控账号额度接口中判定需要的字段，不保存邮箱。
type AccountQuota struct {
	AccountID string        `json:"account_id"`
	PlanType  string        `json:"plan_type"`
	Windows   *QuotaWindows `json:"rate_limit"`
	FetchedAt int64         `json:"fetched_at"`
}

func (c *Client) AccountQuota(ctx context.Context, id int64) (AccountQuota, error) {
	var out AccountQuota
	raw, _, err := c.adminRequest(ctx, http.MethodGet, "/api/v1/admin/openai/accounts/"+strconv.FormatInt(id, 10)+"/quota", nil, "", 30*time.Second)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

type SubscriptionResetRequest struct {
	IDs     []int64 `json:"subscription_ids"`
	Action  string  `json:"action"`
	Daily   bool    `json:"daily"`
	Weekly  bool    `json:"weekly"`
	Monthly bool    `json:"monthly"`
}
type SubscriptionResetItem struct {
	ID      int64  `json:"subscription_id"`
	Success *bool  `json:"success"`
	Error   string `json:"error,omitempty"`
}
type SubscriptionResetResult struct {
	SuccessCount int                     `json:"success_count"`
	FailedCount  int                     `json:"failed_count"`
	Items        []SubscriptionResetItem `json:"results"`
}
type SubscriptionResetReply struct {
	Result     SubscriptionResetResult
	HTTPStatus int
	Raw        string
	Error      error
}

func (c *Client) ResetSubscriptions(ctx context.Context, body []byte, requestID string) SubscriptionResetReply {
	reply := SubscriptionResetReply{}
	var req SubscriptionResetRequest
	if json.Unmarshal(body, &req) != nil || len(req.IDs) == 0 || len(req.IDs) > SubscriptionBatchLimit || req.Action != "reset_quota" || (!req.Daily && !req.Weekly && !req.Monthly) || requestID == "" {
		reply.Error = errors.New("订阅批量重置参数无效")
		return reply
	}
	seen := map[int64]bool{}
	for _, id := range req.IDs {
		if id <= 0 || seen[id] {
			reply.Error = errors.New("订阅 ID 无效或重复")
			return reply
		}
		seen[id] = true
	}
	data, meta, err := c.adminRequest(ctx, http.MethodPost, "/api/v1/admin/subscriptions/bulk-action", body, requestID, 130*time.Second)
	reply.Raw = meta.Raw
	reply.HTTPStatus = meta.Status
	reply.Error = err
	if err != nil {
		return reply
	}
	if json.Unmarshal(data, &reply.Result) != nil || len(reply.Result.Items) != len(req.IDs) {
		reply.Error = errors.New("订阅批量响应缺少完整逐项结果")
		return reply
	}
	successes, failures := 0, 0
	for _, item := range reply.Result.Items {
		if !seen[item.ID] || item.Success == nil {
			reply.Error = errors.New("订阅批量响应存在未知、重复或缺失结果")
			return reply
		}
		delete(seen, item.ID)
		if *item.Success {
			successes++
		} else {
			failures++
		}
	}
	if successes != reply.Result.SuccessCount || failures != reply.Result.FailedCount {
		reply.Error = errors.New("订阅批量响应统计与逐项结果不一致")
	}
	return reply
}

type adminHTTPResult struct {
	Status int
	Raw    string
}

// adminRequest 为查询及批量交付设置独立超时，沿用原管理员凭据和禁止跳转策略。
func (c *Client) adminRequest(ctx context.Context, method, path string, body []byte, key string, timeout time.Duration) (json.RawMessage, adminHTTPResult, error) {
	meta := adminHTTPResult{}
	if c.key == "" {
		return nil, meta, errors.New("原版管理员 API Key 未配置")
	}
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(call, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, meta, err
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("X-Request-ID", key)
	}
	client := *c.http
	client.Timeout = timeout
	res, err := client.Do(req)
	if err != nil {
		return nil, meta, errors.New("原版订阅请求未完成确认")
	}
	defer res.Body.Close()
	meta.Status = res.StatusCode
	raw, err := io.ReadAll(res.Body)
	meta.Raw = string(raw)
	if err != nil {
		return nil, meta, errors.New("原版订阅响应未完整读取")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, meta, &APIError{Status: res.StatusCode, Body: string(raw), Method: method, Path: path}
	}
	var env struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Code == nil || *env.Code != 0 || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, meta, fmt.Errorf("原版订阅响应合同未确认（HTTP %d）", res.StatusCode)
	}
	return env.Data, meta, nil
}
