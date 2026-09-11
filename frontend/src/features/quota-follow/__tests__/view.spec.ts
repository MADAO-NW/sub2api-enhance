import {mount,flushPromises} from '@vue/test-utils'
import {beforeEach,describe,expect,it,vi} from 'vitest'
import QuotaFollowView from '../QuotaFollowView.vue'
const mocks=vi.hoisted(()=>({config:vi.fn(),save:vi.fn(),runtime:vi.fn(),discovery:vi.fn(),records:vi.fn(),detail:vi.fn(),reconcile:vi.fn(),repairCache:vi.fn(),resetNow:vi.fn(),events:vi.fn(),event:vi.fn(),groups:vi.fn(),showError:vi.fn(),showSuccess:vi.fn()}))
vi.mock('@/api/quota-follow',()=>({quotaFollowAPI:mocks}))
vi.mock('@/api/admin/groups',()=>({getAll:mocks.groups}))
vi.mock('@/stores/app',()=>({useAppStore:()=>mocks}))
vi.mock('vue-i18n',()=>({useI18n:()=>({t:(key:string)=>key.split('.').at(-1),te:()=>true})}))
vi.mock('@/features/system-update/SystemUpdatePanel.vue',()=>({default:{template:'<button>versions</button>'}}))
beforeEach(()=>{
 vi.resetAllMocks()
 mocks.config.mockResolvedValue({enabled:false,group_id:7,reset_weekly_enabled:true,reset_daily_enabled:false,min_interval_minutes:10,max_interval_minutes:15,observe_only:true,revision:3,epoch:'epoch',enabled_at:null})
 mocks.groups.mockResolvedValue([{id:7,name:'OpenAI',platform:'openai'}]);mocks.runtime.mockResolvedValue({accounts:[],account_states:[],last_error:'',collector_error:'',redis_status:'not_configured'})
 mocks.discovery.mockResolvedValue({accounts:[],users:[]});mocks.records.mockResolvedValue({items:[],total:0});mocks.events.mockResolvedValue({items:[],total:0});mocks.save.mockResolvedValue({revision:4});mocks.resetNow.mockResolvedValue({group_id:7,windows:['weekly'],total:1,succeeded:1,non_success:0,items:[{user_id:8,username:'User',window:'weekly',request_id:'req',status:'succeeded',http_status:200,error:''}]})
})
describe('quota follow page',()=>{
 it('shows boundaries and keeps configuration drafts when refreshing',async()=>{
  const wrapper=mount(QuotaFollowView);await flushPromises();expect(wrapper.get('[data-test="boundary"]').text()).toBe('boundary')
  expect(wrapper.classes()).toContain('enhance-page')
  const enabled=wrapper.get('[data-test="enabled"]');await enabled.setValue(true)
  await wrapper.findAll('button').find(b=>b.text()==='refresh')!.trigger('click');await flushPromises()
  expect((enabled.element as HTMLInputElement).checked).toBe(true);expect(mocks.config).toHaveBeenCalledTimes(1);wrapper.unmount()
 })
 it('sends only editable configuration with the expected revision',async()=>{
  const wrapper=mount(QuotaFollowView);await flushPromises();await wrapper.get('[data-test="enabled"]').setValue(true);await wrapper.get('form').trigger('submit');await flushPromises()
  expect(mocks.save).toHaveBeenCalledWith({enabled:true,group_id:7,reset_weekly_enabled:true,reset_daily_enabled:false,min_interval_minutes:10,max_interval_minutes:15,observe_only:true},3)
  expect(mocks.reconcile).not.toHaveBeenCalled();wrapper.unmount()
 })
 it('keeps origin and inference evidence distinct in reset records',async()=>{
  mocks.records.mockResolvedValue({items:[{id:1,user_id:7,username:'User',source:'sub2api',source_detail:'natural_weekly',action_type:'reset',evidence_type:'inferred',window:'weekly',status:'succeeded',occurred_at:'2026-09-07T00:00:00Z',before_usage_usd:'1.1234567890',after_usage_usd:'0.0000000001',accounts:[]}],total:1})
  const wrapper=mount(QuotaFollowView);await flushPromises();const text=wrapper.get('[data-test="records"]').text();expect(text).toContain('sub2api');expect(text).toContain('inferred');expect(text).toContain('1.1234567890 → 0.0000000001');wrapper.unmount()
 })
 it('shows carryover separately and schedules only cache cleanup from its detail',async()=>{
  const record={id:9,user_id:7,username:'User',source:'enhance',source_detail:'weekly_carryover',action_type:'carryover',evidence_type:'confirmed',window:'weekly',status:'succeeded',occurred_at:'2026-09-07T00:00:01Z',before_usage_usd:'0.0000000001',after_usage_usd:'10.0000000002',accounts:[]}
  mocks.records.mockResolvedValue({items:[record],total:1});mocks.detail.mockResolvedValue({record,carryover:{id:3,status:'succeeded',cache_status:'retry_pending',error_message:'缓存清理失败'}});mocks.repairCache.mockResolvedValue({scheduled:true})
  const wrapper=mount(QuotaFollowView,{global:{stubs:{BaseDialog:{props:['show'],template:'<div v-if="show"><slot/></div>'}}}});await flushPromises()
  expect(wrapper.get('[data-test="records"]').text()).toContain('weekly_carryover');expect(wrapper.get('[data-test="records"]').text()).toContain('0.0000000001 → 10.0000000002')
  await wrapper.findAll('button').find(b=>b.text()==='details')!.trigger('click');await flushPromises()
  await wrapper.findAll('button').find(b=>b.text()==='repairCache')!.trigger('click');await flushPromises()
  expect(mocks.repairCache).toHaveBeenCalledWith(9);expect(mocks.reconcile).not.toHaveBeenCalled();expect(mocks.save).not.toHaveBeenCalled();wrapper.unmount()
 })
 it('confirms before calling the original reset API',async()=>{
  mocks.discovery.mockResolvedValue({accounts:[],users:[{id:8,username:'User',email:'user@example.com'}]})
  const wrapper=mount(QuotaFollowView,{global:{stubs:{BaseDialog:{props:['show'],template:'<div v-if="show"><slot/><slot name="footer"/></div>'}}}});await flushPromises()
  await wrapper.get('[data-test="immediate-reset"]').trigger('click');expect(mocks.resetNow).not.toHaveBeenCalled();expect(wrapper.get('[data-test="confirm-immediate-reset"]')).toBeTruthy()
  await wrapper.get('[data-test="confirm-immediate-reset"]').trigger('click');await flushPromises();expect(mocks.resetNow).toHaveBeenCalledWith({group_id:7,windows:['weekly']});wrapper.unmount()
 })

})
