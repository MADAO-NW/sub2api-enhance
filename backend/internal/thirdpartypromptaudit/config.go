package thirdpartypromptaudit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	appconfig "sub2api-enhance/internal/config"
	infraerrors "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/pkg/logger"
)

// configLockKey 串行化首次插入配置，随后配置行锁负责并发更新和处置读取。
const configLockKey int64 = 579147893221901941

// DefaultNodeTimeoutMS 为新增节点提供默认总审核预算，已有节点必须显式提交超时。
const DefaultNodeTimeoutMS = 300000

// DefaultNodeMaxConcurrency 保持单个节点相对既有默认 Worker 的安全并发上限。
const DefaultNodeMaxConcurrency = 4

// defaultReviewThreshold 是新配置默认触发指令关联裁决和待复核的风险分数。
const defaultReviewThreshold = 0.5

// defaultBlockThreshold 是新配置默认判定违规的风险分数。
const defaultBlockThreshold = 0.8

// defaultWarningWindow 是违规提醒默认统计的最近正式审核数量。
const defaultWarningWindow = 10

// defaultWarningLimit 是默认统计窗口内触发提醒的违规数量。
const defaultWarningLimit = 2

// defaultDisableLimit 是默认触发用户自动停用的累计违规数量。
const defaultDisableLimit = 1

// legacyDefaultPolicySHA256 仅识别已发布的旧内置默认值，不能覆盖管理员自定义政策。
var legacyDefaultPolicySHA256 = map[string]struct{}{
	"9071cf397aa94bca6982620a56c908a5977fdfc94206c7e198f160c5f65b2e0d": {},
	"4ebb50f2c9183ab5c20214d094f291ae1571cbbd931a16f6bcca9915043a011c": {},
	"88606d5072b66a33a6a91f720e9989a99304c68210589be24131feb605a76734": {},
}

type ModelConfig struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Enabled        bool           `json:"enabled"`
	BaseURL        string         `json:"base_url"`
	Model          string         `json:"model"`
	TimeoutMS      int            `json:"timeout_ms"`
	MaxConcurrency int            `json:"max_concurrency,omitempty"`
	Parameters     map[string]any `json:"parameters,omitempty"`
}

type WarningConfig struct {
	Enabled bool `json:"enabled"`
	Window  int  `json:"window"`
	Limit   int  `json:"limit"`
}

type DisableConfig struct {
	Enabled bool  `json:"enabled"`
	Limit   int64 `json:"limit"`
}

type UserRuleConfig struct {
	UserID          int64         `json:"user_id"`
	Mode            string        `json:"mode"`
	ReviewThreshold float64       `json:"review_threshold"`
	BlockThreshold  float64       `json:"block_threshold"`
	Aggregation     string        `json:"aggregation"`
	Warning         WarningConfig `json:"warning"`
	Disable         DisableConfig `json:"disable"`
}

type Config struct {
	Mode                string           `json:"mode"`
	CaptureWhenAuditOff bool             `json:"capture_when_audit_off"`
	AuditScope          string           `json:"audit_scope"`
	Platforms           []string         `json:"platforms"`
	AllGroups           bool             `json:"all_groups"`
	GroupIDs            []int64          `json:"group_ids"`
	ExcludedUserIDs     []int64          `json:"excluded_user_ids"`
	AuditPrompt         string           `json:"audit_prompt"`
	Models              []ModelConfig    `json:"models"`
	ReviewThreshold     *float64         `json:"review_threshold"`
	BlockThreshold      *float64         `json:"block_threshold"`
	Aggregation         string           `json:"aggregation"`
	WorkerCount         int              `json:"worker_count"`
	Warning             WarningConfig    `json:"warning"`
	Disable             DisableConfig    `json:"disable"`
	UserRules           []UserRuleConfig `json:"user_rules"`
	AdminEmail          string           `json:"admin_email"`
}

type ConfigSnapshot struct {
	Config
	Revision            int64  `json:"revision"`
	WarningRuleRevision int64  `json:"warning_rule_revision"`
	ContractVersion     string `json:"contract_version"`
	FixedContract       string `json:"fixed_contract"`
	// historicalJSON 仅保留不可继续执行的旧快照，供管理员查看当时规则与参数。
	historicalJSON json.RawMessage
}

