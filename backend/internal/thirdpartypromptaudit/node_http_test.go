package thirdpartypromptaudit

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditNodeRequiresHTTPSOutsideLoopback(t *testing.T) {
	for _, value := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "http://localhost:8080", "https://audit.example.com"} {
		url, err := chatCompletionsURL(value)
		require.NoError(t, err)
		require.Contains(t, url, "/v1/chat/completions")
	}
	for _, value := range []string{"http://192.0.2.8:8080", "http://audit.example.com"} {
		_, err := chatCompletionsURL(value)
		require.ErrorContains(t, err, "必须使用 HTTPS")
	}
}
