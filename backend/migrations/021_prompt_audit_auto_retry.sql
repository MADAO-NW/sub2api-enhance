-- 失败任务自动重试：按退避计划最多重排 3 次，无健康节点时保持失败等待人工处理。
-- 计数只统计自动重试次数，不影响 attempts（每次重排会把 attempts 归零，重新获得 3 次评估预算）。
ALTER TABLE sub2api_enhance.audit_jobs
    ADD COLUMN IF NOT EXISTS auto_retry_count INTEGER NOT NULL DEFAULT 0;
