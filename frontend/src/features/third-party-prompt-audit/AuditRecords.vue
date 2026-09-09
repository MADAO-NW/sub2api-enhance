<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Pagination from '@/components/common/Pagination.vue'
import { thirdPartyPromptAuditAPI as api, type AuditCapture, type AuditFilter, type AuditJob, type ReauditRequest, type ReauditResult } from '@/api/admin/third-party-prompt-audit'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import AuditDetail from './AuditDetail.vue'
import { formatMS, formatTime, toLocalInput } from './viewModel'
import { useAuditLabels } from './labels'
import NodeDecision from './NodeDecision.vue'
import CaptureBody from './CaptureBody.vue'
import LatestUserContent from './LatestUserContent.vue'

const props = withDefaults(defineProps<{ initialFilter?: AuditFilter; refreshKey?: number }>(), { refreshKey: 0 })
const emit = defineEmits<{ (event: 'reaudit-created', ids: number[]): void }>()
const label = useAuditLabels(), app = useAppStore()
const filters = reactive<AuditFilter>({}), applied = ref<AuditFilter>({})
const from = ref(''), to = ref('')
const rows = ref<AuditJob[]>([])
const page = ref(1), pageSize = ref(20), total = ref(0), selected = ref<number[]>([])
const loading = ref(false), error = ref(''), detailID = ref<number | null>(null)
const advancedOpen = ref(false)
const capture = ref<AuditCapture | null>(null), captureLoadingID = ref<number | null>(null)
const previewRequest = ref<ReauditRequest | null>(null), preview = ref<ReauditResult | null>(null)
const reauditReuseMode = ref<'allow' | 'force'>('allow')
const previewPage = ref(1), previewLoading = ref(false), submitted = ref(false)
const previewItems = computed(() => preview.value?.items.slice((previewPage.value - 1) * 20, previewPage.value * 20) ?? [])
let generation = 0
async function load() {
  const current = ++generation
  loading.value = true
  rows.value = []
  total.value = 0
  selected.value = []
  error.value = ''
  try {
    const result = await api.jobs(applied.value, page.value, pageSize.value)
    if (current === generation) { rows.value = result.items; total.value = result.total }
    if (current === generation) selected.value = []
  } catch (err) { if (current === generation) error.value = extractApiErrorMessage(err, label('error')) }
  finally { if (current === generation) loading.value = false }
}
function apply() {
  const next = Object.fromEntries(Object.entries(filters).filter(([, value]) => value !== '' && value != null)) as AuditFilter
  if (from.value) next.from = new Date(from.value).toISOString()
  if (to.value) next.to = new Date(to.value).toISOString()
  if (next.from && next.to && new Date(next.from) >= new Date(next.to)) { error.value = label('invalidRange'); return }
  applied.value = next
  page.value = 1
  void load()
}
async function openPreview(useSelection: boolean) {
  reauditReuseMode.value = 'allow'
  previewRequest.value = { reuse_mode: reauditReuseMode.value, filter: JSON.parse(JSON.stringify({ ...applied.value, ...(useSelection ? { ids: selected.value } : {}) })) as AuditFilter }
  preview.value = null
  submitted.value = false
  previewPage.value = 1
  previewLoading.value = true
  try { preview.value = await api.preview(previewRequest.value) }
  catch (err) { app.showError(extractApiErrorMessage(err, label('error'))); previewRequest.value = null }
  finally { previewLoading.value = false }
}
async function submit() {
  if (!previewRequest.value || !preview.value?.ready) return
  previewLoading.value = true
  try {
	preview.value = await api.reaudit({ ...previewRequest.value, reuse_mode: reauditReuseMode.value })
    submitted.value = true
    previewPage.value = 1
    const failures = preview.value.items.filter(item => item.status === 'failed').length
    const created = preview.value.items.filter(item => item.status === 'requeued').length
    const activeIDs = preview.value.items.flatMap(item => item.job_id && ['requeued', 'already_running'].includes(item.status) ? [item.job_id] : [])
    if (activeIDs.length) emit('reaudit-created', activeIDs)
    if (failures) app.showError(`${label('createdJobs')}: ${created} · ${label('failed')}: ${failures}`)
    else app.showSuccess(`${label('createdJobs')}: ${created}`)
    await load()
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { previewLoading.value = false }
}
function closePreview() {
  if (previewLoading.value) return
  previewRequest.value = null
  preview.value = null
  submitted.value = false
  reauditReuseMode.value = 'allow'
}
async function showCapture(id: number) {
  captureLoadingID.value = id
  try { capture.value = await api.capture(id) }
  catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { captureLoadingID.value = null }
}
function changePage(value: number) { page.value = value; void load() }
function changeSize(value: number) { pageSize.value = value; page.value = 1; void load() }
watch(() => props.initialFilter, value => {
  for (const key of Object.keys(filters)) delete filters[key as keyof AuditFilter]
  Object.assign(filters, value ?? {})
  applied.value = { ...value }
  delete filters.from
  delete filters.to
  from.value = value?.from ? toLocalInput(new Date(value.from)) : ''
  to.value = value?.to ? toLocalInput(new Date(value.to)) : ''
  page.value = 1
  void load()
}, { immediate: true })
watch(() => props.refreshKey, () => { void load() })
</script>

<template>
  <div class="audit-records space-y-5">
    <form class="card space-y-4 p-5" @submit.prevent="apply">
      <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <label class="space-y-1 text-sm"><span>{{ label('keyword') }}</span><input v-model="filters.keyword" class="input" /></label>
        <label class="space-y-1 text-sm"><span>{{ label('requestID') }}</span><input v-model="filters.request_id" class="input" /></label>
        <label class="space-y-1 text-sm"><span>{{ label('decision') }}</span><select v-model="filters.decision" class="input"><option value="">{{ label('all') }}</option><option v-for="item in ['pass', 'review', 'block']" :key="item" :value="item">{{ label(item) }}</option></select></label>
        <label class="space-y-1 text-sm"><span>{{ label('status') }}</span><select v-model="filters.status" class="input"><option value="">{{ label('all') }}</option><option v-for="item in ['queued', 'processing', 'retry', 'done', 'failed', 'skipped']" :key="item" :value="item">{{ label(item) }}</option></select></label>
      </div>
      <div><button type="button" class="inline-flex w-fit items-center gap-1 text-sm" :aria-expanded="advancedOpen" data-test="advanced-filter-toggle" @click="advancedOpen = !advancedOpen"><span aria-hidden="true">{{ advancedOpen ? '▾' : '▸' }}</span><span>{{ label('from') }} / {{ label('userID') }} / {{ label('modelID') }}</span></button><div v-show="advancedOpen" class="mt-3 grid gap-3 sm:grid-cols-2 xl:grid-cols-4" data-test="advanced-filters">
        <label class="space-y-1 text-sm"><span>{{ label('from') }}</span><input v-model="from" type="datetime-local" class="input" /></label><label class="space-y-1 text-sm"><span>{{ label('to') }}</span><input v-model="to" type="datetime-local" class="input" /></label>
        <label v-for="field in [{ key: 'user_id', name: 'userID' }, { key: 'api_key_id', name: 'apiKeyID' }, { key: 'group_id', name: 'groupID' }] as const" :key="field.key" class="space-y-1 text-sm"><span>{{ label(field.name) }}</span><input :value="filters[field.key]" type="number" min="1" step="1" class="input" @input="filters[field.key] = ($event.target as HTMLInputElement).value === '' ? undefined : ($event.target as HTMLInputElement).valueAsNumber" /></label>
        <label class="space-y-1 text-sm"><span>{{ label('modelID') }}</span><input v-model="filters.model_id" class="input" /></label><label class="space-y-1 text-sm"><span>{{ label('platform') }}</span><input v-model="filters.platform" class="input" /></label>
        <label class="space-y-1 text-sm"><span>{{ label('mode') }}</span><select v-model="filters.mode" class="input"><option value="">{{ label('all') }}</option><option value="async">{{ label('async') }}</option><option value="blocking">{{ label('blocking') }}</option></select></label>
        <label class="space-y-1 text-sm"><span>{{ label('source') }}</span><select v-model="filters.run_kind" class="input"><option value="">{{ label('all') }}</option><option value="request">{{ label('request') }}</option><option value="reaudit">{{ label('reaudit') }}</option></select></label>
      </div></div>
      <div class="flex flex-wrap gap-3"><button class="btn btn-primary" :disabled="loading">{{ label(loading ? 'loading' : 'apply') }}</button><button type="button" class="btn btn-secondary" :disabled="loading || !selected.length" @click="openPreview(true)">{{ label('reauditSelected') }} ({{ selected.length }})</button><button type="button" class="btn btn-secondary" :disabled="loading || !total" @click="openPreview(false)">{{ label('reauditFilter') }} ({{ total }})</button></div>
      <p v-if="applied.from || applied.to" class="text-xs text-gray-500">{{ formatTime(applied.from) }} — {{ formatTime(applied.to) }}</p>
    </form>
    <p v-if="error" role="alert" class="text-red-600 dark:text-red-400">{{ error }}</p>
    <div class="card overflow-hidden"><div class="overflow-x-auto"><table class="w-full text-left text-sm"><thead class="bg-gray-50 text-gray-500 dark:bg-dark-900 dark:text-dark-400"><tr><th class="p-3"><input type="checkbox" :aria-label="label('all')" :checked="rows.length > 0 && selected.length === rows.length" @change="selected = ($event.target as HTMLInputElement).checked ? rows.map(row => row.id) : []" /></th><th class="p-3">{{ label('created') }}</th><th class="p-3">{{ label('captureID') }}</th><th class="p-3">{{ label('identity') }}</th><th class="p-3">{{ label('platform') }} / {{ label('model') }}</th><th class="p-3">{{ label('status') }} / {{ label('decision') }}</th><th class="p-3">{{ label('auditDuration') }}</th><th class="p-3">{{ label('inputStatus') }}</th><th class="p-3">{{ label('detail') }}</th></tr></thead><tbody>
      <tr v-for="row in rows" :key="row.id" class="border-t border-gray-100 align-top dark:border-dark-700"><td class="p-3"><input v-model="selected" type="checkbox" :value="row.id" :aria-label="`#${row.id}`" /></td><td class="whitespace-nowrap p-3"><p>{{ formatTime(row.created_at) }}</p><p class="mt-1 text-xs text-gray-500">#{{ row.id }} · {{ label(row.current_run_kind || 'request') }} · {{ label('auditRound') }} {{ row.outcome?.audit_round ?? row.audit_round }}</p></td><td class="p-3"><button v-if="row.capture_id" class="text-primary-600 underline" :disabled="captureLoadingID === row.capture_id" @click="showCapture(row.capture_id)">#{{ row.capture_id }}</button><span v-else>—</span></td><td class="min-w-48 max-w-80 break-words p-3"><p>(#{{ row.user_id }})<span v-if="row.display_username || row.identity.username" class="ml-1">{{ row.display_username || row.identity.username }}</span></p><p>{{ row.display_email || row.identity.user_email }}</p><p class="text-xs text-gray-500">{{ row.identity.api_key_name }} · {{ row.identity.group_name }}</p></td><td class="max-w-72 break-words p-3"><p>{{ row.platform }} · {{ row.requested_model }}</p><p class="mt-1 text-xs text-gray-500">{{ row.protocol }} · {{ label(row.execution_mode) }}</p><p class="mt-1 text-xs">{{ label(row.gateway_result) }}</p></td><td class="min-w-40 p-3"><p>{{ label(row.status) }}</p><strong v-if="row.outcome" :class="row.outcome.decision === 'block' ? 'text-red-600 dark:text-red-400' : row.outcome.decision === 'review' ? 'text-amber-700 dark:text-amber-400' : ''">{{ label(row.outcome.decision) }}</strong><p v-if="row.original_decision && row.original_decision !== row.outcome?.decision" class="text-xs text-gray-500">{{ label('originalDecision') }} {{ label(row.original_decision) }} → {{ label('latestDecision') }} {{ label(row.outcome?.decision ?? row.status) }}</p><p v-if="row.outcome?.partial_failure" class="text-xs text-amber-700">{{ label('partial_failure') }}</p><div v-for="model in row.outcome?.models ?? []" :key="model.model_id" class="mt-2"><strong class="text-xs">{{ model.model_name }}</strong><NodeDecision :model="model" :config="row.outcome?.decision_config" /></div><p v-if="row.outcome" class="mt-1 text-xs">{{ label('segmentReuseRate') }} <template v-if="row.outcome.segment_reuse?.rate != null">{{ Math.round(row.outcome.segment_reuse.rate * 100) }}% <span class="ml-2">{{ row.outcome.segment_reuse.reused }} / {{ row.outcome.segment_reuse.total }}</span></template><template v-else>—</template></p><p class="mt-1 text-xs"><LatestUserContent source="job" :id="row.id" variant="popover" /></p><p class="mt-1 text-xs">{{ label('attempts') }} {{ row.attempts }} / {{ row.max_attempts }}</p><p v-if="row.last_error_code" class="mt-1 break-all text-xs text-red-600 dark:text-red-400">{{ label(row.failure_stage) }} · {{ row.last_error_code }}</p><p v-if="row.status === 'retry'" class="mt-1 text-xs">{{ label('nextRetry') }} {{ formatTime(row.next_attempt_at) }}</p></td><td class="whitespace-nowrap p-3">{{ formatMS(row.outcome?.duration_ms ?? row.duration_ms) }}</td><td class="p-3 text-xs">{{ label(row.snapshot_status) }}</td><td class="p-3"><button class="btn btn-secondary btn-sm whitespace-nowrap" @click="detailID = row.id">{{ label('detail') }}</button></td></tr>
      <tr v-if="!loading && !rows.length"><td colspan="9" class="p-10 text-center text-gray-500">{{ label('empty') }}</td></tr>
    </tbody></table></div><Pagination :page="page" :page-size="pageSize" :page-size-options="[20, 50, 100]" :total="total" @update:page="changePage" @update:page-size="changeSize" /></div>
    <AuditDetail :id="detailID" :refresh-key="refreshKey" @close="detailID = null" @changed="load" @reaudit-created="emit('reaudit-created', $event)" />
    <BaseDialog :show="!!capture" :title="label('captureDetail')" :close-on-click-outside="true" @close="capture = null"><CaptureBody v-if="capture" :capture="capture" /></BaseDialog>
    <BaseDialog :show="previewRequest !== null" :title="label('preview')" width="wide" :show-close-button="!previewLoading" :close-on-escape="!previewLoading" :close-on-click-outside="!previewLoading" @close="closePreview">
      <div class="space-y-4"><p class="text-sm text-gray-500 dark:text-dark-400">{{ label('previewHint') }}</p><fieldset class="space-y-2"><legend class="text-sm font-medium">{{ label('reauditReuseMode') }}</legend><label class="flex items-start gap-2 text-sm"><input v-model="reauditReuseMode" type="radio" value="allow" /><span>{{ label('reuseAllow') }}<span class="block text-xs text-gray-500">{{ label('reuseAllowHint') }}</span></span></label><label class="flex items-start gap-2 text-sm"><input v-model="reauditReuseMode" type="radio" value="force" /><span>{{ label('reuseForce') }}<span class="block text-xs text-gray-500">{{ label('reuseForceHint') }}</span></span></label></fieldset><p v-if="previewLoading" role="status">{{ label('loading') }}</p><template v-if="preview"><p>{{ label('matched') }}: {{ preview.matched }} · {{ label('ready') }}: {{ preview.ready }}</p><div class="max-h-96 overflow-auto"><div v-for="item in previewItems" :key="item.job_id" class="border-t border-gray-100 py-3 text-sm dark:border-dark-700"><span>Job #{{ item.job_id }} → {{ label(item.status === 'requeued' ? 'submitted' : item.status) }}</span><p class="text-gray-500 dark:text-dark-400">{{ item.reason }}</p></div></div><Pagination :page="previewPage" :page-size="20" :total="preview.items.length" :show-page-size-selector="false" @update:page="previewPage = $event" /><button v-if="!submitted" class="btn btn-primary" :disabled="previewLoading || !preview.ready" @click="submit">{{ label('submitReaudit') }}</button><p v-else role="status"><span v-for="state in ['requeued', 'already_running', 'skipped', 'failed']" :key="state" class="mr-4">{{ label(state === 'requeued' ? 'createdJobs' : state) }}: {{ preview.items.filter(item => item.status === state).length }}</span></p></template></div>
    </BaseDialog>
  </div>
</template>
