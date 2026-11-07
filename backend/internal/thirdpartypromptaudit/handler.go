package thirdpartypromptaudit

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
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

// ListCategories 返回当前政策声明的违规类别编号与名称，供前端在编号后补上标题。
// 名称只来自政策文本里的 “R01 标题” 行，未声明时返回空列表，前端原样显示编号。
func (h *AdminHandler) ListCategories(c *gin.Context) {
	config, err := h.config.ReadSaved(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	names := policyCategoryNames(config.AuditPrompt)
	codes := make([]string, 0, len(names))
	for code := range names {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	items := make([]gin.H, 0, len(codes))
	for _, code := range codes {
		items = append(items, gin.H{"code": code, "name": names[code]})
	}
	response.Success(c, gin.H{"items": items})
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
	h.service.evaluator.nodeScheduler().resetHealthFor(changedNodeIDs(before.Models, result.Models, input.Keys))
	h.config.mu.RLock()
	if h.config.active != nil {
		result.AppliedRevision = h.config.active.Stored.Revision
	}
	h.config.mu.RUnlock()
	result.InstanceID = h.service.metrics.InstanceID
	response.Success(c, result)
}

// changedNodeIDs 返回保存后需要重新验证健康的节点：连接参数变化，或凭据被替换/清除。
// 新加入的节点还没有观测状态，不需要重置；改名、容量、策略等改动不影响连通性。
func changedNodeIDs(before, after []ModelConfig, keys []KeyUpdate) map[string]bool {
	previous := make(map[string]ModelConfig, len(before))
	for _, model := range before {
		previous[model.ID] = model
	}
	changed := map[string]bool{}
	for _, model := range after {
		old, ok := previous[model.ID]
		if !ok {
			continue
		}
		if old.BaseURL != model.BaseURL || old.Model != model.Model || old.TimeoutMS != model.TimeoutMS ||
			old.Enabled != model.Enabled || !reflect.DeepEqual(old.Parameters, model.Parameters) {
			changed[model.ID] = true
		}
	}
	for _, key := range keys {
		if key.Action != "keep" {
			changed[key.ModelID] = true
		}
	}
	return changed
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
		Mode: c.Query("mode"), Platform: c.Query("platform"), RequestID: c.Query("request_id"), Model: c.Query("model"), Category: c.Query("category")}
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

// SkipActionNotification 跳过一条投递失败的通知，不再重试发送。
func (h *AdminHandler) SkipActionNotification(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	if err := h.repo.SkipNotification(c.Request.Context(), id, adminActor(c)); err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "action_id": id, "operation": "skip_notification"})
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
	// 启用并重置同时解除自动加严；未加严时无需额外动作。
	escalationCleared := false
	if _, err := h.repo.ClearEscalation(c.Request.Context(), id, adminActor(c)); err == nil {
		h.config.SetEscalation(id, false)
		escalationCleared = true
	} else if !errors.Is(err, ErrNotFound) {
		respondError(c, err)
		return
	}
	response.Accepted(c, gin.H{"action_id": actionID, "execution_status": "pending", "escalation_cleared": escalationCleared})
}

// ConfigImpact 计算草稿配置保存后对全库审核缓存的影响；只读，不保存配置。
func (h *AdminHandler) ConfigImpact(c *gin.Context) {
	var draft Config
	if err := bindStrict(c, &draft); err != nil {
		response.BadRequest(c, "配置影响请求无效："+err.Error())
		return
	}
	impact, err := h.config.Impact(c.Request.Context(), draft)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, impact)
}

// actionFilterFromQuery 解析处置动作列表的筛选条件。
func actionFilterFromQuery(c *gin.Context) (ActionFilter, error) {
	filter := ActionFilter{NotificationStatus: c.Query("notification_status"), ActionType: c.Query("action_type")}
	// execution_status 支持逗号分隔的多个状态，如 failed,unknown。
	if value := c.Query("execution_status"); value != "" {
		filter.ExecutionStatuses = strings.Split(value, ",")
	}
	if value := c.Query("user_id"); value != "" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return filter, errors.New("user_id 必须为正整数")
		}
		filter.UserID = &id
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
	return filter, nil
}

