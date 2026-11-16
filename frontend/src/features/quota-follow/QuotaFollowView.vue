<script setup lang="ts">
import {computed,onMounted,onUnmounted,reactive,ref} from 'vue'
import {useI18n} from 'vue-i18n'
import {quotaFollowAPI as api,type FollowRuntime,type MonitorCandidate,type Discovery,type ResetRecord,type RecordDetail,type FollowEvent} from '@/api/quota-follow'
import {useAppStore} from '@/stores/app'
import {extractApiErrorMessage} from '@/utils/apiError'
import BaseDialog from '@/components/common/BaseDialog.vue'
import SystemUpdatePanel from '@/features/system-update/SystemUpdatePanel.vue'

const {t,te}=useI18n(),app=useAppStore()
const label=(key:string)=>te(`quotaFollow.${key}`)?t(`quotaFollow.${key}`):key
const runtime=ref<FollowRuntime|null>(null),discovery=ref<Discovery|null>(null),revision=ref(1),saved=ref('')
const config=reactive({enabled:false,monitor_account_id:0})
const editable=()=>({enabled:config.enabled,monitor_account_id:config.monitor_account_id})
const dirty=computed(()=>JSON.stringify(editable())!==saved.value)
const loading=ref(false),saving=ref(false),error=ref(''),items=ref<ResetRecord[]>([]),total=ref(0),page=ref(1),selected=ref<RecordDetail|null>(null),reconciling=ref(false)
const events=ref<FollowEvent[]>([]),eventPage=ref(1),eventTotal=ref(0),eventDetail=ref<unknown>(null)
const preview=ref<Discovery|null>(null),previewing=ref(false),resetting=ref(false),resetRequestID=ref(''),acceptedEventID=ref<number|null>(null)
const reviewNote=ref('')
const filter=reactive({group_id:'',source:'',window:'',status:'',keyword:'',from:'',to:''})
const time=(value:string|null|undefined)=>value?new Date(value).toLocaleString():'—'
const groupName=(id:number|null)=>discovery.value?.group_counts.find(g=>g.group_id===id)?.group_name??`#${id??'—'}`
const candidates=computed<MonitorCandidate[]>(()=>{const list=discovery.value?.monitor_accounts??[];if(!config.monitor_account_id||list.some(a=>a.id===config.monitor_account_id))return list;return [...list,{id:config.monitor_account_id,name:`#${config.monitor_account_id}`,platform:'',type:'',parent_account_id:null,status:'',schedulable:false,deleted:false,group_count:0,problems:[label('monitorInvalid')],valid:false}]})
const paused=computed(()=>runtime.value?.paused_revision!==null&&runtime.value?.paused_revision!==undefined&&runtime.value?.paused_revision===revision.value)
const pausedHint=computed(()=>paused.value?runtime.value?.last_error||label('paused'):'')
async function loadConfig(){const value=await api.config();revision.value=value.revision;config.enabled=value.enabled;config.monitor_account_id=value.monitor_account_id;saved.value=JSON.stringify(editable())}
async function loadEvents(){const result=await api.events(eventPage.value);events.value=result.items;eventTotal.value=result.total}
async function inspectEvent(id:number){try{eventDetail.value=await api.event(id)}catch(e){app.showError(extractApiErrorMessage(e,label('failure')))}}
async function records(){const {from,to,group_id,...rest}=filter;const result=await api.records({...rest,group_id:group_id?Number(group_id):undefined,from:from?new Date(from).toISOString():undefined,to:to?new Date(to).toISOString():undefined,page:page.value,page_size:20});items.value=result.items;total.value=result.total}
async function refresh(){if(loading.value)return;loading.value=true;error.value='';try{
 const results=await Promise.allSettled([api.runtime(),api.discovery(),records(),loadEvents()])
 if(results[0].status==='fulfilled')runtime.value=results[0].value
 if(results[1].status==='fulfilled')discovery.value=results[1].value
 error.value=results.filter((r):r is PromiseRejectedResult=>r.status==='rejected').map(r=>extractApiErrorMessage(r.reason,label('failure'))).join('；')
 }catch(e){error.value=extractApiErrorMessage(e,label('failure'))}finally{loading.value=false}}
