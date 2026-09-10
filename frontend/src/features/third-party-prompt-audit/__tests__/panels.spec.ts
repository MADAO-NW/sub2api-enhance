import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AuditConfigPanel from '../AuditConfigPanel.vue'
import AuditRecords from '../AuditRecords.vue'
import AuditDetail from '../AuditDetail.vue'
import AuditOverview from '../AuditOverview.vue'
import CaptureBody from '../CaptureBody.vue'
import NodeDecision from '../NodeDecision.vue'
import LatestUserContent from '../LatestUserContent.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ThirdPartyPromptAuditView from '@/views/admin/ThirdPartyPromptAuditView.vue'
import type { AuditJob, SavedConfig } from '@/api/admin/third-party-prompt-audit'

const mocks = vi.hoisted(() => ({ getConfig: vi.fn(), getContract: vi.fn(), groups: vi.fn(), users: vi.fn(), listModels: vi.fn(), enableAndReset: vi.fn(), probeDetails: vi.fn(), probe: vi.fn(), saveConfig: vi.fn(), jobs: vi.fn(), job: vi.fn(), jobLatestUserContent: vi.fn(), captureLatestUserContent: vi.fn(), preview: vi.fn(), reaudit: vi.fn(), stats: vi.fn(), runtime: vi.fn(), capture: vi.fn(), showError: vi.fn(), showSuccess: vi.fn() }))
vi.mock('@/api/admin/third-party-prompt-audit', () => ({ thirdPartyPromptAuditAPI: mocks }))
vi.mock('@/api/admin/groups', () => ({ getAll: mocks.groups }))
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key.split('.').at(-1), te: () => true }) }))

