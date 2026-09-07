import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AuditConfigPanel from '../AuditConfigPanel.vue'
import AuditRecords from '../AuditRecords.vue'
import AuditDetail from '../AuditDetail.vue'
import NodeDecision from '../NodeDecision.vue'
import type { AuditJob, SavedConfig } from '@/api/admin/third-party-prompt-audit'

const mocks = vi.hoisted(() => ({ getConfig: vi.fn(), getContract: vi.fn(), groups: vi.fn(), probe: vi.fn(), saveConfig: vi.fn(), jobs: vi.fn(), job: vi.fn(), events: vi.fn(), preview: vi.fn(), reaudit: vi.fn(), showError: vi.fn(), showSuccess: vi.fn() }))
vi.mock('@/api/admin/third-party-prompt-audit', () => ({ thirdPartyPromptAuditAPI: mocks }))
vi.mock('@/api/admin/groups', () => ({ getAll: mocks.groups }))
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key.split('.').at(-1), te: () => true }) }))

function saved(): SavedConfig {
  return { mode: 'off', audit_scope: 'full_request', platforms: [], all_groups: true, group_ids: [], audit_prompt: 'saved policy',
    models: [{ id: 'a', name: 'Node', base_url: 'https://example.invalid', model: 'test', enabled: true, timeout_ms: 1000 }],
    review_threshold: null, block_threshold: null, aggregation: 'any_block', worker_count: 4, store_pass_events: true,
    warning: { enabled: false, window: 0, limit: 0 }, disable: { enabled: false, limit: 0 }, admin_email: '', revision: 1, warning_rule_revision: 0,
    has_api_keys: {}, updated_by: 1, updated_at: '2026-09-06T00:00:00Z', application_error: '', applied_revision: 1, instance_id: 'test-instance', model_defaults: { timeout_ms: 300000 } }
}
const stubs = { BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /></div>' }, Pagination: true }

beforeEach(() => {
  vi.resetAllMocks()
  mocks.getConfig.mockResolvedValue(saved())
  mocks.getContract.mockResolvedValue({ version: 'v1', output_contract: 'fixed contract', default_policy: 'default policy' })
  mocks.saveConfig.mockImplementation(async value => ({ ...saved(), ...value.config, revision: 2 }))
  mocks.groups.mockResolvedValue([])
  mocks.probe.mockResolvedValue({ ok: true, model_id: 'a', result: { confidence: 0.1, reason: 'normal' }, tested_at: '2026-09-06T00:00:00Z' })
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
})
