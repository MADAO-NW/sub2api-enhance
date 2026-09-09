package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func testConfig() Config {
	cfg := DefaultConfig()
	review, block := 0.5, 0.8
	cfg.Mode = "async"
	cfg.ReviewThreshold, cfg.BlockThreshold = &review, &block
	cfg.Models = []ModelConfig{{ID: "test-node", Name: "测试节点", Model: "test-model", BaseURL: "https://example.invalid", TimeoutMS: 1000, MaxConcurrency: DefaultNodeMaxConcurrency, Enabled: true}}
	return cfg
}

func TestNodeDefaultsAndTimeoutRepresentation(t *testing.T) {
	defaults := DefaultConfig()
	publicDefaults := publicConfig(storedConfig{}).RuleDefaults
	require.NotNil(t, defaults.ReviewThreshold)
	require.NotNil(t, defaults.BlockThreshold)
	require.Equal(t, 0.5, *defaults.ReviewThreshold)
	require.Equal(t, 0.8, *defaults.BlockThreshold)
	require.Equal(t, WarningConfig{Window: 10, Limit: 3}, defaults.Warning)
	require.Equal(t, DisableConfig{Limit: 5}, defaults.Disable)
	require.Empty(t, defaults.UserRules)
	require.Equal(t, "current_turn", defaults.AuditScope)
	require.Equal(t, 0.5, publicDefaults.ReviewThreshold)
	require.Equal(t, 0.8, publicDefaults.BlockThreshold)
	require.Equal(t, 10, publicDefaults.WarningWindow)
	require.Equal(t, 3, publicDefaults.WarningLimit)
	require.EqualValues(t, 5, publicDefaults.DisableLimit)
	model := testConfig().Models[0]
	require.Equal(t, 300000, publicConfig(storedConfig{}).ModelDefaults.TimeoutMS)
	require.Equal(t, 4, publicConfig(storedConfig{}).ModelDefaults.MaxConcurrency)
	oldConfig := Config{Models: []ModelConfig{{ID: "old", Name: "old", Model: "old", BaseURL: "https://example.invalid", TimeoutMS: 1000}}}
	normalizeModelConcurrency(&oldConfig)
	require.Equal(t, DefaultNodeMaxConcurrency, oldConfig.Models[0].MaxConcurrency)
	invalidModel := oldConfig.Models[0]
	invalidModel.MaxConcurrency = 0
	require.ErrorContains(t, validateModel(invalidModel), "最大并发")
	for _, timeout := range []int{1, DefaultNodeTimeoutMS, 86400000, int(math.MaxInt64 / int64(time.Millisecond))} {
		model.TimeoutMS = timeout
		require.NoError(t, validateModel(model))
	}
	for _, timeout := range []int{0, -1, int(math.MaxInt64/int64(time.Millisecond)) + 1} {
		model.TimeoutMS = timeout
		require.Error(t, validateModel(model))
	}
	model.TimeoutMS = 1000
	model.MaxConcurrency = -1
	require.ErrorContains(t, validateModel(model), "最大并发")
}

func TestUserRuleOverridesOnlyTheSelectedUsersDecisionAndActions(t *testing.T) {
	config := testConfig()
	config.UserRules = []UserRuleConfig{{UserID: 7, Mode: "blocking", ReviewThreshold: 0.2, BlockThreshold: 0.4, Aggregation: "all_block", Warning: WarningConfig{Enabled: true, Window: 5, Limit: 2}, Disable: DisableConfig{Enabled: true, Limit: 9}}}
	require.NoError(t, validateConfig(config, true))
	effective := effectiveConfigForUser(config, 7)
	require.Equal(t, 0.2, *effective.ReviewThreshold)
	require.Equal(t, 0.4, *effective.BlockThreshold)
	require.Equal(t, "blocking", effective.Mode)
	require.Equal(t, "all_block", effective.Aggregation)
	require.Equal(t, WarningConfig{Enabled: true, Window: 5, Limit: 2}, effective.Warning)
	require.Equal(t, DisableConfig{Enabled: true, Limit: 9}, effective.Disable)
	require.Nil(t, effective.UserRules)
	inherited := effectiveConfigForUser(config, 8)
	require.Equal(t, *config.BlockThreshold, *inherited.BlockThreshold)
	require.Equal(t, config.Warning, inherited.Warning)
	require.Equal(t, "async", inherited.Mode)
	config.Mode = "blocking"
	config.UserRules[0].Mode = "async"
	require.Equal(t, "async", effectiveConfigForUser(config, 7).Mode)
	require.Equal(t, "blocking", effectiveConfigForUser(config, 8).Mode)
}