// UnmarshalJSON 识别旧执行规则，保留原始事实而不把已移除参数带入新节点配置。
func (snapshot *ConfigSnapshot) UnmarshalJSON(raw []byte) error {
	type snapshotValue ConfigSnapshot
	var value snapshotValue
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	var historical struct {
		FixedRoles json.RawMessage `json:"fixed_roles"`
		Models     []struct {
			Temperature json.RawMessage `json:"temperature"`
			MaxTokens   json.RawMessage `json:"max_tokens"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &historical); err != nil {
		return err
	}
	*snapshot = ConfigSnapshot(value)
	normalizeModelConcurrency(&snapshot.Config)
	if snapshot.AuditScope == "" {
		snapshot.AuditScope = "current_user"
	}
	legacy := len(historical.FixedRoles) > 0
	for _, model := range historical.Models {
		legacy = legacy || len(model.Temperature) > 0 || len(model.MaxTokens) > 0
	}
	if legacy {
		snapshot.historicalJSON = append(json.RawMessage(nil), raw...)
	}
	return nil
}

// MarshalJSON 只为历史展示保留旧快照，新执行快照不写入旧角色或节点参数。
func (snapshot ConfigSnapshot) MarshalJSON() ([]byte, error) {
	if snapshot.historicalJSON != nil {
		return snapshot.historicalJSON, nil
	}
	type snapshotValue ConfigSnapshot
	return json.Marshal(snapshotValue(snapshot))
}

type storedConfig struct {
	Config
	Revision            int64             `json:"revision"`
	WarningRuleRevision int64             `json:"warning_rule_revision"`
	EncryptedKeys       map[string]string `json:"encrypted_keys"`
	UpdatedBy           int64             `json:"updated_by"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

type PublicConfig struct {
	ModelDefaults struct {
		TimeoutMS      int `json:"timeout_ms"`
		MaxConcurrency int `json:"max_concurrency"`
	} `json:"model_defaults"`
	RuleDefaults struct {
		ReviewThreshold float64 `json:"review_threshold"`
		BlockThreshold  float64 `json:"block_threshold"`
		WarningWindow   int     `json:"warning_window"`
		WarningLimit    int     `json:"warning_limit"`
		DisableLimit    int64   `json:"disable_limit"`
	} `json:"rule_defaults"`
	AppliedRevision int64  `json:"applied_revision"`
	InstanceID      string `json:"instance_id"`
	Config
	ApplicationError    string          `json:"application_error"`
	Revision            int64           `json:"revision"`
	WarningRuleRevision int64           `json:"warning_rule_revision"`
	HasAPIKeys          map[string]bool `json:"has_api_keys"`
	UpdatedBy           int64           `json:"updated_by"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

type KeyUpdate struct {
	ModelID string `json:"model_id" binding:"required"`
	Action  string `json:"action" binding:"required,oneof=keep replace clear"`
	APIKey  string `json:"api_key"`
}

type ConfigUpdate struct {
	ExpectedRevision int64       `json:"expected_revision" binding:"required,min=1"`
	Config           Config      `json:"config"`
	Keys             []KeyUpdate `json:"keys"`
}

type activeConfig struct {
	Stored storedConfig
	Keys   map[string]string
}

type ConfigManager struct {
	publicOrigin            string
	reloadMu                sync.Mutex
	db                      *sql.DB
	encryptor               appconfig.SecretEncryptor
	encryptionKeyConfigured bool
	mu                      sync.RWMutex
	active                  *activeConfig
	expectedMode            string
	expectedRevision        int64
	loadError               error
}

func NewConfigManager(db *sql.DB, encryptor appconfig.SecretEncryptor, cfg *appconfig.Config) *ConfigManager {
	origin := ""
	if cfg != nil {
		origin = cfg.PublicOrigin
	}
	return &ConfigManager{publicOrigin: origin, db: db, encryptor: encryptor,
		encryptionKeyConfigured: cfg != nil && cfg.EncryptionKey != ""}
}

func DefaultConfig() Config {
	reviewThreshold, blockThreshold := defaultReviewThreshold, defaultBlockThreshold
	return Config{Mode: "off", AuditScope: "current_user", Platforms: []string{}, AllGroups: true,
		GroupIDs: []int64{}, ExcludedUserIDs: []int64{}, AuditPrompt: DefaultPolicy, Models: []ModelConfig{},
		ReviewThreshold: &reviewThreshold, BlockThreshold: &blockThreshold,
		Aggregation: "any_block", WorkerCount: 4,
		CaptureWhenAuditOff: true,
		Warning:             WarningConfig{Window: defaultWarningWindow, Limit: defaultWarningLimit},
		Disable:             DisableConfig{Limit: defaultDisableLimit}, UserRules: []UserRuleConfig{}}
}

// normalizeModelConcurrency 为旧配置和旧任务快照补齐节点默认并发。
func normalizeModelConcurrency(config *Config) {
	if config == nil {
		return
	}
	for i := range config.Models {
		if config.Models[i].MaxConcurrency == 0 {
			config.Models[i].MaxConcurrency = DefaultNodeMaxConcurrency
		}
	}
}

// normalizeAuditScope 将已发布的范围选项收敛为当前任务 user 固定策略。
func normalizeAuditScope(config *Config) {
	if config != nil {
		config.AuditScope = "current_user"
	}
}

// effectiveConfigForUser 将单个用户覆盖合并到全局默认规则，并移除不再需要的覆盖列表。
func effectiveConfigForUser(config Config, userID int64) Config {
	globalMode := config.Mode
	for _, rule := range config.UserRules {
		if rule.UserID != userID {
			continue
		}
		review, block := rule.ReviewThreshold, rule.BlockThreshold
		config.ReviewThreshold, config.BlockThreshold = &review, &block
		mode := rule.Mode
		if mode == "" {
			mode = globalMode
			if mode == "off" {
				mode = "async"
			}
		}
		config.Mode, config.Aggregation, config.Warning, config.Disable = mode, rule.Aggregation, rule.Warning, rule.Disable
		break
	}
	if globalMode == "off" {
		config.Mode = "off"
	}
	config.UserRules = nil
	return config
}

// normalizeUserRuleModes 为未发布开发版本产生的无 mode 用户规则补齐创建时默认值。
func normalizeUserRuleModes(config *Config) {
	for i := range config.UserRules {
		if config.UserRules[i].Mode == "" {
			config.UserRules[i].Mode = config.Mode
			if config.UserRules[i].Mode == "off" {
				config.UserRules[i].Mode = "async"
			}
		}
	}
}

// warningRuleHash 保留全局规则的历史哈希格式，并让用户覆盖只重置自己的提醒窗口。
func warningRuleHash(config Config, revision, userID int64) (string, error) {
	for _, rule := range config.UserRules {
		if rule.UserID == userID {
			return fingerprint(struct {
				Rule   WarningConfig
				UserID int64
			}{rule.Warning, userID})
		}
	}
	return fingerprint(struct {
		Rule     WarningConfig
		Revision int64
	}{config.Warning, revision})
}

func validateConfig(config Config, activating bool) error {
	if !slices.Contains([]string{"off", "async", "blocking"}, config.Mode) {
		return errors.New("审核模式无效")
	}
	if config.AuditScope != "current_user" {
		return errors.New("审核范围无效")
	}
	if !slices.Contains([]string{"any_block", "majority_block", "all_block"}, config.Aggregation) {
		return errors.New("聚合规则无效")
	}
	if config.WorkerCount < 1 {
		return errors.New("Worker 数必须大于 0")
	}
	if config.AuditPrompt == "" {
		return errors.New("审核提示词不能为空")
	}
	for _, groupID := range config.GroupIDs {
		if groupID <= 0 {
			return errors.New("分组 ID 必须大于 0")
		}
	}
	excluded := make(map[int64]bool, len(config.ExcludedUserIDs))
	for _, userID := range config.ExcludedUserIDs {
		if userID <= 0 || excluded[userID] {
			return errors.New("不审核用户 ID 必须为不重复的正整数")
		}
		excluded[userID] = true
	}
	userRules := make(map[int64]bool, len(config.UserRules))
	for _, rule := range config.UserRules {
		if rule.UserID <= 0 || userRules[rule.UserID] {
			return errors.New("用户规则必须引用不重复的正整数用户 ID")
		}
		userRules[rule.UserID] = true
		if rule.Mode != "" && rule.Mode != "async" && rule.Mode != "blocking" {
			return fmt.Errorf("用户 %d 的处理方式必须是异步审核或同步阻断", rule.UserID)
		}
		if math.IsNaN(rule.ReviewThreshold) || math.IsInf(rule.ReviewThreshold, 0) || rule.ReviewThreshold < 0 || rule.ReviewThreshold > 1 ||
			math.IsNaN(rule.BlockThreshold) || math.IsInf(rule.BlockThreshold, 0) || rule.BlockThreshold < 0 || rule.BlockThreshold > 1 || rule.ReviewThreshold > rule.BlockThreshold {
			return fmt.Errorf("用户 %d 的风险阈值必须满足 0 ≤ 复核阈值 ≤ 阻断阈值 ≤ 1", rule.UserID)
		}
		if !slices.Contains([]string{"any_block", "majority_block", "all_block"}, rule.Aggregation) {
			return fmt.Errorf("用户 %d 的聚合规则无效", rule.UserID)
		}
		if rule.Warning.Enabled && (rule.Warning.Window < 1 || rule.Warning.Limit < 1 || rule.Warning.Limit > rule.Warning.Window) {
			return fmt.Errorf("用户 %d 的提醒规则必须满足 1 ≤ 违规条数 ≤ 成功分类窗口", rule.UserID)
		}
		if rule.Disable.Enabled && rule.Disable.Limit < 1 {
			return fmt.Errorf("用户 %d 的自动停用累计阈值必须大于 0", rule.UserID)
		}
	}
	for _, threshold := range []*float64{config.ReviewThreshold, config.BlockThreshold} {
		if threshold != nil && (math.IsNaN(*threshold) || math.IsInf(*threshold, 0) || *threshold < 0 || *threshold > 1) {
			return errors.New("风险阈值必须是 0 到 1 之间的数值")
		}
	}
	if config.ReviewThreshold != nil && config.BlockThreshold != nil && *config.ReviewThreshold > *config.BlockThreshold {
		return errors.New("复核阈值不能大于阻断阈值")
	}
	if activating && (config.ReviewThreshold == nil || config.BlockThreshold == nil) {
		return errors.New("启用审核前必须填写两个风险阈值")
	}
	ids := make(map[string]bool)
	enabled := 0
	for _, model := range config.Models {
		if model.ID == "" || ids[model.ID] {
			return errors.New("节点 ID 不能为空或重复")
		}
		ids[model.ID] = true
		if model.Enabled {
			enabled++
		}
		if model.Enabled || model.BaseURL != "" || model.Model != "" {
			if err := validateModel(model); err != nil {
				return fmt.Errorf("节点 %s: %w", model.Name, err)
			}
		}
	}
	if activating && enabled == 0 {
		return errors.New("启用审核前至少需要一个启用节点")
	}
	if config.Warning.Enabled && (config.Warning.Window < 1 || config.Warning.Limit < 1 || config.Warning.Limit > config.Warning.Window) {
		return errors.New("提醒规则必须满足 1 ≤ 违规条数 ≤ 成功分类窗口")
	}
	if config.Disable.Enabled && config.Disable.Limit < 1 {
		return errors.New("自动停用累计阈值必须大于 0")
	}
	if config.AdminEmail != "" {
		if _, err := mail.ParseAddress(config.AdminEmail); err != nil {
			return errors.New("管理员通知邮箱格式无效")
		}
	}
	return nil
}

func validateModel(model ModelConfig) error {
	if model.Model == "" || model.Name == "" {
		return errors.New("节点名称和模型名不能为空")
	}
	if model.TimeoutMS <= 0 || int64(model.TimeoutMS) > math.MaxInt64/int64(time.Millisecond) {
		return errors.New("节点超时必须是有效的正毫秒数")
	}
	if model.MaxConcurrency < 1 {
		return errors.New("节点最大并发数必须大于 0")
	}
	if _, err := chatCompletionsURL(model.BaseURL); err != nil {
		return err
	}
	for _, reserved := range []string{"model", "messages", "stream"} {
		if _, exists := model.Parameters[reserved]; exists {
			return fmt.Errorf("节点高级参数不能覆盖 %s", reserved)
		}
	}
	return nil
}

// normalizeModelNames 以模型名称生成稳定节点名称，同模型按配置顺序追加序号。
func normalizeModelNames(config *Config) {
	if config == nil {
		return
	}
	counts := make(map[string]int)
	for i := range config.Models {
		model := strings.TrimSpace(config.Models[i].Model)
		if model == "" {
			config.Models[i].Name = ""
			continue
		}
		count := counts[model]
		config.Models[i].Name = model
		if count > 0 {
			config.Models[i].Name = fmt.Sprintf("%s-%d", model, count)
		}
		counts[model] = count + 1
	}
}

func (m *ConfigManager) Reload(ctx context.Context) (loadErr error) {
	// 数据库读取与发布串行，避免旧查询覆盖新配置。
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	defer func() {
		if loadErr != nil {
			m.mu.Lock()
			m.loadError = loadErr
			m.mu.Unlock()
		}
	}()
	rows, err := m.db.QueryContext(ctx, `SELECT key,value FROM sub2api_enhance.settings WHERE key=$1`, SettingKey)
	if err != nil {
		m.mu.Lock()
		m.loadError = err
		m.mu.Unlock()
		return err
	}
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			_ = rows.Close()
			return err
		}
		values[key] = value
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	stored := storedConfig{Config: DefaultConfig(), Revision: 1, EncryptedKeys: map[string]string{}}
	if values[SettingKey] != "" {
		stored.AuditScope = "current_user"
		err = json.Unmarshal([]byte(values[SettingKey]), &stored)
	}
	// 节点名称是模型选择的派生展示值，旧配置加载后也立即使用统一规则。
	normalizeModelNames(&stored.Config)
	normalizeModelConcurrency(&stored.Config)
	normalizeAuditScope(&stored.Config)
	normalizeUserRuleModes(&stored.Config)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expectedMode = stored.Mode
	m.expectedRevision = stored.Revision
	if err == nil {
		err = validateConfig(stored.Config, stored.Mode != "off")
	}
	keys := make(map[string]string)
	if err == nil {
		for _, model := range stored.Models {
			if cipher := stored.EncryptedKeys[model.ID]; cipher != "" {
				if m.encryptor == nil {
					err = errors.New("节点凭据解密能力不可用")
					break
				}
				key, decryptErr := m.encryptor.Decrypt(cipher)
				if decryptErr != nil {
					err = fmt.Errorf("节点 %s 凭据解密失败", model.ID)
					break
				}
				keys[model.ID] = key
			}
		}
	}
	m.loadError = err
	if err != nil {
		return err
	}
	m.active = &activeConfig{Stored: stored, Keys: keys}
	return nil
}

// upgradeLegacyDefaultPolicy 只升级字节级匹配旧内置值的配置，并保留自定义政策。
func (m *ConfigManager) upgradeLegacyDefaultPolicy(ctx context.Context) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, configLockKey); err != nil {
		return err
	}
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1 FOR UPDATE`, SettingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	current := storedConfig{Config: DefaultConfig(), Revision: 1, EncryptedKeys: map[string]string{}}
	current.AuditScope = "current_user"
	if err := json.Unmarshal([]byte(raw), &current); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(current.AuditPrompt))
	if _, exists := legacyDefaultPolicySHA256[hex.EncodeToString(digest[:])]; !exists {
		return tx.Commit()
	}
	current.AuditPrompt = DefaultPolicy
	current.Revision++
	current.UpdatedBy = 0
	current.UpdatedAt = time.Now().UTC()
	encoded, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.settings SET value=$2,updated_at=clock_timestamp() WHERE key=$1`, SettingKey, string(encoded)); err != nil {
		return err
	}
	logger.LegacyPrintf("third_party_prompt_audit", "内置审核政策已升级 revision=%d", current.Revision)
	return tx.Commit()
}

func (m *ConfigManager) Active() (ConfigSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active == nil || m.loadError != nil {
		return ConfigSnapshot{}, infraerrors.ServiceUnavailable("third_party_audit_config_unavailable", "第三方审核配置暂不可用")
	}
	snapshot := ConfigSnapshot{Config: m.active.Stored.Config, Revision: m.active.Stored.Revision,
		WarningRuleRevision: m.active.Stored.WarningRuleRevision, ContractVersion: ContractVersion,
		FixedContract: OutputContract}
	// 快照只含可序列化的值，复制后不与配置热更新共享切片或指针。
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	var cloned ConfigSnapshot
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return ConfigSnapshot{}, err
	}
	return cloned, nil
}

func (m *ConfigManager) EffectiveMode() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.loadError != nil {
		if m.expectedMode == "blocking" || m.expectedMode == "async" {
			return m.expectedMode
		}
		return "off"
	}
	if m.active == nil {
		return "off"
	}
	return m.active.Stored.Mode
}

func (m *ConfigManager) EffectiveModeForUser(userID int64) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.loadError != nil {
		if m.expectedMode == "blocking" || m.expectedMode == "async" {
			return m.expectedMode
		}
		return "off"
	}
	if m.active == nil || m.active.Stored.Mode == "off" {
		return "off"
	}
	if slices.Contains(m.active.Stored.ExcludedUserIDs, userID) {
		return "off"
	}
	return effectiveConfigForUser(m.active.Stored.Config, userID).Mode
}

func (m *ConfigManager) IsExcluded(userID int64) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active != nil && m.loadError == nil && slices.Contains(m.active.Stored.ExcludedUserIDs, userID)
}

// CaptureWhenAuditOff 返回全局关闭时是否仍启用独立原文采集。
func (m *ConfigManager) CaptureWhenAuditOff() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active != nil && m.loadError == nil && m.active.Stored.CaptureWhenAuditOff
}

// EvaluationBinding 在同一配置读锁内冻结当前用户规则、审核节点及节点凭据。
func (m *ConfigManager) EvaluationBinding(userID int64) (ConfigSnapshot, map[string]string, error) {
	return m.evaluationBinding(userID, false)
}

// EvaluationBindingAllowExcluded 仅供管理员明确发起的人工审核绕过自动排除名单。
func (m *ConfigManager) EvaluationBindingAllowExcluded(userID int64) (ConfigSnapshot, map[string]string, error) {
	return m.evaluationBinding(userID, true)
}

func (m *ConfigManager) evaluationBinding(userID int64, allowExcluded bool) (ConfigSnapshot, map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active == nil || m.loadError != nil {
		return ConfigSnapshot{}, nil, errors.New("审核配置或节点凭据不可用")
	}
	if m.active.Stored.Mode == "off" {
		return ConfigSnapshot{}, nil, ErrAuditPaused
	}
	if !allowExcluded && slices.Contains(m.active.Stored.ExcludedUserIDs, userID) {
		return ConfigSnapshot{}, nil, ErrUserExcluded
	}
	snapshot := ConfigSnapshot{Config: effectiveConfigForUser(m.active.Stored.Config, userID), Revision: m.active.Stored.Revision,
		WarningRuleRevision: m.active.Stored.WarningRuleRevision, ContractVersion: ContractVersion, FixedContract: OutputContract}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return ConfigSnapshot{}, nil, err
	}
	var cloned ConfigSnapshot
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return ConfigSnapshot{}, nil, err
	}
	keys := make(map[string]string)
	for _, model := range cloned.Models {
		if model.Enabled {
			keys[model.ID] = m.active.Keys[model.ID]
		}
	}
	return cloned, keys, nil
}

func (m *ConfigManager) Public() (PublicConfig, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active == nil {
		return PublicConfig{}, infraerrors.ServiceUnavailable("third_party_audit_config_unavailable", "第三方审核配置暂不可用")
	}
	return publicConfig(m.active.Stored), nil
}

func publicConfig(stored storedConfig) PublicConfig {
	keys := make(map[string]bool)
	for id, encrypted := range stored.EncryptedKeys {
		keys[id] = encrypted != ""
	}
	display := stored.Config
	display.Models = append([]ModelConfig(nil), stored.Models...)
	normalizeModelNames(&display)
	normalizeModelConcurrency(&display)
	normalizeUserRuleModes(&display)
	result := PublicConfig{Config: display, Revision: stored.Revision,
		WarningRuleRevision: stored.WarningRuleRevision, HasAPIKeys: keys,
		UpdatedAt: stored.UpdatedAt, UpdatedBy: stored.UpdatedBy}
	result.ModelDefaults.TimeoutMS = DefaultNodeTimeoutMS
	result.ModelDefaults.MaxConcurrency = DefaultNodeMaxConcurrency
	result.RuleDefaults.ReviewThreshold = defaultReviewThreshold
	result.RuleDefaults.BlockThreshold = defaultBlockThreshold
	result.RuleDefaults.WarningWindow = defaultWarningWindow
	result.RuleDefaults.WarningLimit = defaultWarningLimit
	result.RuleDefaults.DisableLimit = defaultDisableLimit
	return result
}

func (m *ConfigManager) Save(ctx context.Context, input ConfigUpdate, actorID int64) (PublicConfig, error) {
	normalizeModelNames(&input.Config)
	normalizeUserRuleModes(&input.Config)
	normalizeAuditScope(&input.Config)
	if err := validateConfig(input.Config, input.Config.Mode != "off"); err != nil {
		return PublicConfig{}, infraerrors.BadRequest("third_party_audit_invalid_config", err.Error())
	}
	for _, model := range input.Config.Models {
		if m.publicOrigin != "" && (model.BaseURL == m.publicOrigin || strings.HasPrefix(model.BaseURL, m.publicOrigin+"/")) {
			return PublicConfig{}, infraerrors.BadRequest("audit_node_recursion", "审核节点不能指向增强公共入口")
		}
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return PublicConfig{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, configLockKey); err != nil {
		return PublicConfig{}, err
	}
	current := storedConfig{Config: DefaultConfig(), Revision: 1, EncryptedKeys: map[string]string{}}
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1 FOR UPDATE`, SettingKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PublicConfig{}, err
	}
	if err == nil {
		current.AuditScope = "current_user"
		if err := json.Unmarshal([]byte(raw), &current); err != nil {
			return PublicConfig{}, err
		}
		normalizeUserRuleModes(&current.Config)
	}
	if current.Revision != input.ExpectedRevision {
		return PublicConfig{}, infraerrors.Conflict("third_party_audit_config_conflict", "配置已被其他管理员更新，请刷新后再保存")
	}
	next := storedConfig{Config: input.Config, Revision: current.Revision + 1, WarningRuleRevision: current.WarningRuleRevision,
		EncryptedKeys: map[string]string{}, UpdatedBy: actorID, UpdatedAt: time.Now().UTC()}
	if !reflect.DeepEqual(current.Warning, next.Warning) {
		next.WarningRuleRevision = next.Revision
	}
	updates := make(map[string]KeyUpdate)
	for _, key := range input.Keys {
		if _, exists := updates[key.ModelID]; exists {
			return PublicConfig{}, infraerrors.BadRequest("third_party_audit_invalid_key", "同一节点不能重复提交凭据更新")
		}
		updates[key.ModelID] = key
	}
	for _, model := range next.Models {
		update, exists := updates[model.ID]
		if !exists {
			update.Action = "keep"
		}
		delete(updates, model.ID)
		switch update.Action {
		case "keep":
			if update.APIKey != "" {
				return PublicConfig{}, infraerrors.BadRequest("third_party_audit_invalid_key", "保持凭据时不能提交新密钥")
			}
			next.EncryptedKeys[model.ID] = current.EncryptedKeys[model.ID]
		case "replace":
			if update.APIKey == "" {
				return PublicConfig{}, infraerrors.BadRequest("third_party_audit_invalid_key", "替换凭据时密钥不能为空")
			}
			if !m.encryptionKeyConfigured || m.encryptor == nil {
				return PublicConfig{}, infraerrors.BadRequest("third_party_audit_encryption_required", "保存节点凭据需要配置应用加密密钥")
			}
			encrypted, err := m.encryptor.Encrypt(update.APIKey)
			if err != nil {
				return PublicConfig{}, errors.New("节点凭据加密失败")
			}
			next.EncryptedKeys[model.ID] = encrypted
		case "clear":
			if update.APIKey != "" {
				return PublicConfig{}, infraerrors.BadRequest("third_party_audit_invalid_key", "清除凭据时不能同时提交新密钥")
			}
		default:
			return PublicConfig{}, infraerrors.BadRequest("third_party_audit_invalid_key", "凭据操作无效")
		}
	}
	if len(updates) != 0 {
		return PublicConfig{}, infraerrors.BadRequest("third_party_audit_invalid_key", "凭据更新引用了不存在的节点")
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return PublicConfig{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.settings (key,value,updated_at) VALUES ($1,$2,NOW()) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at`, SettingKey, string(encoded)); err != nil {
		return PublicConfig{}, err
	}
	newlyExcluded := make([]int64, 0)
	for _, userID := range next.ExcludedUserIDs {
		if !slices.Contains(current.ExcludedUserIDs, userID) {
			newlyExcluded = append(newlyExcluded, userID)
		}
	}
	if len(newlyExcluded) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET
		 status='skipped',finished_at=clock_timestamp(),lease_until=NULL,failure_stage=NULL,last_error_code='user_excluded',last_error_message=$2,updated_at=clock_timestamp()
		 WHERE user_id=ANY($1) AND status IN ('queued','retry') AND result_checkpoint IS NULL`, pq.Array(newlyExcluded), encodeStoredText(ErrUserExcluded.Error())); err != nil {
			return PublicConfig{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return PublicConfig{}, err
	}
	transition, _ := json.Marshal(struct{ Before, After PublicConfig }{publicConfig(current), publicConfig(next)})
	logger.LegacyPrintf("third_party_prompt_audit", "审核配置已更新 actor_user_id=%d revision=%d transition=%s", actorID, next.Revision, string(transition))
	result := publicConfig(next)
	if err := m.Reload(ctx); err != nil {
		result.ApplicationError = err.Error()
	}
	return result, nil
}

// ResolveKeyByModelID 为模型列表查询读取节点当前凭据，新节点没有已保存凭据时返回空值。
func (m *ConfigManager) ResolveKeyByModelID(modelID string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active == nil || m.loadError != nil {
		return "", errors.New("节点配置不可用")
	}
	return m.active.Keys[modelID], nil
}

// ReadSaved 不依赖节点凭据解密或运行快照，使管理员仍能修复不可应用的配置。
func (m *ConfigManager) ReadSaved(ctx context.Context) (PublicConfig, error) {
	stored := storedConfig{Config: DefaultConfig(), Revision: 1, EncryptedKeys: map[string]string{}}
	var raw string
	err := m.db.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1`, SettingKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PublicConfig{}, err
	}
	if err == nil {
		stored.AuditScope = "current_user"
		if err = json.Unmarshal([]byte(raw), &stored); err != nil {
			return PublicConfig{}, err
		}
	}
	normalizeAuditScope(&stored.Config)
	result := publicConfig(stored)
	if err = m.Reload(ctx); err != nil {
		result.ApplicationError = err.Error()
	}
	return result, nil
}

// WorkerCapacity 返回当前已应用的并发上限，配置读取失败时仍使用最后成功值。
func (m *ConfigManager) WorkerCapacity() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active != nil {
		return m.active.Stored.WorkerCount
	}
	return DefaultConfig().WorkerCount
}
