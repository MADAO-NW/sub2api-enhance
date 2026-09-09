package ingress

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"log"
	"net/http"
	"strconv"
	"strings"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
	"sync"
	"time"
)

func (p *Proxy) websocket(w http.ResponseWriter, r *http.Request, state *requestContext) {
	if protocol(r.URL.Path) != "responses" {
		writeError(w, "responses", 503, "unsupported_websocket", "该 WebSocket 协议尚未通过采集适配")
		return
	}
	identity, err := p.identity.Resolve(r.Context(), r, state.clientIP)
	if err != nil && p.audit.ModeForUser(identity.UserID) == "blocking" {
		writeError(w, "responses", 503, "identity_unavailable", "身份暂不可验证")
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
	for _, key := range []string{"Connection", "Upgrade", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol", "Forwarded", "X-Forwarded-For", "X-Real-IP", "X-Enhance-Capture", "X-Enhance-Conversation-ID"} {
		headers.Del(key)
	}
	headers.Set("X-Forwarded-For", state.clientIP)
	headers.Set("X-Real-IP", state.clientIP)
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Subprotocols: websocket.Subprotocols(r), EnableCompression: true, Proxy: nil}
	upstream, response, err := dialer.DialContext(r.Context(), u.String(), headers)
	if err != nil {
		status := 502
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
	connectionKey := uuid.NewString()
	var sequence int64
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		for {
			kind, body, err := upstream.ReadMessage()
			if err != nil {
				return
			}
			writes.Lock()
			err = client.WriteMessage(kind, body)
			writes.Unlock()
			if err != nil {
				return
			}
		}
	}()
	defer func() { cancel(); <-done }()
	sendError := func(code, message string) {
		writes.Lock()
		defer writes.Unlock()
		_ = client.WriteJSON(map[string]any{"type": "error", "error": map[string]string{"type": "enhance_error", "code": code, "message": message}})
	}
	for {
		kind, raw, err := client.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.TextMessage {
			sendError("unsupported_input", "Responses 采集仅支持文本业务消息")
			return
		}
		identity, err = p.identity.Resolve(ctx, r, state.clientIP)
		if err != nil {
			identity.Eligibility = "unknown"
			identity.Reason = "身份查询不可用"
		}
		sequence++
		seq := sequence
		c := &audit.Capture{Key: uuid.NewString(), ConversationKey: connectionKey, Transport: "websocket", ConnectionKey: &connectionKey, Sequence: &seq, Protocol: "responses_websocket", Format: "websocket_message", Raw: raw, SnapshotStatus: "complete", Identity: identity, Metadata: map[string]string{"path": r.URL.Path, "mode": p.audit.ModeForUser(identity.UserID), "client_ip": state.clientIP, "content_type": "application/json"}}
		auditRequired := p.audit.RequiresAudit(identity.UserID, identity.GroupID, identity.Platform)
		c.Metadata["audit_required"] = strconv.FormatBool(auditRequired)
		if err := p.captures.Save(ctx, c); err != nil {
			if auditRequired {
				sendError("capture_unavailable", "原文尚未确认保存，未转发消息")
				return
			}
			log.Printf("WebSocket 原文保存失败 capture_key=%s error=%v", c.Key, err)
			if err := upstream.WriteMessage(kind, raw); err != nil {
				return
			}
			continue
		}
		var event struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &event)
		// 初始化及更新消息留存但不当作一次违规生成；已有提取器负责 response.create 内的字段。
		if !auditRequired {
			if err := p.captures.Finish(ctx, c.ID, "awaiting_review", "仅采集原文，等待管理员审核"); err != nil {
				log.Printf("WebSocket 采集待审核状态补写失败 capture_id=%d error=%v", c.ID, err)
			}
		} else if strings.TrimSpace(event.Type) == "response.create" {
			decision := p.evaluate(ctx, c)
			if decision != nil && decision.Kind != audit.IngressDecisionAllow && decision.Kind != audit.IngressDecisionFlag {
				p.observe(ctx, c.ID, "blocked", map[string]any{"code": decision.ErrorCode})
				sendError(decision.ErrorCode, auditDeniedMessage(decision.Kind))
				continue
			}
		} else {
			_ = p.captures.Finish(ctx, c.ID, "skipped", "非生成业务消息，仅保留原文")
		}
		p.observe(ctx, c.ID, "started", map[string]any{"transport": "websocket"})
		if err := upstream.WriteMessage(kind, raw); err != nil {
			p.observe(ctx, c.ID, "unknown", map[string]any{"reason": "消息发送未完成确认"})
			return
		}
		p.observe(ctx, c.ID, "complete", map[string]any{"phase": "client_message_forwarded", "upstream_result": "not_observed"})
	}
}
