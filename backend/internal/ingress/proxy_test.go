package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sub2api-enhance/internal/sub2api"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
	"sync"
	"testing"
	"time"
)

type testCaptures struct {
	fail            bool
	saved           bool
	raw             []byte
	metadata        map[string]string
	conversationKey string
	mu              sync.Mutex
	observations    []string
	finishedStatus  string
}

func (s *testCaptures) Save(_ context.Context, c *audit.Capture) error {
	if s.fail {
		return errors.New("存储不可用")
	}
	s.saved = true
	s.raw = append([]byte(nil), c.Raw...)
	s.metadata = c.Metadata
	s.conversationKey = c.ConversationKey
	c.ID = 1
	return nil
}
func (s *testCaptures) Observe(_ context.Context, _ int64, state string, _ map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observations = append(s.observations, state)
	return nil
}
func (s *testCaptures) Finish(_ context.Context, _ int64, status, _ string) error {
	s.finishedStatus = status
	return nil
}

type testAudit struct {
	mode           string
	userMode       string
	decision       audit.IngressDecisionKind
	errorCode      string
	captureWhenOff bool
}

func (s testAudit) Mode() string { return s.mode }
func (s testAudit) ModeForUser(int64) string {
	if s.userMode != "" {
		return s.userMode
	}
	return s.mode
}
func (s testAudit) CaptureWhenAuditOff() bool { return s.captureWhenOff }
func (s testAudit) RequiresAudit(_ int64, _ *int64, _ string) bool {
	return s.ModeForUser(0) != "off"
}
func (s testAudit) AuditCapture(context.Context, *audit.Capture) (*audit.IntakeDecision, error) {
	return &audit.IntakeDecision{JobID: 1, Mode: s.mode, Kind: s.decision, ErrorCode: s.errorCode}, nil
}
func (s testAudit) ObserveGateway(context.Context, *audit.IntakeDecision, audit.IngressDecision, time.Duration) {
}

type testIdentity struct{}

func (testIdentity) Resolve(context.Context, *http.Request, string) (sub2api.Identity, error) {
	return sub2api.Identity{UserID: 1, APIKeyID: 2, Platform: "openai", Eligibility: "passed"}, nil
}

type unknownIdentity struct{}

func (unknownIdentity) Resolve(context.Context, *http.Request, string) (sub2api.Identity, error) {
	return sub2api.Identity{UserID: 7, APIKeyID: 2, Platform: "openai", Eligibility: "unknown"}, nil
}

func TestPerUserModeControlsPreAuditFailureHandling(t *testing.T) {
	for _, test := range []struct {
		userMode string
		status   int
		calls    int
	}{{"blocking", http.StatusServiceUnavailable, 0}, {"async", http.StatusNoContent, 1}} {
		t.Run(test.userMode, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			store := &testCaptures{}
			proxy, err := New(upstream.URL, store, testAudit{mode: "async", userMode: test.userMode, decision: audit.IngressDecisionAllow}, unknownIdentity{})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			proxy.Serve(recorder, httptest.NewRequest(http.MethodPost, "http://enhance/v1/responses", strings.NewReader(`{"input":"test"}`)), "192.0.2.7")
			require.Equal(t, test.status, recorder.Code)
			require.Equal(t, test.calls, calls)
			require.Equal(t, test.userMode, store.metadata["mode"])
		})
	}
}

func TestProtectedInputMustPersistBeforeForward(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "保存后透传", true: "保存失败不转发"}[fail], func(t *testing.T) {
			store := &testCaptures{fail: fail}
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.True(t, store.saved)
				require.Equal(t, "192.0.2.7", r.Header.Get("X-Forwarded-For"))
				require.Empty(t, r.Header.Get("Forwarded"))
				require.Empty(t, r.Header.Get("X-Enhance-Conversation-ID"))
				raw, _ := io.ReadAll(r.Body)
				require.Equal(t, store.raw, raw)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"text\":\"hello\"}\n\n")
			}))
			defer upstream.Close()
			p, err := New(upstream.URL, store, testAudit{mode: "async", decision: audit.IngressDecisionAllow}, testIdentity{})
			require.NoError(t, err)
			body := " {\"input\":\"a  b\\n中文\",\"id\":9007199254740993} "
			req := httptest.NewRequest("POST", "http://enhance/v1/responses?stream=true", strings.NewReader(body))
			req.Header.Set("Forwarded", "for=evil")
			req.Header.Set("X-Forwarded-For", "evil")
			req.Header.Set("X-Enhance-Conversation-ID", "conversation-a")
			recorder := httptest.NewRecorder()
			p.Serve(recorder, req, "192.0.2.7")
			if fail {
				require.Equal(t, 503, recorder.Code)
				require.Zero(t, calls)
			} else {
				require.Equal(t, 200, recorder.Code)
				require.Equal(t, body, string(store.raw))
				require.Equal(t, "conversation-a", store.conversationKey)
				require.Equal(t, 1, calls)
				require.Equal(t, "data: {\"text\":\"hello\"}\n\n", recorder.Body.String())
			}
		})
	}
}

func TestCaptureOnlyModeForwardsEvenWhenPersistenceFails(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "保存后等待人工审核", true: "保存失败仍转发"}[fail], func(t *testing.T) {
			store := &testCaptures{fail: fail}
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			proxy, err := New(upstream.URL, store, testAudit{mode: "off", captureWhenOff: true}, testIdentity{})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			proxy.Serve(recorder, httptest.NewRequest(http.MethodPost, "http://enhance/v1/responses", strings.NewReader(`{"input":"test"}`)), "192.0.2.7")
			require.Equal(t, http.StatusNoContent, recorder.Code)
			require.Equal(t, 1, calls)
			if fail {
				require.False(t, store.saved)
			} else {
				require.True(t, store.saved)
				require.Equal(t, "awaiting_review", store.finishedStatus)
			}
		})
	}
}
func TestBlockingDecisionPreventsUpstreamSideEffect(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer upstream.Close()
	p, err := New(upstream.URL, &testCaptures{}, testAudit{mode: "blocking", decision: audit.IngressDecisionBlock, errorCode: "third_party_audit_blocked"}, testIdentity{})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	p.Serve(recorder, httptest.NewRequest("POST", "http://enhance/v1/responses", strings.NewReader(`{"input":"test"}`)), "192.0.2.7")
	require.Equal(t, 403, recorder.Code)
	require.Zero(t, calls)
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "third_party_audit_blocked", body.Error.Code)
	require.Equal(t, "enhance_error", body.Error.Type)
	require.Equal(t, "请求内容未通过第三方模型提示词审核，增强服务已阻止转发。请调整输入后重试，或联系管理员复核。", body.Error.Message)
}
