import { createApp } from 'vue'
import { createPinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import Audit from './views/admin/ThirdPartyPromptAuditView.vue'
import zh from './i18n/zh'
import en from './i18n/en'
import {zh as quotaZh,en as quotaEn} from './features/quota-follow/messages'
import './style.css'
const query = new URLSearchParams(location.search)
// 在创建 Router 前移除凭据，避免初始导航把 token 查询项写回地址栏。
const bootstrapToken=query.get('token')
if(query.get('tab')==='events')query.set('tab','jobs')
query.delete('token');query.delete('src_url')
history.replaceState(null,'',location.pathname+(query.size?'?'+query.toString():'')+location.hash)
const locale = query.get('lang')?.startsWith('en') ? 'en' : 'zh'
document.documentElement.dataset.uiMode = query.get('ui_mode') === 'embedded' ? 'embedded' : 'standalone'
document.documentElement.classList.toggle('dark', query.get('theme') === 'dark')
const router = createRouter({history:createWebHistory('/enhance/'),routes:[{path:'/',redirect:'/third-party-prompt-audit'},{path:'/third-party-prompt-audit',component:Audit},{path:'/quota-follow',component:()=>import('./features/quota-follow/QuotaFollowView.vue')}]})
createApp(App,{bootstrapToken}).use(createPinia()).use(createI18n({legacy:false,locale,messages:{zh:{quotaFollow:quotaZh,admin:zh,nav:{securityAudit:'sub2api++ 独立增强服务'}},en:{quotaFollow:quotaEn,admin:en,nav:{securityAudit:'sub2api++ Enhancement'}}}})).use(router).mount('#app')
