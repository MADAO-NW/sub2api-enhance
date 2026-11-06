package quotafollow

import (
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	infra "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/pkg/response"
	"sub2api-enhance/internal/server/middleware"
)

type Handler struct {
	service *Service
	repo    *Repository
}

func NewHandler(s *Service, r *Repository) *Handler { return &Handler{service: s, repo: r} }
func respondError(c *gin.Context, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		err = infra.NotFound("quota_record_not_found", "订阅跟随记录不存在")
	}
	response.ErrorFrom(c, err)
}
func (h *Handler) GetConfig(c *gin.Context) {
	v, err := h.repo.Config(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, v)
}
func (h *Handler) SaveConfig(c *gin.Context) {
	var input ConfigUpdate
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "配置参数无效")
		return
	}
	if err := input.Config.Validate(); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	for _, g := range input.Config.Groups {
		if _, err := h.service.source.DiscoverSubscriptions(c.Request.Context(), g.GroupID); err != nil {
			response.BadRequest(c, err.Error())
			return
		}
	}
	actor, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		c.AbortWithStatus(401)
		return
	}
	v, err := h.repo.SaveConfig(c.Request.Context(), input, actor.UserID)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, v)
}
func (h *Handler) Runtime(c *gin.Context) {
	v, err := h.service.Runtime(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, v)
}
func (h *Handler) Discovery(c *gin.Context) {
	id, err := strconv.ParseInt(c.Query("group_id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "分组 ID 必须为正整数")
		return
	}
	v, err := h.service.source.DiscoverSubscriptions(c.Request.Context(), id)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, v)
}
func (h *Handler) ImmediateReset(c *gin.Context) {
	var input ImmediateResetRequest
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "重置参数无效")
		return
	}
	v, err := h.service.ImmediateReset(c.Request.Context(), input)
	if err != nil {
		respondError(c, err)
		return
	}
	actor, _ := middleware.GetAuthSubjectFromContext(c)
	middleware.SetAuditExtra(c, map[string]any{"actor_user_id": actor.UserID, "group_id": input.GroupID, "event_id": v.EventID, "result": "已提交订阅重置任务"})
	response.Accepted(c, v)
}
func pagination(c *gin.Context) (int, int, error) {
	page, e1 := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, e2 := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if e1 != nil || e2 != nil || page <= 0 || size <= 0 || size > 100 || page > int(^uint(0)>>1)/size {
		return 0, 0, errors.New("页码或每页数量无效，每页最多 100 条")
	}
	return page, size, nil
}
func (h *Handler) Records(c *gin.Context) {
	f := Filter{Source: c.Query("source"), Detail: c.Query("source_detail"), Window: c.Query("window"), Status: c.Query("status"), Keyword: c.Query("keyword")}
	var err error
	f.Page, f.PageSize, err = pagination(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if value := c.Query("group_id"); value != "" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "分组 ID 必须为正整数")
			return
		}
		f.GroupID = &id
	}
	for _, v := range []struct {
		key    string
		target **time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		if raw := c.Query(v.key); raw != "" {
			at, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				response.BadRequest(c, "筛选时间必须包含时区")
				return
			}
			*v.target = &at
		}
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		response.BadRequest(c, "开始时间必须早于结束时间")
		return
	}
	if f.Window != "" && f.Window != "daily" && f.Window != "weekly" {
		response.BadRequest(c, "窗口无效")
		return
	}
	if f.Source != "" && f.Source != "enhance" && f.Source != "sub2api" {
		response.BadRequest(c, "来源无效")
		return
	}
	v, err := h.repo.Records(c.Request.Context(), f)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, v)
}
func recordID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "记录 ID 无效")
		return 0, false
	}
	return id, true
}
func (h *Handler) Detail(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	v, err := h.repo.Record(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, v)
}
func (h *Handler) Reconcile(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	v, err := h.service.Reconcile(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, v)
}
func (h *Handler) Events(c *gin.Context) {
	page, size, err := pagination(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	v, err := h.repo.Events(c.Request.Context(), page, size)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, v)
}
func (h *Handler) Event(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	v, err := h.repo.Event(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, v)
}

func (h *Handler) Retry(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	v, err := h.service.RetryFailed(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"batch_id": id, "event_id": v.EventID, "result": "已提交失败订阅补充重置任务"})
	response.Accepted(c, v)
}
func (h *Handler) Acknowledge(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	var input struct {
		Note string `json:"note" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "请填写人工核对说明")
		return
	}
	actor, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		c.AbortWithStatus(401)
		return
	}
	if err := h.service.AcknowledgeUncertain(c.Request.Context(), id, actor.UserID, input.Note); err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, gin.H{"status": "reviewed"})
}
