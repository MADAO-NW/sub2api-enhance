import { apiClient } from '@/api/client'

export type AuditDecision = 'pass' | 'review' | 'block'
export type JobStatus = 'queued' | 'processing' | 'retry' | 'done' | 'failed' | 'skipped'
export type AuditMode = 'off' | 'async' | 'blocking'
export type PromptKind = 'instruction' | 'user'
export type Resolution = 'model' | 'cache' | 'inflight' | 'trust'
// ReuseMode 是审核轮次的复用方式；fresh 表示只复用本次重审发起之后产生的结论。
export type ReuseMode = 'allow' | 'force' | 'fresh'
export type SampleExpected = 'pass' | 'block'
export type InboxCategoryKey = 'action_failed' | 'notification_failed' | 'breaker_pending' | 'node_unhealthy' | 'failure_rate'

export interface AuditModel {
  id: string
  name: string
  enabled: boolean
  base_url: string
  model: string
  timeout_ms: number
  max_concurrency: number
  parameters?: Record<string, unknown>
  // allow_self_audit 允许把节点指向本机/内网网关（自审）。
  allow_self_audit?: boolean
}

// UserRuleConfig 只覆盖处理方式与两个阈值；聚合、提醒和停用一律跟随全局配置。
export interface UserRuleConfig {
  user_id: number
  mode: 'async' | 'blocking'
  review_threshold: number
  block_threshold: number
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
  // category_rules 是违规类别到处置级别的映射（immediate_disable / notice_only）。
  category_rules: Record<string, string>
  // escalate_on_warning 表示提醒触发时把 async 用户加严为 blocking。
  escalate_on_warning: boolean
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
  // breaker_user_threshold 是指令块熔断的不同用户数阈值，只读，用于提示停用阈值风险。
  breaker_user_threshold: number
}

export interface KeyUpdate { model_id: string; action: 'keep' | 'replace' | 'clear'; api_key?: string }
export interface ConfigUpdate { expected_revision: number; config: AuditConfig; keys: KeyUpdate[]; legacy_policy_confirmed?: boolean }

// ConfigImpact 是保存前的影响估算：只读，不写库也不调用模型。
export interface ConfigImpact {
  instruction_changed: boolean
  user_changed: boolean
  changed_parts: ('policy' | 'nodes')[]
  recent_instruction_blocks: number
  window_hours: number
}

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
  // sampled 表示该目标已加入误判样本集。
  sampled?: boolean
  categories?: string[]
  review?: ReviewView
}

export interface JobRound {
  round: number
  run_kind: 'request' | 'reaudit'
  requested_by: number | null
  reuse_mode: ReuseMode
  decision: AuditDecision
  trigger_hash?: string
  trigger_kind?: PromptKind
  trigger_categories?: string[]
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

// ActionRecord 是 GET /actions 的一行：动作全字段加用户显示名。
export interface ActionRecord extends AuditAction { username: string }
// SkipNotificationsResult 是批量跳过通知的逐项结果：按请求顺序归类成功、状态冲突与不存在。
export interface SkipNotificationsResult { skipped: number[]; conflicts: number[]; missing: number[] }
export interface ActionFilter {
  // execution_status 可用逗号分隔多个状态，如 failed,unknown。
  execution_status?: string
  notification_status?: string
  action_type?: string
  user_id?: number
  from?: string
  to?: string
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
  reuse_mode: ReuseMode
  trigger_hash?: string
  trigger_kind?: PromptKind
  // trigger_categories 是触发目标的违规类别；升级前的任务为空。
  trigger_categories?: string[]
  // sampled 表示该任务已有目标加入误判样本集。
  sampled?: boolean
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
  // duration_ms 是任务从创建到结束的总耗时，两种模式都有值。
  duration_ms: number
  // processing_ms 是开始处理到结束的耗时，不含排队。
  processing_ms: number
  target_count: number
  started_at: string | null
  finished_at: string | null
  created_at: string
  updated_at: string
}

export interface JobPrompt {
  order: number
  kind: PromptKind
  hash: string
  text: string
  bytes: number
  resolution?: Resolution
  decision?: AuditDecision
  reason?: string
  score: number | null
  trusted: boolean
  // sampled 表示该目标已加入误判样本集。
  sampled?: boolean
}

export interface JobDetail {
  job: AuditJob
  targets: JobTarget[]
  rounds: JobRound[]
  attempts: ModelAttempt[]
  actions: AuditAction[]
}

// MetricsHour 是每小时指标快照。
export interface MetricsHour { hour: string; jobs: number; blocked: number; failed: number; avg_blocking_ms: number | null }

export interface ClientChurn { client: string; jobs: number; distinct: number; churn_rate: number }

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
  user_ids: number[]
  similar: number
}