// ListActions 返回处置动作的分页列表，可按执行与通知状态、类型、用户和时间筛选。
func (h *AdminHandler) ListActions(c *gin.Context) {
	filter, err := actionFilterFromQuery(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	page, size, err := paginationQuery(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	result, err := h.repo.ListActionRecords(c.Request.Context(), filter, page, size)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

// GetInbox 返回待处理工作台的只读聚合。
func (h *AdminHandler) GetInbox(c *gin.Context) {
	result, err := h.service.Inbox(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

// ListRiskUsers 返回时间范围内的用户风险榜。
func (h *AdminHandler) ListRiskUsers(c *gin.Context) {
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
	page, size, err := paginationQuery(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	sortKey := c.DefaultQuery("sort", "blocks")
	if sortKey != "blocks" && sortKey != "violations" && sortKey != "last_block_at" {
		response.BadRequest(c, "排序方式无效")
		return
	}
	result, err := h.repo.RiskUsers(c.Request.Context(), RiskUserQuery{From: from, To: to, Keyword: c.Query("keyword"),
		Sort: sortKey, Page: page, PageSize: size})
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

// GetCostStats 返回审核成本看板：按分桶、节点、调用类型与阶段聚合的调用与 token。
func (h *AdminHandler) GetCostStats(c *gin.Context) {
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
	bucket := c.DefaultQuery("bucket", "day")
	if bucket != "day" && bucket != "hour" {
		response.BadRequest(c, "bucket 只能是 day 或 hour")
		return
	}
	timezone := c.Query("timezone")
	if timezone == "" {
		response.BadRequest(c, "请指定统计时区")
		return
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		response.BadRequest(c, "统计时区无效")
		return
	}
	nodeNames := map[string]string{}
	if snapshot, err := h.config.Active(); err == nil {
		for _, model := range snapshot.Models {
			nodeNames[model.ID] = model.Name
		}
	}
	result, err := h.repo.CostStats(c.Request.Context(), CostQuery{From: from, To: to, Bucket: bucket, Timezone: timezone}, nodeNames)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

// ListSimilarBlocks 返回与指定指令块相似（汉明距离不超过阈值）的块。
func (h *AdminHandler) ListSimilarBlocks(c *gin.Context) {
	hash := c.Param("hash")
	if len(hash) != 64 {
		response.BadRequest(c, "指令块哈希无效")
		return
	}
	result, err := h.repo.SimilarBlocks(c.Request.Context(), hash)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, gin.H{"items": result})
}

// GetSimilarPair 返回指令块与一个相似候选块的全文，供逐行差异对比。
func (h *AdminHandler) GetSimilarPair(c *gin.Context) {
	hash, candidate := c.Param("hash"), c.Param("candidate")
	if len(hash) != 64 || len(candidate) != 64 {
		response.BadRequest(c, "指令块哈希无效")
		return
	}
	result, err := h.repo.SimilarPair(c.Request.Context(), hash, candidate)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

// CreateSample 把一个任务目标加入误判样本集；hash 由后端从任务目标读取。
func (h *AdminHandler) CreateSample(c *gin.Context) {
	var input SampleInput
	if err := bindStrict(c, &input); err != nil {
		response.BadRequest(c, "样本请求无效："+err.Error())
		return
	}
	if input.JobID <= 0 || input.TargetOrder <= 0 {
		response.BadRequest(c, "样本必须引用有效的任务与目标序号")
		return
	}
	sample, err := h.repo.CreateSample(c.Request.Context(), input, adminActor(c))
	if err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "sample_id": sample.ID, "hash": sample.Hash})
	response.Success(c, sample)
}

// ListSamples 返回分页的误判样本。
func (h *AdminHandler) ListSamples(c *gin.Context) {
	page, size, err := paginationQuery(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	result, err := h.repo.ListSamples(c.Request.Context(), c.Query("kind"), c.Query("expected"), page, size)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, result)
}

// UpdateSample 修改样本的期望结论与备注。
func (h *AdminHandler) UpdateSample(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	var input struct {
		Expected string `json:"expected" binding:"required,oneof=pass block"`
		Note     string `json:"note" binding:"max=500"`
	}
	if err := bindStrict(c, &input); err != nil {
		response.BadRequest(c, "样本更新请求无效："+err.Error())
		return
	}
	if err := h.repo.UpdateSample(c.Request.Context(), id, input.Expected, input.Note); err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, gin.H{"id": id})
}

// DeleteSample 删除一个误判样本。
func (h *AdminHandler) DeleteSample(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	if err := h.repo.DeleteSample(c.Request.Context(), id); err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "sample_id": id})
	response.Success(c, gin.H{"id": id})
}

// StartPolicyReplay 用草稿配置回放误判样本；不建任务、不写审核结果。
func (h *AdminHandler) StartPolicyReplay(c *gin.Context) {
	var input PolicyReplayRequest
	if err := bindStrict(c, &input); err != nil {
		response.BadRequest(c, "政策回放请求无效："+err.Error())
		return
	}
	replay, err := h.service.StartPolicyReplay(c.Request.Context(), input, adminActor(c))
	if err != nil {
		respondError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "replay_id": replay.ID, "samples": len(replay.SampleIDs)})
	response.Accepted(c, replay)
}

// ListPolicyReplays 返回最近的政策回放记录。
func (h *AdminHandler) ListPolicyReplays(c *gin.Context) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if err != nil || limit <= 0 || limit > 100 {
		response.BadRequest(c, "limit 必须为 1 至 100 的整数")
		return
	}
	items, err := h.repo.ListReplays(c.Request.Context(), limit)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, gin.H{"items": items})
}

// GetPolicyReplay 返回一次回放的进度与结果。
func (h *AdminHandler) GetPolicyReplay(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	replay, err := h.repo.GetReplay(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	response.Success(c, replay)
}

// ClearEscalation 解除用户的自动加严；只记录动作，不发通知。
func (h *AdminHandler) ClearEscalation(c *gin.Context) {
	id, ok := recordID(c)
	if !ok {
		return
	}
	actionID, err := h.repo.ClearEscalation(c.Request.Context(), id, adminActor(c))
	if err != nil {
		respondError(c, err)
		return
	}
	h.config.SetEscalation(id, false)
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "user_id": id, "action_id": actionID})
	response.Success(c, gin.H{"user_id": id, "action_id": actionID})
}