function saved(): SavedConfig {
  return { mode: 'off', capture_when_audit_off: false, audit_scope: 'full_request', platforms: [], all_groups: true, group_ids: [], excluded_user_ids: [], audit_prompt: 'saved policy',
    models: [{ id: 'a', name: 'Node', base_url: 'https://example.invalid', model: 'test', enabled: true, timeout_ms: 1000, max_concurrency: 4 }],
    review_threshold: null, block_threshold: null, aggregation: 'any_block', worker_count: 4,
    warning: { enabled: false, window: 0, limit: 0 }, disable: { enabled: false, limit: 0 }, user_rules: [], admin_email: '', revision: 1, warning_rule_revision: 0,
    has_api_keys: {}, updated_by: 1, updated_at: '2026-09-06T00:00:00Z', application_error: '', applied_revision: 1, instance_id: 'test-instance', model_defaults: { timeout_ms: 300000, max_concurrency: 4 },
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
  mocks.jobLatestUserContent.mockResolvedValue({ content: 'latest user text', unavailable_reason: '' })
  mocks.captureLatestUserContent.mockResolvedValue({ content: 'captured user text', unavailable_reason: '' })
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
  it('discards an unsaved draft when its tab refresh key changes', async () => {
    const wrapper = mount(AuditConfigPanel, { props: { refreshKey: 0 } }); await flushPromises()
    await wrapper.get('[data-test="audit-policy"]').setValue('unsaved policy')
    await wrapper.setProps({ refreshKey: 1 }); await flushPromises()
    expect((wrapper.get('[data-test="audit-policy"]').element as HTMLTextAreaElement).value).toBe('saved policy')
    wrapper.unmount()
  })
  it('sends structural samples as raw JSON text for lossless backend parsing', async () => {
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    const selector = wrapper.findAll('select').find(select => select.find('option[value="json"]').exists())!
    await selector.setValue('json')
    expect(wrapper.find('[data-test="probe-protocol"]').exists()).toBe(true)
    const raw = '  {"input":{"id":9007199254740993,"key":1,"key":2}}'
    await wrapper.get('[data-test="probe-input"]').setValue(raw)
    await wrapper.findAll('button').find(button => button.text() === 'probe')!.trigger('click'); await flushPromises()
    expect(mocks.probe).toHaveBeenCalledWith(expect.objectContaining({ input: raw, input_kind: 'json' }))
    wrapper.unmount()
  })
  it('preserves plain text that happens to begin with a bracket during a probe', async () => {
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    await wrapper.get('[data-test="probe-input"]').setValue('[this is normal text, not JSON]')
    await wrapper.findAll('button').find(button => button.text() === 'probe')!.trigger('click'); await flushPromises()
    expect(mocks.probe).toHaveBeenCalledWith(expect.objectContaining({ input: '[this is normal text, not JSON]' }))
    wrapper.unmount()
  })
  it('uses the configured risk scenarios by default and supports model discovery', async () => {
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === 'loadModels')!.trigger('click')
    await flushPromises()
    expect(mocks.listModels).toHaveBeenCalledWith({ model_id: 'a', base_url: 'https://example.invalid', timeout_ms: 1000, key_action: 'keep' })
    await wrapper.get('[data-test="model-input"]').setValue('second-model')
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
    const resetSelect = wrapper.findAll('select').find(select => select.text().includes('disableViolationCount'))!
    await resetSelect.setValue('2')
    await wrapper.findAll('button').find(button => button.text() === 'enableAndReset')!.trigger('click')
    await flushPromises()
    expect(mocks.enableAndReset).toHaveBeenCalledWith(2)
    wrapper.unmount()
  })
  it('adds a complete per-user rule while keeping administrator disabling unavailable', async () => {
    mocks.users.mockResolvedValue([
      { id: 1, username: 'root', email: 'root@example.invalid', role: 'admin', status: 'active', disable_violation_count: 0, disable_reset_at: null, action_pending: false }
    ])
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    const selector = wrapper.findAll('select').find(select => select.find('option[value="1"]').exists() && select.text().includes('root@example.invalid'))!
    await selector.setValue('1')
    await wrapper.findAll('button').find(button => button.text() === 'addUserRule')!.trigger('click')
    const mode = wrapper.findAll('label').find(item => item.text().includes('userMode'))!.get('select')
    expect((mode.element as HTMLSelectElement).value).toBe('async')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(mocks.saveConfig.mock.lastCall?.[0].config.user_rules).toEqual([expect.objectContaining({ user_id: 1, mode: 'async', review_threshold: 0.5, block_threshold: 0.8, aggregation: 'any_block', warning: { enabled: false, window: 10, limit: 3 }, disable: { enabled: false, limit: 5 } })])
    expect(wrapper.text()).toContain('adminDisableHint')
    await wrapper.findAll('button').find(button => button.text() === 'removeUserRule')!.trigger('click')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(mocks.saveConfig.mock.lastCall?.[0].config.user_rules).toEqual([])
    wrapper.unmount()
  })
  it('copies every current global rule value when adding a regular-user override', async () => {
    mocks.getConfig.mockResolvedValue({ ...saved(), mode: 'blocking', review_threshold: 0.25, block_threshold: 0.9, aggregation: 'majority_block', warning: { enabled: true, window: 20, limit: 4 }, disable: { enabled: true, limit: 7 } })
    mocks.users.mockResolvedValue([{ id: 2, username: 'user', email: 'user@example.invalid', role: 'user', status: 'active', disable_violation_count: 0, disable_reset_at: null, action_pending: false }])
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    const selector = wrapper.findAll('select').find(select => select.text().includes('user@example.invalid'))!
    await selector.setValue('2')
    await wrapper.findAll('button').find(button => button.text() === 'addUserRule')!.trigger('click')
    const globalMode = wrapper.findAll('label').find(item => item.text().startsWith('mode'))!.get('select')
    await globalMode.setValue('async')
    await wrapper.get('[data-test="review-threshold"]').setValue('40')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(mocks.saveConfig.mock.lastCall?.[0].config.user_rules).toEqual([{ user_id: 2, mode: 'blocking', review_threshold: 0.25, block_threshold: 0.9, aggregation: 'majority_block', warning: { enabled: true, window: 20, limit: 4 }, disable: { enabled: true, limit: 7 } }])
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
  it('applies providers JSON parameters without exposing credentials', async () => {
    const wrapper = mount(AuditConfigPanel); await flushPromises()
    const document = { $schemaVersion: 1, providers: [{ id: 'a', type: 'openai-chat-completions', enabled: true, baseUrl: 'https://example.com/v1/', apiModel: 'manual-model', timeoutMs: 1234, parameters: { reasoning_effort: 'none' } }] }
    await wrapper.get('[data-test="providers-json"]').setValue(JSON.stringify(document))
    await wrapper.findAll('button').find(button => button.text() === 'applyProvidersJSON')!.trigger('click')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(mocks.saveConfig.mock.lastCall?.[0].config.models[0]).toMatchObject({ id: 'a', base_url: 'https://example.com/v1/', model: 'manual-model', timeout_ms: 1234, parameters: { reasoning_effort: 'none' } })
    expect((wrapper.get('[data-test="providers-json"]').element as HTMLTextAreaElement).value).not.toContain('api_key')
    wrapper.unmount()
  })
})

describe('node decision evidence', () => {
  it('shows aggregation short-circuiting instead of an invalid score', () => {
    const wrapper = mount(NodeDecision, { props: { model: { model_id: 'b', model_name: 'Node B', basis: 'aggregation_decided', confidence: null, reason: '', reused: false, skipped: true, skip_reason: 'aggregation_decided', segments: [] } } })
    expect(wrapper.text()).toContain('aggregation_decided')
    expect(wrapper.text()).not.toContain('noValidScore')
  })
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
    expect(wrapper.text()).not.toContain('Attempt #7')
    wrapper.unmount()
  })
})

