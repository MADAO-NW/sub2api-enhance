package thirdpartypromptaudit

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 审核节点支持的接口格式；空值按 OpenAI Chat Completions 处理。
const (
	NodeAPIFormatOpenAIChat         = "openai_chat_completions"
	NodeAPIFormatOpenAIResponses    = "openai_responses"
	NodeAPIFormatAnthropicMessages  = "anthropic_messages"
	NodeAPIFormatGoogleGenerativeAI = "google_generative_ai"
	defaultNodeAPIFormat            = NodeAPIFormatOpenAIChat
)

// normalizeNodeAPIFormat 把历史空值规范为默认格式，未知值原样返回供校验拒绝。
func normalizeNodeAPIFormat(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultNodeAPIFormat
	}
	return value
}

// nodeRequestURL 按接口格式返回真实审核请求地址。
func nodeRequestURL(model ModelConfig) (string, error) {
	switch normalizeNodeAPIFormat(model.APIFormat) {
	case NodeAPIFormatOpenAIResponses:
		return nodeEndpointURL(model.BaseURL, "/v1/responses")
	case NodeAPIFormatAnthropicMessages:
		return nodeEndpointURL(model.BaseURL, "/v1/messages")
	case NodeAPIFormatGoogleGenerativeAI:
		// 手工输入时可能带 Google 的资源前缀，URL 中只保留模型名本身。
		name := strings.TrimPrefix(strings.TrimSpace(model.Model), "models/")
		return nodeEndpointURL(model.BaseURL, "/v1beta/models/"+url.PathEscape(name)+":generateContent")
	default:
		return nodeEndpointURL(model.BaseURL, "/v1/chat/completions")
	}
}

func chatCompletionsURL(raw string) (string, error) {
	return nodeEndpointURL(raw, "/v1/chat/completions")
}

func modelsURL(raw string) (string, error) {
	return nodeEndpointURL(raw, "/v1/models")
}

// nodeModelsURL 按接口格式返回模型列表地址；Google 使用 v1beta 且资源名带 models/ 前缀。
func nodeModelsURL(model ModelConfig) (string, error) {
	if normalizeNodeAPIFormat(model.APIFormat) == NodeAPIFormatGoogleGenerativeAI {
		return nodeEndpointURL(model.BaseURL, "/v1beta/models")
	}
	return modelsURL(model.BaseURL)
}

// nodeEndpointURL 统一校验审核节点地址并追加 OpenAI 兼容接口路径。
func nodeEndpointURL(raw, endpoint string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("节点地址必须是不含凭据和查询项的 HTTP(S) 地址")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	// 地址已经以 /v1 结尾时只追加资源路径，并保留 /openai 等前缀。
	if strings.HasSuffix(u.Path, "/v1") {
		endpoint = strings.TrimPrefix(endpoint, "/v1")
	} else if strings.HasSuffix(u.Path, "/v1beta") {
		// Google 管理端常直接填写 /v1beta，此时同样只追加资源路径。
		endpoint = strings.TrimPrefix(endpoint, "/v1beta")
	}
	u.Path += endpoint
	return u.String(), nil
}
func newNodeHTTPClient(model ModelConfig) (*http.Client, error) {
	if _, err := nodeRequestURL(model); err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{Timeout: time.Duration(model.TimeoutMS) * time.Millisecond, Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 5 * time.Second, IdleConnTimeout: 90 * time.Second}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
