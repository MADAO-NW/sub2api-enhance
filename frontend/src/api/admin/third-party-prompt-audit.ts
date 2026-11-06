import { apiClient } from '@/api/client'

export type AuditDecision = 'pass' | 'review' | 'block'
export type JobStatus = 'queued' | 'processing' | 'retry' | 'done' | 'failed' | 'skipped'
export type AuditMode = 'off' | 'async' | 'blocking'
export type PromptKind = 'instruction' | 'user'
export type Resolution = 'model' | 'cache' | 'inflight' | 'trust'

export interface AuditModel {
  id: string
  name: string
  enabled: boolean
  base_url: string
  model: string
  timeout_ms: number
  max_concurrency: number
  parameters?: Record<string, unknown>
}

export interface UserRuleConfig {
  user_id: number
  mode: 'async' | 'blocking'
  review_threshold: number
  block_threshold: number
  aggregation: 'any_block' | 'majority_block' | 'all_block'
  warning: { enabled: boolean; window: number; limit: number }
  disable: { enabled: boolean; limit: number }
}

export interface AuditConfig {
  mode: AuditMode
  platforms: string[]
  all_groups: boolean
  group_ids: number[]
  excluded_user_ids: number[]
  audit_prompt: string
  models: AuditModel[]
  review_threshold: number | null
  block_threshold: number | null
  aggregation: 'any_block' | 'majority_block' | 'all_block'
  worker_count: number
  warning: { enabled: boolean; window: number; limit: number }
  disable: { enabled: boolean; limit: number }
  user_rules: UserRuleConfig[]
  admin_email: string
  // retention_days 是审计记录保留天数，1–365，默认 30。
  retention_days: number
}

export interface SavedConfig extends AuditConfig {
  model_defaults: { timeout_ms: number; max_concurrency: number }
  rule_defaults: { review_threshold: number; block_threshold: number; warning_window: number; warning_limit: number; disable_limit: number }
  applied_revision: number
  instance_id: string
  revision: number
  warning_rule_revision: number
  has_api_keys: Record<string, boolean>
  updated_by: number
  updated_at: string
  application_error: string
}

export interface KeyUpdate { model_id: string; action: 'keep' | 'replace' | 'clear'; api_key?: string }
export interface ConfigUpdate { expected_revision: number; config: AuditConfig; keys: KeyUpdate[] }

export interface TemplateRule {
  client: string
  name: string
  position: string
  action: string
  max_bytes: number
  fields: string
}

export interface Contract {
  default_policy: string
  version: string
  output_contract: string
  template_rules: TemplateRule[]
}

export interface AuditError { code: string; message: string; stage: string; retryable: boolean }
export interface Score { confidence: number; reason: string }

export interface ProbeResult {
  ok: boolean
  model_id: string
  attempt_id: number
  result: Score | null
  error: AuditError | null
  tested_at: string
  latency_ms: number
}

export interface ModelAttempt {
  id: number
  job_id: number | null
  call_kind: 'audit' | 'probe' | 'health_probe'
  evaluation_round: number | null
  audit_round: number
  target_hash?: string
  model_id: string
  model_snapshot: AuditModel
  stage: string
  repair_of_attempt_id: number | null
  request_metadata: unknown
  status: string
  http_status: number | null
  raw_response: string | null
  result: Score | null
  input_tokens: number | null
  output_tokens: number | null
  latency_ms: number | null
  error_code: string
  error_message: string
  created_at: string
  dispatch_started_at: string | null
  finished_at: string | null
}

export interface DispatchSnapshot {
  order: number
  health: string
  active: number
  max_concurrency: number
  waiting: number
  load_ratio: number
  latency_ewma_ms?: number
  reason: string
}

// NodeResult 是一个审核节点对单个目标的投票。
export interface NodeResult {
  model_id: string
  model_name: string
  confidence: number | null
  reason: string
  attempt_id?: number
  error?: AuditError
  skipped?: boolean
  skip_reason?: string
  dispatch?: DispatchSnapshot
}

export interface ReviewView {
  decision: AuditDecision
  score: number | null
  reason: string
  nodes: NodeResult[]
  run_kind: 'request' | 'reaudit'
}

export interface JobTarget {
  round: number
  order: number
  hash: string
  kind: PromptKind
  text?: string
  bytes: number
  resolution?: Resolution
  decision?: AuditDecision
  error_code?: string
  error_message?: string
  resolved_at?: string
  trusted: boolean
  review?: ReviewView
}

