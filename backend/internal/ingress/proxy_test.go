package ingress

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
)

// fakeAuditor 记录入口交给审核服务的解析结果与决定，不接触数据库。
type fakeAuditor struct {
	mode     string
	userMode string
	decision *audit.IntakeDecision
	mu       sync.Mutex
	parsed   []*audit.ParsedRequest
	requests []audit.IntakeRequest
	mappings map[string]string
	rejected []string
}

func (a *fakeAuditor) Mode() string { return a.mode }

func (a *fakeAuditor) ModeForUser(int64) string {
	if a.userMode != "" {
		return a.userMode
	}
	return a.mode
}

func (a *fakeAuditor) Check(_ context.Context, request audit.IntakeRequest) *audit.IntakeDecision {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.parsed = append(a.parsed, request.Parsed)
	a.requests = append(a.requests, request)
	if a.decision != nil {
		copied := *a.decision
		return &copied
	}
	return &audit.IntakeDecision{JobID: 1, Mode: a.mode, Kind: audit.IngressDecisionAllow, TurnKey: "turn-key"}
}

func (a *fakeAuditor) RegisterResponseID(_ context.Context, responseID, turnKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mappings == nil {
		a.mappings = map[string]string{}
	}
	a.mappings[responseID] = turnKey
}

func (a *fakeAuditor) RejectContinuation(decision *audit.IntakeDecision, _ int64, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rejected = append(a.rejected, reason)
}

func (a *fakeAuditor) mapping(responseID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mappings[responseID]
}

func (a *fakeAuditor) lastParsed() *audit.ParsedRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.parsed) == 0 {
		return nil
	}
	return a.parsed[len(a.parsed)-1]
}

type fakeIdentity struct {
	err error
}

func (i fakeIdentity) Resolve(context.Context, *http.Request, string) (sub2api.Identity, error) {
	return sub2api.Identity{UserID: 7, APIKeyID: 9, Platform: "openai", Eligibility: "passed"}, i.err
}

func newUpstream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func serve(proxy *Proxy, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://enhance"+target, body)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	proxy.Serve(recorder, request, "192.0.2.7")
	return recorder
}

func TestBlockingDecisionPreventsUpstream(t *testing.T) {
	var calls int
	upstream := newUpstream(t, func(http.ResponseWriter, *http.Request) { calls++ })
	auditor := &fakeAuditor{mode: "blocking", decision: &audit.IntakeDecision{Mode: "blocking", Kind: audit.IngressDecisionBlock, ErrorCode: "third_party_audit_blocked"}}
	proxy, err := New(upstream.URL, auditor, fakeIdentity{})
	require.NoError(t, err)
	recorder := serve(proxy, "/v1/responses", strings.NewReader(`{"input":"test"}`), nil)
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Zero(t, calls)
	require.Equal(t, []string{"third_party_audit_blocked"}, auditor.rejected)
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "third_party_audit_blocked", body.Error.Code)
}

func TestAsyncUnavailableStillForwards(t *testing.T) {
	var calls int
	upstream := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	})
	auditor := &fakeAuditor{mode: "async", decision: &audit.IntakeDecision{Mode: "async", Kind: audit.IngressDecisionUnavailable, ErrorCode: "third_party_audit_unavailable"}}
	proxy, err := New(upstream.URL, auditor, fakeIdentity{})
	require.NoError(t, err)
	recorder := serve(proxy, "/v1/responses", strings.NewReader(`{"input":"test"}`), nil)
	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Equal(t, 1, calls)
}

func TestIdentityFailureRespectsPerUserMode(t *testing.T) {
	for _, test := range []struct {
		mode   string
		status int
		calls  int
	}{{"blocking", http.StatusServiceUnavailable, 0}, {"async", http.StatusNoContent, 1}} {
		t.Run(test.mode, func(t *testing.T) {
			calls := 0
			upstream := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusNoContent)
			})
			auditor := &fakeAuditor{mode: "async", userMode: test.mode}
			proxy, err := New(upstream.URL, auditor, fakeIdentity{err: context.DeadlineExceeded})
			require.NoError(t, err)
			recorder := serve(proxy, "/v1/responses", strings.NewReader(`{"input":"test"}`), nil)
			require.Equal(t, test.status, recorder.Code)
			require.Equal(t, test.calls, calls)
		})
	}
}

// 压缩编码按流解压后转发，解压内容仍参与审核解析。
func TestEncodedRequestIsDecompressedAndParsed(t *testing.T) {
	var received string
	upstream := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		received = string(raw)
		w.WriteHeader(http.StatusNoContent)
	})
	payload := []byte(`{"model":"gpt-test","input":"压缩后的正常请求"}`)
	var encoded bytes.Buffer
	gzipWriter := gzip.NewWriter(&encoded)
	_, err := gzipWriter.Write(payload)
	require.NoError(t, err)
	require.NoError(t, gzipWriter.Close())
	auditor := &fakeAuditor{mode: "async"}
	proxy, err := New(upstream.URL, auditor, fakeIdentity{})
	require.NoError(t, err)
	recorder := serve(proxy, "/v1/responses", bytes.NewReader(encoded.Bytes()), map[string]string{"Content-Encoding": "gzip"})
	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Equal(t, string(payload), received)
	require.NotNil(t, auditor.lastParsed())
	require.Equal(t, "压缩后的正常请求", auditor.lastParsed().UserPrompt.Text)
}

