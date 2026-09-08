package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"time"

	"github.com/gin-gonic/gin"
	"sub2api-enhance/internal/pkg/response"
)

type AuditUser struct {
	ID                    int64      `json:"id"`
	Username              string     `json:"username"`
	Email                 string     `json:"email"`
	Role                  string     `json:"role"`
	Status                string     `json:"status"`
	DisableViolationCount int64      `json:"disable_violation_count"`
	DisableResetAt        *time.Time `json:"disable_reset_at"`
	ActionPending         bool       `json:"action_pending"`
}

// ListAuditUsers 返回可配置排除项的全部未删除用户，并附带增强审核累计状态。
func (r *Repository) ListAuditUsers(ctx context.Context) ([]AuditUser, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT u.id,u.username,u.email,u.role,u.status,
 COALESCE(s.disable_violation_count,0),s.disable_reset_at,
 EXISTS(SELECT 1 FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions a
        WHERE a.user_id=u.id AND a.action_type='counter_reset' AND a.execution_status IN ('pending','processing','unknown'))
 FROM public.users u
 LEFT JOIN sub2api_enhance.third_party_prompt_audit_enforcement_states s ON s.user_id=u.id
 WHERE u.deleted_at IS NULL
 ORDER BY lower(u.username),u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]AuditUser, 0)
	for rows.Next() {
		var user AuditUser
		var resetAt sql.NullTime
		if err := rows.Scan(&user.ID, &user.Username, &user.Email, &user.Role, &user.Status, &user.DisableViolationCount, &resetAt, &user.ActionPending); err != nil {
			return nil, err
		}
		if resetAt.Valid {
			user.DisableResetAt = &resetAt.Time
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (h *AdminHandler) ListAuditUsers(c *gin.Context) {
	users, err := h.repo.ListAuditUsers(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, users)
}
