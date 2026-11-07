package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
	"time"
)

// configImpactWindow 是缓存影响估算的统计窗口。
const configImpactWindow = 24 * time.Hour

// ConfigImpact 是保存草稿配置后对全库审核缓存的影响，只读计算结果。
type ConfigImpact struct {
	InstructionChanged bool `json:"instruction_changed"`
	UserChanged        bool `json:"user_changed"`
	// ChangedParts 是导致评估指纹变化的配置部分：policy、nodes。
	ChangedParts []string `json:"changed_parts"`
	// RecentInstructionBlocks 是窗口内出现过且未被信任的不同指令块数。
	RecentInstructionBlocks int64 `json:"recent_instruction_blocks"`
	WindowHours             int   `json:"window_hours"`
}

// Impact 用当前固定协议分别计算已保存与草稿的评估指纹差异，并统计窗口内的指令块数量。
// 只读：不写库，不调用模型。
func (m *ConfigManager) Impact(ctx context.Context, draft Config) (ConfigImpact, error) {
	active, err := m.Active()
	if err != nil {
		return ConfigImpact{}, err
	}
	draftSnapshot := active
	draftSnapshot.Config = draft
	impact := ConfigImpact{ChangedParts: []string{}, WindowHours: int(configImpactWindow / time.Hour)}
	if active.AuditPrompt != draft.AuditPrompt {
		impact.ChangedParts = append(impact.ChangedParts, "policy")
	}
	if !sameNodeSemantics(active.Models, draft.Models) {
		impact.ChangedParts = append(impact.ChangedParts, "nodes")
	}
	for _, kind := range []string{PromptKindInstruction, PromptKindUser} {
		saved, err := evaluationHash(active, kind)
		if err != nil {
			return ConfigImpact{}, err
		}
		drafted, err := evaluationHash(draftSnapshot, kind)
		if err != nil {
			return ConfigImpact{}, err
		}
		if kind == PromptKindInstruction {
			impact.InstructionChanged = saved != drafted
			continue
		}
		impact.UserChanged = saved != drafted
	}
	if err := m.db.QueryRowContext(ctx, `SELECT count(DISTINCT t.hash)
 FROM sub2api_enhance.audit_job_targets t
 JOIN sub2api_enhance.audit_jobs j ON j.id = t.job_id
 WHERE t.kind = 'instruction'
   AND j.created_at > clock_timestamp() - $1::interval
   AND NOT EXISTS (SELECT 1 FROM sub2api_enhance.prompt_trust pt WHERE pt.hash = t.hash)`, interval(configImpactWindow)).Scan(&impact.RecentInstructionBlocks); err != nil {
		return ConfigImpact{}, err
	}
	return impact, nil
}

// sameNodeSemantics 比较两端启用节点的语义列表，顺序变化同样视为变化。
func sameNodeSemantics(saved, drafted []ModelConfig) bool {
	savedNodes, draftedNodes := []any{}, []any{}
	for _, model := range saved {
		if model.Enabled {
			savedNodes = append(savedNodes, nodeSemantics(model))
		}
	}
	for _, model := range drafted {
		if model.Enabled {
			draftedNodes = append(draftedNodes, nodeSemantics(model))
		}
	}
	savedRaw, err := json.Marshal(savedNodes)
	if err != nil {
		return false
	}
	draftedRaw, err := json.Marshal(draftedNodes)
	if err != nil {
		return false
	}
	return string(savedRaw) == string(draftedRaw)
}
