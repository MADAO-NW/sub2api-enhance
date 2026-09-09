package thirdpartypromptaudit

import (
	"context"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSemanticUpstreamFailuresDoNotTriggerFormatRepair(t *testing.T) {
	for _, test := range []struct{ name, body, code string }{
		{"business", `{"error":{"code":"quota_exceeded","message":"额度不足"}}`, "upstream_api_error"},
		{"refusal", `{"choices":[{"message":{"content":null,"refusal":"无法审核"}}]}`, "upstream_refused"},
		{"filter", `{"choices":[{"message":{"content":""},"finish_reason":"content_filter"}]}`, "upstream_refused"},
		{"length", `{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`, "upstream_incomplete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; _, _ = w.Write([]byte(test.body)) }))
			defer server.Close()
			store := &memoryAuditStore{}
			model := ModelConfig{BaseURL: server.URL, TimeoutMS: 1000}
			client, url, err := nodeHTTPClient(model)
			require.NoError(t, err)
			defer client.CloseIdleConnections()
			c := &ModelClient{attempts: store}
			score, _, failure := c.EvaluateTarget(context.Background(), nil, model, "", ConfigSnapshot{}, client, url, "probe", map[string]string{"text": "test"}, nil)
			require.Nil(t, score)
			require.NotNil(t, failure)
			require.Equal(t, test.code, failure.Code)
			require.Equal(t, 1, calls)
			require.Equal(t, test.body, *store.attempts[0].RawResponse)
		})
	}
}

func TestProtocolFailuresReceiveOneFormatRepair(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"empty", ""},
		{"html", "<html>error</html>"},
		{"missing", `{"choices":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; _, _ = w.Write([]byte(test.body)) }))
			defer server.Close()
			store := &memoryAuditStore{}
			model := ModelConfig{BaseURL: server.URL, TimeoutMS: 1000}
			client, url, err := nodeHTTPClient(model)
			require.NoError(t, err)
			defer client.CloseIdleConnections()
			_, _, failure := (&ModelClient{attempts: store}).EvaluateTarget(context.Background(), nil, model, "", ConfigSnapshot{}, client, url, "probe", nil, nil)
			require.Equal(t, "upstream_protocol_error", failure.Code)
			require.Equal(t, 2, calls)
			require.Equal(t, "format_repair", store.attempts[1].Stage)
			require.Equal(t, store.attempts[0].ID, *store.attempts[1].RepairOfAttemptID)
		})
	}
}

func TestOnlyScoreFormatFailureIsRepaired(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"not JSON"}}]}`))
		} else {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"confidence\":0.9,\"reason\":\"test\"}"}}]}`))
		}
	}))
	defer server.Close()
	store := &memoryAuditStore{}
	model := ModelConfig{BaseURL: server.URL, TimeoutMS: 1000}
	client, url, err := nodeHTTPClient(model)
	require.NoError(t, err)
	defer client.CloseIdleConnections()
	c := &ModelClient{attempts: store}
	score, _, failure := c.EvaluateTarget(context.Background(), nil, model, "", ConfigSnapshot{}, client, url, "probe", nil, nil)
	require.Nil(t, failure)
	require.Equal(t, 0.9, score.Confidence)
	require.Equal(t, 2, calls)
	require.Equal(t, store.attempts[0].ID, *store.attempts[1].RepairOfAttemptID)
}
