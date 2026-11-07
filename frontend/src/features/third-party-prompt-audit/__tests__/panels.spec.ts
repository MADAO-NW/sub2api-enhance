import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AuditConfigPanel from '../AuditConfigPanel.vue'
import AuditRecords from '../AuditRecords.vue'
import AuditDetail from '../AuditDetail.vue'
import AuditOverview from '../AuditOverview.vue'
import AuditInbox from '../AuditInbox.vue'
import AuditUsers from '../AuditUsers.vue'
import TrustPanel from '../TrustPanel.vue'
import NodeDecision from '../NodeDecision.vue'
import ThirdPartyPromptAuditView from '@/views/admin/ThirdPartyPromptAuditView.vue'
import type { AuditJob, BreakerEntry, JobDetail, RiskUser, SavedConfig, TrustEntry } from '@/api/admin/third-party-prompt-audit'

const mocks = vi.hoisted(() => ({
  getConfig: vi.fn(), saveConfig: vi.fn(), testAdminEmail: vi.fn(), getContract: vi.fn(), groups: vi.fn(), users: vi.fn(),
  listModels: vi.fn(), enableAndReset: vi.fn(), probeDetails: vi.fn(), probe: vi.fn(), configImpact: vi.fn(),
  jobs: vi.fn(), job: vi.fn(), jobPrompts: vi.fn(), templateChurn: vi.fn(), metricsHistory: vi.fn(), preview: vi.fn(), reaudit: vi.fn(), trustList: vi.fn(), trust: vi.fn(), untrust: vi.fn(),
  breakerList: vi.fn(), similarBlocks: vi.fn(), similarPair: vi.fn(), confirmBreaker: vi.fn(), retryAction: vi.fn(), stats: vi.fn(), runtime: vi.fn(), exportJobs: vi.fn(), exportUserData: vi.fn(), deleteUserData: vi.fn(),
  inbox: vi.fn(), actions: vi.fn(), riskUsers: vi.fn(), cost: vi.fn(), clearEscalation: vi.fn(),
  samples: vi.fn(), createSample: vi.fn(), updateSample: vi.fn(), deleteSample: vi.fn(),
  replays: vi.fn(), replay: vi.fn(), startReplay: vi.fn(),
  showError: vi.fn(), showSuccess: vi.fn(),
}))
vi.mock('@/api/admin/third-party-prompt-audit', () => ({ thirdPartyPromptAuditAPI: mocks }))
vi.mock('@/api/admin/groups', () => ({ getAll: mocks.groups }))
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key.split('.').at(-1), te: () => true }) }))

