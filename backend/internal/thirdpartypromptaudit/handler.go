package thirdpartypromptaudit

import (
	"bytes"
	"context"
	"encoding/csv"
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

// exportLimit 限制单次导出条数，避免一次拉取过多目标正文。
const exportLimit = 5000

type AdminHandler struct {
	service *Service
	config  *ConfigManager
	repo    *Repository
}

func NewAdminHandler(service *Service, config *ConfigManager, repo *Repository) *AdminHandler {
	return &AdminHandler{service: service, config: config, repo: repo}
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

// PrometheusMetrics 输出 Prometheus 文本指标。
func (h *AdminHandler) PrometheusMetrics(ctx context.Context) string {
	return h.service.PrometheusMetrics(ctx)
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
	h.service.refreshNodeLimits()
	h.service.evaluator.nodeScheduler().resetHealth()
	h.config.mu.RLock()
	if h.config.active != nil {
		result.AppliedRevision = h.config.active.Stored.Revision
	}
	h.config.mu.RUnlock()
	result.InstanceID = h.service.metrics.InstanceID
	response.Success(c, result)
}

func (h *AdminHandler) TestAdminEmail(c *gin.Context) {
	if err := h.service.TestAdminEmail(c.Request.Context()); err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "operation": "test_admin_email"})
	response.Success(c, gin.H{})
}

func (h *AdminHandler) GetContract(c *gin.Context) {
	response.Success(c, gin.H{"version": ContractVersion, "default_policy": DefaultPolicy, "output_contract": OutputContract, "template_rules": TemplateRules()})
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
	response.Success(c, h.service.Runtime(c.Request.Context()))
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
	query := StatsQuery{From: from, To: to, Timezone: timezone, Mode: c.Query("mode")}
	if query.Mode != "" && query.Mode != "async" && query.Mode != "blocking" {
		response.BadRequest(c, "运行模式筛选无效")
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
	filter := Filter{Status: c.Query("status"), Decision: Decision(c.Query("decision")), RunKind: c.Query("run_kind"),
		Mode: c.Query("mode"), Platform: c.Query("platform"), RequestID: c.Query("request_id"), Model: c.Query("model")}
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
		for _, part := range strings.Split(value, ",") {
			id, err := strconv.ParseInt(part, 10, 64)
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
	page, size, err := paginationQuery(c)
	return filter, page, size, err
}

func paginationQuery(c *gin.Context) (int, int, error) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page <= 0 {
		return 0, 0, errors.New("页码无效")
	}
	size, err := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if err != nil || size <= 0 || size > 200 {
		return 0, 0, errors.New("每页数量必须为 1 至 200")
	}
	if page > (int(^uint(0)>>1) / size) {
		return 0, 0, errors.New("分页位置超出范围")
	}
	return page, size, nil
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

func (h *AdminHandler) GetJobPrompts(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	result, err := h.repo.JobPrompts(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, gin.H{"targets": result})
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

type TrustRequest struct {
	Hash   string `json:"hash" binding:"required,len=64"`
	Reason string `json:"reason" binding:"max=200"`
}

func (h *AdminHandler) ListTrust(c *gin.Context) {
	page, size, err := paginationQuery(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	result, err := h.repo.ListTrust(c.Request.Context(), c.Query("q"), page, size)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

func (h *AdminHandler) TrustInstruction(c *gin.Context) {
	var input TrustRequest
	if err := bindStrict(c, &input); err != nil {
		response.BadRequest(c, "信任请求无效："+err.Error())
		return
	}
	if err := h.repo.TrustInstruction(c.Request.Context(), input.Hash, adminActor(c), input.Reason); err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "hash": input.Hash})
	response.Success(c, gin.H{"hash": input.Hash})
}

func (h *AdminHandler) UntrustInstruction(c *gin.Context) {
	hash := c.Param("hash")
	if len(hash) != 64 {
		response.BadRequest(c, "指令块哈希无效")
		return
	}
	if err := h.repo.UntrustInstruction(c.Request.Context(), hash, adminActor(c)); err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "hash": hash})
	response.Success(c, gin.H{"hash": hash})
}

func (h *AdminHandler) ListBreakers(c *gin.Context) {
	page, size, err := paginationQuery(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	status := c.Query("status")
	if status != "" && !slices.Contains([]string{BreakerTripped, BreakerTrusted, BreakerConfirmed}, status) {
		response.BadRequest(c, "熔断状态筛选无效")
		return
	}
	result, err := h.repo.ListBreakers(c.Request.Context(), status, c.Query("q"), page, size)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

// DrillBreaker 用假哈希演示“只计一次 + 熔断”判定，不写任何业务表。
func (h *AdminHandler) DrillBreaker(c *gin.Context) {
	var input TrustRequest
	if err := bindStrict(c, &input); err != nil {
		response.BadRequest(c, "演练请求无效："+err.Error())
		return
	}
	result, err := DrillBreaker(input.Hash)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

// TrustBatch 批量信任指令块，返回成功信任的哈希。
func (h *AdminHandler) TrustBatch(c *gin.Context) {
	var input struct {
		Hashes []string `json:"hashes" binding:"required,min=1,max=200,dive,len=64"`
		Reason string   `json:"reason" binding:"max=200"`
	}
	if err := bindStrict(c, &input); err != nil {
		response.BadRequest(c, "批量信任请求无效："+err.Error())
		return
	}
	trusted, err := h.repo.TrustInstructions(c.Request.Context(), input.Hashes, adminActor(c), input.Reason)
	if err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "requested": len(input.Hashes), "trusted": len(trusted)})
	response.Success(c, gin.H{"trusted": trusted})
}

// ExportJobs 按当前过滤条件导出 CSV，用于合规取证。
func (h *AdminHandler) ExportJobs(c *gin.Context) {
	filter, err := filterFromQuery(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "1000"))
	if err != nil || limit <= 0 || limit > exportLimit {
		response.BadRequest(c, "limit 必须为 1 至 5000 的整数")
		return
	}
	offset, err := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if err != nil || offset < 0 {
		response.BadRequest(c, "offset 必须为非负整数")
		return
	}
	rows, err := h.repo.ExportJobs(c.Request.Context(), filter, limit, offset)
	if err != nil {
		respondError(c, err)
		return
	}
	if len(rows) == limit {
		c.Header("X-Export-Has-More", "true")
	}
	writeExportCSV(c, rows, "third-party-prompt-audit.csv")
}

// writeExportCSV 输出 UTF-8 CSV，并对公式注入做转义。
func writeExportCSV(c *gin.Context, rows []ExportRow, filename string) {
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename="+filename)
	_, _ = c.Writer.WriteString("\ufeff")
	writer := csv.NewWriter(c.Writer)
	defer writer.Flush()
	_ = writer.Write([]string{"job_id", "created_at", "user_id", "api_key_id", "platform", "protocol", "mode", "status", "decision", "trigger", "request_id",
		"target_order", "target_kind", "resolution", "target_decision", "score", "reason", "bytes", "text"})
	for _, row := range rows {
		order, kind := "", ""
		if row.Order != nil {
			order = strconv.Itoa(*row.Order)
		}
		kind = row.Kind
		score := ""
		if row.Score != nil {
			score = strconv.FormatFloat(*row.Score, 'f', 4, 64)
		}
		_ = writer.Write([]string{strconv.FormatInt(row.JobID, 10), row.CreatedAt.UTC().Format(time.RFC3339), strconv.FormatInt(row.UserID, 10),
			strconv.FormatInt(row.APIKeyID, 10), row.Platform, row.Protocol, row.Mode, row.Status, row.Decision, row.Trigger, row.RequestID,
			order, kind, row.Resolution, row.TargetDecision, score, row.Reason, strconv.Itoa(row.Bytes), csvSafe(row.Text)})
	}
}

// MetricsHistory 返回最近 N 天的小时指标快照。
func (h *AdminHandler) MetricsHistory(c *gin.Context) {
	days, err := strconv.Atoi(c.DefaultQuery("days", "7"))
	if err != nil || days <= 0 || days > 90 {
		response.BadRequest(c, "days 必须为 1 至 90 的整数")
		return
	}
	items, err := h.repo.MetricsHistory(c.Request.Context(), days)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, gin.H{"items": items})
}

