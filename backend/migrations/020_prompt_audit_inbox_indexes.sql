-- 待处理工作台的计数与最近条目只关心少量需要人工处理的处置动作。处置动作表不在保留期清理范围内，
-- 每次阻断都会写入一行，用部分索引避免每次刷新全表扫描；索引只收录命中谓词的行，体积随待处理事项增长。
-- 001 的 tppa_actions_outbox_idx 只覆盖 pending/processing/unknown/retry 等在途状态，不含 failed。

-- 账号动作待核对：执行失败或结果不确定，不设时间窗口，按 id 倒序取最近条目。
CREATE INDEX IF NOT EXISTS tppa_actions_attention_idx
    ON sub2api_enhance.third_party_prompt_audit_enforcement_actions (id DESC)
    WHERE execution_status IN ('failed', 'unknown');

-- 通知投递失败：工作台只统计最近 7 天请求的动作，按请求时间范围扫描。
CREATE INDEX IF NOT EXISTS tppa_actions_notification_failed_idx
    ON sub2api_enhance.third_party_prompt_audit_enforcement_actions (requested_at DESC, id DESC)
    WHERE notification_status = 'failed';
