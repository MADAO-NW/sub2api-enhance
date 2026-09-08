package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	infraerrors "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/pkg/logger"
)

// modelListInvalidReason 是模型列表响应协议错误的稳定错误码。
const modelListInvalidReason = "third_party_audit_invalid_model_list"

type ModelCatalogRequest struct {
	ActorUserID int64  `json:"-"`
	ModelID     string `json:"model_id" binding:"required"`
	BaseURL     string `json:"base_url" binding:"required"`
	TimeoutMS   int    `json:"timeout_ms" binding:"required,min=1"`
	KeyAction   string `json:"key_action" binding:"required,oneof=keep replace"`
	APIKey      string `json:"api_key"`
}

type ModelCatalogResult struct {
	Models []string `json:"models"`
}

// ListModels 使用节点当前或新输入的凭据读取 OpenAI 兼容模型列表。
func (s *Service) ListModels(ctx context.Context, input ModelCatalogRequest) (*ModelCatalogResult, error) {
	if input.TimeoutMS <= 0 || int64(input.TimeoutMS) > math.MaxInt64/int64(time.Millisecond) {
		return nil, infraerrors.BadRequest("third_party_audit_invalid_model_timeout", "节点超时必须是有效的正毫秒数")
	}
	endpoint, err := modelsURL(input.BaseURL)
	if err != nil {
		return nil, infraerrors.BadRequest("third_party_audit_invalid_model_url", err.Error())
	}
	if s.config.publicOrigin != "" && strings.HasPrefix(endpoint, s.config.publicOrigin+"/") {
		return nil, infraerrors.BadRequest("audit_node_recursion", "审核节点不能指向增强公共入口")
	}
	key := input.APIKey
	if input.KeyAction == "keep" {
		if key != "" {
			return nil, infraerrors.BadRequest("third_party_audit_invalid_key", "凭据留空时才能保留已保存值")
		}
		key, err = s.config.ResolveKeyByModelID(input.ModelID)
		if err != nil {
			return nil, infraerrors.ServiceUnavailable("third_party_audit_config_unavailable", err.Error())
		}
	} else if key == "" {
		return nil, infraerrors.BadRequest("third_party_audit_invalid_key", "使用新凭据获取模型时 API Key 不能为空")
	}
	client, err := newNodeHTTPClient(ModelConfig{BaseURL: input.BaseURL, TimeoutMS: input.TimeoutMS})
	if err != nil {
		return nil, infraerrors.BadRequest("third_party_audit_invalid_model_url", err.Error())
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, infraerrors.BadRequest("third_party_audit_invalid_model_url", err.Error())
	}
	request.Header.Set("Accept", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	logger.LegacyPrintf("third_party_prompt_audit", "开始获取审核节点模型列表 actor_user_id=%d model_id=%s base_url=%s", input.ActorUserID, input.ModelID, input.BaseURL)
	response, err := client.Do(request)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "third_party_audit_model_list_unavailable", "获取模型列表失败："+err.Error())
	}
	raw, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "third_party_audit_model_list_unavailable", "读取模型列表失败："+readErr.Error())
	}
	if closeErr != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "third_party_audit_model_list_unavailable", "关闭模型列表响应失败："+closeErr.Error())
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := fmt.Sprintf("模型列表接口返回 HTTP %d", response.StatusCode)
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			message += "，请检查 API Key 是否填写或仍然有效"
		}
		return nil, infraerrors.New(http.StatusBadGateway, "third_party_audit_model_list_http_error", message)
	}
	var envelope struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, modelListInvalidReason, "模型列表响应不是有效的 OpenAI 兼容 JSON")
	}
	models := make([]string, 0, len(envelope.Data))
	seen := make(map[string]bool, len(envelope.Data))
	for _, item := range envelope.Data {
		id := strings.TrimSpace(item.ID)
		if id != "" && !seen[id] {
			seen[id] = true
			models = append(models, id)
		}
	}
	if len(models) == 0 {
		return nil, infraerrors.New(http.StatusBadGateway, modelListInvalidReason, "模型列表接口没有返回可选模型")
	}
	logger.LegacyPrintf("third_party_prompt_audit", "审核节点模型列表获取完成 actor_user_id=%d model_id=%s model_count=%d", input.ActorUserID, input.ModelID, len(models))
	return &ModelCatalogResult{Models: models}, nil
}
