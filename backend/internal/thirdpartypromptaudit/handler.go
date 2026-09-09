package thirdpartypromptaudit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	infraerrors "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/pkg/response"
	"sub2api-enhance/internal/server/middleware"
)

type AdminHandler struct {
	captures *CaptureStore
	service  *Service
	config   *ConfigManager
	repo     *Repository
}

func NewAdminHandler(service *Service, config *ConfigManager, repo *Repository, captures *CaptureStore) *AdminHandler {
	return &AdminHandler{service: service, config: config, repo: repo, captures: captures}
}

// bindStrict 在配置和管理写入的接入边界拒绝重复字段、固定协议字段和未知属性。
func bindStrict(c *gin.Context, target any) error {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return err
	}
	if _, err = decodeUniqueJSON(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		return err
	}
	return binding.Validator.ValidateStruct(target)
}

func adminActor(c *gin.Context) int64 {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		return 0
	}
	return subject.UserID
}

func respondError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		err = infraerrors.NotFound("third_party_audit_not_found", err.Error())
	case errors.Is(err, ErrLeaseLost):
		err = infraerrors.Conflict("third_party_audit_state_conflict", "记录正在处理或状态已经改变，请刷新后重试")
	}
	response.ErrorFrom(c, err)
}

func (h *AdminHandler) GetConfig(c *gin.Context) {
	config, err := h.config.ReadSaved(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	h.config.mu.RLock()
	if h.config.active != nil {
		config.AppliedRevision = h.config.active.Stored.Revision
	}
	h.config.mu.RUnlock()
	config.InstanceID = h.service.metrics.InstanceID
	response.Success(c, config)
}

func (h *AdminHandler) UpdateConfig(c *gin.Context) {
	var input ConfigUpdate
	if err := bindStrict(c, &input); err != nil {
		response.BadRequest(c, "配置请求无效："+err.Error())
		return
	}
	before, _ := h.config.Public()
	result, err := h.config.Save(c.Request.Context(), input, adminActor(c))
	if err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "previous_revision": before.Revision, "revision": result.Revision})
	h.service.notify()
	h.config.mu.RLock()
	if h.config.active != nil {
		result.AppliedRevision = h.config.active.Stored.Revision
	}
	h.config.mu.RUnlock()
	result.InstanceID = h.service.metrics.InstanceID
	response.Success(c, result)
}

func (h *AdminHandler) GetContract(c *gin.Context) {
	response.Success(c, gin.H{"version": ContractVersion, "default_policy": DefaultPolicy, "output_contract": OutputContract})
}

func (h *AdminHandler) ListModels(c *gin.Context) {
	var input ModelCatalogRequest
	if err := bindStrict(c, &input); err != nil {
		response.BadRequest(c, "模型列表请求无效："+err.Error())
		return
	}
	input.ActorUserID = adminActor(c)
	result, err := h.service.ListModels(c.Request.Context(), input)
	if err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"model_id": input.ModelID, "result": "success", "model_count": len(result.Models)})
	response.Success(c, result)
}

func (h *AdminHandler) ProbeModel(c *gin.Context) {
	var input ProbeRequest
	if err := bindStrict(c, &input); err != nil {
		response.BadRequest(c, "节点测试请求无效："+err.Error())
		return
	}
	input.ActorUserID = adminActor(c)
	result := h.service.Probe(c.Request.Context(), input)
	middleware.SetAuditExtra(c, map[string]any{"model_id": input.Model.ID, "attempt_id": result.AttemptID, "result": result.OK})
	response.Success(c, result)
}

func (h *AdminHandler) GetRuntime(c *gin.Context) {
	result := h.service.Runtime(c.Request.Context())
	if h.captures != nil {
		result.CapturePersistFailures = h.captures.failures.Load()
		result.IngressPool = h.captures.db.Stats()
	}
	response.Success(c, result)
}