// 解压后超过 64MB 返回 413 且不转发（决策 18）。
func TestCompressionBombReturns413(t *testing.T) {
	var calls int
	upstream := newUpstream(t, func(http.ResponseWriter, *http.Request) { calls++ })
	var encoded bytes.Buffer
	gzipWriter := gzip.NewWriter(&encoded)
	chunk := make([]byte, 1<<20)
	for written := 0; written <= maxDecompressedBodySize; written += len(chunk) {
		_, err := gzipWriter.Write(chunk)
		require.NoError(t, err)
	}
	require.NoError(t, gzipWriter.Close())
	auditor := &fakeAuditor{mode: "blocking"}
	proxy, err := New(upstream.URL, auditor, fakeIdentity{})
	require.NoError(t, err)
	recorder := serve(proxy, "/v1/responses", bytes.NewReader(encoded.Bytes()), map[string]string{"Content-Encoding": "gzip"})
	require.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
	require.Zero(t, calls)
	require.Nil(t, auditor.lastParsed())
}

// zstd 编码同样支持，且把原始编码头从转发请求里移除。
func TestZstdEncodingForwardsPlainBody(t *testing.T) {
	var encoding string
	var received string
	upstream := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		encoding = r.Header.Get("Content-Encoding")
		raw, _ := io.ReadAll(r.Body)
		received = string(raw)
		w.WriteHeader(http.StatusNoContent)
	})
	var encoded bytes.Buffer
	encoder, err := zstd.NewWriter(&encoded)
	require.NoError(t, err)
	_, err = encoder.Write([]byte(`{"model":"gpt-test","input":"zstd 请求"}`))
	require.NoError(t, err)
	require.NoError(t, encoder.Close())
	proxy, err := New(upstream.URL, &fakeAuditor{mode: "async"}, fakeIdentity{})
	require.NoError(t, err)
	recorder := serve(proxy, "/v1/responses", bytes.NewReader(encoded.Bytes()), map[string]string{"Content-Encoding": "zstd"})
	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Empty(t, encoding)
	require.Contains(t, received, "zstd 请求")
}

func TestNonStreamResponseIDIsRegistered(t *testing.T) {
	upstream := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-1","output":[]}`)
	})
	auditor := &fakeAuditor{mode: "async"}
	proxy, err := New(upstream.URL, auditor, fakeIdentity{})
	require.NoError(t, err)
	recorder := serve(proxy, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`), nil)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "turn-key", auditor.mapping("resp-1"))
}

func TestStreamedResponseCreatedIDIsRegistered(t *testing.T) {
	upstream := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stream\"}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	})
	auditor := &fakeAuditor{mode: "async"}
	proxy, err := New(upstream.URL, auditor, fakeIdentity{})
	require.NoError(t, err)
	recorder := serve(proxy, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello","stream":true}`), nil)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "turn-key", auditor.mapping("resp-stream"))
}

// 孤儿续跑仍按首次请求审核：入口必须把续跑标记和历史中的用户提示词交给审核服务。
func TestOrphanContinuationIsStillAudited(t *testing.T) {
	upstream := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	auditor := &fakeAuditor{mode: "async"}
	proxy, err := New(upstream.URL, auditor, fakeIdentity{})
	require.NoError(t, err)
	body := `{"model":"gpt-test","input":[{"role":"user","content":"请修复这段代码"},{"role":"assistant","content":"好的"},{"type":"function_call_output","call_id":"c1","output":"done"}]}`
	recorder := serve(proxy, "/v1/responses", strings.NewReader(body), nil)
	require.Equal(t, http.StatusNoContent, recorder.Code)
	parsed := auditor.lastParsed()
	require.NotNil(t, parsed)
	require.True(t, parsed.Continuation)
	require.NotNil(t, parsed.UserPrompt)
	require.Equal(t, "请修复这段代码", parsed.UserPrompt.Text)
}

func TestInboundHeadersAreStripped(t *testing.T) {
	var headers http.Header
	upstream := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	})
	proxy, err := New(upstream.URL, &fakeAuditor{mode: "async"}, fakeIdentity{})
	require.NoError(t, err)
	recorder := serve(proxy, "/v1/responses", strings.NewReader(`{"input":"x"}`), map[string]string{
		"X-Enhance-Capture": "1", "X-Enhance-Conversation-ID": "conversation-a", "X-Forwarded-For": "evil",
	})
	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Empty(t, headers.Get("X-Enhance-Capture"))
	require.Empty(t, headers.Get("X-Enhance-Conversation-ID"))
	require.Equal(t, "192.0.2.7", headers.Get("X-Forwarded-For"))
}

// BenchmarkLargeRequestParse 记录 8MB 请求体的解析分配次数（阶段 4 的内存观测）。
func BenchmarkLargeRequestParse(b *testing.B) {
	messages := make([]map[string]string, 0, 4000)
	for index := 0; index < 4000; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, map[string]string{"role": role, "content": strings.Repeat("长对话内容", 40)})
	}
	body, err := json.Marshal(map[string]any{"model": "gpt-test", "messages": messages})
	require.NoError(b, err)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		parsed, err := audit.ParsePrompt("chat_completions", bytes.NewReader(body), audit.ClientGeneric)
		if err != nil {
			b.Fatal(err)
		}
		if parsed.UserPrompt == nil {
			b.Fatal("缺少用户提示词目标")
		}
	}
	_ = time.Now
}
