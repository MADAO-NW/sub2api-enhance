package thirdpartypromptaudit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// reason 前缀解析：多类别、全角/半角冒号、正常、无前缀与未声明编号。
func TestReasonCategoriesParsesDeclaredPrefixes(t *testing.T) {
	declared := map[string]string{"R01": "恶意使用", "R07": "恶意网络活动与关键基础设施", "R12": "其他"}
	cases := []struct {
		reason string
		want   []string
	}{
		{"违规/R07：扫描内网资产", []string{"R07"}},
		{"违规/R01、R07: 同时涉及多个类别", []string{"R01", "R07"}},
		{"待复核/R12：需要人工判断", []string{"R12"}},
		{"指令风险/R01/R07：多个", []string{"R01", "R07"}},
		{"违规/R99：政策未声明的编号", nil},
		{"正常/说明性内容：没有类别", nil},
		{"没有前缀但提到了 R07", nil},
		{"待复核/R12：需要人工判断", []string{"R12"}},
		{"待复核/R12 没有冒号", nil},
		{"", nil},
	}
	for _, test := range cases {
		require.Equal(t, test.want, reasonCategories(test.reason, declared), test.reason)
	}
	// declared 为 nil 时只校验格式，供运行诊断统计识别率。
	require.Equal(t, []string{"R07"}, reasonCategories("违规/R07：扫描", nil))
	require.Nil(t, reasonCategories("正常/无类别：说明", nil))
}

// 政策中的类别名称按行解析；自定义政策没有该格式时只显示编号。
func TestPolicyCategoryNamesOnlyReadsPolicyTitles(t *testing.T) {
	names := policyCategoryNames("## 五、违规类别\nR01 恶意使用\nR07 恶意网络活动与关键基础设施\n- R12 其他")
	require.Equal(t, "恶意使用", names["R01"])
	require.Equal(t, "恶意网络活动与关键基础设施", names["R07"])
	require.Empty(t, policyCategoryNames("普通政策文本，没有类别标题"))
}

// 目标类别按任务冻结的阈值计算：block 取投 block 票的节点，review 取达到复核阈值的节点。
func TestTargetCategoriesFollowTaskThresholds(t *testing.T) {
	config := testConfig()
	config.AuditPrompt = "R01 恶意使用\nR07 恶意网络活动与关键基础设施"
	block, review := 0.8, 0.5
	low := 0.1
	nodes := []NodeResult{
		{ModelID: "a", Confidence: &block, Reason: "违规/R07：扫描内网"},
		{ModelID: "b", Confidence: &review, Reason: "违规/R01：尝试越权"},
		{ModelID: "c", Confidence: &low, Reason: "正常/没有风险"},
	}
	require.Equal(t, []string{"R07"}, targetCategories(nodes, config, DecisionBlock))
	require.Equal(t, []string{"R01", "R07"}, targetCategories(nodes, config, DecisionReview))
	require.Nil(t, targetCategories(nodes, config, DecisionPass))
	// 任务冻结更高的阻断阈值后，同一批节点只按本任务阈值计算。
	config.BlockThreshold = &review
	require.Equal(t, []string{"R01", "R07"}, targetCategories(nodes, config, DecisionBlock))
}

// 类别处置：立即停用优先，其次仅通知，未配置按普通规则。
func TestCategoryDispositionPrefersImmediateDisable(t *testing.T) {
	config := testConfig()
	config.CategoryRules = map[string]string{"R01": CategoryDispositionNoticeOnly, "R07": CategoryDispositionImmediateDisable}
	require.Equal(t, CategoryDispositionImmediateDisable, categoryDisposition(config, []string{"R01", "R07"}))
	require.Equal(t, CategoryDispositionNoticeOnly, categoryDisposition(config, []string{"R01"}))
	require.Empty(t, categoryDisposition(config, []string{"R12"}))
	require.Empty(t, categoryDisposition(config, nil))
}

// 配置指纹只随政策与启用节点语义变化，并统计窗口内未被信任的指令块。
func TestConfigImpactComparesPolicyAndNodeSemantics(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`count\(DISTINCT t.hash\)`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(9))
	manager := &ConfigManager{db: db, active: &activeConfig{Stored: storedConfig{Config: testConfig(), Revision: 2}}}
	impact, err := manager.Impact(context.Background(), testConfig())
	require.NoError(t, err)
	require.False(t, impact.InstructionChanged)
	require.False(t, impact.UserChanged)
	require.Empty(t, impact.ChangedParts)
	require.EqualValues(t, 9, impact.RecentInstructionBlocks)
	require.Equal(t, 24, impact.WindowHours)

	draft := testConfig()
	draft.AuditPrompt += "\n补充说明"
	draft.Models[0].TimeoutMS = 1234
	mock.ExpectQuery(`count\(DISTINCT t.hash\)`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(9))
	impact, err = manager.Impact(context.Background(), draft)
	require.NoError(t, err)
	require.True(t, impact.InstructionChanged)
	require.True(t, impact.UserChanged)
	require.Equal(t, []string{"policy", "nodes"}, impact.ChangedParts)
	require.NoError(t, mock.ExpectationsWereMet())
}

