package thirdpartypromptaudit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseScoreKeepsSimpleValidResults(t *testing.T) {
	for _, raw := range []string{
		`{"confidence":0.3,"reason":"正常开发"}`,
		" ```json\n{\"confidence\":0.30,\"reason\":\"正常开发\"}\n``` ",
		`{"confidence":0.3,"reason":"正常开发","ignored":{"nested":true}}`,
	} {
		result, err := ParseScore(raw)
		require.NoError(t, err)
		require.Equal(t, Score{Confidence: 0.3, Reason: "正常开发"}, result)
	}
	reason := strings.Repeat("原因不截断。", 100)
	result, err := ParseScore(`{"confidence":1,"reason":"` + reason + `"}`)
	require.NoError(t, err)
	require.Equal(t, reason, result.Reason)
}

func TestParseScoreRejectsAmbiguousAndInvalidResponses(t *testing.T) {
	for _, raw := range []string{
		`{"confidence":0,"confidence":1,"reason":"重复键"}`,
		`{"confidence":"0.2","reason":"字符串"}`,
		`{"confidence":null,"reason":"空值"}`,
		`{"confidence":true,"reason":"布尔"}`,
		`{"confidence":1.1,"reason":"越界"}`,
		`{"confidence":-0.1,"reason":"越界"}`,
		`{"confidence":1e999,"reason":"溢出"}`,
		`{"confidence":0.2}`,
		`{"confidence":0.2,"reason":null}`,
		`{"confidence":0.2,"reason":"未结束"`,
		`{"confidence":0.2,"reason":""} {"confidence":1,"reason":""}`,
		`说明：{"confidence":0.2,"reason":""}`,
		`[{"confidence":0.2,"reason":""}]`,
		"```json\n{\"confidence\":0,\"reason\":\"\"}",
	} {
		t.Run(raw, func(t *testing.T) { _, err := ParseScore(raw); require.Error(t, err) })
	}
}

func TestFixedContractAlwaysFollowsEditablePolicy(t *testing.T) {
	text := systemPromptSnapshot(ConfigSnapshot{Config: Config{AuditPrompt: "管理员测试政策"}, FixedContract: OutputContract}, "修正格式")
	require.True(t, strings.HasPrefix(text, "管理员测试政策"))
	require.True(t, strings.HasSuffix(text, OutputContract))
	require.NotContains(t, text, "【联合审核】")
	require.NotContains(t, text, "【片段审核】")
	require.Contains(t, text, "audit_stage=current_user")
	require.Contains(t, text, "audit_stage=instruction_context")
	require.Contains(t, text, "audit_stage=intent_binding")
	require.Less(t, strings.Index(text, "修正格式"), strings.Index(text, "【固定返回协议"))
}
