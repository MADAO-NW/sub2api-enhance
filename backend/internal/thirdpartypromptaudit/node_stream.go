package thirdpartypromptaudit

import (
	"encoding/json"
	"fmt"
	"strings"
)

// responsesStreamEvent 是 Responses SSE 事件中聚合最终响应所需的最小字段集。
type responsesStreamEvent struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta"`
	Text     string          `json:"text"`
	Response json.RawMessage `json:"response"`
}

// isEventStream 判断响应是否声明为 SSE。
func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// looksLikeEventStream 兼容未声明 text/event-stream、但正文已经是 SSE 事件的网关。
func looksLikeEventStream(raw []byte) bool {
	text := strings.TrimSpace(string(raw))
	return strings.HasPrefix(text, "event:") || strings.HasPrefix(text, "data:")
}

// aggregateResponsesEvents 把 Responses SSE 流折叠为与非流式一致的响应 JSON，
// 使响应解析、原始响应留存和调用详情共用同一种数据形态。
func aggregateResponsesEvents(raw []byte) ([]byte, error) {
	var completed map[string]any
	var output strings.Builder
	var refusal strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event responsesStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil, fmt.Errorf("Responses 流式事件不是有效 JSON：%w", err)
		}
		switch event.Type {
		case "response.output_text.delta":
			output.WriteString(event.Delta)
		case "response.refusal.delta":
			refusal.WriteString(event.Delta)
		case "response.output_text.done":
			if output.Len() == 0 {
				output.WriteString(event.Text)
			}
		}
		if len(event.Response) == 0 {
			continue
		}
		var response map[string]any
		if err := json.Unmarshal(event.Response, &response); err != nil {
			return nil, fmt.Errorf("Responses 终态事件不是有效对象：%w", err)
		}
		completed = response
	}
	if completed == nil {
		return nil, fmt.Errorf("Responses 流式响应没有终态事件")
	}
	// 终态事件已给出正文时原样使用；部分网关只在增量事件中返回正文，此时补回同一结构。
	if !hasResponsesOutputText(completed) && (output.Len() > 0 || refusal.Len() > 0) {
		part := map[string]any{}
		if refusal.Len() > 0 {
			part = map[string]any{"type": "refusal", "refusal": refusal.String()}
		} else {
			part = map[string]any{"type": "output_text", "text": output.String()}
		}
		completed["output"] = []any{map[string]any{"type": "message", "content": []any{part}}}
	}
	return json.Marshal(completed)
}

// hasResponsesOutputText 判断终态事件是否已经包含可解析的文本或拒答内容。
func hasResponsesOutputText(response map[string]any) bool {
	items, ok := response["output"].([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := object["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			text, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if value, ok := text["text"].(string); ok && strings.TrimSpace(value) != "" {
				return true
			}
			if value, ok := text["refusal"].(string); ok && strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}
