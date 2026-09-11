package config

import (
	"encoding/base64"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestNodeSecretRequiresMatchingEncryptionKey(t *testing.T) {
	a, err := NewEncryptor(&Config{EncryptionKey: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))})
	require.NoError(t, err)
	b, err := NewEncryptor(&Config{EncryptionKey: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))})
	require.NoError(t, err)
	cipher, err := a.Encrypt("unit-test-node-key")
	require.NoError(t, err)
	require.NotContains(t, cipher, "unit-test-node-key")
	plain, err := a.Decrypt(cipher)
	require.NoError(t, err)
	require.Equal(t, "unit-test-node-key", plain)
	_, err = b.Decrypt(cipher)
	require.Error(t, err)
}

func TestLoadRequiresEnhanceRedisAndExplicitAuditCacheTTL(t *testing.T) {
	for key, value := range map[string]string{
		"ENHANCE_DATABASE_URL":        "postgres://example.invalid/db",
		"SUB2API_INTERNAL_URL":        "http://127.0.0.1:18080",
		"ENHANCE_PUBLIC_ORIGIN":       "https://gateway.example.invalid",
		"ENHANCE_DB_CONNECTIONS":      "8",
		"ENHANCE_INGRESS_CONNECTIONS": "8",
	} {
		t.Setenv(key, value)
	}
	_, err := Load()
	require.ErrorContains(t, err, "ENHANCE_REDIS_URL")
	t.Setenv("ENHANCE_REDIS_URL", "redis://127.0.0.1:6379/14")
	_, err = Load()
	require.ErrorContains(t, err, "ENHANCE_AUDIT_CACHE_TTL")
	t.Setenv("ENHANCE_AUDIT_CACHE_TTL", "168h")
	loaded, err := Load()
	require.NoError(t, err)
	require.Equal(t, 168*time.Hour, loaded.AuditCacheTTL)
}
