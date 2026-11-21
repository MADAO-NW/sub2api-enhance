package thirdpartypromptaudit

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeRequestURLPerAPIFormat(t *testing.T) {
	for _, test := range []struct{ format, want string }{
		{NodeAPIFormatOpenAIChat, "https://example.com/v1/chat/completions"},
		{NodeAPIFormatOpenAIResponses, "https://example.com/v1/responses"},
		{NodeAPIFormatAnthropicMessages, "https://example.com/v1/messages"},
		{NodeAPIFormatGoogleGenerativeAI, "https://example.com/v1beta/models/gemini-pro:generateContent"},
	} {
		t.Run(test.format, func(t *testing.T) {
			url, err := nodeRequestURL(ModelConfig{BaseURL: "https://example.com", Model: "gemini-pro", APIFormat: test.format})
			require.NoError(t, err)
			require.Equal(t, test.want, url)
		})
	}
	// 带 /v1 前缀的地址不再重复追加版本号。
	url, err := nodeRequestURL(ModelConfig{BaseURL: "https://example.com/openai/v1", Model: "gpt", APIFormat: NodeAPIFormatOpenAIResponses})
	require.NoError(t, err)
	require.Equal(t, "https://example.com/openai/v1/responses", url)
	// Google 模型 ID 常带 models/ 前缀，不能重复拼进路径。
	url, err = nodeRequestURL(ModelConfig{BaseURL: "https://example.com", Model: "models/gemini-pro", APIFormat: NodeAPIFormatGoogleGenerativeAI})
	require.NoError(t, err)
	require.Equal(t, "https://example.com/v1beta/models/gemini-pro:generateContent", url)
}

func TestNodeRequestBodiesPerAPIFormat(t *testing.T) {
	snapshot := ConfigSnapshot{Config: testConfig(), FixedContract: "contract"}
	for _, format := range []string{NodeAPIFormatOpenAIChat, NodeAPIFormatOpenAIResponses, NodeAPIFormatAnthropicMessages, NodeAPIFormatGoogleGenerativeAI} {
		t.Run(format, func(t *testing.T) {
			request, err := buildNodeRequest(ModelConfig{Model: "model", APIFormat: format}, snapshot, PromptKindUser, "", "target text")
			require.NoError(t, err)
			require.NotEmpty(t, request.Body)
			require.Equal(t, "application/json", request.Headers["Content-Type"])
			var body map[string]any
			require.NoError(t, json.Unmarshal(request.Body, &body))
			raw := string(request.Body)
			require.Contains(t, raw, "target text")
			require.Contains(t, raw, "target_kind")
			switch format {
			case NodeAPIFormatOpenAIResponses:
				require.Contains(t, body, "input")
				require.NotContains(t, body, "messages")
				// Responses 默认即可流式返回，显式传 stream:false 会被部分网关拒绝。
				require.NotContains(t, body, "stream")
			case NodeAPIFormatAnthropicMessages:
				require.Contains(t, body, "system")
				require.Equal(t, float64(anthropicMaxTokens), body["max_tokens"])
			case NodeAPIFormatGoogleGenerativeAI:
				require.Contains(t, body, "contents")
				require.Contains(t, body, "systemInstruction")
				require.NotContains(t, body, "model")
			default:
				require.Contains(t, body, "messages")
				require.Equal(t, "model", body["model"])
			}
		})
	}
}

func TestNodeHeadersFollowAPIFormat(t *testing.T) {
	request := &nodeRequest{Headers: map[string]string{}}
	applyNodeHeaders(request, NodeAPIFormatAnthropicMessages, "secret")
	require.Equal(t, "secret", request.Headers["x-api-key"])
	require.NotContains(t, request.Headers, "Authorization")
	require.Equal(t, "2023-06-01", request.Headers["anthropic-version"])
	google := &nodeRequest{Headers: map[string]string{}}
	applyNodeHeaders(google, NodeAPIFormatGoogleGenerativeAI, "secret")
	require.Equal(t, "secret", google.Headers["x-goog-api-key"])
	bearer := &nodeRequest{Headers: map[string]string{}}
	applyNodeHeaders(bearer, NodeAPIFormatOpenAIResponses, "secret")
	require.Equal(t, "Bearer secret", bearer.Headers["Authorization"])
}

