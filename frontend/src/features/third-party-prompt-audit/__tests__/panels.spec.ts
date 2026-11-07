import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AuditConfigPanel from '../AuditConfigPanel.vue'
import AuditRecords from '../AuditRecords.vue'
import AuditDetail from '../AuditDetail.vue'
import AuditOverview from '../AuditOverview.vue'
import TrustPanel from '../TrustPanel.vue'
import NodeDecision from '../NodeDecision.vue'
import ThirdPartyPromptAuditView from '@/views/admin/ThirdPartyPromptAuditView.vue'
import type { AuditJob, BreakerEntry, JobDetail, SavedConfig, TrustEntry } from '@/api/admin/third-party-prompt-audit'

const mocks = vi.hoisted(() => ({
  getConfig: vi.fn(), saveConfig: vi.fn(), testAdminEmail: vi.fn(), getContract: vi.fn(), groups: vi.fn(), users: vi.fn(),
  listModels: vi.fn(), enableAndReset: vi.fn(), probeDetails: vi.fn(), probe: vi.fn(), userKeys: vi.fn(),
  jobs: vi.fn(), job: vi.fn(), jobPrompts: vi.fn(), templateChurn: vi.fn(), metricsHistory: vi.fn(), preview: vi.fn(), reaudit: vi.fn(), trustList: vi.fn(), trust: vi.fn(), untrust: vi.fn(),
  breakerList: vi.fn(), confirmBreaker: vi.fn(), retryAction: vi.fn(), stats: vi.fn(), runtime: vi.fn(), exportJobs: vi.fn(), exportUserData: vi.fn(), deleteUserData: vi.fn(),
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
    revision: 1, warning_rule_revision: 0, has_api_keys: {}, updated_by: 1, updated_at: '2026-09-06T00:00:00Z', application_error: '',
    applied_revision: 1, instance_id: 'test-instance', model_defaults: { timeout_ms: 300000, max_concurrency: 4 },
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
    trigger_hash: 'hash-user', trigger_kind: 'user', enforcement_eligible: true, violation_counted: true, disable_counted: false,
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
        resolution: 'cache', decision: 'pass', trusted: false,
        review: { decision: 'pass', score: 0.02, reason: '正常', run_kind: 'request', nodes: [{ model_id: 'a', model_name: 'Node', confidence: 0.02, reason: '正常' }] },
      },
      {
        round: 1, order: 2, hash: 'b'.repeat(64), kind: 'user', text: '违规用户提示词', bytes: 20,
        resolution: 'model', decision: 'block', trusted: false,
        review: { decision: 'block', score: 0.95, reason: '违规', run_kind: 'request', nodes: [{ model_id: 'a', model_name: 'Node', confidence: 0.95, reason: '违规/R07' }] },
      },
    ],
    rounds: [{ round: 1, run_kind: 'request', requested_by: null, reuse_mode: 'allow', decision: 'block', trigger_hash: 'b'.repeat(64), trigger_kind: 'user', config_revision: 2, started_at: '2026-09-06T00:00:00Z', finished_at: '2026-09-06T00:00:01Z' }],
    attempts: [],
    actions: [],
  }
}
const stubs = { BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' }, Pagination: true, SystemUpdatePanel: true }
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
  mocks.enableAndReset.mockResolvedValue({ action_id: 1, execution_status: 'pending' })
  mocks.probe.mockResolvedValue({ ok: true, model_id: 'a', attempt_id: 1, result: { confidence: 0.1, reason: 'normal' }, error: null, tested_at: '2026-09-06T00:00:00Z', latency_ms: 10 })
  mocks.userKeys.mockResolvedValue([])
  mocks.jobs.mockResolvedValue({ items: [job()], total: 1, page: 1, page_size: 20 })
  mocks.job.mockResolvedValue(detail())
  mocks.templateChurn.mockResolvedValue({ items: [{ client: 'generic', jobs: 10, distinct: 4, churn_rate: 0.4 }] })
  mocks.metricsHistory.mockResolvedValue({ items: [{ hour: '2026-11-06T00:00:00Z', jobs: 5, blocked: 1, failed: 0, avg_blocking_ms: 120 }] })
  mocks.jobPrompts.mockResolvedValue({ targets: [
    { order: 1, kind: 'instruction', hash: 'a'.repeat(64), text: '系统提示词正文', bytes: 12, resolution: 'cache', decision: 'pass', reason: '正常', score: 0.02, trusted: false },
    { order: 2, kind: 'user', hash: 'b'.repeat(64), text: '违规用户提示词', bytes: 20, resolution: 'model', decision: 'block', reason: '违规', score: 0.95, trusted: false },
  ] })
  mocks.preview.mockResolvedValue({ matched: 1, ready: 1, items: [{ job_id: 7, status: 'ready', reason: '' }] })
  mocks.reaudit.mockResolvedValue({ matched: 1, ready: 1, items: [{ job_id: 7, status: 'requeued', reason: '' }] })
  mocks.trustList.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20 })
  mocks.trust.mockResolvedValue({ hash: 'a'.repeat(64) })
  mocks.untrust.mockResolvedValue({ hash: 'a'.repeat(64) })
  const breaker: BreakerEntry = {
    hash: 'c'.repeat(64), preview: '共享指令块', bytes: 10, status: 'tripped', distinct_users: 2,
    tripped_at: '2026-09-06T00:00:00Z', tripped_job_id: 7, resolved_by: null, resolved_at: null,
    user_ids: [3, 9], similar: 1,
  }
  mocks.breakerList.mockResolvedValue({ items: [breaker], total: 1, page: 1, page_size: 20 })
  mocks.confirmBreaker.mockResolvedValue({ hash: 'c'.repeat(64) })
  mocks.retryAction.mockResolvedValue({})
  mocks.stats.mockResolvedValue({
    from: '2026-09-05T00:00:00Z', to: '2026-09-06T00:00:00Z', timezone: 'Asia/Shanghai', mode: '',
    jobs: 12, orphan_jobs: 1, reaudit_jobs: 2, failed_jobs: 3, decisions: { pass: 8, review: 1, block: 3 },
    waiting_jobs: 2, waiting_since: '2026-09-06T00:00:00Z', action_pending: 1, action_by_state: { pending: 1 },
    blocking_jobs: 4, blocking_avg_ms: 120, blocking_max_ms: 400, blocking_p50_ms: 110, blocking_p95_ms: 380, job_p50_ms: 800, job_p95_ms: 2400,
    queue_p50_ms: 200, queue_p95_ms: 900, processing_p50_ms: 600, processing_p95_ms: 1500,
    cache_miss: [{ client: 'codex', kind: 'instruction', targets: 4, model_calls: 1, miss_rate: 0.25 }],
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
  mocks.deleteUserData.mockResolvedValue({ deleted_jobs: 0 })
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

  it('lists jobs with their decision and trigger target', async () => {
    const wrapper = mount(AuditRecords, { global: { stubs } })
    await flushPromises()
    const text = wrapper.text()
    expect(text).toContain('#7')
    expect(text).toContain('alice')
    expect(text).toContain('block')
    expect(text).toContain('user')
    wrapper.unmount()
  })

  it('opens the audited prompts of a row in a dialog and resets filters', async () => {
    const wrapper = mount(AuditRecords, { global: { stubs } })
    await flushPromises()
    const viewButton = wrapper.findAll('button').find(button => button.text() === 'viewPrompt')
    expect(viewButton).toBeTruthy()
    await viewButton!.trigger('click')
    await flushPromises()
    expect(mocks.jobPrompts).toHaveBeenCalledWith(7)
    expect(wrapper.text()).toContain('系统提示词正文')
    expect(wrapper.text()).toContain('违规用户提示词')
    // 重置按钮会清空条件并重新拉取列表。
    const reset = wrapper.get('[data-test="reset-filters"]')
    const before = mocks.jobs.mock.calls.length
    await reset.trigger('click')
    await flushPromises()
    expect(mocks.jobs.mock.calls.length).toBeGreaterThan(before)
    wrapper.unmount()
  })

  it('shows every target with its resolution and allows trusting an instruction block', async () => {
    const wrapper = mount(AuditDetail, { props: { id: 7 }, global: { stubs } })
    await flushPromises()
    const text = wrapper.text()
    expect(text).toContain('系统提示词正文')
    expect(text).toContain('违规用户提示词')
    expect(text).toContain('cache')
    expect(text).toContain('model')
    const trustButton = wrapper.findAll('button').find(button => button.text() === 'trust')
    expect(trustButton).toBeTruthy()
    await trustButton!.trigger('click')
    await flushPromises()
    expect(mocks.trust).toHaveBeenCalledWith('a'.repeat(64), expect.any(String))
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

  it('shows request volume, cache miss rate and blocking wait on the overview', async () => {
    const wrapper = mount(AuditOverview, { global: { stubs } })
    await flushPromises()
    const text = wrapper.text()
    expect(text).toContain('12')
    expect(text).toContain('codex')
    expect(text).toContain('25%')
    expect(text).toContain('120 ms')
    wrapper.unmount()
  })

  it('offers overview, jobs, trust and config tabs and loads trust on demand', async () => {
    const wrapper = mount(ThirdPartyPromptAuditView, { global: { stubs } })
    await flushPromises()
    const labels = wrapper.findAll('nav button').map(button => button.text())
    expect(labels).toEqual(['overview', 'jobs', 'trustTab', 'config'])
    const trustTab = wrapper.findAll('nav button').find(button => button.text() === 'trustTab')
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

  it('offers per-user export and only deletes audit data after confirmation', async () => {
    mocks.users.mockResolvedValue([{ id: 3, username: 'alice', email: 'alice@example.invalid', role: 'user', status: 'active', disable_violation_count: 2, disable_reset_at: null, action_pending: false }])
    mocks.deleteUserData.mockResolvedValue({ deleted_jobs: 4 })
    Object.assign(URL, { createObjectURL: vi.fn(() => 'blob:user'), revokeObjectURL: vi.fn() })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const wrapper = mount(AuditConfigPanel, { global: { stubs } })
    await flushPromises()
    const button = (text: string) => wrapper.findAll('button').find(item => item.text() === text)
    const exportButton = button('exportUserData'), deleteButton = button('deleteUserData')
    expect(exportButton?.attributes('disabled')).toBeDefined()
    expect(deleteButton?.attributes('disabled')).toBeDefined()
    // 页面里还有“用户规则”的用户下拉框，这里定位到计数组件旁边那个 select。
    const selectElement = exportButton!.element.parentElement?.parentElement?.querySelector('select') ?? null
    expect(selectElement).toBeTruthy()
    const userSelect = wrapper.findAll('select').find(item => item.element === selectElement)
    await userSelect!.setValue('3')
    await exportButton!.trigger('click')
    await flushPromises()
    expect(mocks.exportUserData).toHaveBeenCalledWith(3)
    await deleteButton!.trigger('click')
    await flushPromises()
    expect(mocks.deleteUserData).not.toHaveBeenCalled()
    await wrapper.get('[data-test="confirm-delete-user"]').trigger('click')
    await flushPromises()
    expect(mocks.deleteUserData).toHaveBeenCalledWith(3)
    expect(mocks.showSuccess).toHaveBeenCalledWith(expect.stringContaining('4'))
    click.mockRestore()
    wrapper.unmount()
  })
})