// DeleteUserData 删除某用户的审计任务（保留处置、信任与熔断记录）。
func (h *AdminHandler) DeleteUserData(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || userID <= 0 {
		response.BadRequest(c, "用户 ID 无效")
		return
	}
	deleted, err := h.repo.DeleteUserData(c.Request.Context(), userID)
	if err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "user_id": userID, "deleted_jobs": deleted})
	response.Success(c, gin.H{"deleted_jobs": deleted})
}

// ExportUserData 导出单个用户的审计任务 CSV。
func (h *AdminHandler) ExportUserData(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || userID <= 0 {
		response.BadRequest(c, "用户 ID 无效")
		return
	}
	rows, err := h.repo.ExportJobs(c.Request.Context(), Filter{UserID: &userID}, exportLimit, 0)
	if err != nil {
		respondError(c, err)
		return
	}
	writeExportCSV(c, rows, "user-"+strconv.FormatInt(userID, 10)+"-third-party-prompt-audit.csv")
}

// TemplateChurn 展示近 7 天各客户端指令块的唯一哈希占比，用于决定是否补模板规则。
func (h *AdminHandler) TemplateChurn(c *gin.Context) {
	items, err := h.repo.TemplateChurn(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, gin.H{"items": items})
}

// csvSafe 防止单元格被表格软件当作公式执行。
func csvSafe(value string) string {
	if value == "" {
		return value
	}
	if strings.ContainsRune("=+-@\t\r", rune(value[0])) {
		return "'" + value
	}
	return value
}

func (h *AdminHandler) ConfirmBreaker(c *gin.Context) {
	hash := c.Param("hash")
	if len(hash) != 64 {
		response.BadRequest(c, "指令块哈希无效")
		return
	}
	if err := h.repo.ConfirmBreaker(c.Request.Context(), hash, adminActor(c)); err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "hash": hash})
	response.Success(c, gin.H{"hash": hash})
}

// EnableAndReset 通过处置动作发件箱恢复用户并重置累计；已计指令块记录保留（决策 21）。
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
