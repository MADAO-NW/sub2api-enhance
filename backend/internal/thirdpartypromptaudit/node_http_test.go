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

func TestNodeEndpointAcceptsRootTrailingSlashAndVersionPrefix(t *testing.T) {
	for _, raw := range []string{"https://example.com", "https://example.com/", "https://example.com/v1", "https://example.com/v1/"} {
		models, err := modelsURL(raw)
		require.NoError(t, err)
		require.Equal(t, "https://example.com/v1/models", models)
		chat, err := chatCompletionsURL(raw)
		require.NoError(t, err)
		require.Equal(t, "https://example.com/v1/chat/completions", chat)
	}
	models, err := modelsURL("https://example.com/openai/v1/")
	require.NoError(t, err)
	require.Equal(t, "https://example.com/openai/v1/models", models)
}
