package thirdpartypromptaudit

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"slices"
	"strconv"
	"sub2api-enhance/internal/pkg/response"
	"sub2api-enhance/internal/sub2api"
	"time"
)

func (h *AdminHandler) ListCaptures(c *gin.Context) {
	page, size, err := paginationQuery(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	filter := CaptureFilter{Keyword: c.Query("keyword"), Protocol: c.Query("protocol"), SnapshotStatus: c.Query("snapshot_status"),
		EligibilityStatus: c.Query("eligibility_status"), ProcessingStatus: c.Query("status"), ForwardingStatus: c.Query("forwarding_status")}
	for _, field := range []struct {
		name   string
		target **int64
	}{{"user_id", &filter.UserID}, {"api_key_id", &filter.APIKeyID}, {"group_id", &filter.GroupID}} {
		if value := c.Query(field.name); value != "" {
			id, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr != nil || id <= 0 {
				response.BadRequest(c, field.name+" 必须为正整数")
				return
			}
			*field.target = &id
		}
	}
	for _, field := range []struct {
		name   string
		target **time.Time
	}{{"from", &filter.From}, {"to", &filter.To}} {
		if value := c.Query(field.name); value != "" {
			at, parseErr := time.Parse(time.RFC3339Nano, value)
			if parseErr != nil {
				response.BadRequest(c, field.name+" 必须为含时区的时间")
				return
			}
			*field.target = &at
		}
	}
	if filter.From != nil && filter.To != nil && !filter.From.Before(*filter.To) {
		response.BadRequest(c, "开始时间必须早于结束时间")
		return
	}
	if filter.SnapshotStatus != "" && !slices.Contains([]string{"complete", "incomplete"}, filter.SnapshotStatus) {
		response.BadRequest(c, "原文完整性状态无效")
		return
	}
	if filter.EligibilityStatus != "" && !slices.Contains([]string{"passed", "unknown", "rejected", "manual"}, filter.EligibilityStatus) {
		response.BadRequest(c, "身份资格状态无效")
		return
	}
	if filter.ProcessingStatus != "" && !slices.Contains([]string{"queued", "processing", "retry", "done", "failed", "skipped", "awaiting_review"}, filter.ProcessingStatus) {
		response.BadRequest(c, "采集处理状态无效")
		return
	}
	result, err := h.captures.List(c.Request.Context(), page, size, filter)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}
func (h *AdminHandler) GetCapture(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "采集 ID 无效")
		return
	}
	result, err := h.captures.Get(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	var jobID int64
	err = h.repo.db.QueryRowContext(c.Request.Context(), `SELECT id FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE capture_id=$1`, id).Scan(&jobID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		respondError(c, err)
		return
	}
	var linked *int64
	if err == nil {
		linked = &jobID
	}
	response.Success(c, struct {
		*Capture
		JobID *int64 `json:"job_id"`
	}{result, linked})
}

