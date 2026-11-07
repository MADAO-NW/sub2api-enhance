-- 统计与相似块判定所需的结构补充，均为可空列或非阻塞索引。
ALTER TABLE sub2api_enhance.prompt_texts ADD COLUMN IF NOT EXISTS simhash BIGINT;
CREATE INDEX IF NOT EXISTS prompt_texts_simhash_idx ON sub2api_enhance.prompt_texts (simhash) WHERE simhash IS NOT NULL;

-- 统计页按时间范围取分位与总数，补两个部分索引避免全表扫描。
CREATE INDEX IF NOT EXISTS audit_jobs_finished_created_idx ON sub2api_enhance.audit_jobs (created_at) WHERE finished_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS audit_jobs_blocking_created_idx ON sub2api_enhance.audit_jobs (created_at) WHERE blocking_wait_ms IS NOT NULL;
CREATE INDEX IF NOT EXISTS audit_job_targets_kind_idx ON sub2api_enhance.audit_job_targets (kind, job_id);