// SimilarBlock 是与指定指令块相似的候选块；只用于逐块查看差异，不做批量信任。
export interface SimilarBlock {
  hash: string
  preview: string
  bytes: number
  distance: number
  breaker_status: string
  trusted: boolean
  distinct_users: number
}

// SimilarPair 是指令块与所选相似块的全文；预览只有前 500 字，差异对比必须使用全文。
export interface SimilarPair {
  hash: string
  text: string
  candidate: string
  candidate_text: string
  distance: number
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

// RiskUser 是用户风险榜的一行；统计范围受保留天数限制。
export interface RiskUser {
  user_id: number
  username: string
  email: string
  role: string
  status: string
  jobs: number
  blocks: number
  reviews: number
  violations: number
  last_block_at: string | null
  disable_violation_count: number
  disable_reset_at: string | null
  pending_actions: number
  escalated: boolean
  categories: string[]
}

export type RiskUserSort = 'blocks' | 'violations' | 'last_block_at'
export interface RiskUserQuery { from: string; to: string; sort: RiskUserSort; keyword?: string }

export interface InboxEntry {
  category: InboxCategoryKey
  id?: number
  action_type?: string
  user_id?: number
  username?: string
  job_id?: number | null
  status?: string
  detail?: string
  // 节点项：name 是节点显示名，health 用 i18n 本地化，error_code 是最近一次失败码。
  name?: string
  health?: string
  error_code?: string
  hash?: string
  occurred_at: string
}

export interface InboxCategory { key: InboxCategoryKey; count: number; items: InboxEntry[] }

export interface FailureRateSnapshot {
  window_minutes: number
  jobs: number
  failed: number
  rate: number
  threshold: number
  min_jobs: number
  exceeded: boolean
}

export interface Inbox {
  total: number
  action_failed: number
  notification_failed: number
  breaker_pending: number
  node_unhealthy: number
  failure_rate?: FailureRateSnapshot
  categories: InboxCategory[]
  // notification_window_days 是通知投递失败的统计窗口天数，下钻列表按同一窗口筛选。
  notification_window_days: number
}

export interface CostBucket {
  bucket: string
  model_id: string
  // node_name 优先取当前配置名称，配置已删除时回退到调用快照。
  node_name: string
  call_kind: 'audit' | 'probe' | 'health_probe'
  stage: string
  calls: number
  succeeded: number
  input_tokens: number
  output_tokens: number
  unknown_usage: number
  p50_latency_ms: number | null
}

export interface CostReuse { kind: PromptKind; resolution: Resolution; targets: number }

export interface CostReport {
  from: string
  to: string
  bucket: 'day' | 'hour'
  timezone: string
  buckets: CostBucket[]
  reuse: CostReuse[]
}

export interface Sample {
  id: number
  hash: string
  kind: PromptKind
  expected: SampleExpected
  note: string
  source_job_id?: number
  created_by: number
  created_at: string
  updated_at: string
  preview: string
}

export interface SampleInput { job_id: number; target_order: number; expected: SampleExpected; note?: string }

export interface ReplayNodeResult {
  model_id: string
  model_name: string
  score: number | null
  decision: string
  reason: string
  error?: string
}

export interface PolicyReplayResult {
  sample_id: number
  expected: SampleExpected
  actual: string
  score: number | null
  categories: string[]
  nodes: ReplayNodeResult[]
  error?: string
}

export interface PolicyReplay {
  id: number
  status: 'running' | 'done' | 'failed' | 'interrupted'
  config_snapshot: string
  sample_ids: number[]
  results: PolicyReplayResult[]
  error_message: string
  created_by: number
  created_at: string
  finished_at: string | null
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
  // category 按 trigger_categories 过滤，形如 R07。
  category?: string
}
export interface ReauditRequest { filter: AuditFilter }
export interface ReauditResult { matched: number; ready: number; estimated_targets: number; items: { job_id: number; status: string; reason: string }[] }

export interface Distribution { count: number; p50_ms: number | null; p95_ms: number | null }
export interface ClientMissRate { client: string; kind: PromptKind; targets: number; model_calls: number; miss_rate: number }

// RetentionRecord 是最近一次保留期清理结果。
export interface RetentionRecord {
  finished_at: string
  days: number
  jobs: number
  probe_attempts: number
  reviews: number
  texts: number
  duration_ms: number
  // processing_ms 是开始处理到结束的耗时，不含排队。
  processing_ms: number
  error: string
}

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
  retention?: RetentionRecord
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
  redis?: { ok: boolean; status: string; error: string; ttl_seconds: number; check_error: string; last_success: string | null }
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
  blocking_p50_ms: number | null
  blocking_p95_ms: number | null
  job_p50_ms: number | null
  job_p95_ms: number | null
  queue_p50_ms: number | null
  queue_p95_ms: number | null
  processing_p50_ms: number | null
  processing_p95_ms: number | null
  cache_miss: ClientMissRate[]
  // category_recognition 按节点模型统计 block/review 目标的类别识别率。
  category_recognition: CategoryRecognition[]
  // category_distribution 是时间范围内阻断任务的完整类别分布，由后端聚合。
  category_distribution: CategoryCount[]
}

