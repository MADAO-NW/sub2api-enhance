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

func chatCompletionsURL(raw string) (string, error) {
	return nodeEndpointURL(raw, "/v1/chat/completions")
}

func modelsURL(raw string) (string, error) {
	return nodeEndpointURL(raw, "/v1/models")
}

// nodeEndpointURL 统一校验审核节点地址并追加 OpenAI 兼容接口路径。
func nodeEndpointURL(raw, endpoint string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("节点地址必须是不含凭据和查询项的 HTTP(S) 地址")
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("非本机审核节点必须使用 HTTPS，避免审核文本和节点凭据明文传输")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	// 地址已经以 /v1 结尾时只追加资源路径，并保留 /openai 等前缀。
	if strings.HasSuffix(u.Path, "/v1") {
		endpoint = strings.TrimPrefix(endpoint, "/v1")
	}
	u.Path += endpoint
	return u.String(), nil
}
func newNodeHTTPClient(model ModelConfig) (*http.Client, error) {
	if _, err := chatCompletionsURL(model.BaseURL); err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{Timeout: time.Duration(model.TimeoutMS) * time.Millisecond, Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 5 * time.Second, IdleConnTimeout: 90 * time.Second}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