func TestCaptureOnlyAndManualReviewStayIndependentFromAutomaticAuditScope(t *testing.T) {
	config := testConfig()
	config.Mode = "off"
	config.CaptureWhenAuditOff = true
	config.ExcludedUserIDs = []int64{7}
	manager := &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: config}}}
	service := &Service{config: manager, closing: true, metrics: NewRuntimeMetrics()}
	require.True(t, manager.CaptureWhenAuditOff())
	require.False(t, service.RequiresAudit(7, nil, "openai"))
	decision := service.Check(context.Background(), IntakeRequest{Manual: true, UserID: 7, Provider: "openai"})
	require.NotNil(t, decision)
	require.Equal(t, "async", decision.Mode)
	require.Equal(t, IngressDecisionUnavailable, decision.Kind)
}

func TestPerUserModeRespectsGlobalOffAndFreezesEvaluationCredentials(t *testing.T) {
	config := testConfig()
	config.UserRules = []UserRuleConfig{{UserID: 7, Mode: "blocking", ReviewThreshold: 0.2, BlockThreshold: 0.4, Aggregation: "any_block", Warning: WarningConfig{Window: 10, Limit: 3}, Disable: DisableConfig{Limit: 5}}}
	manager := &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: config, Revision: 4}, Keys: map[string]string{"test-node": "first-key"}}}
	require.Equal(t, "blocking", manager.EffectiveModeForUser(7))
	require.Equal(t, "async", manager.EffectiveModeForUser(8))
	snapshot, keys, err := manager.EvaluationBinding(7)
	require.NoError(t, err)
	require.Equal(t, "blocking", snapshot.Mode)
	require.Equal(t, int64(4), snapshot.Revision)
	require.Equal(t, "first-key", keys["test-node"])
	manager.active.Keys["test-node"] = "second-key"
	require.Equal(t, "first-key", keys["test-node"])
	manager.active.Stored.Mode = "off"
	require.Equal(t, "off", manager.EffectiveModeForUser(7))
	_, _, err = manager.EvaluationBinding(7)
	require.ErrorIs(t, err, ErrAuditPaused)
}

func TestEvaluationBindingUsesLatestNodeGeneration(t *testing.T) {
	oldConfig := testConfig()
	manager := &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: oldConfig, Revision: 3}, Keys: map[string]string{"test-node": "old-key"}}}
	newConfig := testConfig()
	newConfig.Models = []ModelConfig{{ID: "replacement", Name: "replacement-model", Model: "replacement-model", BaseURL: "https://replacement.example.invalid", TimeoutMS: 2000, Enabled: true, Parameters: map[string]any{"reasoning_effort": "none"}}}
	manager.active = &activeConfig{Stored: storedConfig{Config: newConfig, Revision: 4}, Keys: map[string]string{"replacement": "new-key"}}
	snapshot, keys, err := manager.EvaluationBinding(8)
	require.NoError(t, err)
	require.Equal(t, int64(4), snapshot.Revision)
	require.Len(t, snapshot.Models, 1)
	require.Equal(t, "replacement", snapshot.Models[0].ID)
	require.Equal(t, "new-key", keys["replacement"])
	require.NotContains(t, keys, "test-node")
}

