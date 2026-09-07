<script setup lang="ts">
import {ref,onMounted} from 'vue'
import {useAuditLabels} from './labels'
import {apiClient} from '@/api/client'
import BaseDialog from '@/components/common/BaseDialog.vue'
import {useAppStore} from '@/stores/app'
import {extractApiErrorMessage} from '@/utils/apiError'
interface Capture {id:number;capture_key:string;protocol:string;body_bytes:number;body_sha256:string;snapshot_status:string;eligibility_status:string;processing_status:string;forwarding_status:string;created_at:string;last_error_message:string;raw_body?:string;identity?:unknown;metadata?:unknown;forwarding_observations?:unknown}
const items=ref<Capture[]>([]),total=ref(0),page=ref(1),selected=ref<Capture|null>(null),error=ref(''),app=useAppStore()
const label=useAuditLabels()
const manualKeyID=ref<number|undefined>()
const base='/admin/third-party-prompt-audit/captures'
async function load(){try{const {data}=await apiClient.get(base,{params:{page:page.value,page_size:20}});items.value=data.items;total.value=data.total;error.value=''}catch(e){error.value=extractApiErrorMessage(e,label('error'))}}
async function detail(id:number){try{selected.value=(await apiClient.get<Capture>(`${base}/${id}`)).data}catch(e){app.showError(extractApiErrorMessage(e,label('error')))}}
async function reprocess(id:number){try{await apiClient.post(`${base}/${id}/reprocess`,{api_key_id:manualKeyID.value});await detail(id);await load();app.showSuccess(label('captureResumed'))}catch(e){app.showError(extractApiErrorMessage(e,label('error')))}}
onMounted(load)
</script>
<template><section class="card"><header class="mb-4 flex justify-between"><div><h2 class="text-xl font-semibold">{{label('captures')}}</h2><p class="mt-2 text-sm text-gray-500">{{label('captureHint')}}</p></div><button class="btn btn-secondary" @click="load">{{label('refresh')}}</button></header><p v-if="error" class="text-red-600">{{error}}</p><div class="overflow-auto"><table class="table"><thead><tr><th>ID</th><th>{{label('protocol')}}</th><th>{{label('captureBytes')}}</th><th>{{label('captureIntegrity')}}</th><th>{{label('captureEligibility')}}</th><th>{{label('captureProcessing')}}</th><th>{{label('forwardingStock')}}</th></tr></thead><tbody><tr v-for="item in items" :key="item.id"><td><button class="text-primary-600 underline" @click="detail(item.id)">{{item.id}}</button></td><td>{{item.protocol}}</td><td>{{item.body_bytes}}</td><td>{{label(item.snapshot_status)}}</td><td>{{label(item.eligibility_status)}}</td><td>{{label(item.processing_status)}}</td><td>{{label(item.forwarding_status)}}</td></tr></tbody></table></div><footer class="mt-4 flex items-center justify-end gap-3"><span>{{total}} · {{page}}</span><button class="btn btn-secondary" :disabled="page<=1" @click="page--;load()">←</button><button class="btn btn-secondary" :disabled="page*20>=total" @click="page++;load()">→</button></footer><BaseDialog :show="!!selected" :title="label('captureDetail')" @close="selected=null"><template v-if="selected"><div class="mb-4 flex gap-3"><a class="btn btn-primary" :href="`/enhance/api/v1${base}/${selected.id}/raw`">{{label('downloadCapture')}}</a><input v-model.number="manualKeyID" class="input max-w-xs" type="number" min="1" :placeholder="label('manualKeyHint')"/><button class="btn btn-secondary" @click="reprocess(selected.id)">{{label('reprocessCapture')}}</button></div><p class="text-red-600">{{selected.last_error_message}}</p><pre class="overflow-auto whitespace-pre-wrap break-all text-xs">{{JSON.stringify({...selected,raw_body:undefined},null,2)}}</pre></template></BaseDialog></section></template>
