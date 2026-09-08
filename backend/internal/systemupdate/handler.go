package systemupdate

import (
	"github.com/gin-gonic/gin"
	infra "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/pkg/response"
	"sub2api-enhance/internal/server/middleware"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }
func (h *Handler) Status(c *gin.Context)   { response.Success(c, h.service.Status()) }
func (h *Handler) Check(c *gin.Context) {
	out, err := h.service.Check(c.Request.Context(), c.Query("force") == "true")
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, out)
}
func (h *Handler) Update(c *gin.Context)   { h.start(c, "update") }
func (h *Handler) Rollback(c *gin.Context) { h.start(c, "rollback") }

// start 领取后台操作与共享认证主体，响应只表示已接受，最终结果由状态查询提供。
func (h *Handler) start(c *gin.Context, action string) {
	var input struct {
		Version string `json:"version" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "缺少已确认的版本号")
		return
	}
	actor, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		c.AbortWithStatus(401)
		return
	}
	if err := h.service.Start(action, actor.UserID, input.Version); err != nil {
		response.ErrorFrom(c, infra.Conflict("system_update_conflict", err.Error()))
		return
	}
	c.JSON(202, gin.H{"code": 0, "data": gin.H{"accepted": true}})
}
func (h *Handler) Restart(c *gin.Context) {
	actor, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		c.AbortWithStatus(401)
		return
	}
	if err := h.service.Restart(actor.UserID); err != nil {
		response.ErrorFrom(c, infra.Conflict("system_update_conflict", err.Error()))
		return
	}
	response.Success(c, gin.H{"restarting": true})
}

func (h *Handler) Health(c *gin.Context) {
	c.JSON(200, gin.H{"status": "ok", "service": "sub2api++", "version": h.service.info.Version, "started_at": h.service.startedAt})
}
