package thirdpartypromptaudit

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parseNodeResponse 按节点接口格式区分服务商错误、明确拒绝、协议异常与可修正的评分格式错误。
func parseNodeResponse(raw []byte, attempt *ModelAttempt, apiFormat string) (*Score, *AuditError) {
	fail := func(code, message string) (*Score, *AuditError) {
		return nil, &AuditError{Code: code, Stage: "response_parse", Message: message}
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return fail("upstream_protocol_error", "上游返回空响应，未取得审核结果")
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fail("upstream_protocol_error", "上游响应不是有效 JSON："+err.Error())
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return fail("upstream_api_error", "上游返回业务错误："+string(envelope.Error))
	}
	content, failure := nodeResponseContent(raw, attempt, apiFormat)
	if failure != nil {
		return nil, failure
	}
	score, err := ParseScore(content)
	if err != nil {
		return fail("invalid_response", fmt.Sprintf("审核评分 JSON 格式无效：%s", err))
	}
	return &score, nil
}

// nodeResponseContent 从各协议响应中提取首条文本内容；失败时返回稳定的上游错误。
func nodeResponseContent(raw []byte, attempt *ModelAttempt, apiFormat string) (string, *AuditError) {
	fail := func(code, message string) *AuditError {
		return &AuditError{Code: code, Stage: "response_parse", Message: message}
	}
	switch normalizeNodeAPIFormat(apiFormat) {
	case NodeAPIFormatOpenAIResponses:
		var envelope struct {
			Status string `json:"status"`
			Error  struct {
				Message string `json:"message"`
			} `json:"error"`
			IncompleteDetails struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type    string `json:"type"`
					Text    string `json:"text"`
					Refusal string `json:"refusal"`
				} `json:"content"`
			} `json:"output"`
			Usage struct {
				InputTokens  *int64 `json:"input_tokens"`
				OutputTokens *int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return "", fail("upstream_protocol_error", "上游响应不是有效 JSON："+err.Error())
		}
		attempt.InputTokens, attempt.OutputTokens = envelope.Usage.InputTokens, envelope.Usage.OutputTokens
		if envelope.Error.Message != "" {
			return "", fail("upstream_api_error", "上游返回业务错误："+envelope.Error.Message)
		}
		if envelope.Status == "failed" {
			return "", fail("upstream_api_error", "上游模型执行失败，请查看调用详情中的原始响应")
		}
		if envelope.Status == "incomplete" && envelope.IncompleteDetails.Reason != "" {
			return "", fail("upstream_incomplete", "上游模型因长度限制未完成审核（"+envelope.IncompleteDetails.Reason+"）")
		}
		for _, output := range envelope.Output {
			for _, part := range output.Content {
				if part.Refusal != "" {
					return "", fail("upstream_refused", "上游模型拒绝执行审核："+part.Refusal)
				}
				if (part.Type == "output_text" || part.Type == "text") && strings.TrimSpace(part.Text) != "" {
					return part.Text, nil
				}
			}
		}
		return "", fail("upstream_protocol_error", "上游响应缺少 output_text；请查看调用详情中的原始响应")
	case NodeAPIFormatAnthropicMessages:
		var envelope struct {
			Type  string `json:"type"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
			Usage      struct {
				InputTokens  *int64 `json:"input_tokens"`
				OutputTokens *int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return "", fail("upstream_protocol_error", "上游响应不是有效 JSON："+err.Error())
		}
		attempt.InputTokens, attempt.OutputTokens = envelope.Usage.InputTokens, envelope.Usage.OutputTokens
		if envelope.Error.Message != "" {
			return "", fail("upstream_api_error", "上游返回业务错误："+envelope.Error.Message)
		}
		if envelope.StopReason == "refusal" {
			return "", fail("upstream_refused", "上游模型拒绝执行审核")
		}
		if envelope.StopReason == "max_tokens" {
			return "", fail("upstream_incomplete", "上游模型因长度限制未完成审核")
		}
		for _, part := range envelope.Content {
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				return part.Text, nil
			}
		}
		return "", fail("upstream_protocol_error", "上游响应缺少文本 content；请查看调用详情中的原始响应")
	case NodeAPIFormatGoogleGenerativeAI:
		var envelope struct {
			PromptFeedback struct {
				BlockReason string `json:"blockReason"`
			} `json:"promptFeedback"`
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text    string `json:"text"`
						Thought bool   `json:"thought"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata struct {
				PromptTokenCount     *int64 `json:"promptTokenCount"`
				CandidatesTokenCount *int64 `json:"candidatesTokenCount"`
			} `json:"usageMetadata"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return "", fail("upstream_protocol_error", "上游响应不是有效 JSON："+err.Error())
		}
		attempt.InputTokens, attempt.OutputTokens = envelope.UsageMetadata.PromptTokenCount, envelope.UsageMetadata.CandidatesTokenCount
		if envelope.PromptFeedback.BlockReason != "" {
			return "", fail("upstream_refused", "上游模型拒绝执行审核："+envelope.PromptFeedback.BlockReason)
		}
		if len(envelope.Candidates) == 0 {
			return "", fail("upstream_protocol_error", "上游响应缺少 candidates；请查看调用详情中的原始响应")
		}
		candidate := envelope.Candidates[0]
		switch candidate.FinishReason {
		case "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII":
			return "", fail("upstream_refused", "上游模型的安全过滤阻止了本次审核（"+candidate.FinishReason+"）")
		case "MAX_TOKENS":
			return "", fail("upstream_incomplete", "上游模型因长度限制未完成审核")
		}
		for _, part := range candidate.Content.Parts {
			// Gemini 思考片段也会带 text，不能当作审核结论。
			if !part.Thought && strings.TrimSpace(part.Text) != "" {
				return part.Text, nil
			}
		}
		return "", fail("upstream_protocol_error", "上游响应缺少文本 parts；请查看调用详情中的原始响应")
	default:
		return parseChatCompletionsContent(raw, attempt)
	}
}

// parseChatCompletionsContent 解析 OpenAI Chat Completions 响应。
func parseChatCompletionsContent(raw []byte, attempt *ModelAttempt) (string, *AuditError) {
	fail := func(code, message string) *AuditError {
		return &AuditError{Code: code, Stage: "response_parse", Message: message}
	}
	var envelope struct {
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
				Refusal string          `json:"refusal"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     *int64 `json:"prompt_tokens"`
			CompletionTokens *int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fail("upstream_protocol_error", "上游响应不是有效 JSON："+err.Error())
	}
	if envelope.PromptFeedback.BlockReason != "" {
		return "", fail("upstream_refused", "上游模型拒绝执行审核："+envelope.PromptFeedback.BlockReason)
	}
	if len(envelope.Choices) == 0 {
		return "", fail("upstream_protocol_error", "上游响应缺少 choices 或 choices 为空；请查看调用详情中的原始响应")
	}
	choice := envelope.Choices[0]
	attempt.InputTokens, attempt.OutputTokens = envelope.Usage.PromptTokens, envelope.Usage.CompletionTokens
	if choice.Message.Refusal != "" {
		return "", fail("upstream_refused", "上游模型拒绝执行审核："+choice.Message.Refusal)
	}
	if choice.FinishReason == "content_filter" {
		return "", fail("upstream_refused", "上游模型的安全过滤阻止了本次审核（content_filter）")
	}
	if choice.FinishReason == "length" {
		return "", fail("upstream_incomplete", "上游模型因长度限制未完成审核")
	}
	var content string
	if err := json.Unmarshal(choice.Message.Content, &content); err == nil && strings.TrimSpace(content) != "" {
		return content, nil
	}
	// 部分 OpenAI 兼容网关用内容片段数组返回文本，按顺序拼接首个非空文本。
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(choice.Message.Content, &parts); err == nil {
		for _, part := range parts {
			if strings.TrimSpace(part.Text) != "" {
				return part.Text, nil
			}
		}
	}
	return "", fail("upstream_protocol_error", "上游 choices 消息未包含非空文本 content")
}
