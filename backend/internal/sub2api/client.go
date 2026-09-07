package sub2api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sub2api-enhance/internal/config"
	"time"
)

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	Status   string `json:"status"`
}
type Client struct {
	base, key string
	http      *http.Client
}
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("原版 API 返回 HTTP %d", e.Status) }
func NewClient(c *config.Config) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{base: c.OfficialURL, key: c.AdminKey, http: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) Configured() bool { return c.key != "" }

// request 只调用固定内部 origin，不将管理员凭据用于访问者身份验证。
func (c *Client) request(ctx context.Context, method, path string, body []byte, token, ip, ua string) (json.RawMessage, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		if c.key == "" {
			return nil, "", errors.New("未配置原版管理员 API Key")
		}
		req.Header.Set("x-api-key", c.key)
	}
	if ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
		req.Header.Set("X-Real-IP", ip)
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, "", errors.New("原版 API 请求未完成确认")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, string(raw), errors.New("原版 API 响应读取未完成")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, string(raw), &APIError{Status: res.StatusCode, Body: string(raw)}
	}
	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Code != 0 || len(envelope.Data) == 0 {
		return nil, string(raw), errors.New("原版 API 响应合同不匹配")
	}
	return envelope.Data, string(raw), nil
}
func (c *Client) VerifyAdmin(ctx context.Context, token, ip, ua string) (User, time.Time, error) {
	if token == "" {
		return User{}, time.Time{}, errors.New("缺少访问者 token")
	}
	data, _, err := c.request(ctx, http.MethodGet, "/api/v1/auth/me", nil, token, ip, ua)
	if err != nil {
		return User{}, time.Time{}, err
	}
	var user User
	if err := json.Unmarshal(data, &user); err != nil || user.ID <= 0 || user.Role != "admin" || user.Status != "active" {
		return User{}, time.Time{}, errors.New("当前访问者不是有效管理员")
	}
	// 只有原版已验证 token 后才读取过期时间，解析声明不替代签名校验。
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return User{}, time.Time{}, errors.New("原版会话格式无效")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return User{}, time.Time{}, err
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= time.Now().Unix() {
		return User{}, time.Time{}, errors.New("原版会话已过期")
	}
	return user, time.Unix(claims.Exp, 0), nil
}
func (c *Client) GetUser(ctx context.Context, id int64) (User, error) {
	data, _, err := c.request(ctx, http.MethodGet, "/api/v1/admin/users/"+strconv.FormatInt(id, 10), nil, "", "", "")
	if err != nil {
		return User{}, err
	}
	var user User
	err = json.Unmarshal(data, &user)
	if err == nil && user.ID != id {
		err = errors.New("原版用户响应身份不匹配")
	}
	return user, err
}
func (c *Client) SetUserStatus(ctx context.Context, id int64, status string) (string, error) {
	if id <= 0 || (status != "active" && status != "disabled") {
		return "", errors.New("账号状态请求无效")
	}
	body, _ := json.Marshal(map[string]string{"status": status})
	data, raw, err := c.request(ctx, http.MethodPut, "/api/v1/admin/users/"+strconv.FormatInt(id, 10), body, "", "", "")
	if err == nil {
		var user User
		if json.Unmarshal(data, &user) != nil || user.ID != id || user.Status != status {
			return raw, errors.New("原版状态更新响应尚未确认目标状态")
		}
	}
	return raw, err
}