export interface CategoryRecognition { model_id: string; total: number; recognized: number; rate: number }
// CategoryCount 是阻断任务按触发类别的计数；category 为空表示未分类，多类别任务在每个类别下各计一次。
export interface CategoryCount { category: string; count: number }
// CategoryName 是政策声明的类别编号与标题，如 R07 恶意使用。
export interface CategoryName { code: string; name: string }

const base = '/admin/third-party-prompt-audit'

export const thirdPartyPromptAuditAPI = {
  async getConfig() { return (await apiClient.get<SavedConfig>(`${base}/config`)).data },
  // categories 返回当前政策声明的类别编号与标题，供界面在编号后补名称。
  async categories() { return (await apiClient.get<{ items: CategoryName[] }>(`${base}/categories`)).data },
  async saveConfig(value: ConfigUpdate) { return (await apiClient.put<SavedConfig>(`${base}/config`, value)).data },
  // configImpact 只做保存前的影响估算，失败不影响保存。
  async configImpact(value: AuditConfig) { return (await apiClient.post<ConfigImpact>(`${base}/config/impact`, value)).data },
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
  async runtime() { return (await apiClient.get<AuditRuntime>(`${base}/runtime`)).data },
  async stats(params: StatsQuery) { return (await apiClient.get<AuditStats>(`${base}/stats`, { params })).data },
  async cost(params: StatsQuery & { bucket: 'day' | 'hour' }) { return (await apiClient.get<CostReport>(`${base}/stats/cost`, { params })).data },
  async inbox() { return (await apiClient.get<Inbox>(`${base}/inbox`)).data },
  async riskUsers(query: RiskUserQuery, page = 1, pageSize = 20) {
    return (await apiClient.get<AuditPage<RiskUser>>(`${base}/users/risk`, { params: { ...query, page, page_size: pageSize } })).data
  },
  async actions(filter: ActionFilter, page = 1, pageSize = 20) {
    return (await apiClient.get<AuditPage<ActionRecord>>(`${base}/actions`, { params: { ...filter, page, page_size: pageSize } })).data
  },
  async jobs(filter: AuditFilter, page = 1, pageSize = 20) {
    return (await apiClient.get<AuditPage<AuditJob>>(`${base}/jobs`, { params: { ...filter, ids: filter.ids?.join(','), page, page_size: pageSize } })).data
  },
  async job(id: number) { return (await apiClient.get<JobDetail>(`${base}/jobs/${id}`)).data },
  async jobPrompts(id: number) { return (await apiClient.get<{ targets: JobPrompt[] }>(`${base}/jobs/${id}/prompts`)).data },
  async preview(value: ReauditRequest) { return (await apiClient.post<ReauditResult>(`${base}/reaudits/preview`, value)).data },
  async reaudit(value: ReauditRequest) { return (await apiClient.post<ReauditResult>(`${base}/reaudits`, value)).data },
  async trustList(page = 1, pageSize = 20, q = '') { return (await apiClient.get<AuditPage<TrustEntry>>(`${base}/trust`, { params: { page, page_size: pageSize, q } })).data },
  async trust(hash: string, reason = '') { return (await apiClient.post(`${base}/trust`, { hash, reason })).data },
  async untrust(hash: string) { return (await apiClient.delete(`${base}/trust/${hash}`)).data },
  async metricsHistory(days = 7) { return (await apiClient.get<{ items: MetricsHour[] }>(`${base}/stats/history`, { params: { days } })).data },
  async deleteUserData(userId: number) { return (await apiClient.delete<{ deleted_jobs: number }>(`${base}/users/${userId}/data`)).data },
  async exportUserData(userId: number) {
    const response = await apiClient.get(`${base}/users/${userId}/export`, { responseType: 'blob', headers: { 'X-Enhance-Request': '1' } })
    return response.data as Blob
  },
  async templateChurn() { return (await apiClient.get<{ items: ClientChurn[] }>(`${base}/templates/churn`)).data },
  async breakerList(status = '', page = 1, pageSize = 20, q = '') {
    return (await apiClient.get<AuditPage<BreakerEntry>>(`${base}/breakers`, { params: { status, page, page_size: pageSize, q } })).data
  },
  async similarBlocks(hash: string) { return (await apiClient.get<{ items: SimilarBlock[] }>(`${base}/breakers/${hash}/similar`)).data },
  async similarPair(hash: string, candidate: string) { return (await apiClient.get<SimilarPair>(`${base}/breakers/${hash}/similar/${candidate}`)).data },
  async confirmBreaker(hash: string) { return (await apiClient.post(`${base}/breakers/${hash}/confirm`)).data },
  async trustBatch(hashes: string[], reason = '') { return (await apiClient.post<{ trusted: string[] }>(`${base}/trust/batch`, { hashes, reason })).data },
  async exportJobs(filter: AuditFilter, limit = 1000, offset = 0) {
    const response = await apiClient.get(`${base}/jobs/export`, { params: { ...filter, ids: filter.ids?.join(','), limit, offset }, responseType: 'blob', headers: { 'X-Enhance-Request': '1' } })
    return { blob: response.data as Blob, hasMore: response.headers['x-export-has-more'] === 'true' }
  },
  async enableAndReset(id: number) { return (await apiClient.post<{ action_id: number; execution_status: string; escalation_cleared: boolean }>(`${base}/users/${id}/enable-and-reset`)).data },
  async clearEscalation(id: number) { return (await apiClient.post<{ user_id: number; action_id: number }>(`${base}/users/${id}/clear-escalation`)).data },
  async retryAction(id: number) { return (await apiClient.post(`${base}/actions/${id}/retry`)).data },
  // skipNotification 跳过一条投递失败的通知，不再重试发送。
  async skipNotification(id: number) { return (await apiClient.post(`${base}/actions/${id}/skip-notification`)).data },
  // skipNotifications 批量跳过投递失败的通知，返回成功、冲突与不存在的逐项结果。
  async skipNotifications(ids: number[]) { return (await apiClient.post<SkipNotificationsResult>(`${base}/actions/skip-notifications`, { ids })).data },
  async samples(params: { kind?: string; expected?: string }, page = 1, pageSize = 20) {
    return (await apiClient.get<AuditPage<Sample>>(`${base}/samples`, { params: { ...params, page, page_size: pageSize } })).data
  },
  async createSample(value: SampleInput) { return (await apiClient.post<Sample>(`${base}/samples`, value)).data },
  async updateSample(id: number, value: { expected: SampleExpected; note: string }) {
    return (await apiClient.patch<{ id: number }>(`${base}/samples/${id}`, value)).data
  },
  async deleteSample(id: number) { return (await apiClient.delete<{ id: number }>(`${base}/samples/${id}`)).data },
  async startReplay(config: AuditConfig, sampleIDs: number[]) {
    return (await apiClient.post<PolicyReplay>(`${base}/policy/replays`, { config, sample_ids: sampleIDs })).data
  },
  async replays(limit = 20) { return (await apiClient.get<{ items: PolicyReplay[] }>(`${base}/policy/replays`, { params: { limit } })).data },
  async replay(id: number) { return (await apiClient.get<PolicyReplay>(`${base}/policy/replays/${id}`)).data },
}
