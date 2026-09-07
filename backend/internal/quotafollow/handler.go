package quotafollow

import (
	"database/sql"
	"errors"
	"github.com/gin-gonic/gin"
	"strconv"
	infra "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/pkg/response"
	"sub2api-enhance/internal/server/middleware"
	"time"
)

type Handler struct {
	service *Service
	repo    *Repository
}

func NewHandler(service *Service, repo *Repository) *Handler {
	return &Handler{service: service, repo: repo}
}
func respondError(c *gin.Context, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		err = infra.NotFound("quota_record_not_found", "额度跟随记录不存在")
	}
	response.ErrorFrom(c, err)
}
func (h *Handler) GetConfig(c *gin.Context) {
	value, err := h.repo.Config(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, value)
}
func (h *Handler) SaveConfig(c *gin.Context) {
	var input ConfigUpdate
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "配置参数无效")
		return
	}
	actor, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		c.AbortWithStatus(401)
		return
	}
	value, err := h.repo.SaveConfig(c.Request.Context(), input, actor.UserID)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, value)
}
func (h *Handler) Runtime(c *gin.Context) {
	value, err := h.service.Runtime(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, value)
}
func (h *Handler) Discovery(c *gin.Context) {
	cfg, err := h.repo.Config(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	if cfg.GroupID == nil {
		response.Success(c, gin.H{"accounts": []any{}, "users": []any{}})
		return
	}
	value, err := h.service.source.Discover(c.Request.Context(), *cfg.GroupID)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, value)
}
func (h *Handler) Records(c *gin.Context) {
	f := Filter{Source: c.Query("source"), Detail: c.Query("source_detail"), Window: c.Query("window"), Status: c.Query("status"), Keyword: c.Query("keyword")}
	var err error
	f.Page, err = strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || f.Page <= 0 {
		response.BadRequest(c, "页码无效")
		return
	}
	f.PageSize, err = strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if err != nil || f.PageSize <= 0 || f.PageSize > 100 {
		response.BadRequest(c, "每页数量必须为 1 至 100")
		return
	}
	for _, entry := range []struct {
		key    string
		target **time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		if raw := c.Query(entry.key); raw != "" {
			value, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				response.BadRequest(c, entry.key+" 必须是含时区时间")
				return
			}
			*entry.target = &value
		}
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		response.BadRequest(c, "结束时间必须晚于开始时间")
		return
	}
	if f.Source != "" && f.Source != "sub2api" && f.Source != "enhance" {
		response.BadRequest(c, "来源筛选无效")
		return
	}
	if f.Window != "" && f.Window != "daily" && f.Window != "weekly" {
		response.BadRequest(c, "窗口筛选无效")
		return
	}
	value, err := h.repo.Records(c.Request.Context(), f)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, value)
}
func (h *Handler) Detail(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "记录 ID 无效")
		return
	}
	value, err := h.repo.Record(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, value)
}
func (h *Handler) Reconcile(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "交付 ID 无效")
		return
	}
	value, err := h.service.Reconcile(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	actor, _ := middleware.GetAuthSubjectFromContext(c)
	middleware.SetAuditExtra(c, map[string]any{"actor_user_id": actor.UserID, "delivery_id": id, "result": "只读复核完成，未重新归零"})
	response.Success(c, value)
}

func (h *Handler) Events(c *gin.Context) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page <= 0 {
		response.BadRequest(c, "页码无效")
		return
	}
	size, err := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if err != nil || size <= 0 || size > 100 {
		response.BadRequest(c, "每页数量必须为 1 至 100")
		return
	}
	value, err := h.repo.Events(c.Request.Context(), page, size)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, value)
}
func (h *Handler) Event(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "事件 ID 无效")
		return
	}
	value, err := h.repo.Event(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, value)
}

func (h *Handler) RepairCache(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "记录 ID 无效")
		return
	}
	actor, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		c.AbortWithStatus(401)
		return
	}
	if err := h.service.RequestCacheRepair(c.Request.Context(), id, actor.UserID); err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, gin.H{"scheduled": true})
}
