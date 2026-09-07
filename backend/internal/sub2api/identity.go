package sub2api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

type Identity struct {
	UserID      int64     `json:"user_id"`
	APIKeyID    int64     `json:"api_key_id"`
	GroupID     *int64    `json:"group_id"`
	Username    string    `json:"username"`
	UserEmail   string    `json:"user_email"`
	APIKeyName  string    `json:"api_key_name"`
	GroupName   string    `json:"group_name"`
	Platform    string    `json:"platform"`
	Eligibility string    `json:"eligibility"`
	Reason      string    `json:"reason"`
	ResolvedAt  time.Time `json:"resolved_at"`
}
type IdentityStore struct{ db *sql.DB }

func NewIdentityStore(db *sql.DB) *IdentityStore { return &IdentityStore{db: db} }

// Resolve 只读取当前凭据对应的必要身份字段，不扫描全库密钥或持久化鉴权头。
func (s *IdentityStore) Resolve(ctx context.Context, r *http.Request, ip string) (Identity, error) {
	if r.URL.Query().Get("key") != "" || r.URL.Query().Get("api_key") != "" {
		return Identity{Eligibility: "rejected", Reason: "原版不接受查询参数中的凭据"}, nil
	}
	key := ""
	if parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		key = strings.TrimSpace(parts[1])
	}
	if key == "" {
		key = r.Header.Get("x-api-key")
	}
	if key == "" {
		key = r.Header.Get("x-goog-api-key")
	}
	if key == "" {
		return Identity{Eligibility: "rejected", Reason: "请求缺少可验证凭据"}, nil
	}
	return s.lookup(ctx, key, 0, ip)
}
func (s *IdentityStore) ByID(ctx context.Context, id int64, ip string) (Identity, error) {
	return s.lookup(ctx, "", id, ip)
}
func (s *IdentityStore) lookup(ctx context.Context, key string, id int64, ip string) (Identity, error) {
	var v Identity
	var keyStatus, userStatus, groupStatus, subscription string
	var expires *time.Time
	var whitelist, blacklist []byte
	var allowed bool
	// 用户公开分组限制、专属分组和订阅组规则均来自 main 0.2.1 的鉴权边界。
	err := s.db.QueryRowContext(ctx, `SELECT k.id,k.user_id,k.group_id,k.name,u.username,u.email,k.status,u.status,k.expires_at,
 COALESCE(g.name,''),COALESCE(g.platform,''),COALESCE(g.status,''),COALESCE(g.subscription_type,''),COALESCE(k.ip_whitelist,'[]'::jsonb),COALESCE(k.ip_blacklist,'[]'::jsonb),
 COALESCE((g.subscription_type='subscription' OR (NOT g.is_exclusive AND NOT u.restrict_public_groups) OR EXISTS(SELECT 1 FROM public.user_allowed_groups ug WHERE ug.user_id=u.id AND ug.group_id=g.id)),false)
 FROM public.api_keys k JOIN public.users u ON u.id=k.user_id AND u.deleted_at IS NULL LEFT JOIN public.groups g ON g.id=k.group_id AND g.deleted_at IS NULL
 WHERE k.deleted_at IS NULL AND (($2::bigint=0 AND k.key=$1) OR ($2::bigint>0 AND k.id=$2))`, key, id).Scan(&v.APIKeyID, &v.UserID, &v.GroupID, &v.APIKeyName, &v.Username, &v.UserEmail, &keyStatus, &userStatus, &expires, &v.GroupName, &v.Platform, &groupStatus, &subscription, &whitelist, &blacklist, &allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{Eligibility: "rejected", Reason: "凭据或用户不存在"}, nil
	}
	if err != nil {
		return Identity{Eligibility: "unknown", Reason: "原版身份查询不可用"}, err
	}
	v.ResolvedAt = time.Now().UTC()
	v.Eligibility = "rejected"
	if keyStatus != "active" || userStatus != "active" {
		v.Reason = "凭据或用户状态不允许审核"
		return v, nil
	}
	if expires != nil && !expires.After(time.Now()) {
		v.Reason = "凭据已过期"
		return v, nil
	}
	if v.GroupID == nil || groupStatus != "active" || !allowed {
		v.Reason = "绑定分组不可用或用户无访问权限"
		return v, nil
	}
	var white, black []string
	if json.Unmarshal(whitelist, &white) != nil || json.Unmarshal(blacklist, &black) != nil {
		v.Eligibility = "unknown"
		v.Reason = "原版 IP 规则格式不兼容"
		return v, nil
	}
	matches := func(rules []string) (bool, error) {
		parsed := net.ParseIP(ip)
		for _, rule := range rules {
			if one := net.ParseIP(rule); one != nil {
				if parsed != nil && one.Equal(parsed) {
					return true, nil
				}
				continue
			}
			_, network, err := net.ParseCIDR(rule)
			if err != nil {
				return false, err
			}
			if network.Contains(parsed) {
				return true, nil
			}
		}
		return false, nil
	}
	denied, e1 := matches(black)
	accepted, e2 := matches(white)
	if e1 != nil || e2 != nil {
		v.Eligibility = "unknown"
		v.Reason = "IP 规则无法安全解释"
		return v, nil
	}
	if denied || (len(white) > 0 && !accepted) {
		v.Reason = "请求未通过原版 IP 范围校验"
		return v, nil
	}
	if v.Platform == "composite" {
		v.Eligibility = "unknown"
		v.Reason = "动态复合分组没有已确认的最终平台来源"
		return v, nil
	}
	v.Eligibility = "passed"
	v.Reason = "前置身份与绑定分组资格通过，计费和最终鉴权仍由原版执行"
	return v, nil
}
func (s *IdentityStore) Groups(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,platform FROM public.groups WHERE deleted_at IS NULL AND status='active' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, platform string
		if err := rows.Scan(&id, &name, &platform); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "name": name, "platform": platform})
	}
	return out, rows.Err()
}
