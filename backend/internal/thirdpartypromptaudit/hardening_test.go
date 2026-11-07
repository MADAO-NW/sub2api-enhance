package thirdpartypromptaudit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 密钥形态必须在入库与送审前被掩码，且不改变其它正文。
func TestMaskSecretsRedactsCredentials(t *testing.T) {
	cases := map[string]string{
		"请用 sk-10753f3230314b56fa90f2de38f78fb1c65c857a8e2a74e2448c81c25336fbe3 调接口": "请用 [REDACTED] 调接口",
		"aws AKIAABCDEFGHIJKLMNOP 是密钥":                                               "aws [REDACTED] 是密钥",
		"token eyJhbGciOiJIUzI1NiJ9.eyJ1c2VyX2lkIjoxfQ.abcdefghijklmnop 过期了":         "token [REDACTED] 过期了",
		"连接 postgres://user:pass@db:5432/app 报错":                                     "连接 postgres://[REDACTED] 报错",
		"password=hunter2secret 写在配置里":                                               "password= [REDACTED] 写在配置里",
	}
	for input, want := range cases {
		require.Equal(t, want, maskSecrets(input), input)
	}
	// 普通技术文本不能被误伤。
	plain := "用 Python 实现二分查找，时间复杂度 O(log n)。"
	require.Equal(t, plain, maskSecrets(plain))
}

// 指令块与用户提示词使用同一套掩码，且哈希基于掩码后的正文。
func TestPromptTextHashesMaskedContent(t *testing.T) {
	withSecret := newPromptText(PromptKindUser, "我的 key 是 sk-10753f3230314b56fa90f2de38f78fb1c65c857a8e2a74e2448c81c25336fbe3")
	require.NotContains(t, withSecret.Text, "sk-1075")
	require.Contains(t, withSecret.Text, "[REDACTED]")
	plain := newPromptText(PromptKindUser, "我的 key 是 [REDACTED]")
	require.Equal(t, plain.Hash, withSecret.Hash)
}

// 本机与内网地址需要显式允许才能作为审核节点。
func TestPrivateNodeHostDetectsLoopbackAndLAN(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:7777", "http://localhost:8080/v1", "https://192.168.1.9", "http://10.0.0.5:9000", "http://[::1]:8080"} {
		require.True(t, privateNodeHost(raw), raw)
	}
	for _, raw := range []string{"https://api.commandcode.ai/provider", "https://codeauv.com", "http://93.184.216.34"} {
		require.False(t, privateNodeHost(raw), raw)
	}
}

// 熔断演练按阈值给出可预测结论，且不依赖数据库。
func TestDrillBreakerReportsThresholdSteps(t *testing.T) {
	result, err := DrillBreaker(strings.Repeat("a", 64))
	require.NoError(t, err)
	require.Equal(t, instructionBreakerUserThreshold, result.Threshold)
	require.True(t, result.Tripped)
	require.Len(t, result.Steps, instructionBreakerUserThreshold+1)
	require.Contains(t, result.Steps[0], "计一次")
	require.Contains(t, result.Steps[1], "触发熔断")
	require.Contains(t, result.Steps[2], "熔断暂停")
}

// 节点凭据被拒绝要能被识别，其它失败不能误判。
func TestAuthFailureClassification(t *testing.T) {
	require.True(t, authFailure(&AuditError{Code: "upstream_http_error", Message: "审核节点返回 HTTP 401"}))
	require.True(t, authFailure(&AuditError{Code: "upstream_http_error", Message: "审核节点返回 HTTP 403"}))
	require.False(t, authFailure(&AuditError{Code: "upstream_http_error", Message: "审核节点返回 HTTP 500"}))
	require.False(t, authFailure(&AuditError{Code: "timeout", Message: "timeout"}))
	require.False(t, authFailure(nil))
}
