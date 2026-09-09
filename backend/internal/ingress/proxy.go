package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sub2api-enhance/internal/sub2api"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
	"sync"
	"time"
	"unicode/utf8"
)

type captureStore interface {
	Save(context.Context, *audit.Capture) error
	Observe(context.Context, int64, string, map[string]any) error
	Finish(context.Context, int64, string, string) error
}
type auditor interface {
	Mode() string
	AuditCapture(context.Context, *audit.Capture) (*audit.IntakeDecision, error)
	ObserveGateway(context.Context, *audit.IntakeDecision, audit.IngressDecision, time.Duration)
}
type identityResolver interface {
	Resolve(context.Context, *http.Request, string) (sub2api.Identity, error)
}
type Proxy struct {
	upstream *url.URL
	http     *httputil.ReverseProxy
	captures captureStore
	audit    auditor
	identity identityResolver
}
type requestContext struct {
	clientIP string
	capture  *audit.Capture
	started  time.Time
}
type contextKey struct{}

func New(upstream string, captures captureStore, service auditor, identities identityResolver) (*Proxy, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	p := &Proxy{upstream: u, captures: captures, audit: service, identity: identities}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	p.http = &httputil.ReverseProxy{Transport: transport, FlushInterval: -1, Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(u)
		r.Out.Host = u.Host
		r.Out.Header.Del("Forwarded")
		r.Out.Header.Del("X-Forwarded-For")
		r.Out.Header.Del("X-Real-IP")
		r.Out.Header.Del("X-Enhance-Capture")
		r.Out.Header.Del("X-Enhance-Conversation-ID")
		if state, ok := r.In.Context().Value(contextKey{}).(*requestContext); ok {
			r.Out.Header.Set("X-Forwarded-For", state.clientIP)
			r.Out.Header.Set("X-Real-IP", state.clientIP)
		}
	}, ModifyResponse: func(r *http.Response) error {
		state, _ := r.Request.Context().Value(contextKey{}).(*requestContext)
		if state != nil && state.capture != nil {
			p.observe(r.Request.Context(), state.capture.ID, "response_started", map[string]any{"http_status": r.StatusCode})
			r.Body = &observedBody{ReadCloser: r.Body, done: func(status string) {
				p.observe(r.Request.Context(), state.capture.ID, status, map[string]any{"http_status": r.StatusCode})
			}}
		}
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		state, _ := r.Context().Value(contextKey{}).(*requestContext)
		if state != nil && state.capture != nil {
			p.observe(r.Context(), state.capture.ID, "unknown", map[string]any{"reason": "原版响应未完成确认"})
		}
		writeError(w, protocol(r.URL.Path), 502, "upstream_unavailable", "原版服务暂不可用")
	}}
	return p, nil
}

