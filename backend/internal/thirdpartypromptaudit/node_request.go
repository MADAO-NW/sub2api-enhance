package thirdpartypromptaudit

import (
	"encoding/json"
	"errors"
)

// anthropicMaxTokens 是 Anthropic Messages 协议必填的输出上限；
// 审核结果只有一个短 JSON，固定取够用的最小预算。
const anthropicMaxTokens = 2048

// nodeRequest 是一次审核调用的请求头与请求体。
type nodeRequest struct {
	Headers map[string]string
	Body    []byte
}

// buildNodeRequest 按节点接口格式组装真实请求；固定协议字段始终由服务端控制。
func buildNodeRequest(model ModelConfig, snapshot ConfigSnapshot, kind, correction, text string) (*nodeRequest, error) {
	system := systemPromptSnapshot(snapshot, kind, correction)
	envelope, err := json.Marshal(auditEnvelope{TargetKind: kind, Target: text})
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
	var body any
	switch normalizeNodeAPIFormat(model.APIFormat) {
	case NodeAPIFormatOpenAIResponses:
		body = map[string]any{
			"model": model.Model,
			"input": []map[string]any{
				{"role": "system", "content": []map[string]any{{"type": "input_text", "text": system}}},
				{"role": "user", "content": []map[string]any{{"type": "input_text", "text": string(envelope)}}},
			},
		}
	case NodeAPIFormatAnthropicMessages:
		body = map[string]any{
			"model":      model.Model,
			"max_tokens": anthropicMaxTokens,
			"system":     system,
			"messages":   []map[string]any{{"role": "user", "content": string(envelope)}},
		}
	case NodeAPIFormatGoogleGenerativeAI:
		body = map[string]any{
			"systemInstruction": map[string]any{"parts": []map[string]any{{"text": system}}},
			"contents":          []map[string]any{{"role": "user", "parts": []map[string]any{{"text": string(envelope)}}}},
		}
	case NodeAPIFormatOpenAIChat:
		body = map[string]any{
			"model":    model.Model,
			"stream":   false,
			"messages": []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: string(envelope)}},
		}
	default:
		return nil, errors.New("节点接口格式无效")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &nodeRequest{Headers: headers, Body: raw}, nil
}

// applyNodeHeaders 只在服务端明确支持的接口格式上写入凭据头，避免把 Key 发给错误协议。
func applyNodeHeaders(request *nodeRequest, apiFormat, key string) {
	if key == "" {
		return
	}
	switch normalizeNodeAPIFormat(apiFormat) {
	case NodeAPIFormatAnthropicMessages:
		request.Headers["x-api-key"] = key
		request.Headers["anthropic-version"] = "2023-06-01"
	case NodeAPIFormatGoogleGenerativeAI:
		request.Headers["x-goog-api-key"] = key
	default:
		request.Headers["Authorization"] = "Bearer " + key
	}
}