function saved(): SavedConfig {
  return {
    mode: 'async', platforms: [], all_groups: true, group_ids: [], excluded_user_ids: [], audit_prompt: 'saved policy',
    models: [{ id: 'a', name: 'Node', base_url: 'https://example.invalid', model: 'test', enabled: true, timeout_ms: 1000, max_concurrency: 4 }],
    review_threshold: null, block_threshold: null, aggregation: 'any_block', worker_count: 4,
    warning: { enabled: false, window: 0, limit: 0 }, disable: { enabled: false, limit: 0 }, user_rules: [], admin_email: '', retention_days: 30,
    category_rules: {}, escalate_on_warning: false,
    revision: 1, warning_rule_revision: 0, has_api_keys: {}, updated_by: 1, updated_at: '2026-09-06T00:00:00Z', application_error: '',
    applied_revision: 1, instance_id: 'test-instance', model_defaults: { timeout_ms: 300000, max_concurrency: 4 }, breaker_user_threshold: 2,
    rule_defaults: { review_threshold: 0.5, block_threshold: 0.8, warning_window: 10, warning_limit: 3, disable_limit: 5 },
  }
}
function job(): AuditJob {
  return {
    id: 7, user_id: 3, api_key_id: 4, group_id: null,
    identity: { username: 'alice', user_email: 'alice@example.invalid', api_key_name: 'key', group_name: 'default', endpoint: '/v1/responses' },
    display_username: 'alice', display_email: 'alice@example.invalid', platform: 'openai', request_id: 'req-1', model: 'gpt-test',
    protocol: 'responses', client_fingerprint: 'codex', mode: 'async', turn_fingerprint: 'turn', orphan_continuation: false,
    status: 'done', decision: 'block', current_round: 1, current_run_kind: 'request', current_requested_by: null, reuse_mode: 'allow',
    trigger_hash: 'hash-user', trigger_kind: 'user', trigger_categories: ['R07'], sampled: true,
    enforcement_eligible: true, violation_counted: true, disable_counted: false,
    config_revision: 2, attempts: 1, max_attempts: 3, lease_until: null, next_attempt_at: '2026-09-06T00:00:00Z', failure_stage: '',
    last_error_code: '', last_error_message: '', blocking_wait_ms: null, duration_ms: 1200, processing_ms: 900, target_count: 2,
    started_at: '2026-09-06T00:00:00Z', finished_at: '2026-09-06T00:00:01Z', created_at: '2026-09-06T00:00:00Z', updated_at: '2026-09-06T00:00:01Z',
  }
}
function detail(): JobDetail {
  return {
    job: job(),
    targets: [
      {
        round: 1, order: 1, hash: 'a'.repeat(64), kind: 'instruction', text: '系统提示词正文', bytes: 12,
        resolution: 'cache', decision: 'pass', trusted: false, sampled: false,
        review: { decision: 'pass', score: 0.02, reason: '正常', run_kind: 'request', nodes: [{ model_id: 'a', model_name: 'Node', confidence: 0.02, reason: '正常' }] },
      },
      {
        round: 1, order: 2, hash: 'b'.repeat(64), kind: 'user', text: '违规用户提示词', bytes: 20,
        resolution: 'model', decision: 'block', trusted: false,
        review: { decision: 'block', score: 0.95, reason: '违规', run_kind: 'request', nodes: [{ model_id: 'a', model_name: 'Node', confidence: 0.95, reason: '违规/R07' }] },
      },
    ],
    rounds: [{ round: 1, run_kind: 'request', requested_by: null, reuse_mode: 'fresh', decision: 'block', trigger_hash: 'b'.repeat(64), trigger_kind: 'user', trigger_categories: ['R07'], config_revision: 2, started_at: '2026-09-06T00:00:00Z', finished_at: '2026-09-06T00:00:01Z' }],
    attempts: [],
    actions: [],
  }
}
function riskUser(): RiskUser {
  return {
    user_id: 3, username: 'alice', email: 'alice@example.invalid', role: 'user', status: 'active',
    jobs: 12, blocks: 3, reviews: 1, violations: 2, last_block_at: '2026-09-06T00:00:00Z',
    disable_violation_count: 2, disable_reset_at: null, pending_actions: 1, escalated: true, categories: ['R07'],
  }
}
const stubs = { BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' }, Pagination: true, SystemUpdatePanel: true, TrendChart: true }
// jsdom 的 Blob 没有 text()，而导出流程依赖它，这里补一个等价的 FileReader 实现。
function readBlob(blob: Blob): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result ?? ''))
    reader.onerror = () => reject(reader.error)
    reader.readAsText(blob)
  })
}
const blobProto = Blob.prototype as unknown as { text?: () => Promise<string> }
function readBytes(blob: Blob): Promise<Uint8Array> {
  return new Promise<Uint8Array>((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(new Uint8Array(reader.result as ArrayBuffer))
    reader.onerror = () => reject(reader.error)
    reader.readAsArrayBuffer(blob)
  })
}
if (typeof blobProto.text !== 'function') blobProto.text = function (this: Blob) { return readBlob(this) }