async function save(){saving.value=true;try{const value=await api.save(editable(),revision.value);revision.value=value.revision;config.enabled=value.enabled;config.monitor_account_id=value.monitor_account_id;saved.value=JSON.stringify(editable());app.showSuccess(label('saved'));await refresh()}catch(e){app.showError(extractApiErrorMessage(e,label('failure')))}finally{saving.value=false}}
async function reload(){try{await loadConfig();await refresh()}catch(e){app.showError(extractApiErrorMessage(e,label('failure')))}}
async function search(){try{await records()}catch(e){app.showError(extractApiErrorMessage(e,label('failure')))}}
async function detail(id:number){try{selected.value=await api.detail(id);reviewNote.value=''}catch(e){app.showError(extractApiErrorMessage(e,label('failure')))}}
async function eventTurn(step:number){eventPage.value+=step;try{await loadEvents()}catch(e){app.showError(extractApiErrorMessage(e,label('failure')))}}
async function openReset(){if(dirty.value||previewing.value)return;previewing.value=true;acceptedEventID.value=null;try{
 preview.value=await api.discovery()
 const key=`quota-follow-reset:${revision.value}`
 resetRequestID.value=sessionStorage.getItem(key)??crypto.randomUUID();sessionStorage.setItem(key,resetRequestID.value)
 }catch(e){app.showError(extractApiErrorMessage(e,label('failure')))}finally{previewing.value=false}}
function closeReset(){if(resetting.value)return;preview.value=null}
async function immediateReset(){if(!preview.value||dirty.value||resetting.value||acceptedEventID.value)return;resetting.value=true;try{
 const result=await api.resetNow({expected_revision:revision.value,request_id:resetRequestID.value});acceptedEventID.value=result.event_id
 sessionStorage.removeItem(`quota-follow-reset:${revision.value}`)
 app.showSuccess(label('resetSubmitted'));await refresh()
 }catch(e){app.showError(extractApiErrorMessage(e,label('failure')))}finally{resetting.value=false}}
async function reconcile(){if(!selected.value?.batch)return;reconciling.value=true;try{await api.reconcile(selected.value.batch.id);await detail(selected.value.record.id);app.showSuccess(label('reconciled'))}catch(e){app.showError(extractApiErrorMessage(e,label('failure')))}finally{reconciling.value=false}}
async function retryFailed(){if(!selected.value?.batch||!window.confirm(label('retryConfirm')))return;reconciling.value=true;try{const result=await api.retry(selected.value.batch.id);app.showSuccess(`${label('resetSubmitted')} #${result.event_id}`);await refresh()}catch(e){app.showError(extractApiErrorMessage(e,label('failure')))}finally{reconciling.value=false}}
async function acknowledge(){if(!selected.value?.batch||!reviewNote.value||!window.confirm(label('reviewConfirm')))return;reconciling.value=true;try{await api.acknowledge(selected.value.batch.id,reviewNote.value);await detail(selected.value.record.id);await refresh()}catch(e){app.showError(extractApiErrorMessage(e,label('failure')))}finally{reconciling.value=false}}
let poll:ReturnType<typeof setInterval>|undefined
onMounted(async()=>{try{await loadConfig()}catch(e){error.value=extractApiErrorMessage(e,label('failure'))}await refresh();poll=setInterval(()=>{if(!saving.value)void refresh()},10000)})
onUnmounted(()=>{if(poll)clearInterval(poll)})
</script>

