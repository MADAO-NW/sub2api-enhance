-- 审核节点凭据失效（401/403）需要单独通知管理员，新增动作类型。
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    DROP CONSTRAINT third_party_prompt_audit_enforcement_actions_action_type_check;
ALTER TABLE sub2api_enhance.third_party_prompt_audit_enforcement_actions
    ADD CONSTRAINT third_party_prompt_audit_enforcement_actions_action_type_check
    CHECK (action_type IN ('warning', 'disable', 'counter_reset', 'blocking_notice', 'breaker_notice', 'node_auth_notice'));