func (h *AdminHandler) GetCaptureLatestUserContent(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "采集 ID 无效")
		return
	}
	result, err := h.captures.Get(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, captureLatestUserContent(result))
}
func (h *AdminHandler) DownloadCapture(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "采集 ID 无效")
		return
	}
	result, err := h.captures.Get(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=capture-%d.bin", id))
	c.Data(200, "application/octet-stream", result.Raw)
}
func (h *AdminHandler) ReprocessCapture(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "采集 ID 无效")
		return
	}
	capture, err := h.captures.Get(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	var existing int64
	err = h.repo.db.QueryRowContext(c.Request.Context(), `SELECT id FROM sub2api_enhance.third_party_prompt_audit_jobs WHERE capture_id=$1`, id).Scan(&existing)
	if err == nil {
		_, _ = h.repo.db.ExecContext(c.Request.Context(), `UPDATE sub2api_enhance.captures SET processing_status='done',updated_at=clock_timestamp() WHERE id=$1`, id)
		response.Success(c, gin.H{"job_id": existing})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		respondError(c, err)
		return
	}
	if capture.SnapshotStatus != "complete" {
		response.BadRequest(c, "原文不完整或不可恢复，无法重新提取")
		return
	}
	if capture.ProcessingStatus == "processing" || ((capture.ProcessingStatus == "queued" || capture.ProcessingStatus == "retry") && capture.Metadata["mode"] == "async" && capture.Eligibility == "passed") {
		response.BadRequest(c, "采集正在处理或等待自动重试，请刷新后查看")
		return
	}
	var input struct {
		APIKeyID int64 `json:"api_key_id"`
	}
	if err := c.ShouldBindJSON(&input); err != nil && !errors.Is(err, io.EOF) {
		response.BadRequest(c, "恢复参数无效")
		return
	}
	if capture.Identity.UserID <= 0 {
		if input.APIKeyID <= 0 {
			response.BadRequest(c, "身份未知，请指定用于人工关联的原版 API Key ID；恢复审核不追溯处罚")
			return
		}
		identity, err := sub2api.NewIdentityStore(h.repo.db).ByID(c.Request.Context(), input.APIKeyID, capture.Metadata["client_ip"])
		if err != nil || identity.UserID <= 0 {
			response.BadRequest(c, "该 API Key ID 无法关联有效用户")
			return
		}
		identity.Eligibility = "manual"
		identity.ResolvedAt = time.Now().UTC()
		identity.Reason = "管理员后来关联，仅用于人工审核，不代表请求当时通过鉴权"
		capture.Identity = identity
		capture.Metadata["identity_source"] = "manual"
		capture.Metadata["identity_actor_id"] = strconv.FormatInt(adminActor(c), 10)
		raw, _ := json.Marshal(identity)
		metadata, _ := json.Marshal(capture.Metadata)
		result, err := h.repo.db.ExecContext(c.Request.Context(), `UPDATE sub2api_enhance.captures SET user_id=$2,api_key_id=$3,group_id=$4,identity_snapshot=$5,identity_resolved_at=$6,request_metadata=$7,eligibility_status='unknown',updated_at=clock_timestamp() WHERE id=$1 AND user_id IS NULL`, id, identity.UserID, identity.APIKeyID, identity.GroupID, string(raw), identity.ResolvedAt, string(metadata))
		if err := checkLeaseUpdate(result, err); err != nil {
			respondError(c, err)
			return
		}
	}
	capture.Metadata["background"] = "true"
	capture.Metadata["manual_reprocess"] = "true"
	metadata, err := json.Marshal(capture.Metadata)
	if err != nil {
		respondError(c, err)
		return
	}
	if _, err := h.repo.db.ExecContext(c.Request.Context(), `UPDATE sub2api_enhance.captures SET request_metadata=$2,updated_at=clock_timestamp() WHERE id=$1`, id, string(metadata)); err != nil {
		respondError(c, err)
		return
	}
	result, err := h.service.ReviewCapture(c.Request.Context(), capture, adminActor(c))
	if err != nil {
		respondError(c, err)
		return
	}
	if result == nil || result.JobID <= 0 {
		response.BadRequest(c, "无法创建人工审核任务")
		return
	}
	if _, err := h.repo.db.ExecContext(c.Request.Context(), `UPDATE sub2api_enhance.captures SET processing_status='done',updated_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

func (h *AdminHandler) PreviewAwaitingReviews(c *gin.Context) {
	result, err := h.service.PreviewAwaitingReviews(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}
func (h *AdminHandler) CreateAwaitingReviews(c *gin.Context) {
	batch, err := h.repo.CreateBatch(c.Request.Context(), BatchRequest{Type: BatchPendingReview}, adminActor(c))
	if err != nil {
		respondError(c, err)
		return
	}
	h.service.notify()
	response.Accepted(c, gin.H{"batch_id": batch.ID, "status": batch.Status, "matched": batch.Matched, "ready": batch.Ready})
}
func (h *AdminHandler) PreviewRecoveries(c *gin.Context) {
	result, err := h.service.PreviewRecoveries(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

func (h *AdminHandler) CreateRecoveries(c *gin.Context) {
	batch, err := h.repo.CreateBatch(c.Request.Context(), BatchRequest{Type: BatchFailedRecovery}, adminActor(c))
	if err != nil {
		respondError(c, err)
		return
	}
	h.service.notify()
	response.Accepted(c, gin.H{"batch_id": batch.ID, "status": batch.Status, "matched": batch.Matched, "ready": batch.Ready})
}

func (h *AdminHandler) EnableAndReset(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "用户 ID 无效")
		return
	}
	if h.service.accounts == nil {
		response.BadRequest(c, "账号 API 未配置")
		return
	}
	user, err := h.service.accounts.GetUser(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	if user.Role != "user" {
		response.BadRequest(c, "只允许处理普通用户")
		return
	}
	actionID, err := h.repo.CreateCounterReset(c.Request.Context(), id, adminActor(c))
	if err != nil {
		respondError(c, err)
		return
	}
	response.Accepted(c, gin.H{"action_id": actionID, "execution_status": "pending"})
}
func (h *AdminHandler) ListGroups(c *gin.Context) {
	result, err := sub2api.NewIdentityStore(h.repo.db).Groups(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}