func TestUserWarningRuleHashDoesNotResetOtherUsers(t *testing.T) {
	config := testConfig()
	globalHash, err := warningRuleHash(config, 5, 8)
	require.NoError(t, err)
	config.UserRules = []UserRuleConfig{{UserID: 7, Mode: "blocking", ReviewThreshold: 0.2, BlockThreshold: 0.4, Aggregation: "any_block", Warning: WarningConfig{Enabled: true, Window: 5, Limit: 2}, Disable: DisableConfig{Limit: 9}}}
	otherHash, err := warningRuleHash(config, 5, 8)
	require.NoError(t, err)
	require.Equal(t, globalHash, otherHash)
	userHash, err := warningRuleHash(config, 5, 7)
	require.NoError(t, err)
	require.NotEqual(t, globalHash, userHash)
}

func TestUserRulesRequireUniqueUsersAndCompleteValidRules(t *testing.T) {
	config := testConfig()
	rule := UserRuleConfig{UserID: 7, Mode: "async", ReviewThreshold: 0.2, BlockThreshold: 0.4, Aggregation: "any_block", Warning: WarningConfig{Window: 3, Limit: 1}, Disable: DisableConfig{Limit: 2}}
	config.UserRules = []UserRuleConfig{rule, rule}
	require.ErrorContains(t, validateConfig(config, true), "不重复")
	config.UserRules = []UserRuleConfig{rule}
	config.UserRules[0].ReviewThreshold = 0.5
	require.ErrorContains(t, validateConfig(config, true), "风险阈值")
}

func TestEmbeddedDefaultPolicyUsesDocumentBodyWithoutCodeFence(t *testing.T) {
	require.NotContains(t, DefaultPolicy, "```")
	require.True(t, strings.HasPrefix(DefaultPolicy, "OpenAI / Anthropic 共用输入审核政策"))
	require.Contains(t, DefaultPolicy, "R14 平台滥用、规避与特殊用途")
	require.Contains(t, DefaultPolicy, `{"confidence":0.08,"reason":`)
}

func TestAdvancedParametersCannotOverrideFixedRequestFields(t *testing.T) {
	model := testConfig().Models[0]
	model.Parameters = map[string]any{"temperature": 0, "reasoning_effort": "none"}
	require.NoError(t, validateModel(model))
	for _, field := range []string{"model", "messages", "stream"} {
		model.Parameters = map[string]any{field: "override"}
		require.ErrorContains(t, validateModel(model), field)
	}
}

func TestModelNamesFollowSelectedModelAndUseStableDuplicateSuffixes(t *testing.T) {
	config := Config{Models: []ModelConfig{{Model: "same"}, {Model: "same"}, {Model: "other"}, {Model: "same"}}}
	normalizeModelNames(&config)
	require.Equal(t, []string{"same", "same-1", "other", "same-2"}, []string{config.Models[0].Name, config.Models[1].Name, config.Models[2].Name, config.Models[3].Name})
}

func TestExcludedUserBypassesAudit(t *testing.T) {
	config := testConfig()
	config.ExcludedUserIDs = []int64{7}
	manager := &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: config}}}
	service := &Service{config: manager}
	require.Nil(t, service.Check(context.Background(), IntakeRequest{UserID: 7, Provider: "openai"}))
}

func TestUserModeIsChosenAtIntakeAndExclusionStillWins(t *testing.T) {
	config := testConfig()
	config.UserRules = []UserRuleConfig{{UserID: 7, Mode: "blocking", ReviewThreshold: 0.5, BlockThreshold: 0.8, Aggregation: "any_block", Warning: WarningConfig{Window: 10, Limit: 3}, Disable: DisableConfig{Limit: 5}}}
	manager := &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: config}}}
	service := &Service{config: manager, closing: true, metrics: NewRuntimeMetrics()}
	decision := service.Check(context.Background(), IntakeRequest{UserID: 7, Provider: "openai"})
	require.NotNil(t, decision)
	require.Equal(t, "blocking", decision.Mode)
	decision = service.Check(context.Background(), IntakeRequest{UserID: 7, Provider: "openai", Background: true})
	require.NotNil(t, decision)
	require.Equal(t, "async", decision.Mode)
	config.ExcludedUserIDs = []int64{7}
	manager.active.Stored.Config = config
	require.Equal(t, "off", manager.EffectiveModeForUser(7))
	require.Nil(t, service.Check(context.Background(), IntakeRequest{UserID: 7, Provider: "openai"}))
}