// fresh 重审只复用发起之后产生的结论。
func TestFindReusableReviewFiltersByReuseAfter(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	columns := []string{"id", "hash", "eval_hash", "run_kind", "decision", "score", "reason", "node_results", "reusable", "requested_by", "origin_job_id", "created_at"}
	cutoff := time.Now().Add(-time.Minute)
	mock.ExpectQuery(`created_at>=\$3`).WithArgs("hash-1", "eval-1", cutoff).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(3, "hash-1", "eval-1", "reaudit", "block", 0.9, encodeStoredText("违规/R07：扫描"), `[]`, true, nil, nil, time.Now()))
	review, err := NewRepository(db).FindReusableReview(context.Background(), "hash-1", "eval-1", &cutoff)
	require.NoError(t, err)
	require.Equal(t, int64(3), review.ID)
	require.Equal(t, DecisionBlock, review.Decision)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 批量重审整批共用一个发起时间：每个追加的 fresh 轮次写入同一个 reuse_after。
func TestCreateReauditsSharesOneReuseAfterAcrossBatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	requestedAt := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT j.id,j.user_id,j.status IN`).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "ready"}).AddRow(11, 7, true).AddRow(12, 8, true))
	mock.ExpectQuery(`SELECT count\(DISTINCT t.hash\)`).WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT clock_timestamp\(\)`).WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(requestedAt))
	for _, jobID := range []int64{11, 12} {
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT status,current_round`).WithArgs(jobID).WillReturnRows(sqlmock.NewRows([]string{"status", "current_round"}).AddRow("done", 1))
		mock.ExpectExec(`INSERT INTO sub2api_enhance.audit_job_targets`).WithArgs(jobID, 1, 2).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`reuse_mode='fresh',reuse_after=\$7`).WithArgs(jobID, 2, int64(99), int64(4), sqlmock.AnyArg(), MaxEvaluationAttempts, requestedAt).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
	}
	snapshot := ConfigSnapshot{Config: testConfig(), Revision: 4}
	result, err := NewRepository(db).CreateReaudits(context.Background(), ReauditRequest{}, snapshot, 99)
	require.NoError(t, err)
	require.Equal(t, []string{"requeued", "requeued"}, []string{result.Items[0].Status, result.Items[1].Status})
	require.NoError(t, mock.ExpectationsWereMet())
}

// 重审预览按目标去重并排除已信任指令块，返回预计真实调用数。
func TestPreviewReauditsEstimatesTargets(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT count\(DISTINCT t.hash\)`).WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))
	result, err := NewRepository(db).previewWithCandidates(context.Background(), []reauditCandidate{{ID: 11, UserID: 7, Ready: true}})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Ready)
	require.EqualValues(t, 5, result.EstimatedTargets)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 相似块候选范围为熔断与已信任块，距离阈值固定为常量。
