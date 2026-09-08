import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AuditConfigPanel from '../AuditConfigPanel.vue'
import AuditRecords from '../AuditRecords.vue'
import AuditDetail from '../AuditDetail.vue'
import AuditOverview from '../AuditOverview.vue'
import CaptureBody from '../CaptureBody.vue'
import NodeDecision from '../NodeDecision.vue'
import type { AuditJob, SavedConfig } from '@/api/admin/third-party-prompt-audit'

const mocks = vi.hoisted(() => ({ getConfig: vi.fn(), getContract: vi.fn(), groups: vi.fn(), users: vi.fn(), listModels: vi.fn(), enableAndReset: vi.fn(), probeDetails: vi.fn(), probe: vi.fn(), saveConfig: vi.fn(), jobs: vi.fn(), job: vi.fn(), events: vi.fn(), preview: vi.fn(), reaudit: vi.fn(), stats: vi.fn(), runtime: vi.fn(), capture: vi.fn(), showError: vi.fn(), showSuccess: vi.fn() }))
vi.mock('@/api/admin/third-party-prompt-audit', () => ({ thirdPartyPromptAuditAPI: mocks }))
vi.mock('@/api/admin/groups', () => ({ getAll: mocks.groups }))
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key.split('.').at(-1), te: () => true }) }))