export interface JobRound {
  round: number
  run_kind: 'request' | 'reaudit'
  requested_by: number | null
  reuse_mode: 'allow' | 'force'
  decision: AuditDecision
  trigger_hash?: string
  trigger_kind?: PromptKind
  config_revision: number | null
  started_at: string | null
  finished_at: string
}

export interface AuditAction {
  id: number
  user_id: number
  actor_user_id: number | null
  job_id: number | null
  action_type: string
  rule_snapshot: unknown
  business_snapshot: unknown
  execution_status?: string
  attempt_history?: unknown[]
  requested_at?: string
  completed_at?: string | null
  applied_at: string | null
  notification_status: string
  auth_cache_status: string
  auth_cache_error: string
  deliveries: { recipient: string; kind: string; status: string; last_error: string; attempts: unknown[] }[]
}

export interface AuditJob {
  id: number
  user_id: number
  api_key_id: number
  group_id: number | null
  identity: { username: string; user_email: string; api_key_name: string; group_name: string; endpoint: string }
  display_username: string
  display_email: string
  platform: string
  request_id: string
  model: string
  protocol: string
  client_fingerprint: string
  mode: 'async' | 'blocking'
  turn_fingerprint: string
  orphan_continuation: boolean
  status: JobStatus
  decision?: AuditDecision
  current_round: number
  current_run_kind: 'request' | 'reaudit'
  current_requested_by: number | null
  reuse_mode: 'allow' | 'force'
  trigger_hash?: string
  trigger_kind?: PromptKind
  enforcement_eligible: boolean
  violation_counted: boolean
  disable_counted: boolean
  config_revision: number | null
  attempts: number
  max_attempts: number
  lease_until: string | null
  next_attempt_at: string
  failure_stage: string
  last_error_code: string
  last_error_message: string
  blocking_wait_ms: number | null
  target_count: number
  started_at: string | null
  finished_at: string | null
  created_at: string
  updated_at: string
}

export interface JobDetail {
  job: AuditJob
  targets: JobTarget[]
  rounds: JobRound[]
  attempts: ModelAttempt[]
  actions: AuditAction[]
}

export interface TrustEntry { hash: string; preview: string; bytes: number; trusted_by: number; reason: string; created_at: string }

export interface BreakerEntry {
  hash: string
  preview: string
  bytes: number
  status: 'tripped' | 'trusted' | 'confirmed'
  distinct_users: number
  tripped_at: string
  tripped_job_id: number | null
  resolved_by: number | null
  resolved_at: string | null
}

export interface AuditUser {
  id: number
  username: string
  email: string
  role: string
  status: string
  disable_violation_count: number
  disable_reset_at: string | null
  action_pending: boolean
}

export interface AuditPage<T> { items: T[]; total: number; page: number; page_size: number }
export interface AuditFilter {
  ids?: number[]
  from?: string
  to?: string
  user_id?: number
  api_key_id?: number
  group_id?: number
  status?: string
  decision?: string
  run_kind?: string
  mode?: string
  platform?: string
  request_id?: string
  model?: string
}
export interface ReauditRequest { reuse_mode?: 'allow' | 'force'; filter: AuditFilter }
export interface ReauditResult { matched: number; ready: number; items: { job_id: number; status: string; reason: string }[] }

export interface Distribution { count: number; p50_ms: number | null; p95_ms: number | null }
export interface ClientMissRate { client: string; targets: number; model_calls: number; miss_rate: number }

export interface AuditRuntime {
  warning_enabled: boolean
  disable_enabled: boolean
  instance_id: string
  started_at: string
  as_of: string
  running: boolean
  mode: AuditMode
  revision: number
  expected_revision: number
  worker_capacity: number
  active_workers: number
  database_ok: boolean
  database_error: string
  config_error: string
  intake_failures: number
  last_success: string | null
  last_error: { code: string; message: string; at: string } | null
  evaluation_latency: Distribution & { capacity: number; from: string | null; to: string | null }
  probes: ProbeResult[]
  scheduler?: {
    backpressured_total: number
    blocking_waiters: number
    nodes: {
      model_id: string; health: string; active: number; max_concurrency: number; eligible_waiters: number; load_ratio: number
      latency_ewma_ms: number | null; consecutive_failures: number; cooldown_until: string | null; next_probe_at: string | null
      last_error_code: string; last_observed_at: string | null
    }[]
  }
  redis?: { ok: boolean; status: string; error: string; ttl_seconds: number; last_success: string | null }
}

