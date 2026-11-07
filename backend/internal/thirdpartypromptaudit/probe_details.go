package thirdpartypromptaudit

import (
	"github.com/gin-gonic/gin"
	"strconv"
	"sub2api-enhance/internal/pkg/response"
)

// ProbeDetails 只读取节点测试及其格式修正调用，不混入正式任务。
func (h *AdminHandler) ProbeDetails(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "调用 ID 无效")
		return
	}
	items, err := h.repo.ListModelAttempts(c.Request.Context(), `call_kind='probe' AND (id=(SELECT COALESCE(repair_of_attempt_id,id) FROM sub2api_enhance.model_attempts WHERE id=$1 AND call_kind='probe') OR repair_of_attempt_id=(SELECT COALESCE(repair_of_attempt_id,id) FROM sub2api_enhance.model_attempts WHERE id=$1 AND call_kind='probe'))`, id)
	if err != nil {
		return
	}
	if len(items) == 0 {
		respondError(c, ErrNotFound)
		return
	}
	response.Success(c, items)
}