func TestNodeResponseParsersPerAPIFormat(t *testing.T) {
	for _, test := range []struct{ name, format, body string }{
		{"responses", NodeAPIFormatOpenAIResponses, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"confidence\":0.9,\"reason\":\"responses\"}"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`},
		{"anthropic", NodeAPIFormatAnthropicMessages, `{"type":"message","content":[{"type":"text","text":"{\"confidence\":0.8,\"reason\":\"anthropic\"}"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":6}}`},
		{"google", NodeAPIFormatGoogleGenerativeAI, `{"candidates":[{"content":{"parts":[{"text":"{\"confidence\":0.7,\"reason\":\"google\"}"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":7}}`},
		{"chat", NodeAPIFormatOpenAIChat, `{"choices":[{"message":{"content":"{\"confidence\":0.6,\"reason\":\"chat\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":13,"completion_tokens":8}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempt := &ModelAttempt{}
			score, failure := parseNodeResponse([]byte(test.body), attempt, test.format)
			require.Nil(t, failure)
			require.NotNil(t, score)
			require.NotZero(t, score.Confidence)
			require.NotZero(t, attempt.InputTokens)
			require.NotZero(t, attempt.OutputTokens)
		})
	}
}

func TestNodeResponseRefusalAndTruncation(t *testing.T) {
	for _, test := range []struct{ format, body, code string }{
		{NodeAPIFormatOpenAIResponses, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, "upstream_incomplete"},
		{NodeAPIFormatAnthropicMessages, `{"type":"message","content":[],"stop_reason":"max_tokens"}`, "upstream_incomplete"},
		{NodeAPIFormatAnthropicMessages, `{"type":"message","content":[],"stop_reason":"refusal"}`, "upstream_refused"},
		{NodeAPIFormatGoogleGenerativeAI, `{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}`, "upstream_refused"},
		{NodeAPIFormatGoogleGenerativeAI, `{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`, "upstream_refused"},
	} {
		t.Run(test.format+":"+test.code, func(t *testing.T) {
			_, failure := parseNodeResponse([]byte(test.body), &ModelAttempt{}, test.format)
			require.NotNil(t, failure)
			require.Equal(t, test.code, failure.Code)
		})
	}
}

func TestModelCatalogPerAPIFormat(t *testing.T) {
	models, err := parseModelCatalog([]byte(`{"models":[{"name":"models/gemini-pro"},{"name":"models/gemini-flash"}]}`), NodeAPIFormatGoogleGenerativeAI)
	require.NoError(t, err)
	require.Equal(t, []string{"gemini-pro", "gemini-flash"}, models)
	models, err = parseModelCatalog([]byte(`{"data":[{"id":"claude-1"},{"id":"claude-1"},{"id":"claude-2"}]}`), NodeAPIFormatAnthropicMessages)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-1", "claude-2"}, models)
}

func TestResponsesStreamAggregatesDeltas(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"status":"in_progress","output":[]}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"{\"confidence\":0.9,"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"\"reason\":\"stream\"}"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":9,"output_tokens":4}}}`,
		``,
	}, "\n")
	raw, err := aggregateResponsesEvents([]byte(stream))
	require.NoError(t, err)
	attempt := &ModelAttempt{}
	score, failure := parseNodeResponse(raw, attempt, NodeAPIFormatOpenAIResponses)
	require.Nil(t, failure)
	require.Equal(t, 0.9, score.Confidence)
	require.Equal(t, "stream", score.Reason)
	require.Equal(t, int64(9), *attempt.InputTokens)
}

func TestResponsesStreamWithoutTerminalEventFails(t *testing.T) {
	_, err := aggregateResponsesEvents([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n"))
	require.ErrorContains(t, err, "终态事件")
}

func TestGoogleIgnoresThoughtParts(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":"thinking","thought":true},{"text":"{\"confidence\":0.4,\"reason\":\"answer\"}"}]},"finishReason":"STOP"}]}`
	attempt := &ModelAttempt{}
	score, failure := parseNodeResponse([]byte(body), attempt, NodeAPIFormatGoogleGenerativeAI)
	require.Nil(t, failure)
	require.Equal(t, "answer", score.Reason)
}

func TestResponsesFailedStatusIsUpstreamError(t *testing.T) {
	body := `{"status":"failed","error":{"message":"provider overloaded"},"output":[]}`
	_, failure := parseNodeResponse([]byte(body), &ModelAttempt{}, NodeAPIFormatOpenAIResponses)
	require.NotNil(t, failure)
	require.Equal(t, "upstream_api_error", failure.Code)
}

func TestNodeSemanticsIncludesAPIFormat(t *testing.T) {
	base := ModelConfig{ID: "node", BaseURL: "https://example.invalid", Model: "model", TimeoutMS: 1000, APIFormat: NodeAPIFormatOpenAIChat}
	other := base
	other.APIFormat = NodeAPIFormatAnthropicMessages
	require.NotEqual(t, nodeSemantics(base), nodeSemantics(other))
}
