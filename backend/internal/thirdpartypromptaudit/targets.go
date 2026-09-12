package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
)

// SaveCanonicalTargets 保存可跨任务复用的规范化审核目标；正文不再依赖某一条 Capture 才能读取。
func (r *Repository) SaveCanonicalTargets(ctx context.Context, job *Job, target auditTarget) error {
	if r == nil || r.db == nil || job == nil {
		return nil
	}
	items := []struct {
		kind string
		body any
	}{
		{TargetKindCurrentUser, messageBundle{Protocol: target.Protocol, Messages: target.CurrentUser}},
		{TargetKindInstructionContext, messageBundle{Protocol: target.Protocol, Messages: target.InstructionContext}},
	}
	for _, item := range items {
		if item.kind == TargetKindInstructionContext && len(target.InstructionContext) == 0 {
			continue
		}
		hash, err := targetKeyHash(job, item.kind, item.body)
		if err != nil {
			return err
		}
		if _, err := r.db.ExecContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_job_targets(job_id,target_hash,target_kind,target_order) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, job.ID, hash, item.kind, 1); err != nil {
			return err
		}
		body, err := json.Marshal(auditEnvelope{Stage: item.kind, Target: item.body})
		if err != nil {
			return err
		}
		if _, err := r.db.ExecContext(ctx, `
INSERT INTO sub2api_enhance.third_party_prompt_audit_targets
(target_hash,target_kind,protocol,contract_version,target_order,target_body)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (target_hash) DO UPDATE SET updated_at=clock_timestamp()`,
			hash, item.kind, target.Protocol, job.Config.ContractVersion, 1, string(body)); err != nil {
			return err
		}
	}
	for _, segment := range target.EffectiveBehavior {
		body := messageBundle{Protocol: target.Protocol, Messages: []Segment{segment}}
		hash, err := targetKeyHash(job, TargetKindEffectiveBehavior, body)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(auditEnvelope{Stage: TargetKindEffectiveBehavior, Target: body})
		if err != nil {
			return err
		}
		if _, err := r.db.ExecContext(ctx, `
INSERT INTO sub2api_enhance.third_party_prompt_audit_targets
(target_hash,target_kind,protocol,contract_version,target_order,target_body)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (target_hash) DO UPDATE SET updated_at=clock_timestamp()`,
			hash, TargetKindEffectiveBehavior, target.Protocol, job.Config.ContractVersion, segment.Order, string(raw)); err != nil {
			return err
		}
		if _, err := r.db.ExecContext(ctx, `INSERT INTO sub2api_enhance.third_party_prompt_audit_job_targets(job_id,target_hash,target_kind,target_order) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, job.ID, hash, TargetKindEffectiveBehavior, segment.Order); err != nil {
			return err
		}
	}
	return nil
}

// LoadCanonicalTargets 从目标表恢复审核输入；旧任务没有目标记录时由调用方回退到 full_input_snapshot。
func (r *Repository) LoadCanonicalTargets(ctx context.Context, job *Job) (auditTarget, bool, error) {
	var target auditTarget
	if r == nil || r.db == nil || job == nil {
		return target, false, nil
	}
	target.Protocol = job.Protocol
	rows, err := r.db.QueryContext(ctx, `SELECT jt.target_kind,t.target_body FROM sub2api_enhance.third_party_prompt_audit_job_targets jt JOIN sub2api_enhance.third_party_prompt_audit_targets t ON t.target_hash=jt.target_hash WHERE jt.job_id=$1 ORDER BY jt.target_kind,jt.target_order`, job.ID)
	if err != nil {
		return target, false, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var kind, raw string
		if err := rows.Scan(&kind, &raw); err != nil {
			return target, false, err
		}
		var envelope auditEnvelope
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			return target, false, err
		}
		switch kind {
		case TargetKindCurrentUser:
			var bundle messageBundle
			if err := json.Unmarshal(mustJSON(envelope.Target), &bundle); err != nil {
				return target, false, err
			}
			target.CurrentUser = bundle.Messages
		case TargetKindInstructionContext:
			var bundle messageBundle
			if err := json.Unmarshal(mustJSON(envelope.Target), &bundle); err != nil {
				return target, false, err
			}
			target.InstructionContext = bundle.Messages
		case TargetKindEffectiveBehavior:
			var bundle messageBundle
			if err := json.Unmarshal(mustJSON(envelope.Target), &bundle); err != nil {
				return target, false, err
			}
			target.EffectiveBehavior = append(target.EffectiveBehavior, bundle.Messages...)
		}
		found = true
	}
	return target, found, rows.Err()
}

func mustJSON(value any) []byte { raw, _ := json.Marshal(value); return raw }

func targetKeyHash(job *Job, kind string, body any) (string, error) {
	return fingerprint(struct {
		Protocol        string
		ContractVersion string
		Kind            string
		Body            any
	}{Protocol: job.Protocol, ContractVersion: job.Config.ContractVersion, Kind: kind, Body: body})
}
