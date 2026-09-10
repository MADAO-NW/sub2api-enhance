import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CaptureRecords from '../CaptureRecords.vue'

const mocks = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), captures: vi.fn(), capture: vi.fn(), users: vi.fn(), userKeys: vi.fn(), previewRecoveries: vi.fn(), createRecoveries: vi.fn(), showError: vi.fn(), showSuccess: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: mocks }))
vi.mock('@/api/admin/third-party-prompt-audit', () => ({ thirdPartyPromptAuditAPI: mocks }))
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key.split('.').at(-1), te: () => true }) }))
const base = { id: 2, snapshot_status: 'complete', processing_status: 'failed', identity: { user_id: 1 } }
const options = { global: { stubs: { BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /></div>' }, CaptureBody: true, AuditDetail: true } } }
beforeEach(() => {
 vi.resetAllMocks()
 mocks.captures.mockResolvedValue({ items: [base], total: 1 })
 mocks.capture.mockResolvedValue(base)
 mocks.users.mockResolvedValue([{ id: 1, username: 'user', email: 'user@example.invalid' }])
 mocks.userKeys.mockResolvedValue([{ id: 3, name: 'key' }])
 mocks.previewRecoveries.mockResolvedValue({ matched: 1, ready: 1, items: [{ capture_id: 2, action: 'create_job', status: 'ready' }] })
 mocks.createRecoveries.mockResolvedValue({ matched: 1, ready: 1, items: [{ capture_id: 2, job_id: 9, action: 'create_job', status: 'created' }] })
})
describe('capture recovery state', () => {
 it('reloads the current page when the tab refresh key changes', async () => {
  const wrapper = mount(CaptureRecords, { ...options, props: { refreshKey: 0 } }); await flushPromises()
  expect(mocks.captures).toHaveBeenCalledTimes(1)
  await wrapper.setProps({ refreshKey: 1 }); await flushPromises()
  expect(mocks.captures).toHaveBeenCalledTimes(2)
  wrapper.unmount()
 })
 it('shows username and email together with a forwarding-specific status label', async () => {
  mocks.captures.mockResolvedValue({ items: [{ ...base, display_username: '测试用户', display_email: 'user@example.invalid', forwarding_status: 'complete' }], total: 1 })
  const wrapper = mount(CaptureRecords, options); await flushPromises()
  expect(wrapper.text()).toContain('(#1)测试用户')
  expect(wrapper.text()).toContain('user@example.invalid')
  expect(wrapper.text()).toContain('forwarding_complete')
  wrapper.unmount()
 })
 it.each([
  [{ ...base, job_id: 8 }, 'linkedJob', false],
  [{ ...base, processing_status: 'processing' }, 'captureProcessingWait', false],
  [{ ...base, snapshot_status: 'incomplete' }, 'captureCannotRecover', false],
  [base, 'reprocessCapture', true]
 ])('shows the appropriate operation for %j', async (capture, expected, recover) => {
  mocks.capture.mockResolvedValue(capture)
  const wrapper = mount(CaptureRecords, options); await flushPromises()
  await wrapper.findAll('button').find(b => b.text() === 'viewCapture')!.trigger('click'); await flushPromises()
  expect(wrapper.text()).toContain(expected)
  expect(wrapper.findAll('button').some(b => b.text() === 'reprocessCapture')).toBe(recover)
  expect(wrapper.findAll('select')).toHaveLength(5)
  wrapper.unmount()
 })
 it('requires a selected key only for unknown identity and refreshes the linked job after recovery', async () => {
  mocks.capture.mockResolvedValue({ ...base, identity: { user_id: 0 } })
  const wrapper = mount(CaptureRecords, options); await flushPromises()
  await wrapper.findAll('button').find(b => b.text() === 'viewCapture')!.trigger('click'); await flushPromises()
  const recovery = wrapper.findAll('button').find(b => b.text() === 'reprocessCapture')!
  expect(recovery.attributes('disabled')).toBeDefined()
  const selects = wrapper.findAll('select')
  await selects[selects.length - 2]!.setValue('1'); await flushPromises()
  expect(mocks.userKeys).toHaveBeenCalledWith(1)
  await selects[selects.length - 1]!.setValue('3')
  mocks.capture.mockResolvedValue({ ...base, job_id: 9 })
  mocks.post.mockResolvedValue({ data: { job_id: 9 } })
  await recovery.trigger('click'); await flushPromises()
  expect(mocks.post).toHaveBeenCalledWith('/admin/third-party-prompt-audit/captures/2/reprocess', { api_key_id: 3 })
  expect(wrapper.text()).toContain('linkedJob #9')
 wrapper.unmount()
 })
 it('previews and submits all failed capture recoveries', async () => {
  const wrapper = mount(CaptureRecords, options); await flushPromises()
  await wrapper.findAll('button').find(b => b.text() === 'recoverAllFailures')!.trigger('click'); await flushPromises()
  expect(mocks.previewRecoveries).toHaveBeenCalledTimes(1)
  expect(wrapper.text()).toContain('Capture #2')
  await wrapper.findAll('button').find(b => b.text() === 'submitRecovery')!.trigger('click'); await flushPromises()
  expect(mocks.createRecoveries).toHaveBeenCalledTimes(1)
  expect(wrapper.emitted('recovery-created')?.[0]).toEqual([[9]])
  wrapper.unmount()
 })
})