func TestSimilarBlocksUsesDistanceConstant(t *testing.T) {
	require.Equal(t, 3, similarSimhashDistance)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM sub2api_enhance.prompt_texts`).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`b2.hash IS NOT NULL OR pt.hash IS NOT NULL`).WithArgs("hash-1", similarSimhashDistance).
		WillReturnRows(sqlmock.NewRows([]string{"hash", "preview", "bytes", "distance", "status", "trusted", "users"}).
			AddRow("hash-2", "预览", 12, 2, BreakerTripped, true, 2))
	items, err := NewRepository(db).SimilarBlocks(context.Background(), "hash-1")
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "hash-2", items[0].Hash)
	require.True(t, items[0].Trusted)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 相似块对比读取两侧全文，候选必须仍在相似范围内；不在范围内返回 ErrNotFound。
func TestSimilarPairReturnsFullTextsWithinSimilarRange(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	long := strings.Repeat("相同的前缀\n", 200) + "新增的一段指令"
	mock.ExpectQuery(`SELECT p.hash,p.text,p2.hash,p2.text`).WithArgs("hash-1", "hash-2", similarSimhashDistance).
		WillReturnRows(sqlmock.NewRows([]string{"hash", "text", "candidate", "candidate_text", "distance"}).
			AddRow("hash-1", strings.Repeat("相同的前缀\n", 200), "hash-2", long, 2))
	pair, err := NewRepository(db).SimilarPair(context.Background(), "hash-1", "hash-2")
	require.NoError(t, err)
	require.Equal(t, long, pair.CandidateText)
	require.Equal(t, 2, pair.Distance)
	mock.ExpectQuery(`SELECT p.hash,p.text,p2.hash,p2.text`).WithArgs("hash-1", "hash-9", similarSimhashDistance).
		WillReturnRows(sqlmock.NewRows([]string{"hash", "text", "candidate", "candidate_text", "distance"}))
	_, err = NewRepository(db).SimilarPair(context.Background(), "hash-1", "hash-9")
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 工作台计数：账号动作待核对含 failed 与 unknown 且不设窗口；通知投递失败只统计 7 天窗口。
func TestInboxCountsLimitNotificationFailuresToWindow(t *testing.T) {
	require.Equal(t, 7*24*time.Hour, inboxNotificationWindow)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`execution_status IN \('failed','unknown'\)\),\s+count\(\*\) FILTER \(WHERE notification_status='failed' AND requested_at>=clock_timestamp\(\)-\$1::interval\)`).
		WithArgs(interval(inboxNotificationWindow)).WillReturnRows(sqlmock.NewRows([]string{"actions", "notifications"}).AddRow(3, 1))
	mock.ExpectQuery(`SELECT count\(\*\) FROM sub2api_enhance.instruction_breakers`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`WHERE execution_status IN \('failed','unknown'\) ORDER BY id DESC`).WithArgs(inboxRecentLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id", "type", "user", "job", "status", "at", "username"}))
	mock.ExpectQuery(`FROM sub2api_enhance.instruction_breakers b`).WithArgs(inboxRecentLimit).
		WillReturnRows(sqlmock.NewRows([]string{"hash", "preview", "users", "at"}))
	counts, err := NewRepository(db).LoadInboxCounts(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(3), counts.ActionFailed)
	require.Equal(t, int64(1), counts.NotificationFailed)
	mock.ExpectQuery(`notification_status='failed' AND requested_at>=clock_timestamp\(\)-\$2::interval`).WithArgs(inboxRecentLimit, interval(inboxNotificationWindow)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "type", "user", "job", "status", "at", "username"}))
	_, err = NewRepository(db).NotificationFailedItems(context.Background(), inboxRecentLimit)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 待处理工作台只报真正的节点异常：未验证与未启用不算异常，时间取最近一次观测。
func TestUnhealthyNodeEntriesSkipsUnverifiedNodes(t *testing.T) {
	observed := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	cooldown := observed.Add(2 * time.Minute)
	now := observed.Add(time.Hour)
	items := unhealthyNodeEntries([]nodeRuntimeSnapshot{
		{ModelID: "healthy", Health: nodeHealthHealthy},
		{ModelID: "fresh", Health: nodeHealthUnknown},
		{ModelID: "off", Health: nodeHealthDisabled},
		{ModelID: "degraded", Name: "节点 A", Health: nodeHealthDegraded, LastErrorCode: "timeout", LastObservedAt: &observed},
		{ModelID: "cooling", Health: nodeHealthCooldown, CooldownUntil: &cooldown},
	}, now)
	require.Len(t, items, 2)
	require.Equal(t, InboxEntry{Category: "node_unhealthy", ActionType: "node", Name: "节点 A", Detail: "degraded",
		Health: nodeHealthDegraded, ErrorCode: "timeout", OccurredAt: observed}, items[0])
	require.Equal(t, "cooling", items[1].Detail)
	require.Equal(t, nodeHealthCooldown, items[1].Health)
	require.Equal(t, cooldown, items[1].OccurredAt)
}

// 保存配置只在连接参数或凭据变化时重置对应节点；改名、容量、策略等改动不影响节点健康。
func TestChangedNodeIDsOnlyMarksConnectionChanges(t *testing.T) {
	base := []ModelConfig{{ID: "a", Name: "A", Enabled: true, BaseURL: "https://a", Model: "m", TimeoutMS: 1000, MaxConcurrency: 2}}
	same := append([]ModelConfig{}, base...)
	require.Empty(t, changedNodeIDs(base, same, []KeyUpdate{{ModelID: "a", Action: "keep"}}))
	renamed := []ModelConfig{{ID: "a", Name: "A2", Enabled: true, BaseURL: "https://a", Model: "m", TimeoutMS: 1000, MaxConcurrency: 8}}
	require.Empty(t, changedNodeIDs(base, renamed, nil))
	moved := []ModelConfig{{ID: "a", Name: "A", Enabled: true, BaseURL: "https://b", Model: "m", TimeoutMS: 1000, MaxConcurrency: 2}}
	require.Equal(t, map[string]bool{"a": true}, changedNodeIDs(base, moved, nil))
	require.Equal(t, map[string]bool{"a": true}, changedNodeIDs(base, same, []KeyUpdate{{ModelID: "a", Action: "replace"}}))
	added := append(append([]ModelConfig{}, base...), ModelConfig{ID: "b", Enabled: true})
	require.Empty(t, changedNodeIDs(base, added, nil))
}

// 动作列表可同时按多个执行状态筛选，工作台下钻与计数口径一致；非法状态直接拒绝。
func TestListActionRecordsAcceptsMultipleExecutionStatuses(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	statuses := []string{"failed", "unknown"}
	mock.ExpectQuery(`SELECT count\(\*\) FROM sub2api_enhance.third_party_prompt_audit_enforcement_actions WHERE 1=1 AND execution_status=ANY\(\$1\)`).
		WithArgs(pq.Array(statuses)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`WHERE 1=1 AND execution_status=ANY\(\$1\) ORDER BY requested_at DESC,id DESC LIMIT \$2 OFFSET \$3`).
		WithArgs(pq.Array(statuses), 20, 0).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	page, err := NewRepository(db).ListActionRecords(context.Background(), ActionFilter{ExecutionStatuses: statuses}, 1, 20)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	_, err = NewRepository(db).ListActionRecords(context.Background(), ActionFilter{ExecutionStatuses: []string{"failed", "running"}}, 1, 20)
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 类别分布在数据库内聚合整个时间范围的阻断任务，未分类单独成行，并沿用统计接口的模式筛选。
func TestCategoryDistributionAggregatesWholeRange(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	from, to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`LEFT JOIN LATERAL unnest\(j.trigger_categories\) AS c\(code\) ON true\s+WHERE j.created_at>=\$1 AND j.created_at<\$2 AND j.decision='block'\s+GROUP BY 1`).
		WithArgs(from, to).WillReturnRows(sqlmock.NewRows([]string{"category", "count"}).AddRow("R07", 260).AddRow("", 12))
	items, err := NewRepository(db).categoryDistribution(context.Background(), StatsQuery{From: from, To: to})
	require.NoError(t, err)
	require.Equal(t, []CategoryCount{{Category: "R07", Count: 260}, {Category: "", Count: 12}}, items)
	mock.ExpectQuery(`AND j.decision='block' AND j.mode=\$3`).WithArgs(from, to, "blocking").
		WillReturnRows(sqlmock.NewRows([]string{"category", "count"}))
	items, err = NewRepository(db).categoryDistribution(context.Background(), StatsQuery{From: from, To: to, Mode: "blocking"})
	require.NoError(t, err)
	require.Empty(t, items)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 成本聚合按真实发出调用时间统计，用量未知单独计数，节点测试与政策回放单列。
func TestCostStatsGroupsCallsAndReuse(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	bucket := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`date_trunc\(\$3, a.dispatch_started_at AT TIME ZONE \$4\)`).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "model_id", "node_name", "call_kind", "stage", "calls", "succeeded", "input", "output", "unknown", "p50"}).
			AddRow(bucket, "node-1", "快照名称", "audit", "user", 3, 2, 100, 50, 1, 120.5).
			AddRow(bucket, "node-1", "快照名称", "probe", "format_repair", 1, 1, 10, 5, 0, 80.0))
	mock.ExpectQuery(`t.resolution IS NOT NULL`).WillReturnRows(sqlmock.NewRows([]string{"kind", "resolution", "targets"}).
		AddRow("instruction", "cache", 4).AddRow("user", "model", 2))
	report, err := NewRepository(db).CostStats(context.Background(), CostQuery{From: time.Now().Add(-24 * time.Hour), To: time.Now(), Bucket: "day", Timezone: "UTC"},
		map[string]string{"node-1": "当前配置名称"})
	require.NoError(t, err)
	require.Len(t, report.Buckets, 2)
	require.EqualValues(t, 1, report.Buckets[0].UnknownUsage)
	require.Equal(t, "probe", report.Buckets[1].CallKind)
	require.Len(t, report.Reuse, 2)
	require.Equal(t, "当前配置名称", report.Buckets[0].NodeName)
	require.EqualValues(t, 4, report.Reuse[0].Targets)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 并发复用只允许 fresh 轮次加入发起之后开始的调用。
func TestReusableFlightRespectsReuseAfter(t *testing.T) {
	started := time.Now()
	flight := &reviewFlight{started: started}
	require.True(t, reusableFlight(&AuditJob{}, flight))
	later := started.Add(time.Second)
	require.False(t, reusableFlight(&AuditJob{ReuseAfter: &later}, flight))
	earlier := started.Add(-time.Second)
	require.True(t, reusableFlight(&AuditJob{ReuseAfter: &earlier}, flight))
}
