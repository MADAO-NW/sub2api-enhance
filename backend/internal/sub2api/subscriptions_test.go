package sub2api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"sub2api-enhance/internal/config"
	"sync/atomic"
	"testing"
	"time"
)

func TestSubscriptionBatchUsesFixedIdempotencyAndPreservesPerItemResults(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/api/v1/admin/subscriptions/bulk-action", r.URL.Path)
		require.Equal(t, "stable-key", r.Header.Get("Idempotency-Key"))
		require.Equal(t, "stable-key", r.Header.Get("X-Request-ID"))
		body, _ := io.ReadAll(r.Body)
		require.JSONEq(t, `{"subscription_ids":[11,12],"action":"reset_quota","daily":true,"weekly":true,"monthly":false}`, string(body))
		fmt.Fprint(w, `{"code":0,"data":{"success_count":1,"failed_count":1,"results":[{"subscription_id":11,"success":true},{"subscription_id":12,"success":false,"error":"expired"}]}}`)
	}))
	defer server.Close()
	client := NewClient(&config.Config{OfficialURL: server.URL, AdminKey: "unit-test"})
	reply := client.ResetSubscriptions(context.Background(), []byte(`{"subscription_ids":[11,12],"action":"reset_quota","daily":true,"weekly":true,"monthly":false}`), "stable-key")
	require.NoError(t, reply.Error)
	require.Equal(t, 1, reply.Result.SuccessCount)
	require.Equal(t, "expired", reply.Result.Items[1].Error)
	require.EqualValues(t, 1, calls.Load())
	require.Equal(t, 15*time.Second, client.http.Timeout)
}
func TestSubscriptionBatchRejectsIncompleteOrDuplicateSuccessResults(t *testing.T) {
	for _, payload := range []string{`{"success_count":1,"failed_count":0,"results":[{"subscription_id":11,"success":true}]}`, `{"success_count":2,"failed_count":0,"results":[{"subscription_id":11,"success":true},{"subscription_id":11,"success":true}]}`, `{"success_count":0,"failed_count":2,"results":[{"subscription_id":11,"success":true},{"subscription_id":12,"success":true}]}`, `{"success_count":2,"failed_count":0,"results":[{"subscription_id":11},{"subscription_id":12,"success":true}]}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"code":0,"data":%s}`, payload) }))
		reply := NewClient(&config.Config{OfficialURL: server.URL, AdminKey: "unit-test"}).ResetSubscriptions(context.Background(), []byte(`{"subscription_ids":[11,12],"action":"reset_quota","weekly":true}`), "key")
		server.Close()
		require.Error(t, reply.Error)
	}
}
func TestSubscriptionResetDoesNotRetryServerErrorsOrFollowRedirects(t *testing.T) {
	for _, status := range []int{307, 500} {
		var calls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Location", "/unexpected")
			w.WriteHeader(status)
			fmt.Fprint(w, "unknown")
		}))
		reply := NewClient(&config.Config{OfficialURL: server.URL, AdminKey: "unit-test"}).ResetSubscriptions(context.Background(), []byte(`{"subscription_ids":[1],"action":"reset_quota","weekly":true}`), "key")
		server.Close()
		require.Error(t, reply.Error)
		require.EqualValues(t, 1, calls.Load())
		require.Equal(t, status, reply.HTTPStatus)
	}
}
func TestAccountQuotaUsesDedicatedReadEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/api/v1/admin/openai/accounts/7/quota", r.URL.Path)
		fmt.Fprint(w, `{"code":0,"data":{"fetched_at":1792000000,"rate_limit":{"primary_window":{"used_percent":12.123456789,"limit_window_seconds":604800,"reset_at":1792500000}}}}`)
	}))
	defer server.Close()
	q, err := NewClient(&config.Config{OfficialURL: server.URL, AdminKey: "unit-test"}).AccountQuota(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, "12.123456789", q.Windows.Primary.UsedPercent.String())
}
func TestDiscoveryUsesActiveSubscriptionsAndPreservesDecimalSnapshots(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT name .*subscription_type='subscription'").WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("OpenAI"))
	mock.ExpectQuery("SELECT a.id,a.name,a.type,a.parent_account_id").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "type", "parent"}).AddRow(1, "account", "oauth", nil))
	mock.ExpectQuery("SELECT .* FROM public.user_subscriptions .*s.expires_at>clock_timestamp.*u.role='user'.*subscription_type='subscription'").WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "group_id", "username", "daily", "weekly", "monthly", "daily_start", "weekly_start", "monthly_start", "expires_at", "observed_at"}).AddRow(99, 8, 7, "User", "0.0000000001", "123456.1234567890", "3", now, now, now, now.Add(time.Hour), now))
	mock.ExpectCommit()
	d, err := NewQuotaData(db).DiscoverSubscriptions(context.Background(), 7)
	require.NoError(t, err)
	require.Len(t, d.Subscriptions, 1)
	require.Equal(t, int64(99), d.Subscriptions[0].ID)
	require.Equal(t, "123456.1234567890", d.Subscriptions[0].WeeklyUsage)
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestSubscriptionBatchRefusesMonthlyOrOversizedRequestsBeforeNetwork(t *testing.T) {
	c := NewClient(&config.Config{OfficialURL: "http://127.0.0.1:1", AdminKey: "unit-test"})
	req := SubscriptionResetRequest{IDs: []int64{1}, Action: "reset_quota", Monthly: true, Weekly: true}
	raw, _ := json.Marshal(req)
	reply := c.ResetSubscriptions(context.Background(), raw, "key")
	require.ErrorContains(t, reply.Error, "参数无效")
	req.Monthly = false
	for i := 2; i <= 101; i++ {
		req.IDs = append(req.IDs, int64(i))
	}
	raw, _ = json.Marshal(req)
	reply = c.ResetSubscriptions(context.Background(), raw, "key")
	require.ErrorContains(t, reply.Error, "参数无效")
}
