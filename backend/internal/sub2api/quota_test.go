package sub2api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"sub2api-enhance/internal/config"
	"sync/atomic"
	"testing"
)

func TestQuotaBatchPreservesPerAccountErrorsAndOriginalPercentage(t *testing.T) {
	original := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/admin/accounts/usage/batch", r.URL.Path)
		var req struct {
			IDs   []int64 `json:"account_ids"`
			Force bool    `json:"force"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.True(t, req.Force)
		require.Equal(t, []int64{7, 8}, req.IDs)
		fmt.Fprint(w, `{"code":0,"data":{"usage":{"7":{"seven_day":{"utilization":0.123456789,"resets_at":"2026-09-08T00:00:00Z"}}},"errors":{"8":"上游拒绝"}}}`)
	}))
	defer original.Close()
	result, err := NewClient(&config.Config{OfficialURL: original.URL, AdminKey: "unit-test"}).AccountUsageBatch(context.Background(), []int64{7, 8})
	require.NoError(t, err)
	require.Equal(t, "0.123456789", result[7].Utilization.String())
	require.Equal(t, "上游拒绝", result[8].Error)
}
func TestQuotaResetUsesExactWindowAndRequestIDWithoutRetry(t *testing.T) {
	var calls atomic.Int64
	original := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/api/v1/admin/users/7/platform-quotas/reset", r.URL.Path)
		require.Equal(t, "unit-correlation", r.Header.Get("X-Request-ID"))
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"platform":"openai","window":"weekly"}`, string(raw))
		w.WriteHeader(500)
		fmt.Fprint(w, "response unknown")
	}))
	defer original.Close()
	reply := NewClient(&config.Config{OfficialURL: original.URL, AdminKey: "unit-test"}).ResetQuota(context.Background(), 7, "weekly", "unit-correlation")
	require.Error(t, reply.Error)
	require.Equal(t, 500, reply.HTTPStatus)
	require.Equal(t, "response unknown", reply.Raw)
	require.EqualValues(t, 1, calls.Load())
}