describe('third-party audit records', () => {
  it('opens advanced filters only from the compact text button', async () => {
    mocks.jobs.mockResolvedValue({ items: [], total: 0 })
    const wrapper = mount(AuditRecords, { global: { stubs: { ...stubs, AuditDetail: true } } })
    await flushPromises()
    const toggle = wrapper.get('[data-test="advanced-filter-toggle"]')
    expect(toggle.attributes('aria-expanded')).toBe('false')
    expect(wrapper.get('[data-test="advanced-filters"]').attributes('style')).toContain('display: none')
    await wrapper.get('form').trigger('click')
    expect(toggle.attributes('aria-expanded')).toBe('false')
    await toggle.trigger('click')
    expect(toggle.attributes('aria-expanded')).toBe('true')
    expect(wrapper.get('[data-test="advanced-filters"]').attributes('style') ?? '').not.toContain('display: none')
    wrapper.unmount()
  })
  it('can clear a time window inherited from the overview', async () => {
    mocks.jobs.mockResolvedValue({ items: [], total: 0 })
    const wrapper = mount(AuditRecords, { props: { initialFilter: { from: '2026-09-05T00:00:00Z', to: '2026-09-06T00:00:00Z' } }, global: { stubs: { ...stubs, AuditDetail: true } } })
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
    const job = { id: 9, current_run_kind: 'request', identity: {}, status: 'done', snapshot_status: 'complete', attempts: 1, max_attempts: 3 } as unknown as AuditJob
    mocks.jobs.mockResolvedValueOnce({ items: [job], total: 1 }).mockRejectedValueOnce(new Error('query failed'))
    const wrapper = mount(AuditRecords, { global: { stubs: { ...stubs, AuditDetail: true } } })
    await flushPromises()
    await wrapper.get('form').trigger('submit'); await flushPromises()
    const batch = wrapper.findAll('button').find(button => button.text().startsWith('reauditFilter'))!
    expect(batch.attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })
  it('shows the source capture id and opens the matching captured input', async () => {
    const job = { id: 9, capture_id: 4, current_run_kind: 'request', identity: {}, status: 'done', snapshot_status: 'complete', attempts: 1, max_attempts: 3 } as unknown as AuditJob
    mocks.jobs.mockResolvedValue({ items: [job], total: 1 })
    mocks.capture.mockResolvedValue({ id: 4, raw_body: btoa('{}'), protocol: 'responses', body_bytes: 2, created_at: '2026-09-06T00:00:00Z' })
    const wrapper = mount(AuditRecords, { global: { stubs: { ...stubs, AuditDetail: true } } })
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '#4')!.trigger('click')
    await flushPromises()
    expect(mocks.capture).toHaveBeenCalledWith(4)
    wrapper.unmount()
  })
  it('shows live identity order and the latest outcome segment reuse rate', async () => {
    const job = { id: 9, user_id: 2, display_username: 'nw', display_email: 'nwjump@163.com', current_run_kind: 'request', identity: { api_key_name: 'nw', group_name: 'openai' }, status: 'done', snapshot_status: 'complete', attempts: 1, max_attempts: 3,
      outcome: { decision: 'pass', models: [], segment_reuse: { reused: 18, total: 20, rate: 0.9 } } } as unknown as AuditJob
    mocks.jobs.mockResolvedValue({ items: [job], total: 1 })
    const wrapper = mount(AuditRecords, { global: { stubs: { ...stubs, AuditDetail: true, LatestUserContent: true } } })
    await flushPromises()
    expect(wrapper.text()).toContain('(#2)nw')
    expect(wrapper.text()).toContain('nwjump@163.com')
    expect(wrapper.text()).toContain('segmentReuseRate 90% 18 / 20')
    wrapper.unmount()
  })
  it('defaults batch reaudit to global reuse and can force fresh model calls', async () => {
    const job = { id: 9, current_run_kind: 'request', identity: {}, status: 'failed', snapshot_status: 'complete', attempts: 3, max_attempts: 3 } as unknown as AuditJob
    mocks.jobs.mockResolvedValue({ items: [job], total: 1 })
    mocks.preview.mockResolvedValue({ matched: 1, ready: 1, items: [{ job_id: 9, status: 'ready', reason: '' }] })
    mocks.reaudit.mockResolvedValue({ matched: 1, ready: 1, items: [{ job_id: 9, status: 'requeued', reason: '' }] })
    const wrapper = mount(AuditRecords, { global: { stubs: { ...stubs, AuditDetail: true } } })
    await flushPromises()
    expect(wrapper.text()).toContain('attempts 3 / 3')
    await wrapper.findAll('button').find(button => button.text().startsWith('reauditFilter'))!.trigger('click')
    await flushPromises()
    expect(mocks.preview).toHaveBeenCalledWith({ reuse_mode: 'allow', filter: {} })
    expect((wrapper.get('input[value="allow"]').element as HTMLInputElement).checked).toBe(true)
    await wrapper.get('input[value="force"]').setValue(true)
    await wrapper.findAll('button').find(button => button.text() === 'submitReaudit')!.trigger('click')
    await flushPromises()
    expect(mocks.reaudit).toHaveBeenCalledWith({ reuse_mode: 'force', filter: {} })
    wrapper.unmount()
  })
})

