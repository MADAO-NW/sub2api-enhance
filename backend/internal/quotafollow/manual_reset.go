package quotafollow

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"log"
)

// 立即重置请求的参数校验错误。
var (
	errImmediateResetInvalidGroup = errors.New("目标分组无效")
	errImmediateResetNoWindow     = errors.New("至少选择一个重置窗口")
	errImmediateResetWindow       = errors.New("重置窗口只能是 daily 或 weekly")
)

type ImmediateResetRequest struct {
	GroupID int64    `json:"group_id"`
	Windows []string `json:"windows"`
}

type ImmediateResetItem struct {
	UserID     int64  `json:"user_id"`
	Username   string `json:"username"`
	Window     string `json:"window"`
	RequestID  string `json:"request_id"`
	Status     string `json:"status"`
	HTTPStatus int    `json:"http_status"`
	Error      string `json:"error"`
}

type ImmediateResetResult struct {
	GroupID    int64                `json:"group_id"`
	Windows    []string             `json:"windows"`
	Total      int                  `json:"total"`
	Succeeded  int                  `json:"succeeded"`
	NonSuccess int                  `json:"non_success"`
	Items      []ImmediateResetItem `json:"items"`
}

func normalizeResetWindows(windows []string) ([]string, error) {
	seen := make(map[string]bool, len(windows))
	result := make([]string, 0, len(windows))
	for _, window := range windows {
		if window != "daily" && window != "weekly" {
			return nil, errImmediateResetWindow
		}
		if !seen[window] {
			seen[window] = true
			result = append(result, window)
		}
	}
	if len(result) == 0 {
		return nil, errImmediateResetNoWindow
	}
	return result, nil
}

// ImmediateReset 按管理员明确选择的分组和窗口逐项调用原版额度重置接口。
func (s *Service) ImmediateReset(ctx context.Context, input ImmediateResetRequest) (ImmediateResetResult, error) {
	result := ImmediateResetResult{GroupID: input.GroupID}
	windows, err := normalizeResetWindows(input.Windows)
	if err != nil {
		return result, err
	}
	result.Windows = windows
	if input.GroupID <= 0 {
		return result, errImmediateResetInvalidGroup
	}
	if !s.api.Configured() {
		return result, errors.New("原版管理员 API Key 未配置")
	}
	discovery, err := s.source.Discover(ctx, input.GroupID)
	if err != nil {
		return result, err
	}
	result.Items = make([]ImmediateResetItem, 0, len(discovery.Users)*len(windows))
	for _, user := range discovery.Users {
		for _, window := range windows {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			requestID := uuid.NewString()
			reply := s.api.ResetQuota(ctx, user.ID, window, requestID)
			item := ImmediateResetItem{UserID: user.ID, Username: user.Username, Window: window, RequestID: requestID, Status: deliveryStatus(reply), HTTPStatus: reply.HTTPStatus}
			if reply.Error != nil {
				item.Error = reply.Error.Error()
			}
			result.Items = append(result.Items, item)
			result.Total++
			if item.Status == "succeeded" {
				result.Succeeded++
			} else {
				result.NonSuccess++
			}
			log.Printf("立即重置额度完成 user_id=%d window=%s request_id=%s status=%s http_status=%d error=%v", item.UserID, item.Window, item.RequestID, item.Status, item.HTTPStatus, reply.Error)
		}
	}
	return result, nil
}