func TestRemovedParametersRejectedAtConfigAndProbeBoundaries(t *testing.T) {
	for _, field := range []string{`"temperature":0`, `"max_tokens":2048`, `"temperature":null`} {
		for _, probe := range []bool{false, true} {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			var body string
			var target any = &ConfigUpdate{}
			if probe {
				body = `{"key_action":"clear","audit_prompt":"自定义政策","input_kind":"text","model":{` + field + `}}`
				target = &ProbeRequest{}
			} else {
				body = `{"expected_revision":1,"config":{"models":[{` + field + `}]}}`
			}
			ctx.Request = httptest.NewRequest("POST", "/", strings.NewReader(body))
			require.ErrorContains(t, bindStrict(ctx, target), "unknown field")
		}
	}
}

func TestHistoricalSnapshotsAreDisplayOnlyAndNewSnapshotsStayClean(t *testing.T) {
	snapshot := ConfigSnapshot{Config: testConfig(), Revision: 3, ContractVersion: ContractVersion, FixedContract: OutputContract}
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "fixed_roles")
	require.NotContains(t, string(raw), "temperature")
	for _, extra := range []string{`"fixed_roles":"旧角色判断",`, `"models":[{"temperature":0.2,"max_tokens":4000}],`} {
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &fields))
		delete(fields, "models")
		base, err := json.Marshal(fields)
		require.NoError(t, err)
		old := `{` + extra + string(base[1:])
		var historical ConfigSnapshot
		require.NoError(t, json.Unmarshal([]byte(old), &historical))
		display, err := json.Marshal(historical)
		require.NoError(t, err)
		require.JSONEq(t, old, string(display))
		// 没有仓储和客户端；旧快照必须在调用或缓存读取前失败。
		_, failure := (&Evaluator{}).Evaluate(context.Background(), &Job{Config: historical}, nil)
		require.Equal(t, "audit_snapshot_requires_reaudit", failure.Code)
		require.False(t, failure.Retryable)
	}
	var current ConfigSnapshot
	require.NoError(t, json.Unmarshal(raw, &current))
	require.Nil(t, current.historicalJSON)
}

func TestHistoricalSnapshotWithoutScopeKeepsFullRequestBehavior(t *testing.T) {
	config := testConfig()
	snapshot := ConfigSnapshot{Config: config, Revision: 2, ContractVersion: ContractVersion, FixedContract: OutputContract}
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	delete(fields, "audit_scope")
	raw, err = json.Marshal(fields)
	require.NoError(t, err)
	var restored ConfigSnapshot
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.Equal(t, "full_request", restored.AuditScope)
}

func TestConfigRequiresCalibratedThresholdsOnlyWhenActivating(t *testing.T) {
	config := DefaultConfig()
	require.NoError(t, validateConfig(config, false))
	config.Mode = "async"
	config.Models = testConfig().Models
	require.NoError(t, validateConfig(config, true))
	config.ReviewThreshold = nil
	require.Error(t, validateConfig(config, true))
	config = testConfig()
	require.NoError(t, validateConfig(config, true))
	*config.ReviewThreshold = 0.9
	require.Error(t, validateConfig(config, true))
}

func TestConfigKeepsEnforcementValidationIndependent(t *testing.T) {
	config := testConfig()
	require.NoError(t, validateConfig(config, true))
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 4}
	require.Error(t, validateConfig(config, true))
	config.Warning.Limit = 2
	require.NoError(t, validateConfig(config, true))
	config.AdminEmail = "admin@example.invalid"
	require.NoError(t, validateConfig(config, true))
	config.Disable = DisableConfig{Enabled: true}
	require.Error(t, validateConfig(config, true))
}
