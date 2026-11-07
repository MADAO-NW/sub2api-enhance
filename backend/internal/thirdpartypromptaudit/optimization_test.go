package thirdpartypromptaudit

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
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

// 重审轮次写入 fresh 与 reuse_after，并复制当前轮目标。
func TestAppendRoundWritesFreshReuseAfter(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status,current_round`).WithArgs(int64(11)).WillReturnRows(sqlmock.NewRows([]string{"status", "current_round"}).AddRow("done", 1))
	mock.ExpectExec(`INSERT INTO sub2api_enhance.audit_job_targets`).WithArgs(int64(11), 1, 2).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`reuse_mode='fresh',reuse_after=clock_timestamp\(\)`).WithArgs(int64(11), 2, int64(99), int64(4), "{}", MaxEvaluationAttempts).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, NewRepository(db).appendRound(context.Background(), 11, 99, 4, "{}"))
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
