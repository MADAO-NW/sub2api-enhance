package thirdpartypromptaudit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListModelsUsesStoredCredentialAndOpenAICompatiblePath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Equal(t, "Bearer stored-key", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data":[{"id":"model-b"},{"id":"model-a"},{"id":"model-b"}]}`))
	}))
	defer server.Close()
	manager := &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: testConfig()}, Keys: map[string]string{"node": "stored-key"}}}
	service := &Service{config: manager}
	result, err := service.ListModels(context.Background(), ModelCatalogRequest{ActorUserID: 9, ModelID: "node", BaseURL: server.URL, TimeoutMS: 1000, KeyAction: "keep"})
	require.NoError(t, err)
	require.Equal(t, []string{"model-b", "model-a"}, result.Models)
}

func TestListModelsExplainsCredentialRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	manager := &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: testConfig()}, Keys: map[string]string{}}}
	service := &Service{config: manager}
	_, err := service.ListModels(context.Background(), ModelCatalogRequest{ModelID: "new-node", BaseURL: server.URL, TimeoutMS: 1000, KeyAction: "keep"})
	require.ErrorContains(t, err, "请检查 API Key")
}
