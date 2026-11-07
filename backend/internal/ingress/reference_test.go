package ingress

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
)

// lastRequestID 返回入口最近一次交给审核服务的请求 ID。
func (a *fakeAuditor) lastRequestID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.requests) == 0 {
		return ""
	}
	return a.requests[len(a.requests)-1].RequestID
}

// referenceEnvelopes 覆盖三种协议信封的 403 正文，确认都带参考编号。
func TestBlockedResponsesCarryReferenceForEveryProtocol(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{"openai", "/v1/responses", `{"model":"gpt-4o","input":"hello"}`},
		{"anthropic", "/v1/messages", `{"model":"claude-3","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`},
		{"gemini", "/v1beta/models/gemini-pro:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			upstream := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
			auditor := &fakeAuditor{mode: "blocking", decision: &audit.IntakeDecision{JobID: 42, Mode: "blocking",
				Kind: audit.IngressDecisionBlock, ErrorCode: "third_party_audit_blocked"}}
			proxy, err := New(upstream.URL, auditor, fakeIdentity{})
			require.NoError(t, err)
			recorder := serve(proxy, test.path, strings.NewReader(test.body), nil)
			require.Equal(t, http.StatusForbidden, recorder.Code)
			message := errorMessage(t, recorder.Body.Bytes())
			require.Contains(t, message, "参考编号：#42")
			require.NotContains(t, message, "policy")
			require.NotEmpty(t, recorder.Header().Get("X-Enhance-Request-Id"))
		})
	}
}

// 不可用（503）且没有任务时用请求 ID 作为参考编号，并回写响应头。
func TestUnavailableResponseWithoutJobUsesRequestID(t *testing.T) {
	upstream := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	auditor := &fakeAuditor{mode: "blocking", userMode: "blocking"}
	proxy, err := New(upstream.URL, auditor, fakeIdentity{err: context.DeadlineExceeded})
	require.NoError(t, err)
	recorder := serve(proxy, "/v1/responses", strings.NewReader(`{"input":"hello"}`), nil)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	requestID := recorder.Header().Get("X-Enhance-Request-Id")
	require.NotEmpty(t, requestID)
	require.Contains(t, errorMessage(t, recorder.Body.Bytes()), "参考编号："+requestID)
}

// 客户端自带请求 ID 时也回写同一个值，便于对账。
func TestClientRequestIDIsEchoed(t *testing.T) {
	upstream := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	auditor := &fakeAuditor{mode: "async"}
	proxy, err := New(upstream.URL, auditor, fakeIdentity{})
	require.NoError(t, err)
	recorder := serve(proxy, "/v1/responses", strings.NewReader(`{"input":"hello"}`), map[string]string{"X-Request-Id": "client-request-1"})
	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Equal(t, "client-request-1", recorder.Header().Get("X-Enhance-Request-Id"))
	require.Equal(t, "client-request-1", auditor.lastRequestID())
}

// errorMessage 按协议信封取出 message 字段。
func errorMessage(t *testing.T, raw []byte) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	return envelope.Error.Message
}