<template>
 <main class="enhance-page enhance-shell space-y-6 p-4 pt-3 sm:p-6 sm:pt-4">
  <header><div class="mb-2 flex flex-wrap items-center gap-3"><p class="text-sm font-semibold text-primary-600">sub2api++</p><SystemUpdatePanel /></div><h1 class="text-2xl font-bold sm:text-3xl">{{label('title')}}</h1><p class="mt-3 text-gray-500">{{label('description')}}</p></header>
  <p class="rounded-xl border border-amber-200 bg-amber-50 p-4 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950/30 dark:text-amber-200" data-test="boundary">{{label('boundary')}}</p>
  <p v-if="error" role="alert" class="text-red-600">{{error}}</p>
  <form class="card space-y-5" @submit.prevent="save">
   <div class="flex flex-wrap items-center justify-between gap-3"><h2 class="text-xl font-semibold">{{label('settings')}}</h2><span class="text-sm text-gray-500">{{label('revision')}} {{revision}}</span></div>
   <label class="flex items-center gap-2"><input v-model="config.enabled" type="checkbox" :disabled="saving" data-test="enabled">{{label('enabled')}}</label>
   <label class="block space-y-2"><span class="label">{{label('monitor')}}</span><select v-model.number="config.monitor_account_id" :disabled="saving" class="input" data-test="monitor"><option :value="0">{{label('chooseMonitor')}}</option><option v-for="candidate in candidates" :key="candidate.id" :value="candidate.id" :disabled="!candidate.valid">{{candidate.name}} · #{{candidate.id}}{{candidate.valid?'':` · ${label('monitorInvalid')}`}}</option></select></label>
   <p class="text-sm text-gray-500">{{label('monitorHint')}}</p>
   <p v-if="discovery&&!candidates.length" class="text-sm text-gray-500">{{label('noCandidates')}}</p>
   <ul v-for="candidate in candidates.filter(c=>!c.valid)" :key="`problem-${candidate.id}`" class="list-disc pl-5 text-sm text-amber-700 dark:text-amber-300"><li>{{candidate.name}} · #{{candidate.id}}：{{candidate.problems.join('；')}}</li></ul>
   <p v-if="dirty" class="text-sm text-amber-700 dark:text-amber-300">{{label('saveFirst')}}</p>
   <div class="flex flex-wrap gap-3"><button class="btn btn-primary" type="submit" :disabled="saving||loading" data-test="save">{{label(saving?'saving':'save')}}</button><button class="btn btn-secondary" type="button" :disabled="loading" @click="refresh">{{label('refresh')}}</button><button class="btn btn-secondary" type="button" :disabled="loading||saving" @click="reload">{{label('reloadConfig')}}</button></div>
  </form>
  <section class="card space-y-4" data-test="status">
   <h2 class="text-xl font-semibold">{{label('status')}}</h2>
   <p v-if="!runtime?.monitor_account_id" class="text-gray-500">{{label('noMonitor')}}</p>
   <template v-else>
    <div class="grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-3">
     <p>{{label('account')}}：{{runtime.account_name||`#${runtime.monitor_account_id}`}} · #{{runtime.monitor_account_id}}</p>
     <p>{{label('planType')}}：{{runtime.plan_type||'—'}}</p>
     <p>{{label('upstreamAccount')}}：{{runtime.upstream_account_id||'—'}}</p>
     <p>{{label('baseline')}}：{{time(runtime.baseline_reset_at)}}</p>
     <p>{{label('usedPercent')}}：{{runtime.used_percent===null?'—':`${runtime.used_percent}%`}}</p>
     <p>{{label('lastCheck')}}：{{time(runtime.last_checked_at)}}</p>
     <p>{{label('nextCheck')}}：{{time(runtime.next_check_at)}}</p>
     <p>{{label('lastFollow')}}：{{runtime.last_event_id?`#${runtime.last_event_id} · ${time(runtime.last_reset_at)}`:'—'}}</p>
     <p v-if="runtime.awaiting_window">{{label('awaiting')}}</p>
    </div>
    <p v-if="!runtime.last_checked_at&&!runtime.last_error" class="text-sm text-gray-500">{{label('noRuntime')}}</p>
    <p v-if="runtime.last_error" role="status" class="text-sm text-red-600">{{runtime.last_error}}</p>
    <p v-if="paused" role="status" class="text-sm text-amber-700 dark:text-amber-300">{{label('pauseHint')}} {{pausedHint}}</p>
   </template>
  </section>
  <section class="card space-y-4">
   <div class="flex flex-wrap items-center justify-between gap-3"><h2 class="text-xl font-semibold">{{label('immediateReset')}}</h2><span class="text-sm text-gray-500">{{label('subscriptionCount')}}：{{discovery?.subscription_count??'—'}}</span></div>
   <p class="text-sm text-gray-500">{{label('recordsHint')}}</p>
   <button class="btn btn-secondary" type="button" :disabled="dirty||previewing" data-test="immediate-reset" @click="openReset">{{label('immediateReset')}}</button>
  </section>
  <section class="card space-y-4" data-test="records"><h2 class="text-xl font-semibold">{{label('records')}}</h2><p class="text-sm text-gray-500">{{label('recordsHint')}}</p>
   <form class="grid gap-3 sm:grid-cols-2 lg:grid-cols-4" @submit.prevent="page=1;search()"><label><span class="label">{{label('group')}}</span><select v-model="filter.group_id" class="input"><option value="">{{label('all')}}</option><option v-for="g in discovery?.group_counts??[]" :key="g.group_id" :value="String(g.group_id)">{{g.group_name}}</option></select></label><label><span class="label">{{label('source')}}</span><select v-model="filter.source" class="input"><option value="">{{label('all')}}</option><option value="enhance">{{label('enhance')}}</option><option value="sub2api">{{label('sub2api')}}</option></select></label><label><span class="label">{{label('window')}}</span><select v-model="filter.window" class="input"><option value="">{{label('all')}}</option><option value="daily">{{label('daily')}}</option><option value="weekly">{{label('weekly')}}</option><option value="monthly">{{label('monthly')}}</option></select></label><label><span class="label">{{label('result')}}</span><select v-model="filter.status" class="input"><option value="">{{label('all')}}</option><option v-for="status in ['pending','inflight','succeeded','failed','skipped','uncertain']" :key="status" :value="status">{{label(status)}}</option></select></label><label><span class="label">{{label('keyword')}}</span><input v-model="filter.keyword" class="input"></label><label><span class="label">{{label('from')}}</span><input v-model="filter.from" class="input" type="datetime-local"></label><label><span class="label">{{label('to')}}</span><input v-model="filter.to" class="input" type="datetime-local"></label><button class="btn btn-secondary self-end" type="submit">{{label('filter')}}</button></form>
   <div class="overflow-auto"><table class="table"><thead><tr><th>{{label('time')}}</th><th>{{label('group')}}</th><th>{{label('subscription')}}</th><th>{{label('user')}}</th><th>{{label('source')}}</th><th>{{label('trigger')}}</th><th>{{label('window')}}</th><th>{{label('usageChange')}}</th><th>{{label('result')}}</th><th>{{label('details')}}</th></tr></thead><tbody><tr v-for="record in items" :key="record.id"><td>{{time(record.occurred_at)}}</td><td>{{record.group_id?groupName(record.group_id):'—'}}</td><td>{{record.subscription_id??label('legacy')}}</td><td>{{record.username}} #{{record.user_id}}</td><td>{{label(record.source)}} · {{label(record.evidence_type)}}</td><td>{{label(record.source_detail)}}</td><td>{{label(record.window??'—')}}</td><td>{{record.before_usage_usd??'—'}} → {{record.after_usage_usd??'—'}}</td><td>{{label(record.status)}}</td><td><button class="text-primary-600 underline" @click="detail(record.id)">{{label('details')}}</button></td></tr></tbody></table><p v-if="!items.length" class="py-8 text-center text-gray-500">{{label('empty')}}</p></div>
   <footer class="flex items-center justify-end gap-3"><span>{{total}} · {{page}}</span><button class="btn btn-secondary" :disabled="page<=1" :aria-label="label('previous')" @click="page--;search()">←</button><button class="btn btn-secondary" :disabled="page*20>=total" :aria-label="label('next')" @click="page++;search()">→</button></footer>
  </section>
  <section class="card space-y-4"><h2 class="text-xl font-semibold">{{label('events')}}</h2><div class="overflow-auto"><table class="table"><thead><tr><th>ID</th><th>{{label('trigger')}}</th><th>{{label('time')}}</th><th>{{label('subscriptionCount')}}</th><th>{{label('result')}}</th><th>{{label('details')}}</th></tr></thead><tbody><tr v-for="event in events" :key="event.id"><td>{{event.id}}</td><td>{{label(event.trigger_type)}}</td><td>{{time(event.reset_at)}}</td><td>{{event.subscription_count}}</td><td>{{label(event.status)}}</td><td><button class="text-primary-600 underline" @click="inspectEvent(event.id)">{{label('details')}}</button></td></tr></tbody></table><p v-if="!events.length" class="py-8 text-center text-gray-500">{{label('empty')}}</p></div><footer class="flex items-center justify-end gap-3"><span>{{eventTotal}} · {{eventPage}}</span><button class="btn btn-secondary" :disabled="eventPage<=1" :aria-label="label('previous')" @click="eventTurn(-1)">←</button><button class="btn btn-secondary" :disabled="eventPage*20>=eventTotal" :aria-label="label('next')" @click="eventTurn(1)">→</button></footer></section>
  <BaseDialog :show="!!preview" :title="label('immediateResetConfirmTitle')" @close="closeReset"><template v-if="preview"><p class="text-sm text-red-700 dark:text-red-200">{{label('immediateResetConfirm')}}</p><dl class="my-4 space-y-2 text-sm"><div><dt>{{label('subscriptionCount')}}</dt><dd data-test="preview-count">{{preview.subscription_count}}</dd></div><div><dt>{{label('groups')}}</dt><dd><span v-for="g in preview.group_counts" :key="g.group_id" class="mr-3">{{g.group_name}} · {{g.count}}</span><span v-if="!preview.group_counts.length">{{label('noSubscriptions')}}</span></dd></div><div><dt>{{label('window')}}</dt><dd>{{[label('daily'),label('weekly'),label('monthly')].join('、')}}</dd></div></dl><p v-if="acceptedEventID" role="status">{{label('resetSubmitted')}} #{{acceptedEventID}}</p></template><template #footer><div class="flex gap-3"><button class="btn btn-secondary" :disabled="resetting" @click="closeReset">{{label('close')}}</button><button v-if="!acceptedEventID" class="btn btn-primary" :disabled="resetting||!preview?.subscription_count||dirty" data-test="confirm-immediate-reset" @click="immediateReset">{{label(resetting?'resetting':'confirm')}}</button></div></template></BaseDialog>
  <BaseDialog :show="!!selected" :title="label('recordDetail')" @close="selected=null"><template v-if="selected"><p v-if="selected.record.error_message" class="text-red-600">{{selected.record.error_message}}</p><div v-if="selected.batch" class="my-4 space-y-3"><p>{{label('batch')}} #{{selected.batch.id}} · {{label(selected.batch.status)}}</p><p class="text-sm text-gray-500">{{label('reconcileHint')}}</p><button class="btn btn-secondary" :disabled="reconciling||['pending','inflight'].includes(selected.batch.status)" @click="reconcile">{{label('reconcile')}}</button><button v-if="['failed','partial'].includes(selected.batch.status)" class="btn btn-secondary" :disabled="reconciling" @click="retryFailed">{{label('retryFailed')}}</button><div v-if="selected.batch.status==='uncertain'" class="space-y-2"><label><span class="label">{{label('reviewNote')}}</span><textarea v-model="reviewNote" class="input" rows="3" /></label><button class="btn btn-secondary" :disabled="reconciling||!reviewNote" @click="acknowledge">{{label('acknowledge')}}</button></div></div><details open><summary>{{label('raw')}}</summary><pre class="mt-3 overflow-auto whitespace-pre-wrap break-all text-xs">{{JSON.stringify(selected,null,2)}}</pre></details></template></BaseDialog>
  <BaseDialog :show="!!eventDetail" :title="label('eventDetail')" @close="eventDetail=null"><pre class="overflow-auto whitespace-pre-wrap break-all text-xs">{{JSON.stringify(eventDetail,null,2)}}</pre></BaseDialog>
 </main>
</template>