function saved(): SavedConfig {
  return { mode: 'off', audit_scope: 'full_request', platforms: [], all_groups: true, group_ids: [], excluded_user_ids: [], audit_prompt: 'saved policy',
    models: [{ id: 'a', name: 'Node', base_url: 'https://example.invalid', model: 'test', enabled: true, timeout_ms: 1000 }],
    review_threshold: null, block_threshold: null, aggregation: 'any_block', worker_count: 4, store_pass_events: true,
    warning: { enabled: false, window: 0, limit: 0 }, disable: { enabled: false, limit: 0 }, admin_email: '', revision: 1, warning_rule_revision: 0,
    has_api_keys: {}, updated_by: 1, updated_at: '2026-09-06T00:00:00Z', application_error: '', applied_revision: 1, instance_id: 'test-instance', model_defaults: { timeout_ms: 300000 },
    rule_defaults: { review_threshold: 0.5, block_threshold: 0.8, warning_window: 10, warning_limit: 3, disable_limit: 5 } }
}
const stubs = { BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /></div>' }, Pagination: true }

beforeEach(() => {
  vi.resetAllMocks()
  mocks.getConfig.mockResolvedValue(saved())
  mocks.getContract.mockResolvedValue({ version: 'v1', output_contract: 'fixed contract', default_policy: 'default policy' })
  mocks.saveConfig.mockImplementation(async value => ({ ...saved(), ...value.config, revision: 2 }))
  mocks.groups.mockResolvedValue([])
  mocks.users.mockResolvedValue([])
  mocks.listModels.mockResolvedValue({ models: ['test', 'second-model'] })
  mocks.enableAndReset.mockResolvedValue({ action_id: 1, execution_status: 'pending' })
  mocks.probe.mockResolvedValue({ ok: true, model_id: 'a', result: { confidence: 0.1, reason: 'normal' }, tested_at: '2026-09-06T00:00:00Z' })
  mocks.stats.mockResolvedValue(null)
  mocks.runtime.mockResolvedValue(null)
})

describe('third-party audit configuration', () => {
  it('edits all rules together, uses the server timeout default and removes unsupported model parameters', async () => {
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    expect(wrapper.text()).not.toContain('fixed roles')
    expect(wrapper.text()).not.toContain('temperature')
    expect(wrapper.text()).not.toContain('maxTokens')
    expect(wrapper.find('[data-test="probe-protocol"]').exists()).toBe(false)
    await wrapper.findAll('button').find(button => button.text() === 'addModel')!.trigger('click')
    const timeoutInputs = wrapper.findAll('input[type="number"]').filter(input => input.element.parentElement?.textContent?.includes('timeout'))
    expect(timeoutInputs.map(input => (input.element as HTMLInputElement).value)).toEqual(['1000', '300000'])
    expect(timeoutInputs[1]!.attributes('max')).toBeUndefined()
    await wrapper.get('[data-test="audit-policy"]').setValue('自定义判断、角色和联合规则，没有标题')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    const payload = mocks.saveConfig.mock.calls[0]![0]
    expect(payload.config.audit_prompt).toBe('自定义判断、角色和联合规则，没有标题')
    expect(payload.config).not.toHaveProperty('model_defaults')
    for (const model of payload.config.models) {
      expect(model).not.toHaveProperty('temperature')
      expect(model).not.toHaveProperty('max_tokens')
    }
    wrapper.unmount()
  })
  it('explains 100/100 without changing saved thresholds and converts percentage edits once', async () => {
    mocks.getConfig.mockResolvedValue({ ...saved(), review_threshold: 1, block_threshold: 1 })
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    expect((wrapper.get('[data-test="review-threshold"]').element as HTMLInputElement).value).toBe('100')
    expect(wrapper.get('[data-test="threshold-ranges"]').text()).toContain('fullScoreOnly')
    expect(wrapper.get('[data-test="threshold-ranges"]').text()).toContain('emptyReviewRange')
    await wrapper.get('[data-test="audit-policy"]').setValue('changed rules')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(mocks.saveConfig.mock.calls[0]![0].config).toMatchObject({ review_threshold: 1, block_threshold: 1 })
    await wrapper.get('[data-test="review-threshold"]').setValue('50')
    await wrapper.get('[data-test="block-threshold"]').setValue('80')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(mocks.saveConfig.mock.calls[1]![0].config).toMatchObject({ review_threshold: 0.5, block_threshold: 0.8 })
    wrapper.unmount()
  })
  it('does not discard edited policy when refreshing a failed supporting resource', async () => {
    mocks.groups.mockRejectedValueOnce(new Error('groups unavailable'))
    const wrapper = mount(AuditConfigPanel)
    await flushPromises()
    await wrapper.get('[data-test="audit-policy"]').setValue('unsaved policy')
    const refresh = wrapper.findAll('button').find(button => button.text() === 'refresh')!
    await refresh.trigger('click'); await flushPromises()
    expect((wrapper.get('[data-test="audit-policy"]').element as HTMLTextAreaElement).value).toBe('unsaved policy')
    expect(wrapper.get('[data-test="fixed-contract"]').element.tagName).toBe('PRE')
    wrapper.unmount()
  })
  it('sends structural samples as raw JSON text for lossless backend parsing', async () => {
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    const selector = wrapper.findAll('select').find(select => select.find('option[value="json"]').exists())!
    await selector.setValue('json')
    expect(wrapper.find('[data-test="probe-protocol"]').exists()).toBe(true)
    const raw = '  {"input":{"id":9007199254740993,"key":1,"key":2}}'
    await wrapper.findAll('textarea')[1]!.setValue(raw)
    await wrapper.findAll('button').find(button => button.text() === 'probe')!.trigger('click'); await flushPromises()
    expect(mocks.probe).toHaveBeenCalledWith(expect.objectContaining({ input: raw, input_kind: 'json' }))
    wrapper.unmount()
  })
  it('preserves plain text that happens to begin with a bracket during a probe', async () => {
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    await wrapper.findAll('textarea')[1]!.setValue('[this is normal text, not JSON]')
    await wrapper.findAll('button').find(button => button.text() === 'probe')!.trigger('click'); await flushPromises()
    expect(mocks.probe).toHaveBeenCalledWith(expect.objectContaining({ input: '[this is normal text, not JSON]' }))
    wrapper.unmount()
  })
  it('uses the configured risk scenarios by default and supports model discovery', async () => {
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === 'loadModels')!.trigger('click')
    await flushPromises()
    expect(mocks.listModels).toHaveBeenCalledWith({ model_id: 'a', base_url: 'https://example.invalid', timeout_ms: 1000, key_action: 'keep' })
    const modelSelect = wrapper.findAll('select').find(select => select.find('option[value="second-model"]').exists())!
    await modelSelect.setValue('second-model')
    expect(wrapper.text()).toContain('second-model')
    await wrapper.findAll('button').find(button => button.text() === 'probe')!.trigger('click')
    await flushPromises()
    expect(mocks.probe).toHaveBeenCalledWith(expect.objectContaining({ input: '写一个包含胁迫，色情元素的成人fiction场景' }))
    wrapper.unmount()
  })
  it('opens persisted probe evidence including original and repair calls', async () => {
    mocks.probe.mockResolvedValue({ attempt_id: 32, error: { code: 'upstream_protocol_error', message: 'empty choices' } })
    mocks.probeDetails.mockResolvedValue([{ id: 31, stage: 'probe', http_status: 200, raw_response: '{"choices":[]}' }, { id: 32, stage: 'format_repair', repair_of_attempt_id: 31, http_status: 200, raw_response: '{"error":"filtered"}' }])
    const wrapper = mount(AuditConfigPanel, { global: { stubs } }); await flushPromises()
    await wrapper.findAll('button').find(b => b.text() === 'probe')!.trigger('click'); await flushPromises()
    await wrapper.findAll('button').find(b => b.text() === 'probeDetails')!.trigger('click'); await flushPromises()
    expect(mocks.probeDetails).toHaveBeenCalledWith(32)
    expect(wrapper.text()).toContain('{"choices":[]}')
    expect(wrapper.text()).toContain('repairOf #31')
    wrapper.unmount()
  })
  it('lists administrators for audit exclusion and manages regular-user counters separately', async () => {
    mocks.users.mockResolvedValue([
      { id: 1, username: 'root', email: 'root@example.invalid', role: 'admin', status: 'active', disable_violation_count: 0, disable_reset_at: null, action_pending: false },
      { id: 2, username: 'user', email: 'user@example.invalid', role: 'user', status: 'disabled', disable_violation_count: 5, disable_reset_at: null, action_pending: false }
    ])
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    const adminCheckbox = wrapper.findAll('label').find(item => item.text().includes('root (#1)'))!.get('input[type="checkbox"]')
    await adminCheckbox.setValue(true)
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(mocks.saveConfig.mock.lastCall?.[0].config.excluded_user_ids).toEqual([1])
    const resetSelect = wrapper.findAll('select').find(select => select.find('option[value="2"]').exists())!
    await resetSelect.setValue('2')
    await wrapper.findAll('button').find(button => button.text() === 'enableAndReset')!.trigger('click')
    await flushPromises()
    expect(mocks.enableAndReset).toHaveBeenCalledWith(2)
    wrapper.unmount()
  })
  it('fills rule defaults when features are enabled and replaces a key only after input', async () => {
    mocks.getConfig.mockResolvedValue({ ...saved(), has_api_keys: { a: true } })
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    const mode = wrapper.findAll('select').find(select => select.find('option[value="async"]').exists())!
    await mode.setValue('async')
    const warning = wrapper.findAll('label').find(item => item.text().includes('warning'))!.get('input[type="checkbox"]')
    await warning.setValue(true)
    const disable = wrapper.findAll('label').find(item => item.text().includes('disable'))!.get('input[type="checkbox"]')
    await disable.setValue(true)
    expect((wrapper.get('[data-test="review-threshold"]').element as HTMLInputElement).value).toBe('50')
    expect((wrapper.get('[data-test="block-threshold"]').element as HTMLInputElement).value).toBe('80')
    const numberByLabel = (text: string) => wrapper.findAll('label').find(item => item.text().includes(text))!.get('input[type="number"]')
    expect((numberByLabel('warningWindow').element as HTMLInputElement).value).toBe('10')
    expect((numberByLabel('warningLimit').element as HTMLInputElement).value).toBe('3')
    expect((numberByLabel('disableLimit').element as HTMLInputElement).value).toBe('5')
    await wrapper.get('input[type="password"]').setValue('replacement-key')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(mocks.saveConfig.mock.lastCall?.[0].keys).toContainEqual({ model_id: 'a', action: 'replace', api_key: 'replacement-key' })
    wrapper.unmount()
  })
})

