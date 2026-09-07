package ingress

import (
	"context"
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
	fail         bool
	saved        bool
	raw          []byte
	mu           sync.Mutex
	observations []string
}

func (s *testCaptures) Save(_ context.Context, c *audit.Capture) error {
	if s.fail {
		return errors.New("存储不可用")
	}
	s.saved = true
	s.raw = append([]byte(nil), c.Raw...)
	c.ID = 1
	return nil
}
func (s *testCaptures) Observe(_ context.Context, _ int64, state string, _ map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observations = append(s.observations, state)
	return nil
}
func (s *testCaptures) Finish(context.Context, int64, string, string) error { return nil }

type testAudit struct {
	mode     string
	decision audit.IngressDecisionKind
}

func (s testAudit) Mode() string { return s.mode }
func (s testAudit) AuditCapture(context.Context, *audit.Capture) (*audit.IntakeDecision, error) {
	return &audit.IntakeDecision{JobID: 1, Mode: s.mode, Kind: s.decision}, nil
}
func (s testAudit) ObserveGateway(context.Context, *audit.IntakeDecision, audit.IngressDecision, time.Duration) {
}

type testIdentity struct{}

func (testIdentity) Resolve(context.Context, *http.Request, string) (sub2api.Identity, error) {
	return sub2api.Identity{UserID: 1, APIKeyID: 2, Platform: "openai", Eligibility: "passed"}, nil
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
				raw, _ := io.ReadAll(r.Body)
				require.Equal(t, store.raw, raw)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"text\":\"hello\"}\n\n")
			}))
			defer upstream.Close()
			p, err := New(upstream.URL, store, testAudit{"async", audit.IngressDecisionAllow}, testIdentity{})
			require.NoError(t, err)
			body := " {\"input\":\"a  b\\n中文\",\"id\":9007199254740993} "
			req := httptest.NewRequest("POST", "http://enhance/v1/responses?stream=true", strings.NewReader(body))
			req.Header.Set("Forwarded", "for=evil")
			req.Header.Set("X-Forwarded-For", "evil")
			recorder := httptest.NewRecorder()
			p.Serve(recorder, req, "192.0.2.7")
			if fail {
				require.Equal(t, 503, recorder.Code)
				require.Zero(t, calls)
			} else {
				require.Equal(t, 200, recorder.Code)
				require.Equal(t, body, string(store.raw))
				require.Equal(t, 1, calls)
				require.Equal(t, "data: {\"text\":\"hello\"}\n\n", recorder.Body.String())
			}
		})
	}
}
func TestBlockingDecisionPreventsUpstreamSideEffect(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer upstream.Close()
	p, err := New(upstream.URL, &testCaptures{}, testAudit{"blocking", audit.IngressDecisionBlock}, testIdentity{})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	p.Serve(recorder, httptest.NewRequest("POST", "http://enhance/v1/responses", strings.NewReader(`{"input":"test"}`)), "192.0.2.7")
	require.Equal(t, 403, recorder.Code)
	require.Zero(t, calls)
}
