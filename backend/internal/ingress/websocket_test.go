package ingress

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
)

func dialIngress(t *testing.T, auditor *fakeAuditor, upstreamURL string) *websocket.Conn {
	t.Helper()
	proxy, err := New(upstreamURL, auditor, fakeIdentity{})
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxy.Serve(w, r, "192.0.2.8") }))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	return conn
}

func TestWebSocketBlocksBeforeForward(t *testing.T) {
	var received atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			kind, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			received.Add(1)
			if err := conn.WriteMessage(kind, body); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	auditor := &fakeAuditor{mode: "blocking", decision: &audit.IntakeDecision{Mode: "blocking", Kind: audit.IngressDecisionBlock, ErrorCode: "third_party_audit_blocked"}}
	conn := dialIngress(t, auditor, upstream.URL)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":"违规请求"}}`)))
	_, body, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Contains(t, string(body), `"type":"error"`)
	require.Zero(t, received.Load())
	require.NotEmpty(t, auditor.rejected)
}

func TestWebSocketRegistersResponseIDAndForwards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp-ws"}}`))
	}))
	defer upstream.Close()
	auditor := &fakeAuditor{mode: "async"}
	conn := dialIngress(t, auditor, upstream.URL)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":"正常问题"}}`)))
	_, body, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Contains(t, string(body), "resp-ws")
	require.Equal(t, "turn-key", auditor.mapping("resp-ws"))
}

func TestWebSocketGlobalOffSkipsAudit(t *testing.T) {
	var received atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		kind, body, err := conn.ReadMessage()
		if err == nil {
			received.Add(1)
			_ = conn.WriteMessage(kind, body)
		}
	}))
	defer upstream.Close()
	auditor := &fakeAuditor{mode: "off"}
	conn := dialIngress(t, auditor, upstream.URL)
	raw := []byte(`{"type":"response.create","response":{"input":"全局关闭"}}`)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, raw))
	_, body, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, raw, body)
	require.EqualValues(t, 1, received.Load())
	require.Nil(t, auditor.lastParsed())
}
