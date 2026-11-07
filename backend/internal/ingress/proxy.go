package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"sub2api-enhance/internal/sub2api"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
)

type auditor interface {
	Mode() string
	ModeForUser(int64) string
	Check(context.Context, audit.IntakeRequest) *audit.IntakeDecision
	RegisterResponseID(context.Context, string, string)
	RejectContinuation(*audit.IntakeDecision, int64, string)
}

type identityResolver interface {
	Resolve(context.Context, *http.Request, string) (sub2api.Identity, error)
}

type Proxy struct {
	upstream *url.URL
	http     *httputil.ReverseProxy
	audit    auditor
	identity identityResolver
}

type requestContext struct {
	clientIP string
	turnKey  string
	started  time.Time
}

type contextKey struct{}

func New(upstream string, service auditor, identities identityResolver) (*Proxy, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	p := &Proxy{upstream: u, audit: service, identity: identities}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	p.http = &httputil.ReverseProxy{Transport: transport, FlushInterval: -1, Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(u)
		r.Out.Host = u.Host
		r.Out.Header.Del("Forwarded")
		r.Out.Header.Del("X-Forwarded-For")
		r.Out.Header.Del("X-Real-IP")
		// 增强专用请求头继续剥离，不交给原版。
		r.Out.Header.Del("X-Enhance-Capture")
		r.Out.Header.Del("X-Enhance-Conversation-ID")
		if state, ok := r.In.Context().Value(contextKey{}).(*requestContext); ok {
			r.Out.Header.Set("X-Forwarded-For", state.clientIP)
			r.Out.Header.Set("X-Real-IP", state.clientIP)
		}
	}, ModifyResponse: func(response *http.Response) error {
		state, _ := response.Request.Context().Value(contextKey{}).(*requestContext)
		if state == nil || state.turnKey == "" || response.Body == nil {
			return nil
		}
		// 只在前缀范围内嗅探响应 ID，不缓存整个响应体。
		response.Body = &idSniffer{ReadCloser: response.Body, turnKey: state.turnKey, service: p.audit,
			context: response.Request.Context(), streaming: strings.Contains(response.Header.Get("Content-Type"), "event-stream")}
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		writeError(w, protocol(r.URL.Path), 502, "upstream_unavailable", "原版服务暂不可用")
	}}
	return p, nil
}

// 响应 ID 嗅探只在前 256KB 内进行，避免代理缓存长响应。
const responseIDSniffLimit = 256 << 10

type idSniffer struct {
	io.ReadCloser
	buffer    []byte
	turnKey   string
	service   auditor
	context   context.Context
	streaming bool
	finished  bool
}

func (s *idSniffer) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if n > 0 && !s.finished {
		remaining := responseIDSniffLimit - len(s.buffer)
		if remaining > 0 {
			s.buffer = append(s.buffer, p[:min(n, remaining)]...)
		}
		if id := extractResponseID(s.buffer, s.streaming); id != "" {
			s.finished, s.buffer = true, nil
			s.service.RegisterResponseID(s.context, id, s.turnKey)
		} else if len(s.buffer) >= responseIDSniffLimit {
			s.finished, s.buffer = true, nil
		}
	}
	if err != nil && !s.finished {
		s.finished, s.buffer = true, nil
	}
	return n, err
}

// extractResponseID 从非流式响应或 SSE 的 response.created 事件中取出响应 ID。
func extractResponseID(raw []byte, streaming bool) string {
	if !streaming {
		var value struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return ""
		}
		return value.ID
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") || !strings.Contains(line, "response.created") {
			continue
		}
		var event struct {
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			continue
		}
		if event.Response.ID != "" {
			return event.Response.ID
		}
	}
	return ""
}