export interface StatsQuery { from: string; to: string; timezone: string; mode?: string }

// AuditStats 只包含基于 audit_jobs 的指标，不再有 capture 与逐请求网关库存。
export interface AuditStats extends StatsQuery {
  jobs: number
  orphan_jobs: number
  reaudit_jobs: number
  failed_jobs: number
  decisions: Record<string, number>
  waiting_jobs: number
  waiting_since: string | null
  action_pending: number
  action_by_state: Record<string, number>
  blocking_jobs: number
  blocking_avg_ms: number | null
  blocking_max_ms: number | null
  cache_miss: ClientMissRate[]
}

const base = '/admin/third-party-prompt-audit'

export const thirdPartyPromptAuditAPI = {
  async getConfig() { return (await apiClient.get<SavedConfig>(`${base}/config`)).data },
  async saveConfig(value: ConfigUpdate) { return (await apiClient.put<SavedConfig>(`${base}/config`, value)).data },
  async testAdminEmail() { return (await apiClient.post(`${base}/test-email`)).data },
  async getContract() { return (await apiClient.get<Contract>(`${base}/contract`)).data },
  async listModels(value: { model_id: string; base_url: string; timeout_ms: number; key_action: 'keep' | 'replace'; api_key?: string }) {
    return (await apiClient.post<{ models: string[] }>(`${base}/models/list`, value)).data
  },
  async probe(value: { model: AuditModel; key_action: KeyUpdate['action']; api_key?: string; audit_prompt: string; input?: string; input_kind: 'text' | 'json' }) {
    // 节点总预算由后端控制，避免浏览器毫秒计时范围截断管理员设置的大值。
    return (await apiClient.post<ProbeResult>(`${base}/models/probe`, value, { timeout: 0 })).data
  },
  async probeDetails(id: number) { return (await apiClient.get<ModelAttempt[]>(`${base}/models/probes/${id}`)).data },
  async groups() { return (await apiClient.get<Record<string, unknown>[]>(`${base}/groups`)).data },
  async users() { return (await apiClient.get<AuditUser[]>(`${base}/users`)).data },
  async userKeys(id: number) { return (await apiClient.get<{ id: number; name: string }[]>(`${base}/users/${id}/api-keys`)).data },
  async runtime() { return (await apiClient.get<AuditRuntime>(`${base}/runtime`)).data },
  async stats(params: StatsQuery) { return (await apiClient.get<AuditStats>(`${base}/stats`, { params })).data },
  async jobs(filter: AuditFilter, page = 1, pageSize = 20) {
    return (await apiClient.get<AuditPage<AuditJob>>(`${base}/jobs`, { params: { ...filter, ids: filter.ids?.join(','), page, page_size: pageSize } })).data
  },
  async job(id: number) { return (await apiClient.get<JobDetail>(`${base}/jobs/${id}`)).data },
  async preview(value: ReauditRequest) { return (await apiClient.post<ReauditResult>(`${base}/reaudits/preview`, value)).data },
  async reaudit(value: ReauditRequest) { return (await apiClient.post<ReauditResult>(`${base}/reaudits`, value)).data },
  async trustList(page = 1, pageSize = 20) { return (await apiClient.get<AuditPage<TrustEntry>>(`${base}/trust`, { params: { page, page_size: pageSize } })).data },
  async trust(hash: string, reason = '') { return (await apiClient.post(`${base}/trust`, { hash, reason })).data },
  async untrust(hash: string) { return (await apiClient.delete(`${base}/trust/${hash}`)).data },
  async breakerList(status = '', page = 1, pageSize = 20) {
    return (await apiClient.get<AuditPage<BreakerEntry>>(`${base}/breakers`, { params: { status, page, page_size: pageSize } })).data
  },
  async confirmBreaker(hash: string) { return (await apiClient.post(`${base}/breakers/${hash}/confirm`)).data },
  async enableAndReset(id: number) { return (await apiClient.post<{ action_id: number; execution_status: string }>(`${base}/users/${id}/enable-and-reset`)).data },
  async retryAction(id: number) { return (await apiClient.post(`${base}/actions/${id}/retry`)).data }
}
