package ingress

import (
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebSocketCapturesMessagesAndBlocksBeforeForward(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "逐消息保存与透传", true: "协议错误且未发送生成"}[blocked], func(t *testing.T) {
			var received atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				u := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := u.Upgrade(w, r, nil)
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
			decision := audit.IngressDecisionAllow
			if blocked {
				decision = audit.IngressDecisionBlock
			}
			store := &testCaptures{}
			errorCode := ""
			if blocked {
				errorCode = "third_party_audit_blocked"
			}
			proxy, err := New(upstream.URL, store, testAudit{mode: "blocking", decision: decision, errorCode: errorCode}, testIdentity{})
			require.NoError(t, err)
			ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxy.Serve(w, r, "192.0.2.8") }))
			defer ingress.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ingress.URL, "http")+"/v1/responses", nil)
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
			raw := []byte(`{"type":"response.create","response":{"input":"a  b","sequence":9007199254740993}}`)
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, raw))
			_, body, err := conn.ReadMessage()
			require.NoError(t, err)
			if blocked {
				require.Contains(t, string(body), `"type":"error"`)
				require.Contains(t, string(body), "请调整输入后重试")
				require.Zero(t, received.Load())
			} else {
				require.Equal(t, raw, body)
				require.EqualValues(t, 1, received.Load())
			}
			require.Equal(t, raw, store.raw)
		})
	}
}
