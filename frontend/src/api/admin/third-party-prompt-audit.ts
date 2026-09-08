import { apiClient } from '@/api/client'

export type AuditDecision = 'pass' | 'review' | 'block'
export type JobStatus = 'queued' | 'processing' | 'retry' | 'done' | 'failed' | 'skipped'
export type AuditMode = 'off' | 'async' | 'blocking'
export interface AuditModel {
  id: string
  name: string
  enabled: boolean
  base_url: string
  model: string
  timeout_ms: number
}
export interface AuditConfig {
  mode: AuditMode
  audit_scope: 'full_request' | 'current_turn'
  platforms: string[]
  all_groups: boolean
  group_ids: number[]
  audit_prompt: string
  models: AuditModel[]
  review_threshold: number | null
  block_threshold: number | null
  aggregation: 'any_block' | 'majority_block' | 'all_block'
  worker_count: number
  store_pass_events: boolean
  warning: { enabled: boolean; window: number; limit: number }
  disable: { enabled: boolean; limit: number }
  admin_email: string
}
export interface SavedConfig extends AuditConfig {
  model_defaults: { timeout_ms: number }
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
export interface Contract { default_policy: string; version: string; output_contract: string }
export interface DecisionConfig { revision: number; review_threshold: number | null; block_threshold: number | null }
export interface AuditError { code: string; message: string; stage: string; retryable: boolean }
export interface Score { confidence: number; reason: string }
export interface ProbeResult { ok: boolean; model_id: string; attempt_id: number; result: Score | null; error: AuditError | null; tested_at: string; latency_ms: number }
export interface Reuse { whole_lookups: number; whole_hits: number; segment_lookups: number; segment_hits: number; within_job_hits: number }
export interface AuditInput { protocol: string; fields?: Record<string, unknown>; non_text: { source_path: string; type: string }[]; raw_body_base64?: string }
export interface SegmentMeta { order: number; source_path: string; source_role: string; policy_role: string; turn_scope: string; selected: boolean }
export interface ModelResult {
  model_id: string
  model_name: string
  decision?: AuditDecision
  basis: string
  confidence: number | null
  max_segment_confidence?: number | null
  reason: string
  reused: boolean
  joint_attempt_id?: number
  error?: AuditError
  segments: { order: number; source_path: string; reuse_kind: string; result: Score & { id: number; source_attempt_id: number; source_role: string; policy_role: string; turn_scope: string } }[]
}
export interface Outcome { id: number; job_id: number; user_id: number; decision: AuditDecision; models: ModelResult[]; partial_failure: boolean; enforcement_eligible: boolean; source_outcome_id?: number; created_at: string; decision_config?: DecisionConfig }
export interface AuditJob {
  capture_id?: number|null
  id: number
  run_kind: 'request' | 'reaudit'
  source_job_id: number | null
  requested_by: number | null
  user_id: number
  api_key_id: number | null
  group_id: number | null
  request_id: string
  identity: { username: string; user_email: string; api_key_name: string; group_name: string; endpoint: string }
  platform: string
  protocol: string
  ingress_stage: string
  requested_model: string
  execution_mode: AuditMode
  config_snapshot?: AuditConfig & { revision: number; contract_version: string; fixed_contract: string; fixed_roles?: string }
  decision_config?: DecisionConfig
  full_input_snapshot?: AuditInput
  snapshot_status: string
  input_manifest?: SegmentMeta[]
  input_hash: string
  target_hash: string
  evaluation_hash: string
  status: JobStatus
  attempts: number
  max_attempts: number
  result_checkpoint?: { decision: AuditDecision; models: ModelResult[] }
  reuse_metrics: Reuse
  failure_stage: string
  last_error_code: string
  last_error_message: string
  gateway_result: string
  next_attempt_at: string
  started_at: string | null
  finished_at: string | null
  created_at: string
  updated_at: string
}
export interface AuditEvent { id: number; job_id: number; original_outcome_id: number | null; latest_outcome_id: number; job?: AuditJob; latest?: Outcome; original?: Outcome; reaudit_status?: string; created_at: string; updated_at: string }
export interface AuditCapture {
  id: number; capture_key: string; transport: string; protocol: string; body_format: string; raw_body?: string; body_bytes: number; body_sha256: string
  snapshot_status: string; eligibility_status: string; processing_status: string; forwarding_status: string; created_at: string; last_error_message: string
  identity?: unknown; metadata?: unknown; forwarding_observations?: unknown
}
export interface ModelAttempt {
  id: number; job_id: number | null; call_kind: string; evaluation_round: number | null; model_id: string; model_snapshot: AuditModel
  stage: string; segment_order: number | null; repair_of_attempt_id: number | null; request_metadata: unknown; status: string
  http_status: number | null; raw_response: string | null; result: Score | null; input_tokens: number | null; output_tokens: number | null
  latency_ms: number | null; error_code: string; error_message: string; created_at: string; dispatch_started_at: string | null; finished_at: string | null
}
export interface AuditAction {
  id: number; user_id: number; actor_user_id: number | null; outcome_id: number | null; action_type: string; rule_snapshot: unknown; business_snapshot: unknown
  execution_status?: string; attempt_history?: unknown[]; requested_at?: string; completed_at?: string|null;
  applied_at: string|null; notification_status: string; auth_cache_status: string; auth_cache_error: string
  deliveries: { recipient: string; kind: string; status: string; last_error: string; attempts: unknown[] }[]
}
export interface JobDetail { input_json: string; input_parts: (SegmentMeta & { content: { type: string; text: string; source_path: string }[] })[]; non_text: { type: string; source_path: string }[]; job: AuditJob; outcome: Outcome | null; reaudits: AuditJob[]; attempts: ModelAttempt[]; actions: AuditAction[] }
export interface EventDetail { event: AuditEvent; detail: JobDetail }
export interface AuditFilter { ids?: number[]; from?: string; to?: string; user_id?: number; api_key_id?: number; group_id?: number; status?: string; decision?: string; run_kind?: string; mode?: string; platform?: string; request_id?: string; keyword?: string; model_id?: string }
export interface AuditPage<T> { items: T[]; total: number; page: number; page_size: number }
export interface ReauditRequest { source: 'jobs' | 'events'; filter: AuditFilter }
export interface ReauditResult { matched: number; ready: number; items: { source_job_id: number; job_id: number | null; status: string; reason: string }[] }
export interface Distribution { count: number; p50_ms: number | null; p95_ms: number | null }
export interface AuditRuntime {
  warning_enabled: boolean; disable_enabled: boolean
  instance_id: string; started_at: string; as_of: string; running: boolean; mode: AuditMode; revision: number; expected_revision: number
  worker_capacity: number; active_workers: number; database_ok: boolean; database_error: string; config_error: string; input_persist_failures: number
  last_success: string | null; last_error: { code: string; message: string; at: string } | null
  evaluation_latency: Distribution & { capacity: number; from: string | null; to: string | null }; probes: ProbeResult[]
}
export interface StatsQuery { from: string; to: string; timezone: string; mode?: string; model_id?: string; stage?: string }
export interface AuditStats extends StatsQuery {
  capture_stock?: Record<string,number>; forwarding_stock?: Record<string,number>; action_execution_stock?: Record<string,number>;
  as_of: string; stock: Record<string, number>; oldest_waiting_at: string | null; waiting_for_slot: number; cohort: Record<string, number>
  received: number; reaudits_created: number; formal: Record<string, number>; reaudit: Record<string, number>; failures: Record<string, number>
  events: Record<string, number>; gateway: Record<string, number>; gateway_latency: Distribution; task_latency: Distribution
  calls: { model_id: string; call_kind: string; stage: string; total: number; http_success: number; valid_result: number; failed: number; unknown: number; in_flight: number; errors: Record<string, number>; latency: Distribution }[]
  evaluation_rounds: number; reuse: Reuse; actions: Record<string, number>; notification_stock: Record<string, number>; delivery_stock: Record<string, number>; auth_cache_stock: Record<string, number>
}

const base = '/admin/third-party-prompt-audit'
export const thirdPartyPromptAuditAPI = {
  async getConfig() { return (await apiClient.get<SavedConfig>(`${base}/config`)).data },
  async saveConfig(value: ConfigUpdate) { return (await apiClient.put<SavedConfig>(`${base}/config`, value)).data },
  async getContract() { return (await apiClient.get<Contract>(`${base}/contract`)).data },
  async probe(value: { model: AuditModel; key_action: KeyUpdate['action']; api_key?: string; audit_prompt: string; protocol?: string; input?: string; input_kind: 'text' | 'json' }) {
    // 节点总预算由后端控制，避免浏览器毫秒计时范围截断管理员设置的大值。
    return (await apiClient.post<ProbeResult>(`${base}/models/probe`, value, { timeout: 0 })).data
  },
  async runtime() { return (await apiClient.get<AuditRuntime>(`${base}/runtime`)).data },
  async stats(params: StatsQuery) { return (await apiClient.get<AuditStats>(`${base}/stats`, { params })).data },
  async jobs(filter: AuditFilter, page = 1, pageSize = 20) { return (await apiClient.get<AuditPage<AuditJob>>(`${base}/jobs`, { params: { ...filter, ids: filter.ids?.join(','), page, page_size: pageSize } })).data },
  async events(filter: AuditFilter, page = 1, pageSize = 20) { return (await apiClient.get<AuditPage<AuditEvent>>(`${base}/events`, { params: { ...filter, ids: filter.ids?.join(','), page, page_size: pageSize } })).data },
  async job(id: number) { return (await apiClient.get<JobDetail>(`${base}/jobs/${id}`)).data },
  async event(id: number) { return (await apiClient.get<EventDetail>(`${base}/events/${id}`)).data },
  async capture(id: number) { return (await apiClient.get<AuditCapture>(`${base}/captures/${id}`)).data },
  async preview(value: ReauditRequest) { return (await apiClient.post<ReauditResult>(`${base}/reaudits/preview`, value)).data },
  async reaudit(value: ReauditRequest) { return (await apiClient.post<ReauditResult>(`${base}/reaudits`, value)).data },
  async resume(id: number) { return (await apiClient.post(`${base}/jobs/${id}/resume`)).data },
  async retryAction(id: number) { return (await apiClient.post(`${base}/actions/${id}/retry`)).data }
}
