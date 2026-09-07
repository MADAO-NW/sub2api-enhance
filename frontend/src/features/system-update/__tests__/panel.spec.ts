import {mount,flushPromises} from '@vue/test-utils'
import {beforeEach,describe,expect,it,vi} from 'vitest'
import Panel from '../SystemUpdatePanel.vue'
const mocks=vi.hoisted(()=>({status:vi.fn(),check:vi.fn(),update:vi.fn(),rollback:vi.fn(),restart:vi.fn()}))
vi.mock('@/api/system-update',()=>({systemUpdateAPI:mocks}))
vi.mock('vue-i18n',()=>({useI18n:()=>({locale:{value:'zh'}})}))
const current={version:'1.0.0',build_type:'release',commit:'abc',date:'2026-09-07',schema_digest:'hash'}
beforeEach(()=>{vi.resetAllMocks();mocks.status.mockResolvedValue({current,supported:true,managed:true,restart_required:false,can_rollback:false,state:{phase:'idle',error:''}});mocks.check.mockResolvedValue({latest_version:'1.1.0',has_update:true,release:{html_url:'https://github.com/MADAO-NW/sub2api-enhance/releases/tag/v1.1.0',body:'Release notes'}});mocks.update.mockResolvedValue({accepted:true})})
const options={global:{stubs:{BaseDialog:{props:['show'],template:'<section v-if="show"><slot/></section>'}}}}
describe('enhancement updates',()=>{
 it('checks only on request and confirms before changing the installed version',async()=>{
  const wrapper=mount(Panel,options);expect(mocks.status).not.toHaveBeenCalled();expect(mocks.check).not.toHaveBeenCalled()
  await wrapper.get('[data-test="open-update"]').trigger('click');await flushPromises();await wrapper.get('[data-test="check-update"]').trigger('click');await flushPromises()
  await wrapper.get('[data-test="apply-update"]').trigger('click');expect(mocks.update).not.toHaveBeenCalled()
  await wrapper.get('[data-test="confirm-update"]').trigger('click');await flushPromises();expect(mocks.update).toHaveBeenCalledWith('1.1.0');expect(mocks.restart).not.toHaveBeenCalled();wrapper.unmount()
 })
 it('does not expose installation actions for source builds',async()=>{
  mocks.status.mockResolvedValue({current:{...current,build_type:'source'},supported:false,managed:false,can_rollback:false,state:{phase:'idle',error:''}})
  const wrapper=mount(Panel,options);await wrapper.get('[data-test="open-update"]').trigger('click');await flushPromises();await wrapper.get('[data-test="check-update"]').trigger('click');await flushPromises();expect(wrapper.find('[data-test="apply-update"]').exists()).toBe(false);expect(wrapper.text()).toContain('源码运行');wrapper.unmount()
 })
})
