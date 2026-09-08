import {flushPromises} from '@vue/test-utils'
import {describe,it,expect,vi} from 'vitest'
import type {App} from 'vue'
const api=vi.hoisted(()=>({post:vi.fn().mockResolvedValue({data:{}}),get:vi.fn().mockResolvedValue({data:{}})}))
vi.mock('@/api/client',()=>({apiClient:api,setBootstrapToken:vi.fn(),reloadMenu:vi.fn()}))
vi.mock('@/views/admin/ThirdPartyPromptAuditView.vue',()=>({default:{template:'<div>Audit page</div>'}}))
describe('embedded administrator bootstrap',()=>{
 it('removes token before initial router navigation and keeps it out of persistent storage',async()=>{
  document.body.innerHTML='<div id="app"></div>'
  history.replaceState(null,'','/enhance/third-party-prompt-audit?token=unit-test-token&theme=dark&lang=en')
  await import('../main')
  await flushPromises()
  expect(location.search).not.toContain('token')
  expect(api.post).toHaveBeenCalledWith('/auth/bootstrap',{token:'unit-test-token'})
  expect(api.get).not.toHaveBeenCalled()
  expect(document.documentElement.classList.contains('dark')).toBe(true)
  expect(JSON.stringify(localStorage)).not.toContain('unit-test-token')
  expect(JSON.stringify(sessionStorage)).not.toContain('unit-test-token')
  const root=document.querySelector('#app') as HTMLElement & {__vue_app__?:App}
  root.__vue_app__?.unmount()
 })
})