func protocol(path string) string {
	switch {
	case strings.Contains(path, "/responses"):
		return "responses"
	case strings.HasSuffix(path, "/chat/completions"):
		return "chat_completions"
	case strings.Contains(path, "/messages"):
		return "anthropic_messages"
	case strings.Contains(path, "/models/") && (strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent")):
		return "gemini"
	case strings.HasSuffix(path, "/embeddings"):
		return "openai_embeddings"
	case strings.HasSuffix(path, "/alpha/search"):
		return "openai_alpha_search"
	case strings.Contains(path, "/images/") || strings.Contains(path, "/videos") || strings.Contains(path, "/audio/") || path == "/tts" || path == "/stt" || strings.Contains(path, "/custom-voices") || strings.HasSuffix(path, "/web_search") || strings.HasSuffix(path, "/x_search"):
		return "media"
	default:
		return "unsupported"
	}
}

func writeError(w http.ResponseWriter, protocol string, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var value any = map[string]any{"error": map[string]any{"type": "enhance_error", "code": code, "message": message}}
	if protocol == "anthropic_messages" {
		value = map[string]any{"type": "error", "error": map[string]string{"type": code, "message": message}}
	} else if protocol == "gemini" {
		value = map[string]any{"error": map[string]any{"code": status, "status": strings.ToUpper(code), "message": message}}
	}
	_ = json.NewEncoder(w).Encode(value)
}

// auditDeniedMessage 向调用方说明同步审核未转发请求，同时避免泄露审核策略和模型原始理由。
func auditDeniedMessage(kind audit.IngressDecisionKind) string {
	if kind == audit.IngressDecisionBlock {
		return "请求内容未通过第三方模型提示词审核，增强服务已阻止转发。请调整输入后重试，或联系管理员复核。"
	}
	return "增强审核暂时无法完成，当前为同步审核模式，原请求未转发。请稍后重试，或联系管理员。"
}

// Serve 接收的 clientIP 必须由配置了可信代理范围的 Gin 得到。
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, clientIP string) {
	state := &requestContext{clientIP: clientIP, started: time.Now()}
	r = r.WithContext(context.WithValue(r.Context(), contextKey{}, state))
	if p.audit.Mode() == "off" {
		p.http.ServeHTTP(w, r)
		return
	}
	if websocket.IsWebSocketUpgrade(r) {
		p.websocket(w, r, state)
		return
	}
	if r.Method != http.MethodPost {
		p.http.ServeHTTP(w, r)
		return
	}
	kind := protocol(r.URL.Path)
	body, err := spoolRequest(r)
	if err != nil {
		switch {
		case errors.Is(err, ErrBodyTooLarge):
			writeError(w, kind, http.StatusRequestEntityTooLarge, "request_too_large", "请求体解压后超过 64MB 上限，未转发")
		case errors.Is(err, ErrUnsupportedEncoding):
			writeError(w, kind, http.StatusUnsupportedMediaType, "unsupported_encoding", "请求内容编码暂不支持审核解析")
		default:
			writeError(w, kind, http.StatusServiceUnavailable, "input_unavailable", "无法读取请求输入")
		}
		return
	}
	defer body.close()
	if body.encoding != "" {
		r.Header.Del("Content-Encoding")
	}
	parsedProtocol := kind
	if body.format == "multipart_text_fields" {
		parsedProtocol = "chat_completions"
	}
	identityCtx, identityCancel := context.WithTimeout(r.Context(), 5*time.Second)
	identity, identityErr := p.identity.Resolve(identityCtx, r, clientIP)
	identityCancel()
	if identityErr != nil {
		// 身份解析失败：blocking 沿用 eligibility_unknown 拒绝，async 直接转发且不落库（决策 12）。
		if p.audit.ModeForUser(identity.UserID) == "blocking" {
			writeError(w, kind, http.StatusServiceUnavailable, "eligibility_unknown", "身份暂不可验证，当前为同步审核模式，请求未转发")
			return
		}
		p.forward(w, r, kind, body)
		return
	}
	if parsedProtocol == "unsupported" {
		if p.audit.ModeForUser(identity.UserID) == "blocking" {
			writeError(w, kind, http.StatusServiceUnavailable, "eligibility_unknown", "该协议尚不支持审核，当前为同步审核模式，请求未转发")
			return
		}
		p.forward(w, r, kind, body)
		return
	}
	if err := body.rewind(); err != nil {
		writeError(w, kind, http.StatusServiceUnavailable, "input_unavailable", "无法读取请求输入")
		return
	}
	parsed, err := audit.ParsePrompt(parsedProtocol, body.file, audit.DetectClient(r.Header))
	if err != nil {
		if errors.Is(err, audit.ErrNotAuditable) {
			// 非生成请求直接转发，不建任务、不落库。
			p.forward(w, r, kind, body)
			return
		}
		if p.audit.ModeForUser(identity.UserID) == "blocking" {
			writeError(w, kind, http.StatusServiceUnavailable, "input_parse_failed", "请求无法按协议解析，当前为同步审核模式，请求未转发")
			return
		}
		p.forward(w, r, kind, body)
		return
	}
	// 客户端未带请求 ID 时生成一个，并回写响应头，保证任务与日志可对账。
	requestID := headerValue(r, "X-Request-Id")
	if requestID == "" {
		requestID = "enh-" + uuid.NewString()
		w.Header().Set("X-Enhance-Request-Id", requestID)
	}
	decision := p.audit.Check(r.Context(), audit.IntakeRequest{
		Parsed: parsed, UserID: identity.UserID, APIKeyID: identity.APIKeyID, GroupID: identity.GroupID,
		Identity: audit.Identity{Username: identity.Username, UserEmail: identity.UserEmail, APIKeyName: identity.APIKeyName, GroupName: identity.GroupName, Endpoint: r.URL.Path},
		Provider: identity.Platform, Model: parsed.Model, RequestID: requestID,
	})
	if !p.allow(w, kind, identity.APIKeyID, decision) {
		return
	}
	if decision != nil {
		state.turnKey = decision.TurnKey
	}
	p.forward(w, r, kind, body)
}

// forward 用已落盘的正文转发原请求，保留原始长度与查询参数。
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, kind string, body *spooledBody) {
	if err := body.rewind(); err != nil {
		writeError(w, kind, http.StatusServiceUnavailable, "input_unavailable", "请求转发准备失败")
		return
	}
	r.Body = io.NopCloser(body.file)
	r.ContentLength = body.size
	r.GetBody = nil
	p.http.ServeHTTP(w, r)
}

func headerValue(r *http.Request, name string) string {
	if value := r.Header.Get(name); value != "" {
		return value
	}
	return ""
}

// allow 决定是否继续转发：异步审核不可用时不阻断原请求。
func (p *Proxy) allow(w http.ResponseWriter, kind string, apiKeyID int64, d *audit.IntakeDecision) bool {
	if d == nil || d.Kind == audit.IngressDecisionAllow || d.Kind == audit.IngressDecisionFlag ||
		(d.Mode == "async" && d.Kind == audit.IngressDecisionUnavailable) {
		return true
	}
	status := http.StatusServiceUnavailable
	if d.Kind == audit.IngressDecisionBlock {
		status = http.StatusForbidden
		p.audit.RejectContinuation(d, apiKeyID, d.ErrorCode)
	}
	writeError(w, kind, status, d.ErrorCode, auditDeniedMessage(d.Kind))
	return false
}

// responseIDFromEvent 读取 WebSocket 事件里的 response.created 响应 ID。
func responseIDFromEvent(raw []byte) string {
	var event struct {
		Type     string `json:"type"`
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return ""
	}
	if event.Type != "response.created" {
		return ""
	}
	return event.Response.ID
}
