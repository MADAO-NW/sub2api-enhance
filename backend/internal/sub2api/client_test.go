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