describe('lossless full input', () => {
  it('renders the server-supplied text without rounding business integers', async () => {
    const raw = '{"fields":{"input":{"id":9007199254740993}}}'
    mocks.job.mockResolvedValue({ job: { id: 1, identity: {}, status: 'failed', snapshot_status: 'complete', config_snapshot: {} }, outcome: null, attempts: [], actions: [], input_json: raw, input_parts: [], non_text: [], rounds: [] })
    const wrapper = mount(AuditDetail, { props: { id: 1 }, global: { stubs } })
    await flushPromises()
    expect(wrapper.text()).toContain('9007199254740993')
    expect(wrapper.text()).not.toContain('9007199254740992')
    wrapper.unmount()
  })
  it('renders captured UTF-8 bytes directly in the web view', () => {
    const original = '{"input":"原始请求"}'
    const wrapper = mount(CaptureBody, { props: { capture: { id: 2, capture_key: 'c', transport: 'http', protocol: 'responses', body_format: 'entity_bytes', raw_body: btoa(unescape(encodeURIComponent(original))), body_bytes: original.length, body_sha256: 'sha', snapshot_status: 'complete', eligibility_status: 'passed', processing_status: 'done', forwarding_status: 'complete', forwarding_observations: [{ status: 'complete', http_status: 200 }], created_at: '2026-09-06T00:00:00Z', last_error_message: '' } } })
    expect(wrapper.text()).toContain(original)
    expect(wrapper.text()).toContain('forwarding_complete')
    expect(wrapper.text()).toContain('http_status')
    wrapper.unmount()
  })
  it('requeues the same job for a new audit round and exposes it for global refresh', async () => {
    mocks.job.mockResolvedValue({ job: { id: 1, capture_id: 2, identity: {}, status: 'done', snapshot_status: 'complete', config_snapshot: {} }, outcome: null, attempts: [], actions: [], input_json: '{}', input_parts: [], non_text: [], rounds: [] })
    mocks.reaudit.mockResolvedValue({ matched: 1, ready: 1, items: [{ job_id: 1, status: 'requeued', reason: '' }] })
    const wrapper = mount(AuditDetail, { props: { id: 1 }, global: { stubs } })
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === 'reaudit')!.trigger('click')
    await flushPromises()
    expect(mocks.reaudit).toHaveBeenCalledWith({ reuse_mode: 'allow', filter: { ids: [1] } })
    expect(wrapper.emitted('reaudit-created')?.[0]).toEqual([[1]])
    expect(wrapper.text()).not.toContain('enableAndReset')
    expect(wrapper.text()).not.toContain('downloadCapture')
    wrapper.unmount()
  })
})