func (h *AdminHandler) GetStats(c *gin.Context) {
	from, err := time.Parse(time.RFC3339Nano, c.Query("from"))
	if err != nil {
		response.BadRequest(c, "from 必须为含时区的时间")
		return
	}
	to, err := time.Parse(time.RFC3339Nano, c.Query("to"))
	if err != nil || !from.Before(to) {
		response.BadRequest(c, "to 必须晚于 from，并包含时区")
		return
	}
	timezone := c.Query("timezone")
	if timezone == "" {
		response.BadRequest(c, "请指定统计时区")
		return
	}
	if _, err = time.LoadLocation(timezone); err != nil {
		response.BadRequest(c, "统计时区无效")
		return
	}
	query := StatsQuery{From: from, To: to, Timezone: timezone, Mode: c.Query("mode"), ModelID: c.Query("model_id"), Stage: c.Query("stage")}
	if query.Mode != "" && query.Mode != "async" && query.Mode != "blocking" {
		response.BadRequest(c, "运行模式筛选无效")
		return
	}
	if query.Stage != "" && !slices.Contains([]string{"segment", "joint", "format_repair", "probe"}, query.Stage) {
		response.BadRequest(c, "调用阶段筛选无效")
		return
	}
	result, err := h.repo.Stats(c.Request.Context(), query)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

func filterFromQuery(c *gin.Context) (Filter, error) {
	filter := Filter{Status: c.Query("status"), Decision: Decision(c.Query("decision")), RunKind: c.Query("run_kind"), Mode: c.Query("mode"), Platform: c.Query("platform"), RequestID: c.Query("request_id"), Keyword: c.Query("keyword"), ModelID: c.Query("model_id")}
	for _, field := range []struct {
		name   string
		target **int64
	}{{"user_id", &filter.UserID}, {"api_key_id", &filter.APIKeyID}, {"group_id", &filter.GroupID}} {
		if value := c.Query(field.name); value != "" {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id <= 0 {
				return filter, fmt.Errorf("%s 必须为正整数", field.name)
			}
			*field.target = &id
		}
	}
	for _, field := range []struct {
		name   string
		target **time.Time
	}{{"from", &filter.From}, {"to", &filter.To}} {
		if value := c.Query(field.name); value != "" {
			at, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return filter, fmt.Errorf("%s 必须为含时区的时间", field.name)
			}
			*field.target = &at
		}
	}
	if value := c.Query("ids"); value != "" {
		for _, value := range strings.Split(value, ",") {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return filter, errors.New("ID 列表无效")
			}
			filter.IDs = append(filter.IDs, id)
		}
	}
	return filter, validateFilter(filter)
}

func listQuery(c *gin.Context) (Filter, int, int, error) {
	filter, err := filterFromQuery(c)
	if err != nil {
		return filter, 0, 0, err
	}
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page <= 0 {
		return filter, 0, 0, errors.New("页码无效")
	}
	size, err := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if err != nil || size <= 0 || size > 100 {
		return filter, 0, 0, errors.New("每页数量必须为 1 至 100")
	}
	if page > (int(^uint(0)>>1) / size) {
		return filter, 0, 0, errors.New("分页位置超出范围")
	}
	return filter, page, size, nil
}

func (h *AdminHandler) ListJobs(c *gin.Context) {
	filter, page, size, err := listQuery(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	result, err := h.repo.ListJobs(c.Request.Context(), filter, page, size)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

func recordID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "记录 ID 无效")
		return 0, false
	}
	return id, true
}

func (h *AdminHandler) GetJob(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	result, err := h.repo.JobDetail(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

func (h *AdminHandler) ResumeResult(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	if err := h.repo.ResumeResult(c.Request.Context(), id); err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "job_id": id})
	h.service.notify()
	response.Success(c, gin.H{"id": id, "status": "retry"})
}

func (h *AdminHandler) RetryAction(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	if err := h.repo.RetryAction(c.Request.Context(), id); err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "action_id": id})
	h.service.notify()
	response.Success(c, gin.H{"id": id})
}

func (h *AdminHandler) PreviewReaudits(c *gin.Context) {
	var input ReauditRequest
	if err := bindStrict(c, &input); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if err := validateFilter(input.Filter); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	result, err := h.repo.PreviewReaudits(c.Request.Context(), input)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

func (h *AdminHandler) CreateReaudits(c *gin.Context) {
	var input ReauditRequest
	if err := bindStrict(c, &input); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if err := validateFilter(input.Filter); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	result, err := h.service.CreateReaudits(c.Request.Context(), input, adminActor(c))
	if err != nil {
		respondError(c, err)
		return
	}
	counts := map[string]any{"matched_count": result.Matched}
	for _, item := range result.Items {
		key := item.Status + "_count"
		count, _ := counts[key].(int)
		counts[key] = count + 1
	}
	middleware.SetAuditExtra(c, counts)
	response.Accepted(c, result)
}
