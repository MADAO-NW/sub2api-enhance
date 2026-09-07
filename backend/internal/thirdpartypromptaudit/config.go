package thirdpartypromptaudit

import (
	"context"
	"database/sql"
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

	appconfig "sub2api-enhance/internal/config"
	infraerrors "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/pkg/logger"
)

// configLockKey 串行化首次插入配置，随后配置行锁负责并发更新和处置读取。
const configLockKey int64 = 579147893221901941

// DefaultNodeTimeoutMS 为新增节点提供默认总审核预算，已有节点必须显式提交超时。
const DefaultNodeTimeoutMS = 300000

type ModelConfig struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	TimeoutMS int    `json:"timeout_ms"`
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

type Config struct {
	Mode            string        `json:"mode"`
	AuditScope      string        `json:"audit_scope"`
	Platforms       []string      `json:"platforms"`
	AllGroups       bool          `json:"all_groups"`
	GroupIDs        []int64       `json:"group_ids"`
	AuditPrompt     string        `json:"audit_prompt"`
	Models          []ModelConfig `json:"models"`
	ReviewThreshold *float64      `json:"review_threshold"`
	BlockThreshold  *float64      `json:"block_threshold"`
	Aggregation     string        `json:"aggregation"`
	WorkerCount     int           `json:"worker_count"`
	StorePassEvents bool          `json:"store_pass_events"`
	Warning         WarningConfig `json:"warning"`
	Disable         DisableConfig `json:"disable"`
	AdminEmail      string        `json:"admin_email"`
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
		TimeoutMS int `json:"timeout_ms"`
	} `json:"model_defaults"`
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
	Stored             storedConfig
	Keys               map[string]string
	RiskControlEnabled bool
}

type ConfigManager struct {
	publicOrigin               string
	reloadMu                   sync.Mutex
	expectedRiskControlEnabled bool
	db                         *sql.DB
	encryptor                  appconfig.SecretEncryptor
	encryptionKeyConfigured    bool
	mu                         sync.RWMutex
	active                     *activeConfig
	expectedMode               string
	expectedRevision           int64
	loadError                  error
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
	return Config{Mode: "off", AuditScope: "full_request", Platforms: []string{}, AllGroups: true,
		GroupIDs: []int64{}, AuditPrompt: DefaultPolicy, Models: []ModelConfig{},
		Aggregation: "any_block", WorkerCount: 4, StorePassEvents: true}
}

func validateConfig(config Config, activating bool) error {
	if !slices.Contains([]string{"off", "async", "blocking"}, config.Mode) {
		return errors.New("审核模式无效")
	}
	if !slices.Contains([]string{"full_request", "current_turn"}, config.AuditScope) {
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
	if config.Warning.Enabled || config.Disable.Enabled {
		if _, err := mail.ParseAddress(config.AdminEmail); err != nil {
			return errors.New("启用处置前必须填写有效管理员通知邮箱")
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
	if _, err := chatCompletionsURL(model.BaseURL); err != nil {
		return err
	}
	return nil
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
	rows, err := m.db.QueryContext(ctx, `SELECT key,value FROM sub2api_enhance.settings WHERE key=$1 UNION ALL SELECT key,value FROM public.settings WHERE key=$2`, SettingKey, "risk_control_enabled")
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
		err = json.Unmarshal([]byte(values[SettingKey]), &stored)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expectedRiskControlEnabled = values["risk_control_enabled"] == "true"
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
	m.active = &activeConfig{Stored: stored, Keys: keys, RiskControlEnabled: values["risk_control_enabled"] == "true"}
	return nil
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
	if !m.expectedRiskControlEnabled {
		return "off"
	}
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

func (m *ConfigManager) ResolveKey(model ModelConfig) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active == nil || m.loadError != nil {
		return "", errors.New("节点配置不可用")
	}
	for _, current := range m.active.Stored.Models {
		if current.ID == model.ID && current.BaseURL == model.BaseURL && current.Model == model.Model {
			return m.active.Keys[model.ID], nil
		}
	}
	return "", errors.New("节点身份或凭据绑定已变化，请使用当前配置重新审核")
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
	result := PublicConfig{Config: stored.Config, Revision: stored.Revision,
		WarningRuleRevision: stored.WarningRuleRevision, HasAPIKeys: keys,
		UpdatedAt: stored.UpdatedAt, UpdatedBy: stored.UpdatedBy}
	result.ModelDefaults.TimeoutMS = DefaultNodeTimeoutMS
	return result
}

func (m *ConfigManager) Save(ctx context.Context, input ConfigUpdate, actorID int64) (PublicConfig, error) {
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
		if err := json.Unmarshal([]byte(raw), &current); err != nil {
			return PublicConfig{}, err
		}
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
	oldModels := make(map[string]ModelConfig)
	for _, model := range current.Models {
		oldModels[model.ID] = model
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
			old := oldModels[model.ID]
			if current.EncryptedKeys[model.ID] != "" && (old.BaseURL != model.BaseURL || old.Model != model.Model) {
				return PublicConfig{}, infraerrors.BadRequest("third_party_audit_key_binding_changed", "变更节点地址或模型时，请明确替换或清除该节点凭据")
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

// ReadSaved 不依赖节点凭据解密或运行快照，使管理员仍能修复不可应用的配置。
func (m *ConfigManager) ReadSaved(ctx context.Context) (PublicConfig, error) {
	stored := storedConfig{Config: DefaultConfig(), Revision: 1, EncryptedKeys: map[string]string{}}
	var raw string
	err := m.db.QueryRowContext(ctx, `SELECT value FROM sub2api_enhance.settings WHERE key=$1`, SettingKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PublicConfig{}, err
	}
	if err == nil {
		if err = json.Unmarshal([]byte(raw), &stored); err != nil {
			return PublicConfig{}, err
		}
	}
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