beforeEach(() => {
  vi.resetAllMocks()
  mocks.getConfig.mockResolvedValue(saved())
  mocks.getContract.mockResolvedValue({ version: 'v6', output_contract: 'fixed contract', default_policy: 'default policy', template_rules: [] })
  mocks.saveConfig.mockImplementation(async (value: { config: SavedConfig }) => ({ ...saved(), ...value.config, revision: 2 }))
  mocks.testAdminEmail.mockResolvedValue({})
  mocks.groups.mockResolvedValue([])
  mocks.users.mockResolvedValue([])
  mocks.listModels.mockResolvedValue({ models: ['test', 'second-model'] })
  mocks.enableAndReset.mockResolvedValue({ action_id: 1, execution_status: 'pending', escalation_cleared: true })
  mocks.clearEscalation.mockResolvedValue({ user_id: 3, action_id: 9 })
  mocks.configImpact.mockResolvedValue({ instruction_changed: true, user_changed: false, changed_parts: ['policy'], recent_instruction_blocks: 123, window_hours: 24 })
  mocks.probe.mockResolvedValue({ ok: true, model_id: 'a', attempt_id: 1, result: { confidence: 0.1, reason: 'normal' }, error: null, tested_at: '2026-09-06T00:00:00Z', latency_ms: 10 })
  mocks.jobs.mockResolvedValue({ items: [job()], total: 1, page: 1, page_size: 20 })
  mocks.job.mockResolvedValue(detail())
  mocks.templateChurn.mockResolvedValue({ items: [{ client: 'codex', jobs: 10, distinct: 4, churn_rate: 0.4 }] })
  mocks.metricsHistory.mockResolvedValue({ items: [{ hour: '2026-11-06T00:00:00Z', jobs: 5, blocked: 1, failed: 0, avg_blocking_ms: 120 }] })
  mocks.jobPrompts.mockResolvedValue({ targets: [
    { order: 1, kind: 'instruction', hash: 'a'.repeat(64), text: '系统提示词正文', bytes: 12, resolution: 'cache', decision: 'pass', reason: '正常', score: 0.02, trusted: false, sampled: false },
    { order: 2, kind: 'user', hash: 'b'.repeat(64), text: '违规用户提示词', bytes: 20, resolution: 'model', decision: 'block', reason: '违规', score: 0.95, trusted: false, sampled: true },
  ] })
  mocks.preview.mockResolvedValue({ matched: 1, ready: 1, estimated_targets: 4, items: [{ job_id: 7, status: 'ready', reason: '' }] })
  mocks.reaudit.mockResolvedValue({ matched: 1, ready: 1, estimated_targets: 4, items: [{ job_id: 7, status: 'requeued', reason: '' }] })
  mocks.trustList.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20 })
  mocks.trust.mockResolvedValue({ hash: 'a'.repeat(64) })
  mocks.untrust.mockResolvedValue({ hash: 'a'.repeat(64) })
  const breaker: BreakerEntry = {
    hash: 'c'.repeat(64), preview: '共享指令块', bytes: 10, status: 'tripped', distinct_users: 2,
    tripped_at: '2026-09-06T00:00:00Z', tripped_job_id: 7, resolved_by: null, resolved_at: null,
    user_ids: [3, 9], similar: 1,
  }
  mocks.breakerList.mockResolvedValue({ items: [breaker], total: 1, page: 1, page_size: 20 })
  mocks.similarBlocks.mockResolvedValue({ items: [
    { hash: 'd'.repeat(64), preview: '共享指令块\n新增一行', bytes: 18, distance: 2, breaker_status: '', trusted: false, distinct_users: 1 },
  ] })
  // 全文的差异在第 500 字之后，预览里看不出来；对比必须读取全文。
  mocks.similarPair.mockResolvedValue({ hash: 'c'.repeat(64), text: '共享指令块\n' + '相同内容\n'.repeat(120) + '结尾',
    candidate: 'd'.repeat(64), candidate_text: '共享指令块\n' + '相同内容\n'.repeat(120) + '结尾\n夹带的新指令', distance: 2 })
  mocks.confirmBreaker.mockResolvedValue({ hash: 'c'.repeat(64) })
  mocks.retryAction.mockResolvedValue({})
  mocks.stats.mockResolvedValue({
    from: '2026-09-05T00:00:00Z', to: '2026-09-06T00:00:00Z', timezone: 'Asia/Shanghai', mode: '',
    jobs: 12, orphan_jobs: 1, reaudit_jobs: 2, failed_jobs: 3, decisions: { pass: 8, review: 1, block: 3 },
    waiting_jobs: 2, waiting_since: '2026-09-06T00:00:00Z', action_pending: 1, action_by_state: { pending: 1 },
    blocking_jobs: 4, blocking_avg_ms: 120, blocking_max_ms: 400, blocking_p50_ms: 110, blocking_p95_ms: 380, job_p50_ms: 800, job_p95_ms: 2400,
    queue_p50_ms: 200, queue_p95_ms: 900, processing_p50_ms: 600, processing_p95_ms: 1500,
    cache_miss: [{ client: 'codex', kind: 'instruction', targets: 4, model_calls: 1, miss_rate: 0.25 }],
    category_distribution: [{ category: 'R07', count: 260 }, { category: '', count: 12 }],
  })
  mocks.runtime.mockResolvedValue({
    warning_enabled: true, disable_enabled: true, instance_id: 'i', started_at: '2026-09-06T00:00:00Z', as_of: '2026-09-06T00:00:01Z',
    running: true, mode: 'async', revision: 1, expected_revision: 1, worker_capacity: 4, active_workers: 1, database_ok: true,
    database_error: '', config_error: '', intake_failures: 0, last_success: null, last_error: null,
    evaluation_latency: { count: 0, capacity: 1000, p50_ms: null, p95_ms: null, from: null, to: null }, probes: [],
    scheduler: { backpressured_total: 0, blocking_waiters: 0, nodes: [] },
    redis: { ok: true, status: 'ok', error: '', ttl_seconds: 1800, check_error: '', last_success: null },
  })
  mocks.probeDetails.mockResolvedValue([])
  mocks.exportJobs.mockResolvedValue({ blob: new Blob(['\ufeffid,user\n'], { type: 'text/csv' }), hasMore: false })
  mocks.exportUserData.mockResolvedValue(new Blob(['\ufeffjob_id\n'], { type: 'text/csv' }))
  mocks.deleteUserData.mockResolvedValue({ deleted_jobs: 4 })
  mocks.inbox.mockResolvedValue({
    total: 3, action_failed: 1, notification_failed: 1, breaker_pending: 1, node_unhealthy: 0,
    failure_rate: { window_minutes: 30, jobs: 20, failed: 3, rate: 0.15, threshold: 0.1, min_jobs: 10, exceeded: true },
    categories: [
      { key: 'action_failed', count: 1, items: [{ category: 'action_failed', id: 5, action_type: 'disable', user_id: 3, username: 'alice', job_id: 7, status: 'failed', occurred_at: '2026-09-06T00:00:00Z' }] },
      { key: 'notification_failed', count: 1, items: [{ category: 'notification_failed', id: 6, action_type: 'warning', user_id: 3, status: 'failed', occurred_at: '2026-09-06T00:00:00Z' }] },
      { key: 'breaker_pending', count: 1, items: [{ category: 'breaker_pending', hash: 'c'.repeat(64), detail: '共享指令块', status: '2 个用户', occurred_at: '2026-09-06T00:00:00Z' }] },
      { key: 'node_unhealthy', count: 0, items: [] },
      { key: 'failure_rate', count: 1, items: [] },
    ],
    notification_window_days: 7,
  })
  mocks.actions.mockResolvedValue({ items: [
    { id: 5, user_id: 3, actor_user_id: null, job_id: 7, action_type: 'disable', execution_status: 'failed', notification_status: 'failed', applied_at: null, auth_cache_status: 'ok', auth_cache_error: '', deliveries: [], rule_snapshot: {}, business_snapshot: {}, requested_at: '2026-09-06T00:00:00Z', username: 'alice' },
  ], total: 1, page: 1, page_size: 20 })
  mocks.riskUsers.mockResolvedValue({ items: [riskUser()], total: 1, page: 1, page_size: 20 })
  mocks.cost.mockResolvedValue({
    from: '2026-09-05T00:00:00Z', to: '2026-09-06T00:00:00Z', bucket: 'day', timezone: 'Asia/Shanghai',
    buckets: [{ bucket: '2026-09-05T00:00:00Z', model_id: 'node-a', call_kind: 'audit', stage: 'audit', calls: 6, succeeded: 6, input_tokens: 100, output_tokens: 20, unknown_usage: 1, p50_latency_ms: 900 }],
    reuse: [{ kind: 'instruction', resolution: 'cache', targets: 8 }],
  })
  mocks.samples.mockResolvedValue({ items: [
    { id: 1, hash: 'a'.repeat(64), kind: 'instruction', expected: 'pass', note: '', source_job_id: 7, created_by: 1, created_at: '2026-09-06T00:00:00Z', updated_at: '2026-09-06T00:00:00Z', preview: '样本正文' },
  ], total: 1, page: 1, page_size: 20 })
  mocks.createSample.mockResolvedValue({ id: 1 })
  mocks.updateSample.mockResolvedValue({ id: 1 })
  mocks.deleteSample.mockResolvedValue({ id: 1 })
  mocks.replays.mockResolvedValue({ items: [] })
  mocks.replay.mockResolvedValue({ id: 1, status: 'done', config_snapshot: '{}', sample_ids: [1], results: [], error_message: '', created_by: 1, created_at: '2026-09-06T00:00:00Z', finished_at: '2026-09-06T00:00:01Z' })
  mocks.startReplay.mockResolvedValue({ id: 1, status: 'running', config_snapshot: '{}', sample_ids: [1], results: [], error_message: '', created_by: 1, created_at: '2026-09-06T00:00:00Z', finished_at: null })
})

