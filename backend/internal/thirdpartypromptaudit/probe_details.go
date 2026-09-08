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
	items, err := h.repo.listAttempts(c.Request.Context(), `call_kind='probe' AND (id=(SELECT COALESCE(repair_of_attempt_id,id) FROM sub2api_enhance.third_party_prompt_audit_model_attempts WHERE id=$1 AND call_kind='probe') OR repair_of_attempt_id=(SELECT COALESCE(repair_of_attempt_id,id) FROM sub2api_enhance.third_party_prompt_audit_model_attempts WHERE id=$1 AND call_kind='probe'))`, id)
	if err != nil {
		respondError(c, err)
		return
	}
	if len(items) == 0 {
		respondError(c, ErrNotFound)
		return
	}
	response.Success(c, items)
}

// UserKeys 仅提供人工关联所需的记录 ID 与名称，不读取真实密钥。
func (h *AdminHandler) UserKeys(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "用户 ID 无效")
		return
	}
	rows, err := h.repo.db.QueryContext(c.Request.Context(), `SELECT k.id,k.name FROM public.api_keys k JOIN public.users u ON u.id=k.user_id AND u.deleted_at IS NULL WHERE k.user_id=$1 AND k.deleted_at IS NULL ORDER BY k.id`, id)
	if err != nil {
		respondError(c, err)
		return
	}
	defer rows.Close()
	items := []gin.H{}
	for rows.Next() {
		var keyID int64
		var name string
		if err := rows.Scan(&keyID, &name); err != nil {
			respondError(c, err)
			return
		}
		items = append(items, gin.H{"id": keyID, "name": name})
	}
	if err := rows.Err(); err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, items)
}