describe('latest user content and dialogs', () => {
  it('loads job user content lazily on hover', async () => {
    const wrapper = mount(LatestUserContent, { props: { source: 'job', id: 9, variant: 'popover' }, attachTo: document.body })
    await wrapper.get('span.inline-block').trigger('mouseenter'); await flushPromises()
    expect(mocks.jobLatestUserContent).toHaveBeenCalledWith(9)
    expect(document.body.textContent).toContain('latest user text')
    await wrapper.get('span.inline-block').trigger('mouseenter'); await flushPromises()
    expect(mocks.jobLatestUserContent).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })
  it('only closes a dialog from its backdrop when enabled', async () => {
    const enabled = mount(BaseDialog, { props: { show: true, title: 'test', closeOnClickOutside: true }, slots: { default: 'body' }, attachTo: document.body })
    const enabledBackdrop = document.querySelector('[data-test="dialog-backdrop"]') as HTMLElement
    enabledBackdrop.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await flushPromises()
    expect(enabled.emitted('close')).toHaveLength(1)
    enabled.unmount()
    const disabled = mount(BaseDialog, { props: { show: true, title: 'test', closeOnClickOutside: false, showCloseButton: false }, attachTo: document.body })
    const disabledBackdrop = document.querySelector('[data-test="dialog-backdrop"]') as HTMLElement
    disabledBackdrop.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await flushPromises()
    expect(disabled.emitted('close')).toBeUndefined()
    expect(disabled.find('button').exists()).toBe(false)
    disabled.unmount()
  })
})

describe('audit page tab refresh', () => {
  it('increments only the clicked tab refresh key, including repeated clicks', async () => {
    history.replaceState(null, '', '/enhance/third-party-prompt-audit')
    const component = (name: string) => ({ props: ['refreshKey'], template: `<div data-test="${name}">{{ refreshKey }}</div>` })
    const wrapper = mount(ThirdPartyPromptAuditView, { global: { stubs: { AuditOverview: component('overview'), AuditRecords: component('jobs'), CaptureRecords: component('captures'), AuditConfigPanel: component('config'), SystemUpdatePanel: true } } })
    const jobs = wrapper.findAll('button').find(button => button.text() === 'jobs')!
    await jobs.trigger('click')
    expect(wrapper.get('[data-test="jobs"]').text()).toBe('1')
    await jobs.trigger('click')
    expect(wrapper.get('[data-test="jobs"]').text()).toBe('2')
    expect(wrapper.get('[data-test="overview"]').text()).toBe('0')
    wrapper.unmount()
  })
})

describe('audit overview refresh', () => {
  it('shows the global segment reuse rate for the current filter', async () => {
    mocks.stats.mockResolvedValue({
      from: '2026-09-09T00:00:00Z', to: '2026-09-10T00:00:00Z', timezone: 'Asia/Shanghai', mode: '', model_id: '', stage: '', as_of: '2026-09-10T00:00:00Z',
      capture_stock: {}, forwarding_stock: {}, action_execution_stock: {}, stock: {}, oldest_waiting_at: null, waiting_for_slot: 0, cohort: {}, received: 0, reaudits_created: 0,
      formal: {}, reaudit: {}, failures: {}, current_decisions: {}, gateway: {}, gateway_latency: { count: 0, p50_ms: null, p95_ms: null }, task_latency: { count: 0, p50_ms: null, p95_ms: null },
      calls: [], evaluation_rounds: 0, reuse: { whole_lookups: 0, whole_hits: 0, segment_lookups: 0, segment_hits: 0, within_job_hits: 0, inflight_hits: 0, short_circuited_nodes: 0 },
      segment_reuse: { reused: 18, total: 20, rate: 0.9 }, segment_reuse_by_user: [{ user_id: 2, username: 'nw', email: 'nwjump@163.com', reused: 9, total: 10, rate: 0.9 }, { user_id: 7, username: '', email: '', reused: 0, total: 0, rate: null }], actions: {}, notification_stock: {}, delivery_stock: {}, auth_cache_stock: {}
    })
    const wrapper = mount(AuditOverview)
    await flushPromises()
    expect(wrapper.text()).toContain('globalSegmentReuseRate')
    expect(wrapper.text()).toContain('90% 18 / 20')
    expect(wrapper.text()).toContain('userSegmentReuseRate')
    expect(wrapper.text()).toContain('(#2)nw')
    expect(wrapper.text()).toContain('nwjump@163.com')
    const users = wrapper.get('[data-test="user-segment-reuse"]').text()
    expect(users).toContain('90%')
    expect(users).toContain('9 / 10')
    expect(users).toContain('(#7)')
    expect(users).toContain('0 / 0')
    wrapper.unmount()
  })
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
