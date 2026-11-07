-- 重新审核统一为“只复用本次重审发起之后产生的结论”。

ALTER TABLE sub2api_enhance.audit_jobs ADD COLUMN IF NOT EXISTS reuse_after TIMESTAMPTZ;

ALTER TABLE sub2api_enhance.audit_jobs DROP CONSTRAINT IF EXISTS audit_jobs_reuse_mode_check;
ALTER TABLE sub2api_enhance.audit_jobs ADD CONSTRAINT audit_jobs_reuse_mode_check
    CHECK (reuse_mode IN ('allow', 'force', 'fresh'));

ALTER TABLE sub2api_enhance.audit_job_rounds DROP CONSTRAINT IF EXISTS audit_job_rounds_reuse_mode_check;
ALTER TABLE sub2api_enhance.audit_job_rounds ADD CONSTRAINT audit_job_rounds_reuse_mode_check
    CHECK (reuse_mode IN ('allow', 'force', 'fresh'));

-- 新进程在启动阶段执行迁移，此时旧 Worker 已停止；尚未结束的强制重审改用新语义。
UPDATE sub2api_enhance.audit_jobs
SET reuse_mode = 'fresh', reuse_after = updated_at
WHERE reuse_mode = 'force' AND status IN ('queued', 'processing', 'retry');
