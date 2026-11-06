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

type SubscriptionAccount struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	ParentID *int64 `json:"parent_account_id,omitempty"`
	IsShadow bool   `json:"is_shadow"`
}
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
type SubscriptionDiscovery struct {
	GroupID       int64                  `json:"group_id"`
	GroupName     string                 `json:"group_name"`
	Accounts      []SubscriptionAccount  `json:"accounts"`
	Subscriptions []SubscriptionSnapshot `json:"subscriptions"`
}

// SubscriptionColumns 保留数据库金额原文，快照只表示采样值而非原子操作前后值。
const SubscriptionColumns = `s.id,s.user_id,s.group_id,u.username,s.daily_usage_usd::text,s.weekly_usage_usd::text,s.monthly_usage_usd::text,s.daily_window_start,s.weekly_window_start,s.monthly_window_start,s.expires_at,clock_timestamp()`

// SubscriptionEligibility 以真实订阅关系确定目标，不套用普通分组的访问授权规则。
const SubscriptionEligibility = `s.deleted_at IS NULL AND s.status='active' AND s.expires_at>clock_timestamp() AND u.deleted_at IS NULL AND u.status='active' AND u.role='user' AND g.deleted_at IS NULL AND g.status='active' AND g.platform='openai' AND g.subscription_type='subscription'`

func (s *QuotaData) DiscoverSubscriptions(ctx context.Context, groupID int64) (SubscriptionDiscovery, error) {
	out := SubscriptionDiscovery{GroupID: groupID, Accounts: []SubscriptionAccount{}, Subscriptions: []SubscriptionSnapshot{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT name FROM public.groups WHERE id=$1 AND deleted_at IS NULL AND status='active' AND platform='openai' AND subscription_type='subscription'`, groupID).Scan(&out.GroupName)
	if errors.Is(err, sql.ErrNoRows) {
		return out, errors.New("请选择有效的 OpenAI 订阅分组")
	}
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.name,a.type,a.parent_account_id FROM public.accounts a JOIN public.account_groups ag ON ag.account_id=a.id WHERE ag.group_id=$1 AND a.platform='openai' AND a.status='active' AND a.deleted_at IS NULL ORDER BY a.id`, groupID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var a SubscriptionAccount
		if err = rows.Scan(&a.ID, &a.Name, &a.Type, &a.ParentID); err != nil {
			rows.Close()
			return out, err
		}
		a.IsShadow = a.ParentID != nil
		out.Accounts = append(out.Accounts, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT `+SubscriptionColumns+` FROM public.user_subscriptions s JOIN public.users u ON u.id=s.user_id JOIN public.groups g ON g.id=s.group_id WHERE s.group_id=$1 AND `+SubscriptionEligibility+` ORDER BY s.id`, groupID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var v SubscriptionSnapshot
		if err = scanSubscription(rows, &v); err != nil {
			return out, err
		}
		out.Subscriptions = append(out.Subscriptions, v)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	return out, tx.Commit()
}
func scanSubscription(row interface{ Scan(...any) error }, v *SubscriptionSnapshot) error {
	return row.Scan(&v.ID, &v.UserID, &v.GroupID, &v.Username, &v.DailyUsage, &v.WeeklyUsage, &v.MonthlyUsage, &v.DailyStart, &v.WeeklyStart, &v.MonthlyStart, &v.ExpiresAt, &v.ObservedAt)
}

func (s *QuotaData) SubscriptionSnapshots(ctx context.Context, groupID int64, ids []int64) (map[int64]SubscriptionSnapshot, error) {
	out := map[int64]SubscriptionSnapshot{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+SubscriptionColumns+` FROM public.user_subscriptions s JOIN public.users u ON u.id=s.user_id JOIN public.groups g ON g.id=s.group_id WHERE s.group_id=$1 AND s.id=ANY($2) AND `+SubscriptionEligibility, groupID, pq.Array(ids))
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
type AccountQuota struct {
	Windows    *QuotaWindows `json:"rate_limit"`
	Additional []struct {
		Feature string        `json:"metered_feature"`
		Windows *QuotaWindows `json:"rate_limit"`
	} `json:"additional_rate_limits"`
	FetchedAt int64 `json:"fetched_at"`
}

func (c *Client) AccountQuota(ctx context.Context, id int64) (AccountQuota, error) {
	var out AccountQuota
	raw, _, err := c.subscriptionRequest(ctx, http.MethodGet, "/api/v1/admin/openai/accounts/"+strconv.FormatInt(id, 10)+"/quota", nil, "", 30*time.Second)
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
	if json.Unmarshal(body, &req) != nil || len(req.IDs) == 0 || len(req.IDs) > SubscriptionBatchLimit || req.Action != "reset_quota" || (!req.Daily && !req.Weekly) || req.Monthly || requestID == "" {
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
	data, meta, err := c.subscriptionRequest(ctx, http.MethodPost, "/api/v1/admin/subscriptions/bulk-action", body, requestID, 130*time.Second)
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

type subscriptionHTTPResult struct {
	Status int
	Raw    string
}

// subscriptionRequest 为查询及批量交付设置独立超时，沿用原管理员凭据和禁止跳转策略。
func (c *Client) subscriptionRequest(ctx context.Context, method, path string, body []byte, key string, timeout time.Duration) (json.RawMessage, subscriptionHTTPResult, error) {
	meta := subscriptionHTTPResult{}
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
		return nil, meta, &APIError{Status: res.StatusCode, Body: string(raw)}
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