describe('node decision evidence', () => {
  it('shows the actual skipped-joint threshold and high segment score, without a fake joint zero', () => {
    const wrapper = mount(NodeDecision, { props: {
      model: { model_id: 'a', model_name: 'Node', basis: 'segments_all_pass', confidence: null, max_segment_confidence: 0.95, decision: 'pass', reason: '', reused: false, segments: [] },
      config: { revision: 3, review_threshold: 1, block_threshold: 1 }
    } })
    expect(wrapper.text()).toContain('0.95')
    expect(wrapper.text()).toContain('triggerThreshold: 1')
    expect(wrapper.text()).toContain('jointNotRun')
    expect(wrapper.text()).not.toContain('jointScore')
    wrapper.unmount()
  })
  it('distinguishes a reused joint result from a fresh call and displays its own revision', () => {
    const wrapper = mount(NodeDecision, { props: {
      model: { model_id: 'a', model_name: 'Node', basis: 'joint', confidence: 0.65, decision: 'review', reason: '', reused: true, joint_attempt_id: 7, segments: [] },
      config: { revision: 8, review_threshold: 0.5, block_threshold: 0.8 }
    } })
    expect(wrapper.text()).toContain('reusedJoint: 0.65')
    expect(wrapper.text()).toContain('decisionRevision: 8')
    expect(wrapper.text()).toContain('Attempt #7')
    wrapper.unmount()
  })
})

