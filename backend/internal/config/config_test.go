package config

import (
	"encoding/base64"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
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
