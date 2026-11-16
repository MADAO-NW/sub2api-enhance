import {mount,flushPromises} from '@vue/test-utils'
import {beforeEach,describe,expect,it,vi} from 'vitest'
import QuotaFollowView from '../QuotaFollowView.vue'
const mocks=vi.hoisted(()=>({config:vi.fn(),save:vi.fn(),runtime:vi.fn(),discovery:vi.fn(),records:vi.fn(),detail:vi.fn(),reconcile:vi.fn(),retry:vi.fn(),acknowledge:vi.fn(),resetNow:vi.fn(),events:vi.fn(),event:vi.fn(),showError:vi.fn(),showSuccess:vi.fn()}))
vi.mock('@/api/quota-follow',()=>({quotaFollowAPI:mocks}))
vi.mock('@/stores/app',()=>({useAppStore:()=>mocks}))
vi.mock('vue-i18n',()=>({useI18n:()=>({t:(key:string)=>key.split('.').at(-1),te:()=>true})}))
vi.mock('@/features/system-update/SystemUpdatePanel.vue',()=>({default:{template:'<button>versions</button>'}}))
const saved={enabled:true,monitor_account_id:7,epoch:'epoch',enabled_at:'2026-11-16T08:00:00Z',revision:3,updated_by:1,updated_at:'2026-11-16T08:00:00Z'}
const candidate=(id:number,overrides:Record<string,unknown>={})=>({id,name:`Monitor ${id}`,platform:'openai',type:'oauth',parent_account_id:null,status:'active',schedulable:false,deleted:false,group_count:0,problems:[],valid:true,...overrides})
const runtime={epoch:'epoch',monitor_account_id:7,account_name:'Monitor 7',upstream_account_id:'acc-1',plan_type:'plus',baseline_reset_at:'2026-11-20T07:22:09Z',awaiting_window:false,used_percent:78,fetched_at:'2026-11-16T08:00:00Z',last_checked_at:'2026-11-16T08:00:00Z',next_check_at:'2026-11-16T08:12:00Z',last_reset_at:'2026-11-09T07:22:09Z',last_event_id:55,paused_revision:null,last_error:''}
const discovery={monitor_accounts:[candidate(7),candidate(8),candidate(9,{schedulable:true,problems:['账号仍在调度中，请先在原版关闭调度'],valid:false})],subscription_count:3,group_counts:[{group_id:1,group_name:'OpenAI A',count:2},{group_id:2,group_name:'OpenAI B',count:1}]}
const dialogs={BaseDialog:{props:['show'],template:'<div v-if="show"><slot/><slot name="footer"/></div>'}}
beforeEach(()=>{
 vi.resetAllMocks();sessionStorage.clear()
 mocks.config.mockResolvedValue(structuredClone(saved));mocks.save.mockResolvedValue({...structuredClone(saved),monitor_account_id:8,revision:4})
 mocks.runtime.mockResolvedValue(structuredClone(runtime));mocks.discovery.mockResolvedValue(structuredClone(discovery))
 mocks.records.mockResolvedValue({items:[],total:0});mocks.events.mockResolvedValue({items:[],total:0})
 mocks.resetNow.mockResolvedValue({event_id:101,status:'pending'})
})
describe('subscription quota follow',()=>{
 it('keeps drafts while refreshing status and loads configuration only once',async()=>{
  const w=mount(QuotaFollowView);await flushPromises()
  await w.get('[data-test="enabled"]').setValue(false)
  await w.findAll('button').find(b=>b.text()==='refresh')!.trigger('click');await flushPromises()
  expect((w.get('[data-test="enabled"]').element as HTMLInputElement).checked).toBe(false)
  expect(mocks.config).toHaveBeenCalledTimes(1)
  expect(w.get('[data-test="immediate-reset"]').attributes('disabled')).toBeDefined()
  w.unmount()
 })
 it('shows monitor candidates with validation results and saves the flat configuration',async()=>{
  const w=mount(QuotaFollowView);await flushPromises()
  const select=w.get('[data-test="monitor"]')
  expect(select.text()).toContain('Monitor 8')
  expect(select.text()).toContain('monitorInvalid')
  await select.setValue(8)
  await w.get('form').trigger('submit');await flushPromises()
  expect(mocks.save).toHaveBeenCalledWith({enabled:true,monitor_account_id:8},3)
  w.unmount()
 })
 it('previews every active subscription and reuses the request key after a lost response',async()=>{
  mocks.resetNow.mockRejectedValueOnce(new Error('timeout'))
  const w=mount(QuotaFollowView,{global:{stubs:dialogs}});await flushPromises()
  await w.get('[data-test="immediate-reset"]').trigger('click');await flushPromises()
  expect(mocks.discovery).toHaveBeenCalled()
  expect(w.get('[data-test="preview-count"]').text()).toBe('3')
  await w.get('[data-test="confirm-immediate-reset"]').trigger('click');await flushPromises()
  const first=mocks.resetNow.mock.calls[0][0]
  expect(first).toEqual({expected_revision:3,request_id:expect.any(String)})
  await w.get('[data-test="confirm-immediate-reset"]').trigger('click');await flushPromises()
  expect(mocks.resetNow.mock.calls[1][0]).toEqual(first)
  expect(w.text()).toContain('101')
  w.unmount()
 })
 it('shows pause guidance and the last follow event',async()=>{
  mocks.runtime.mockResolvedValue({...structuredClone(runtime),paused_revision:3,last_error:'监控账号授权失效，请在 sub2api 重新授权后重新保存配置'})
  const w=mount(QuotaFollowView);await flushPromises()
  const status=w.get('[data-test="status"]').text()
  expect(status).toContain('55')
  expect(status).toContain('监控账号授权失效')
  expect(status).toContain('pauseHint')
  w.unmount()
 })
 it('shows legacy records without a cache repair action',async()=>{
  const record={id:9,user_id:7,username:'User',source:'enhance',source_detail:'weekly_carryover',action_type:'carryover',evidence_type:'confirmed',window:'weekly',status:'succeeded',occurred_at:'2026-09-07T00:00:01Z',before_usage_usd:'0.0000000001',after_usage_usd:'10.0000000002',subscription_id:null,group_id:null}
  mocks.records.mockResolvedValue({items:[record],total:1});mocks.detail.mockResolvedValue({record,carryover:{status:'succeeded'}})
  const w=mount(QuotaFollowView,{global:{stubs:dialogs}});await flushPromises()
  expect(w.get('[data-test="records"]').text()).toContain('legacy')
  expect(w.text()).toContain('0.0000000001 → 10.0000000002')
  await w.findAll('button').find(b=>b.text()==='details')!.trigger('click');await flushPromises()
  expect(w.text()).not.toContain('repairCache')
  w.unmount()
 })
})