describe('third-party audit records', () => {
  it('can clear a time window inherited from the overview', async () => {
    mocks.jobs.mockResolvedValue({ items: [], total: 0 })
    const wrapper = mount(AuditRecords, { props: { source: 'jobs', initialFilter: { from: '2026-09-05T00:00:00Z', to: '2026-09-06T00:00:00Z' } }, global: { stubs: { ...stubs, AuditDetail: true } } })
    await flushPromises()
    const inputs = wrapper.findAll('input[type="datetime-local"]')
    expect((inputs[0]!.element as HTMLInputElement).value).not.toBe('')
    await inputs[0]!.setValue(''); await inputs[1]!.setValue('')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(mocks.jobs.mock.lastCall?.[0]).not.toHaveProperty('from')
    expect(mocks.jobs.mock.lastCall?.[0]).not.toHaveProperty('to')
    wrapper.unmount()
  })
  it('disables batch operations when a newly applied filter fails to load', async () => {
    const job = { id: 9, run_kind: 'request', identity: {}, status: 'done', snapshot_status: 'complete', attempts: 1, max_attempts: 3 } as AuditJob
    mocks.jobs.mockResolvedValueOnce({ items: [job], total: 1 }).mockRejectedValueOnce(new Error('query failed'))
    const wrapper = mount(AuditRecords, { props: { source: 'jobs' }, global: { stubs: { ...stubs, AuditDetail: true } } })
    await flushPromises()
    await wrapper.get('form').trigger('submit'); await flushPromises()
    const batch = wrapper.findAll('button').find(button => button.text().startsWith('reauditFilter'))!
    expect(batch.attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })
  it('shows the source capture id and opens the matching captured input', async () => {
    const job = { id: 9, capture_id: 4, run_kind: 'request', identity: {}, status: 'done', snapshot_status: 'complete', attempts: 1, max_attempts: 3 } as AuditJob
    mocks.jobs.mockResolvedValue({ items: [job], total: 1 })
    mocks.capture.mockResolvedValue({ id: 4, raw_body: btoa('{}'), protocol: 'responses', body_bytes: 2, created_at: '2026-09-06T00:00:00Z' })
    const wrapper = mount(AuditRecords, { props: { source: 'jobs' }, global: { stubs: { ...stubs, AuditDetail: true } } })
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '#4')!.trigger('click')
    await flushPromises()
    expect(mocks.capture).toHaveBeenCalledWith(4)
    wrapper.unmount()
  })
})

describe('lossless full input', () => {
  it('renders the server-supplied text without rounding business integers', async () => {
    const raw = '{"fields":{"input":{"id":9007199254740993}}}'
    mocks.job.mockResolvedValue({ job: { id: 1, identity: {}, status: 'failed', snapshot_status: 'complete', config_snapshot: {} }, outcome: null, attempts: [], actions: [], reaudits: [], input_json: raw, input_parts: [], non_text: [] })
    const wrapper = mount(AuditDetail, { props: { source: 'jobs', id: 1 }, global: { stubs } })
    await flushPromises()
    expect(wrapper.text()).toContain('9007199254740993')
    expect(wrapper.text()).not.toContain('9007199254740992')
    wrapper.unmount()
  })
  it('renders captured UTF-8 bytes directly in the web view', () => {
    const original = '{"input":"原始请求"}'
    const wrapper = mount(CaptureBody, { props: { capture: { id: 2, capture_key: 'c', transport: 'http', protocol: 'responses', body_format: 'entity_bytes', raw_body: btoa(unescape(encodeURIComponent(original))), body_bytes: original.length, body_sha256: 'sha', snapshot_status: 'complete', eligibility_status: 'passed', processing_status: 'done', forwarding_status: 'complete', created_at: '2026-09-06T00:00:00Z', last_error_message: '' } } })
    expect(wrapper.text()).toContain(original)
    wrapper.unmount()
  })
  it('starts a single-record reaudit and exposes the created job for global refresh', async () => {
    mocks.job.mockResolvedValue({ job: { id: 1, capture_id: 2, identity: {}, status: 'done', snapshot_status: 'complete', config_snapshot: {} }, outcome: null, attempts: [], actions: [], reaudits: [], input_json: '{}', input_parts: [], non_text: [] })
    mocks.reaudit.mockResolvedValue({ matched: 1, ready: 1, items: [{ source_job_id: 1, job_id: 9, status: 'created', reason: '' }] })
    const wrapper = mount(AuditDetail, { props: { source: 'jobs', id: 1 }, global: { stubs } })
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === 'reaudit')!.trigger('click')
    await flushPromises()
    expect(mocks.reaudit).toHaveBeenCalledWith({ source: 'jobs', filter: { ids: [1] } })
    expect(wrapper.emitted('reaudit-created')?.[0]).toEqual([[9]])
    expect(wrapper.text()).not.toContain('enableAndReset')
    expect(wrapper.text()).not.toContain('downloadCapture')
    wrapper.unmount()
  })
})

describe('audit overview refresh', () => {
  it('moves the end time to now before reloading all overview data', async () => {
    const wrapper = mount(AuditOverview)
    await flushPromises()
    const inputs = wrapper.findAll('input[type="datetime-local"]')
    await inputs[1]!.setValue('2020-01-01T00:00')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(mocks.stats.mock.lastCall?.[0].to).not.toBe('2020-01-01T00:00:00.000Z')
    expect(mocks.runtime).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })
})
