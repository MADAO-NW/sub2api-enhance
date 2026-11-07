package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"sub2api-enhance/internal/sub2api"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
)

// websocket 只透传 Responses 的 WebSocket 会话，逐条审核 response.create 并登记响应 ID。
func (p *Proxy) websocket(w http.ResponseWriter, r *http.Request, state *requestContext) {
	if protocol(r.URL.Path) != "responses" {
		writeError(w, "responses", http.StatusServiceUnavailable, "unsupported_websocket", auditDeniedMessage(audit.IngressDecisionUnavailable, state.requestID))
		return
	}
	identity, err := p.identity.Resolve(r.Context(), r, state.clientIP)
	if err != nil && p.audit.ModeForUser(identity.UserID) == "blocking" {
		writeError(w, "responses", http.StatusServiceUnavailable, "eligibility_unknown", auditDeniedMessage(audit.IngressDecisionUnavailable, state.requestID))
		return
	}
	u := *p.upstream
	u.Path = r.URL.Path
	u.RawQuery = r.URL.RawQuery
	u.Scheme = "ws"
	if p.upstream.Scheme == "https" {
		u.Scheme = "wss"
	}
	headers := r.Header.Clone()
	for _, key := range []string{"Connection", "Upgrade", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol",
		"Forwarded", "X-Forwarded-For", "X-Real-IP", "X-Enhance-Capture", "X-Enhance-Conversation-ID"} {
		headers.Del(key)
	}
	headers.Set("X-Forwarded-For", state.clientIP)
	headers.Set("X-Real-IP", state.clientIP)
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Subprotocols: websocket.Subprotocols(r), EnableCompression: true, Proxy: nil}
	upstream, response, err := dialer.DialContext(r.Context(), u.String(), headers)
	if err != nil {
		status := http.StatusBadGateway
		if response != nil {
			status = response.StatusCode
			_ = response.Body.Close()
		}
		writeError(w, "responses", status, "upstream_unavailable", "原版 WebSocket 未连接")
		return
	}
	defer upstream.Close()
	upgrader := websocket.Upgrader{EnableCompression: true, Subprotocols: []string{upstream.Subprotocol()}, CheckOrigin: func(_ *http.Request) bool { return true }}
	// Origin 已原样交给原版握手验证，增强端不扩大原版允许的客户端范围。
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var writes sync.Mutex
	stop := context.AfterFunc(ctx, func() { _ = upstream.Close(); _ = client.Close() })
	defer stop()
	for _, pair := range [][2]*websocket.Conn{{client, upstream}, {upstream, client}} {
		source, dest := pair[0], pair[1]
		source.SetPingHandler(func(data string) error {
			return dest.WriteControl(websocket.PingMessage, []byte(data), time.Now().Add(5*time.Second))
		})
		source.SetPongHandler(func(data string) error {
			return dest.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
		})
		source.SetCloseHandler(func(code int, text string) error {
			return dest.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(5*time.Second))
		})
	}
	turnKeys := make(chan string, 8)
	reader := make(chan struct{})
	go func() {
		defer close(reader)
		defer cancel()
		for {
			kind, body, err := upstream.ReadMessage()
			if err != nil {
				return
			}
			select {
			case turnKey := <-turnKeys:
				if id := responseIDFromEvent(body); id != "" {
					p.audit.RegisterResponseID(ctx, id, turnKey)
				} else {
					// 事件顺序变化时放回，等下一个事件确认映射。
					select {
					case turnKeys <- turnKey:
					default:
					}
				}
			default:
			}
			writes.Lock()
			err = client.WriteMessage(kind, body)
			writes.Unlock()
			if err != nil {
				return
			}
		}
	}()
	defer func() { cancel(); <-reader }()
	sendError := func(code, message string) {
		writes.Lock()
		defer writes.Unlock()
		_ = client.WriteJSON(map[string]any{"type": "error", "error": map[string]string{"type": "enhance_error", "code": code, "message": message}})
	}
	// 每连接令牌桶：上限 40，按每秒 20 条回填，防止单连接把审核放大成风暴。
	const wsBurst, wsRatePerSecond = 40.0, 20.0
	tokens, lastRefill := wsBurst, time.Now()
	for {
		kind, raw, err := client.ReadMessage()
		if err != nil {
			return
		}
		if elapsed := time.Since(lastRefill).Seconds(); elapsed > 0 {
			tokens = min(wsBurst, tokens+elapsed*wsRatePerSecond)
			lastRefill = time.Now()
		}
		if tokens < 1 {
			sendError("rate_limited", "WebSocket 消息过于频繁，请降低频率后重试")
			continue
		}
		tokens--
		if kind != websocket.TextMessage {
			sendError("unsupported_input", "Responses WebSocket 仅支持文本业务消息")
			return
		}
		if eventType(raw) == "response.create" {
			decision := p.auditMessage(ctx, r, identity, raw, state.requestID)
			if decision != nil && decision.Kind != audit.IngressDecisionAllow && decision.Kind != audit.IngressDecisionFlag &&
				!(decision.Mode == "async" && decision.Kind == audit.IngressDecisionUnavailable) {
				p.audit.RejectContinuation(decision, identity.APIKeyID, decision.ErrorCode)
				sendError(decision.ErrorCode, auditDeniedMessage(decision.Kind, decisionReference(decision, state.requestID)))
				continue
			}
			if decision != nil && decision.TurnKey != "" {
				select {
				case turnKeys <- decision.TurnKey:
				default:
				}
			}
		}
		if err := upstream.WriteMessage(kind, raw); err != nil {
			return
		}
	}
}

// auditMessage 把一条 WebSocket 业务消息按 Responses 协议解析后交给审核服务。
func (p *Proxy) auditMessage(ctx context.Context, r *http.Request, identity sub2api.Identity, raw []byte, requestID string) *audit.IntakeDecision {
	parsed, err := audit.ParsePrompt("responses_websocket", bytes.NewReader(raw), audit.DetectClient(r.Header))
	if err != nil {
		if strings.Contains(err.Error(), "不是可审核的生成请求") {
			return nil
		}
		if p.audit.ModeForUser(identity.UserID) == "blocking" {
			return &audit.IntakeDecision{Mode: "blocking", Kind: audit.IngressDecisionUnavailable, ErrorCode: "input_parse_failed"}
		}
		return nil
	}
	return p.audit.Check(ctx, audit.IntakeRequest{
		Parsed: parsed, UserID: identity.UserID, APIKeyID: identity.APIKeyID, GroupID: identity.GroupID,
		Identity: audit.Identity{Username: identity.Username, UserEmail: identity.UserEmail, APIKeyName: identity.APIKeyName, GroupName: identity.GroupName, Endpoint: r.URL.Path},
		Provider: identity.Platform, Model: parsed.Model, RequestID: requestID,
	})
}

// eventType 只读取 WebSocket 事件的类型字段。
func eventType(raw []byte) string {
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return ""
	}
	return event.Type
}