describe('third-party prompt audit panels', () => {
  it('keeps the record retention days in the editable configuration', async () => {
    const wrapper = mount(AuditConfigPanel, { global: { stubs } })
    await flushPromises()
    const retention = wrapper.get('input[type="number"][max="365"]')
    expect((retention.element as HTMLInputElement).value).toBe('30')
    await retention.setValue('7')
    expect((retention.element as HTMLInputElement).value).toBe('7')
    wrapper.unmount()
  })

  it('drops the providers.json block and the counter management section', async () => {
    const wrapper = mount(AuditConfigPanel, { global: { stubs } })
    await flushPromises()
    expect(wrapper.find('[data-test="providers-json"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('providersJSON')
    expect(wrapper.text()).not.toContain('counterManagement')
    expect(wrapper.text()).not.toContain('providersJSONHint')
    wrapper.unmount()
  })

  it('validates node advanced parameters in place and blocks saving them', async () => {
    const wrapper = mount(AuditConfigPanel, { global: { stubs } })
    await flushPromises()
    const model = wrapper.get('[data-test="model-card"]')
    await model.get('summary label input').trigger('click')
    const parameters = wrapper.get('[data-test="node-parameters"]')
    expect((parameters.element as HTMLTextAreaElement).value).toContain('{}')
    await parameters.setValue('[1,2]')
    await flushPromises()
    expect(wrapper.text()).toContain('nodeParametersInvalid')
    expect(wrapper.get('[data-test="save-config"]').attributes('disabled')).toBeDefined()
    await parameters.setValue('{"messages":[]}')
    await flushPromises()
    expect(wrapper.text()).toContain('nodeParametersReserved')
    await parameters.setValue('{"temperature":0.2}')
    await flushPromises()
    expect(wrapper.find('[data-test="node-parameters-error"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('estimates the cache impact before saving and warns about a low disable threshold', async () => {
    mocks.getConfig.mockResolvedValue({ ...saved(), disable: { enabled: true, limit: 1 } })
    const wrapper = mount(AuditConfigPanel, { global: { stubs } })
    await flushPromises()
    expect(wrapper.get('[data-test="disable-threshold-warning"]').text()).toContain('disableThresholdWarning')
    await wrapper.get('[data-test="audit-policy"]').setValue('new policy')
    await flushPromises()
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(mocks.configImpact).toHaveBeenCalled()
    expect(wrapper.get('[data-test="cache-impact"]').text()).toContain('cacheImpactInstruction')
    wrapper.unmount()
  })

  it('manages misjudgement samples and confirms the replay call count', async () => {
    const wrapper = mount(AuditConfigPanel, { global: { stubs } })
    await flushPromises()
    expect(wrapper.get('[data-test="samples-panel"]').text()).toContain('样本正文')
    await wrapper.get('[data-test="start-replay"]').trigger('click')
    await flushPromises()
    // 1 个样本 × 1 个启用节点 = 1 次调用。
    expect(wrapper.get('[data-test="replay-calls"]').text()).toContain('1')
    await wrapper.get('[data-test="confirm-replay"]').trigger('click')
    await flushPromises()
    expect(mocks.startReplay).toHaveBeenCalledWith(expect.objectContaining({ mode: 'async' }), [])
    expect(mocks.replay).toHaveBeenCalledWith(1)
    // 结论为 review 的样本单独成组，不能因为期望只有通过/阻断而从结果里消失。
    mocks.replay.mockResolvedValueOnce({ id: 1, status: 'done', config_snapshot: '{}', sample_ids: [1, 2], error_message: '', created_by: 1,
      created_at: '2026-09-06T00:00:00Z', finished_at: '2026-09-06T00:00:01Z', results: [
        { sample_id: 1, expected: 'pass', actual: 'review', score: 0.6, categories: [], nodes: [] },
        { sample_id: 2, expected: 'block', actual: 'block', score: 0.9, categories: [], nodes: [] },
      ] })
    await wrapper.get('[data-test="start-replay"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-test="confirm-replay"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="replay-group-replayReview"]').text()).toContain('(1)')
    expect(wrapper.get('[data-test="replay-group-replayConsistent"]').text()).toContain('(1)')
    await wrapper.get('[data-test="sample-delete"]').trigger('click')
    await flushPromises()
    expect(mocks.deleteSample).not.toHaveBeenCalled()
    await wrapper.get('[data-test="confirm-delete-sample"]').trigger('click')
    await flushPromises()
    expect(mocks.deleteSample).toHaveBeenCalledWith(1)
    wrapper.unmount()
  })

  it('lists jobs with their decision and trigger categories', async () => {
    const wrapper = mount(AuditRecords, { global: { stubs } })
    await flushPromises()
    const text = wrapper.text()
    expect(text).toContain('#7')
    expect(text).toContain('alice')
    expect(text).toContain('block')
    expect(text).toContain('R07')
    expect(text).toContain('sampledMark')
    wrapper.unmount()
  })

  it('looks up job IDs with a # prefix and categories in R01 form', async () => {
    const wrapper = mount(AuditRecords, { global: { stubs } })
    await flushPromises()
    await wrapper.get('[data-test="request-id"]').setValue('#7')
    await wrapper.get('[data-test="category-filter"]').setValue('r07')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(mocks.jobs).toHaveBeenLastCalledWith({ ids: [7], category: 'R07' }, 1, 20)
    await wrapper.get('[data-test="category-filter"]').setValue('violations')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('categoryInvalid')
    wrapper.unmount()
  })

  it('opens the audited prompts of a row in a dialog, resets filters and adds samples', async () => {
    const wrapper = mount(AuditRecords, { global: { stubs } })
    await flushPromises()
    const viewButton = wrapper.findAll('button').find(button => button.text() === 'viewPrompt')
    expect(viewButton).toBeTruthy()
    await viewButton!.trigger('click')
    await flushPromises()
    expect(mocks.jobPrompts).toHaveBeenCalledWith(7)
    expect(wrapper.text()).toContain('系统提示词正文')
    expect(wrapper.text()).toContain('违规用户提示词')
    const sample = wrapper.get('[data-test="sample-block"]')
    await sample.trigger('click')
    await flushPromises()
    expect(mocks.createSample).toHaveBeenCalledWith({ job_id: 7, target_order: 1, expected: 'block' })
    // 重置按钮会清空条件并重新拉取列表。
    const reset = wrapper.get('[data-test="reset-filters"]')
    const before = mocks.jobs.mock.calls.length
    await reset.trigger('click')
    await flushPromises()
    expect(mocks.jobs.mock.calls.length).toBeGreaterThan(before)
    wrapper.unmount()
  })

  it('previews a reaudit without a reuse mode and shows the estimated targets', async () => {
    const wrapper = mount(AuditRecords, { global: { stubs } })
    await flushPromises()
    const reaudit = wrapper.findAll('button').find(button => button.text().startsWith('reauditFilter'))
    await reaudit!.trigger('click')
    await flushPromises()
    expect(mocks.preview).toHaveBeenCalledWith({ filter: {} })
    expect(wrapper.get('[data-test="estimated-targets"]').text()).toContain('estimatedTargets')
    wrapper.unmount()
  })

  it('shows every target with its resolution, samples and allows trusting an instruction block', async () => {
    const wrapper = mount(AuditDetail, { props: { id: 7 }, global: { stubs } })
    await flushPromises()
    const text = wrapper.text()
    expect(text).toContain('系统提示词正文')
    expect(text).toContain('违规用户提示词')
    expect(text).toContain('cache')
    expect(text).toContain('model')
    expect(text).toContain('reuseFresh')
    const trustButton = wrapper.findAll('button').find(button => button.text() === 'trust')
    expect(trustButton).toBeTruthy()
    await trustButton!.trigger('click')
    await flushPromises()
    expect(mocks.trust).toHaveBeenCalledWith('a'.repeat(64), expect.any(String))
    await wrapper.get('[data-test="detail-sample-pass"]').trigger('click')
    await flushPromises()
    expect(mocks.createSample).toHaveBeenCalledWith({ job_id: 7, target_order: 1, expected: 'pass' })
    wrapper.unmount()
  })

  it('renders node votes for a target', async () => {
    const wrapper = mount(NodeDecision, { props: { node: { model_id: 'a', model_name: 'Node', confidence: 0.9, reason: '违规/R07' } }, global: { stubs } })
    expect(wrapper.text()).toContain('违规/R07')
    wrapper.unmount()
  })

  it('confirms a tripped breaker from the breaker list', async () => {
    const wrapper = mount(TrustPanel, { global: { stubs } })
    await flushPromises()
    const text = wrapper.text()
    expect(text).toContain('共享指令块')
    expect(text).toContain('2')
    const confirm = wrapper.findAll('button').find(button => button.text() === 'confirmViolation')
    expect(confirm).toBeTruthy()
    await confirm!.trigger('click')
    await flushPromises()
    expect(mocks.confirmBreaker).toHaveBeenCalledWith('c'.repeat(64))
    wrapper.unmount()
  })

  it('expands similar blocks, shows a line diff and trusts only the selected block', async () => {
    const wrapper = mount(TrustPanel, { global: { stubs } })
    await flushPromises()
    expect(wrapper.find('[data-test="breaker-drill"]').exists()).toBe(false)
    await wrapper.get('[data-test="similar-toggle"]').trigger('click')
    await flushPromises()
    expect(mocks.similarBlocks).toHaveBeenCalledWith('c'.repeat(64))
    expect(wrapper.find('[data-test="similar-preview"]').text()).toContain('新增一行')
    await wrapper.get('[data-test="similar-compare"]').trigger('click')
    await flushPromises()
    expect(mocks.similarPair).toHaveBeenCalledWith('c'.repeat(64), 'd'.repeat(64))
    const diff = wrapper.get('[data-test="similar-diff"]')
    expect(diff.text()).toContain('diffChanged')
    expect(diff.text()).toContain('夹带的新指令')
    expect(diff.find('[data-test="diff-skipped"]').exists()).toBe(true)
    await wrapper.get('[data-test="similar-trust"]').trigger('click')
    await flushPromises()
    expect(mocks.trust).toHaveBeenCalledWith('d'.repeat(64), expect.any(String))
    wrapper.unmount()
  })

  it('lists trusted instruction blocks and can untrust them', async () => {
    const entry: TrustEntry = { hash: 'd'.repeat(64), preview: '已信任指令', bytes: 6, trusted_by: 1, reason: '误判', created_at: '2026-09-06T00:00:00Z' }
    mocks.trustList.mockResolvedValue({ items: [entry], total: 1, page: 1, page_size: 20 })
    const wrapper = mount(TrustPanel, { global: { stubs } })
    await flushPromises()
    expect(wrapper.text()).toContain('已信任指令')
    const untrust = wrapper.findAll('button').find(button => button.text() === 'untrust')
    expect(untrust).toBeTruthy()
    await untrust!.trigger('click')
    await flushPromises()
    expect(mocks.untrust).toHaveBeenCalledWith('d'.repeat(64))
    wrapper.unmount()
  })

  it('shows the risk overview, pending summary and cost, and keeps diagnostics folded by default', async () => {
    window.localStorage.clear()
    const wrapper = mount(AuditOverview, { global: { stubs } })
    await flushPromises()
    const text = wrapper.text()
    expect(text).toContain('12')
    expect(text).toContain('riskOverview')
    expect(text).toContain('costCard')
    expect(wrapper.find('[data-test="pending-summary"]').exists()).toBe(true)
    // 类别分布直接使用后端聚合结果，不再从前 200 条阻断任务推算。
    const distribution = wrapper.get('[data-test="category-distribution"]').text()
    expect(distribution).toContain('R07')
    expect(distribution).toContain('260')
    expect(distribution).toContain('uncategorized')
    expect(mocks.jobs).not.toHaveBeenCalledWith(expect.objectContaining({ decision: 'block' }), 1, 200)
    // 运行诊断默认折叠，展开后才显示缓存未命中与同步等待。
    expect(wrapper.find('[data-test="cache-miss"]').exists()).toBe(false)
    await wrapper.get('[data-test="toggle-diagnostics"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="cache-miss"]').text()).toContain('codex')
    expect(wrapper.get('[data-test="cache-miss"]').text()).toContain('25%')
    expect(wrapper.text()).toContain('120 ms')
    expect(window.localStorage.getItem('enhance.third-party-prompt-audit.diagnostics')).toBe('1')
    wrapper.unmount()
  })

  it('refreshes the pending summary every 30 seconds instead of with the 5-second core refresh', async () => {
    vi.useFakeTimers()
    try {
      const wrapper = mount(AuditOverview, { global: { stubs } })
      await flushPromises()
      expect(wrapper.emitted('inbox-loaded')?.[0]).toEqual([3])
      const inboxCalls = mocks.inbox.mock.calls.length, statsCalls = mocks.stats.mock.calls.length
      await vi.advanceTimersByTimeAsync(5000)
      expect(mocks.stats.mock.calls.length).toBeGreaterThan(statsCalls)
      expect(mocks.inbox.mock.calls.length).toBe(inboxCalls)
      await vi.advanceTimersByTimeAsync(30000)
      expect(mocks.inbox.mock.calls.length).toBeGreaterThan(inboxCalls)
      wrapper.unmount()
    } finally { vi.useRealTimers() }
  })

  it('jumps from the pending summary to the inbox filter', async () => {
    const wrapper = mount(AuditOverview, { global: { stubs } })
    await flushPromises()
    await wrapper.get('[data-test="overview-inbox-action_failed"]').trigger('click')
    expect(wrapper.emitted('open-inbox')?.[0]).toEqual(['action_failed'])
    await wrapper.get('[data-test="overview-open-users"]').trigger('click')
    expect(wrapper.emitted('open-users')?.[0]).toEqual([{ from: '2026-09-05T00:00:00Z', to: '2026-09-06T00:00:00Z' }])
    wrapper.unmount()
  })

  it('shows inbox categories and retries a failed action', async () => {
    const wrapper = mount(AuditInbox, { global: { stubs } })
    await flushPromises()
    const text = wrapper.text()
    expect(text).toContain('action_failed')
    expect(text).toContain('failure_rate')
    expect(text).toContain('alice')
    await wrapper.get('[data-test="inbox-retry"]').trigger('click')
    await flushPromises()
    expect(mocks.retryAction).toHaveBeenCalledWith(5)
    expect(wrapper.emitted('loaded')?.[0]).toEqual([3])
    expect(wrapper.get('[data-test="inbox-notification-window"]').text()).toContain('notificationWindowHint')
    // 下钻列表与分类计数同口径：通知失败只看下发的 7 天窗口，待核对同时列出 failed 与 unknown。
    const before = Date.now()
    await wrapper.get('[data-test="inbox-category-notification_failed"]').trigger('click')
    await flushPromises()
    const notificationFilter = mocks.actions.mock.lastCall![0] as { notification_status: string; from: string }
    expect(notificationFilter.notification_status).toBe('failed')
    expect(Math.abs(before - 7 * 86400000 - new Date(notificationFilter.from).getTime())).toBeLessThan(60000)
    await wrapper.get('[data-test="inbox-category-action_failed"]').trigger('click')
    await flushPromises()
    expect(mocks.actions).toHaveBeenLastCalledWith({ execution_status: 'failed,unknown' }, 1, 20)
    wrapper.unmount()
  })

  it('expands a risk user with recent jobs and only deletes after confirmation', async () => {
    Object.assign(URL, { createObjectURL: vi.fn(() => 'blob:user'), revokeObjectURL: vi.fn() })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const wrapper = mount(AuditUsers, { global: { stubs } })
    await flushPromises()
    expect(wrapper.text()).toContain('alice')
    await wrapper.get('[data-test="risk-toggle-3"]').trigger('click')
    await flushPromises()
    expect(mocks.jobs).toHaveBeenCalledWith({ user_id: 3 }, 1, 20)
    expect(mocks.actions).toHaveBeenCalledWith({ user_id: 3 }, 1, 20)
    await wrapper.get('[data-test="risk-export"]').trigger('click')
    await flushPromises()
    expect(mocks.exportUserData).toHaveBeenCalledWith(3)
    await wrapper.get('[data-test="risk-delete"]').trigger('click')
    await flushPromises()
    expect(mocks.deleteUserData).not.toHaveBeenCalled()
    await wrapper.get('[data-test="confirm-delete-user"]').trigger('click')
    await flushPromises()
    expect(mocks.deleteUserData).toHaveBeenCalledWith(3)
    expect(mocks.showSuccess).toHaveBeenCalledWith(expect.stringContaining('4'))
    click.mockRestore()
    wrapper.unmount()
  })

  it('offers overview, inbox, jobs, users, trust and config tabs and loads trust on demand', async () => {
    const wrapper = mount(ThirdPartyPromptAuditView, { global: { stubs } })
    await flushPromises()
    const labels = wrapper.findAll('nav button').map(button => button.element.querySelector('[data-test="inbox-badge"]') ? button.text().replace(/\d+$/, '') : button.text())
    expect(labels).toEqual(['overview', 'inbox', 'jobs', 'users', 'trustTab', 'config'])
    const trustTab = wrapper.findAll('nav button').find(button => button.text().startsWith('trustTab'))
    await trustTab!.trigger('click')
    await flushPromises()
    expect(mocks.breakerList).toHaveBeenCalled()
    wrapper.unmount()
  })
  it('merges every export page into one CSV with a single header', async () => {
    const pages = [
      { blob: new Blob(['\ufeffid,user\n1,a\n'], { type: 'text/csv' }), hasMore: true },
      { blob: new Blob(['\ufeffid,user\n2,b\n'], { type: 'text/csv' }), hasMore: false },
    ]
    mocks.exportJobs.mockImplementation(async (_filter: unknown, _limit: number, offset: number) => pages[offset / 1000] ?? { blob: new Blob(['']), hasMore: false })
    const written: Blob[] = []
    Object.assign(URL, { createObjectURL: vi.fn((blob: Blob) => { written.push(blob); return 'blob:audit' }), revokeObjectURL: vi.fn() })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const wrapper = mount(AuditRecords, { global: { stubs } })
    await flushPromises()
    await wrapper.get('[data-test="export-jobs"]').trigger('click')
    await flushPromises()
    // 第一页有后续页时必须再拉一次，并且只保留一份表头与 BOM。
    await vi.waitFor(() => expect(mocks.exportJobs).toHaveBeenCalledTimes(2))
    expect(mocks.exportJobs).toHaveBeenLastCalledWith(expect.anything(), 1000, 1000)
    // Blob.text() 按规范会吃掉页首 BOM，导出文件必须自己补回一个，且表头只能出现一次。
    const bytes = await readBytes(written[0])
    expect(Array.from(bytes.slice(0, 3))).toEqual([0xef, 0xbb, 0xbf])
    const text = new TextDecoder().decode(bytes.slice(3))
    expect(text).toBe('id,user\n1,a\n2,b\n')
    expect(text.match(/id,user/g)).toHaveLength(1)
    click.mockRestore()
    wrapper.unmount()
  })
})