type observedBody struct {
	io.ReadCloser
	done func(string)
	once sync.Once
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		status := "unknown"
		if errors.Is(err, io.EOF) {
			status = "complete"
		}
		b.once.Do(func() { b.done(status) })
	}
	return n, err
}
func (b *observedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.done("unknown") })
	return err
}
func (p *Proxy) observe(ctx context.Context, id int64, status string, detail map[string]any) {
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.captures.Observe(save, id, status, detail); err != nil {
		log.Printf("采集转发观测补写失败 capture_id=%d status=%s", id, status)
	}
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
	file, err := os.CreateTemp("", "sub2api-enhance-input-*")
	if err != nil {
		writeError(w, kind, 503, "capture_unavailable", "无法保存请求输入")
		return
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	count, readErr := io.Copy(file, r.Body)
	_ = r.Body.Close()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, kind, 503, "capture_unavailable", "无法读取已接收输入")
		return
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		writeError(w, kind, 503, "capture_unavailable", "无法读取已接收输入")
		return
	}
	c := &audit.Capture{Key: uuid.NewString(), ConversationKey: r.Header.Get("X-Enhance-Conversation-ID"), Transport: "http", Protocol: kind, Format: "entity_bytes", Raw: raw, SnapshotStatus: "complete", Metadata: map[string]string{"path": r.URL.Path, "method": r.Method, "content_type": r.Header.Get("Content-Type"), "content_encoding": r.Header.Get("Content-Encoding"), "client_ip": clientIP, "mode": p.audit.Mode()}}
	if readErr != nil {
		c.SnapshotStatus = "incomplete"
		c.Error = "客户端正文未接收完整"
	}
	if contentType, params, e := mime.ParseMediaType(r.Header.Get("Content-Type")); e == nil && contentType == "multipart/form-data" && readErr == nil {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			writeError(w, kind, 503, "capture_unavailable", "无法读取媒体输入")
			return
		}
		fields := []map[string]any{}
		reader := multipart.NewReader(file, params["boundary"])
		sequence := 0
		for {
			part, e := reader.NextRawPart()
			if errors.Is(e, io.EOF) {
				break
			}
			if e != nil {
				c.Error = "媒体表单未完整解析"
				c.SnapshotStatus = "incomplete"
				break
			}
			sequence++
			entry := map[string]any{"name": part.FormName(), "order": sequence, "content_type": part.Header.Get("Content-Type"), "headers": part.Header}
			if part.FileName() == "" {
				data, e := io.ReadAll(part)
				if e != nil {
					c.SnapshotStatus = "incomplete"
					c.Error = "媒体文本字段读取失败"
				}
				entry["raw_base64"] = data
				if utf8.Valid(data) {
					entry["text"] = string(data)
				}
			} else {
				entry["filename"] = part.FileName()
				entry["binary_omitted"] = true
				_, _ = io.Copy(io.Discard, part)
			}
			_ = part.Close()
			fields = append(fields, entry)
		}
		c.Raw, err = json.Marshal(fields)
		if err != nil {
			writeError(w, kind, 503, "capture_unavailable", "媒体文本封装失败")
			return
		}
		c.Format = "multipart_text_fields"
		c.Metadata["content_encoding"] = ""
	}
	identityCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	c.Identity, err = p.identity.Resolve(identityCtx, r, clientIP)
	cancel()
	if err != nil {
		c.Error = c.Identity.Reason
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	err = p.captures.Save(save, c)
	cancel()
	if err != nil {
		log.Printf("请求原文保存失败 capture_key=%s", c.Key)
		writeError(w, kind, 503, "capture_unavailable", "原文尚未确认保存，未转发请求")
		return
	}
	state.capture = c
	if readErr != nil || c.SnapshotStatus != "complete" {
		writeError(w, kind, 400, "incomplete_input", "请求输入不完整")
		return
	}
	decision := p.evaluate(r.Context(), c)
	if !p.allow(w, kind, c, decision) {
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, kind, 503, "capture_unavailable", "请求转发准备失败")
		return
	}
	r.Body = io.NopCloser(file)
	r.ContentLength = count
	r.GetBody = nil
	p.observe(r.Context(), c.ID, "started", map[string]any{"transport": "http"})
	p.http.ServeHTTP(w, r)
	if decision != nil {
		p.audit.ObserveGateway(r.Context(), decision, audit.IngressDecision{AllowNextStage: true, Kind: audit.IngressDecisionAllow}, time.Since(state.started))
	}
}
func (p *Proxy) evaluate(ctx context.Context, c *audit.Capture) *audit.IntakeDecision {
	if c.Identity.Eligibility != "passed" || c.Protocol == "unsupported" {
		if p.audit.Mode() == "blocking" {
			return &audit.IntakeDecision{Mode: "blocking", Kind: audit.IngressDecisionUnavailable, ErrorCode: "eligibility_unknown"}
		}
		return nil
	}
	result, err := p.audit.AuditCapture(ctx, c)
	status := "done"
	message := ""
	if err != nil {
		status = "failed"
		message = err.Error()
		if p.audit.Mode() == "blocking" {
			result = &audit.IntakeDecision{Mode: "blocking", Kind: audit.IngressDecisionUnavailable, ErrorCode: "input_parse_failed"}
		}
	} else if result != nil && result.JobID == 0 {
		status = "retry"
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.captures.Finish(finish, c.ID, status, message); err != nil {
		log.Printf("采集处理状态补写失败 capture_id=%d", c.ID)
	}
	return result
}
func (p *Proxy) allow(w http.ResponseWriter, kind string, c *audit.Capture, d *audit.IntakeDecision) bool {
	if d == nil || d.Kind == audit.IngressDecisionAllow || d.Kind == audit.IngressDecisionFlag {
		return true
	}
	status := 503
	if d.Kind == audit.IngressDecisionBlock {
		status = 403
	}
	p.observe(context.Background(), c.ID, "blocked", map[string]any{"code": d.ErrorCode})
	writeError(w, kind, status, d.ErrorCode, auditDeniedMessage(d.Kind))
	return false
}
