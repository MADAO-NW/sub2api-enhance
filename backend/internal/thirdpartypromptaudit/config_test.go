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
	cfg.Models = []ModelConfig{{ID: "test-node", Name: "测试节点", Model: "test-model", BaseURL: "https://example.invalid", TimeoutMS: 1000, Enabled: true}}
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
	require.Equal(t, 0.5, publicDefaults.ReviewThreshold)
	require.Equal(t, 0.8, publicDefaults.BlockThreshold)
	require.Equal(t, 10, publicDefaults.WarningWindow)
	require.Equal(t, 3, publicDefaults.WarningLimit)
	require.EqualValues(t, 5, publicDefaults.DisableLimit)
	model := testConfig().Models[0]
	require.Equal(t, 300000, publicConfig(storedConfig{}).ModelDefaults.TimeoutMS)
	for _, timeout := range []int{1, DefaultNodeTimeoutMS, 86400000, int(math.MaxInt64 / int64(time.Millisecond))} {
		model.TimeoutMS = timeout
		require.NoError(t, validateModel(model))
	}
	for _, timeout := range []int{0, -1, int(math.MaxInt64/int64(time.Millisecond)) + 1} {
		model.TimeoutMS = timeout
		require.Error(t, validateModel(model))
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
		_, failure := (&Evaluator{}).Evaluate(context.Background(), &Job{Config: historical})
		require.Equal(t, "audit_snapshot_requires_reaudit", failure.Code)
		require.False(t, failure.Retryable)
	}
	var current ConfigSnapshot
	require.NoError(t, json.Unmarshal(raw, &current))
	require.Nil(t, current.historicalJSON)
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
	require.Error(t, validateConfig(config, true))
	config.AdminEmail = "admin@example.invalid"
	require.NoError(t, validateConfig(config, true))
	config.Disable = DisableConfig{Enabled: true}
	require.Error(t, validateConfig(config, true))
}
