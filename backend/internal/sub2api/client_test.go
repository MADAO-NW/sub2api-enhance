package sub2api

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"sub2api-enhance/internal/config"
	"testing"
	"time"
)

func TestAdminVerificationUsesVisitorsTokenAndRejectsRoleDowngrade(t *testing.T) {
	role := "admin"
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix()))) + ".signature"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/auth/me", r.URL.Path)
		require.Equal(t, "Bearer "+token, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("x-api-key"))
		require.Equal(t, "192.0.2.8", r.Header.Get("X-Forwarded-For"))
		fmt.Fprintf(w, `{"code":0,"data":{"id":1,"role":%q,"status":"active"}}`, role)
	}))
	defer upstream.Close()
	client := NewClient(&config.Config{OfficialURL: upstream.URL, AdminKey: "backend-test-only"})
	_, _, err := client.VerifyAdmin(context.Background(), token, "192.0.2.8", "test-agent")
	require.NoError(t, err)
	role = "user"
	_, _, err = client.VerifyAdmin(context.Background(), token, "192.0.2.8", "test-agent")
	require.Error(t, err)
}
func TestAccountUpdateDoesNotFollowRedirectOrSendOtherFields(t *testing.T) {
	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked = true }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "PUT", r.Method)
		require.Equal(t, "/api/v1/admin/users/7", r.URL.Path)
		buffer := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buffer)
		require.Equal(t, `{"status":"disabled"}`, strings.TrimSpace(string(buffer)))
		http.Redirect(w, r, other.URL, 307)
	}))
	defer server.Close()
	client := NewClient(&config.Config{OfficialURL: server.URL, AdminKey: "test-only"})
	_, err := client.SetUserStatus(context.Background(), 7, "disabled")
	require.Error(t, err)
	require.False(t, leaked)
}

func TestAPIErrorDescribesEndpointAndUpstreamMessage(t *testing.T) {
	err := &APIError{Status: 404, Body: `{"code":404,"message":"账户不存在"}`, Method: "GET", Path: "/api/v1/admin/openai/accounts/2/quota"}
	require.Equal(t, "原版 API 返回 HTTP 404（GET /api/v1/admin/openai/accounts/2/quota）：账户不存在", err.Error())
	require.Equal(t, "账户不存在", err.Detail())
	require.Empty(t, err.Reason())

	plain := &APIError{Status: 500, Body: "not json", Method: "POST", Path: "/api/v1/admin/subscriptions/bulk-action"}
	require.Equal(t, "原版 API 返回 HTTP 500（POST /api/v1/admin/subscriptions/bulk-action）", plain.Error())
	require.Empty(t, plain.Detail())

	reasoned := &APIError{Status: 502, Body: `{"message":"上游失败","reason":"OPENAI_QUOTA_UPSTREAM_ERROR"}`}
	require.Equal(t, "OPENAI_QUOTA_UPSTREAM_ERROR", reasoned.Reason())
	require.Equal(t, "上游失败 / OPENAI_QUOTA_UPSTREAM_ERROR", reasoned.Detail())

	truncated := &APIError{Status: 400, Body: `{"message":"` + strings.Repeat("长", 300) + `"}`}
	require.LessOrEqual(t, len([]rune(truncated.Detail())), 201)
	require.True(t, strings.HasSuffix(truncated.Detail(), "…"))
}
