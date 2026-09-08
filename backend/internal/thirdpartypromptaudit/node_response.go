package thirdpartypromptaudit

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parseNodeResponse 区分服务商错误、明确拒绝、协议异常与可修正的评分格式错误。
func parseNodeResponse(raw []byte, attempt *ModelAttempt) (*Score, *AuditError) {
	fail := func(code, message string) (*Score, *AuditError) {
		return nil, &AuditError{Code: code, Stage: "response_parse", Message: message}
	}
	var envelope struct {
		Error          json.RawMessage `json:"error"`
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
	if len(strings.TrimSpace(string(raw))) == 0 {
		return fail("upstream_protocol_error", "上游返回空响应，未取得审核结果")
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fail("upstream_protocol_error", "上游响应不是有效 JSON："+err.Error())
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return fail("upstream_api_error", "上游返回业务错误："+string(envelope.Error))
	}
	if envelope.PromptFeedback.BlockReason != "" {
		return fail("upstream_refused", "上游模型拒绝执行审核："+envelope.PromptFeedback.BlockReason)
	}
	if len(envelope.Choices) == 0 {
		return fail("upstream_protocol_error", "上游响应缺少 choices 或 choices 为空；请查看调用详情中的原始响应")
	}
	choice := envelope.Choices[0]
	attempt.InputTokens, attempt.OutputTokens = envelope.Usage.PromptTokens, envelope.Usage.CompletionTokens
	if choice.Message.Refusal != "" {
		return fail("upstream_refused", "上游模型拒绝执行审核："+choice.Message.Refusal)
	}
	if choice.FinishReason == "content_filter" {
		return fail("upstream_refused", "上游模型的安全过滤阻止了本次审核（content_filter）")
	}
	if choice.FinishReason == "length" {
		return fail("upstream_incomplete", "上游模型因长度限制未完成审核")
	}
	var content string
	if err := json.Unmarshal(choice.Message.Content, &content); err != nil || strings.TrimSpace(content) == "" {
		return fail("upstream_protocol_error", "上游 choices 消息未包含非空文本 content")
	}
	score, err := ParseScore(content)
	if err != nil {
		return fail("invalid_response", fmt.Sprintf("审核评分 JSON 格式无效：%s", err))
	}
	return &score, nil
}
