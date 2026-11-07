<script setup lang="ts">
import { computed, onUnmounted, reactive, ref, watch } from 'vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Pagination from '@/components/common/Pagination.vue'
import {
  thirdPartyPromptAuditAPI as api, type AuditFilter, type AuditJob, type JobPrompt, type ReauditRequest, type ReauditResult,
} from '@/api/admin/third-party-prompt-audit'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import AuditDetail from './AuditDetail.vue'
import { formatMS, formatTime, toLocalInput } from './viewModel'
import { useAuditLabels } from './labels'

const props = withDefaults(defineProps<{ initialFilter?: AuditFilter; refreshKey?: number; active?: boolean }>(), { refreshKey: 0, active: true })
const emit = defineEmits<{ (event: 'reaudit-created', ids: number[]): void }>()
const label = useAuditLabels(), app = useAppStore()
const filters = reactive<AuditFilter>({}), applied = ref<AuditFilter>({})
const from = ref(''), to = ref('')
const rows = ref<AuditJob[]>([])
const page = ref(1), pageSize = ref(20), total = ref(0), selected = ref<number[]>([])
const loading = ref(false), error = ref(''), detailID = ref<number | null>(null)
const previewRequest = ref<ReauditRequest | null>(null), preview = ref<ReauditResult | null>(null)
const draftReuse = ref<'allow' | 'force'>('allow')
const previewPage = ref(1), previewLoading = ref(false), submitted = ref(false)
const previewItems = computed(() => preview.value?.items.slice((previewPage.value - 1) * 20, previewPage.value * 20) ?? [])
const states = ['queued', 'processing', 'retry', 'done', 'failed', 'skipped']
const promptJobID = ref<number | null>(null)
const promptTargets = ref<JobPrompt[]>([])
const promptLoading = ref(false)
// 正文按任务缓存，重复打开同一行不再请求。
const promptCache = new Map<number, JobPrompt[]>()
// 快捷筛选：裁决与失败状态一键过滤。
const quickFilters = [
  { value: 'decision:block', label: 'block' },
  { value: 'decision:review', label: 'review' },
  { value: 'status:failed', label: 'failed' },
  { value: 'run_kind:reaudit', label: 'reaudit' },
]
function applyQuick(value: string) {
  const [field, target] = value.split(':')
  const next: AuditFilter = { ...(applied.value ?? {}) }
  if (field === 'decision') { next.decision = target; delete next.status }
  else if (field === 'status') { next.status = target; delete next.decision }
  else if (field === 'run_kind') next.run_kind = target
  applied.value = next
  page.value = 1
  void load()
}
let generation = 0
// 自适应轮询：有待办变化时 5 秒一次，连续两次无变化后退避到 15 秒；弹窗或加载中跳过。
let autoDelay = 5000
let lastSignature = ''
let autoTimer = 0
function scheduleRefresh() {
  autoTimer = window.setTimeout(async () => {
    if (props.active && document.visibilityState === 'visible' && !loading.value && detailID.value === null && promptJobID.value === null && previewRequest.value === null) {
      const before = lastSignature
      await load(true)
      const signature = `${total.value}:${rows.value.map(row => `${row.id}-${row.status}-${row.decision ?? ''}`).join(',')}`
      lastSignature = signature
      autoDelay = signature !== before && before !== '' ? 5000 : Math.min(autoDelay === 5000 ? 15000 : autoDelay, 15000)
    }
    scheduleRefresh()
  }, autoDelay)
}
scheduleRefresh()

async function load(silent = false) {
  const current = ++generation
  if (!silent) {
    loading.value = true
    rows.value = []
    total.value = 0
    selected.value = []
    error.value = ''
  }
  try {
    const result = await api.jobs(applied.value, page.value, pageSize.value)
    if (current === generation) { rows.value = result.items; total.value = result.total }
  } catch (err) {
    if (current === generation && !silent) error.value = extractApiErrorMessage(err, label('error'))
  } finally {
    if (current === generation && !silent) loading.value = false
  }
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
function openPreview(useSelection: boolean) {
  draftReuse.value = 'allow'
  previewRequest.value = { reuse_mode: draftReuse.value, filter: JSON.parse(JSON.stringify({ ...applied.value, ...(useSelection ? { ids: selected.value } : {}) })) as AuditFilter }
  preview.value = null
  submitted.value = false
  previewPage.value = 1
  void runPreview()
}
async function runPreview() {
  if (!previewRequest.value) return
  previewLoading.value = true
  try { preview.value = await api.preview({ ...previewRequest.value, reuse_mode: draftReuse.value }) }
  catch (err) { app.showError(extractApiErrorMessage(err, label('error'))); previewRequest.value = null }
  finally { previewLoading.value = false }
}
async function submit() {
  if (!previewRequest.value || !preview.value?.ready) return
  previewLoading.value = true
  try {
    preview.value = await api.reaudit({ ...previewRequest.value, reuse_mode: draftReuse.value })
    submitted.value = true
    previewPage.value = 1
    const failures = preview.value.items.filter(item => item.status === 'failed').length
    const requeued = preview.value.items.filter(item => item.status === 'requeued').length
    const ids = preview.value.items.flatMap(item => ['requeued', 'already_running'].includes(item.status) ? [item.job_id] : [])
    if (ids.length) emit('reaudit-created', ids)
    if (failures) app.showError(`${label('createdJobs')}: ${requeued} · ${label('failed')}: ${failures}`)
    else app.showSuccess(`${label('createdJobs')}: ${requeued}`)
    await load()
  } catch (err) {
    app.showError(extractApiErrorMessage(err, label('error')))
  } finally { previewLoading.value = false }
}
function closePreview() {
  if (previewLoading.value) return
  previewRequest.value = null
  preview.value = null
  submitted.value = false
  draftReuse.value = 'allow'
}
const promptTrusting = ref(false)
const exporting = ref(false)
async function togglePromptTrust(target: JobPrompt) {
  promptTrusting.value = true
  try {
    if (target.trusted) await api.untrust(target.hash)
    else await api.trust(target.hash, '任务列表手工信任')
    target.trusted = !target.trusted
    promptCache.delete(promptJobID.value ?? 0)
    app.showSuccess(label('saved'))
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { promptTrusting.value = false }
}
async function copyPrompt(target: JobPrompt) {
  try { await navigator.clipboard.writeText(target.text ?? ''); app.showSuccess(label('copied')) }
  catch { app.showError(label('error')) }
}
function downloadPrompt(target: JobPrompt, jobID: number | null) {
  const blob = new Blob([target.text ?? ''], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = `prompt-${jobID ?? 0}-${target.order}-${target.kind}.txt`
  link.click()
  URL.revokeObjectURL(url)
}
// 导出全部：按 1000 行一页循环拉取并合并，去掉后续页的表头，合并后统一补回一个 BOM。
async function exportCSV() {
  exporting.value = true
  try {
    const pageSize = 1000
    let offset = 0
    let merged = ''
    for (let page = 0; page < 20; page++) {
      const { blob, hasMore } = await api.exportJobs(applied.value, pageSize, offset)
      // Blob.text() 会吃掉页首 BOM，逐页剥掉后由最终文件补一个，避免表头重复。
      const text = (await blob.text()).replace(/^\ufeff/, '')
      merged += page === 0 ? text : text.replace(/^[^\n]*\n/, '')
      if (!hasMore) break
      offset += pageSize
    }
    const url = URL.createObjectURL(new Blob(['\ufeff' + merged], { type: 'text/csv;charset=utf-8' }))
    const link = document.createElement('a')
    link.href = url
    link.download = `third-party-prompt-audit-${new Date().toISOString().slice(0, 10)}.csv`
    link.click()
    URL.revokeObjectURL(url)
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { exporting.value = false }
}

async function openPrompts(id: number) {
  promptJobID.value = id
  const cached = promptCache.get(id)
  if (cached) { promptTargets.value = cached; return }
  promptTargets.value = []
  promptLoading.value = true
  try {
    promptTargets.value = (await api.jobPrompts(id)).targets
    promptCache.set(id, promptTargets.value)
  } catch (err) {
    app.showError(extractApiErrorMessage(err, label('error')))
    promptJobID.value = null
  } finally {
    promptLoading.value = false
  }
}
function resetFilters() {
  for (const key of Object.keys(filters)) delete filters[key as keyof AuditFilter]
  from.value = ''
  to.value = ''
  applied.value = {}
  page.value = 1
  selected.value = []
  void load()
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
onUnmounted(() => {
  window.clearTimeout(autoTimer)
  generation++
})
</script>

<template>
  <div class="audit-records space-y-5">
    <form class="card space-y-4 p-5" @submit.prevent="apply">
      <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <label class="space-y-1 text-sm"><span>{{ label('requestID') }}</span><input v-model="filters.request_id" class="input" /></label>
        <label class="space-y-1 text-sm"><span>{{ label('decision') }}</span><select v-model="filters.decision" class="input"><option value="">{{ label('all') }}</option><option v-for="item in ['pass', 'review', 'block']" :key="item" :value="item">{{ label(item) }}</option></select></label>
        <label class="space-y-1 text-sm"><span>{{ label('status') }}</span><select v-model="filters.status" class="input"><option value="">{{ label('all') }}</option><option v-for="item in states" :key="item" :value="item">{{ label(item) }}</option></select></label>
        <label class="space-y-1 text-sm"><span>{{ label('mode') }}</span><select v-model="filters.mode" class="input"><option value="">{{ label('all') }}</option><option value="async">{{ label('async') }}</option><option value="blocking">{{ label('blocking') }}</option></select></label>
        <label class="space-y-1 text-sm"><span>{{ label('from') }}</span><input v-model="from" type="datetime-local" class="input" /></label>
        <label class="space-y-1 text-sm"><span>{{ label('to') }}</span><input v-model="to" type="datetime-local" class="input" /></label>
        <label class="space-y-1 text-sm"><span>{{ label('userID') }}</span><input v-model.number="filters.user_id" type="number" min="1" class="input" /></label>
        <label class="space-y-1 text-sm"><span>{{ label('source') }}</span><select v-model="filters.run_kind" class="input"><option value="">{{ label('all') }}</option><option value="request">{{ label('request') }}</option><option value="reaudit">{{ label('reaudit') }}</option></select></label>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        <span class="text-xs text-gray-500 dark:text-dark-400">{{ label('quickFilter') }}</span>
        <button v-for="quick in quickFilters" :key="quick.value" type="button" class="btn btn-secondary btn-sm" data-test="quick-filter" @click="applyQuick(quick.value)">{{ label(quick.label) }}</button>
      </div>
      <div class="flex flex-wrap gap-3">
        <button class="btn btn-primary" :disabled="loading">{{ label(loading ? 'loading' : 'apply') }}</button>
        <button type="button" class="btn btn-secondary" :disabled="loading" data-test="reset-filters" @click="resetFilters">{{ label('resetFilters') }}</button>
        <button type="button" class="btn btn-secondary" :disabled="exporting" data-test="export-jobs" @click="exportCSV">{{ label(exporting ? 'loading' : 'exportCSV') }}</button>
        <button type="button" class="btn btn-secondary" :disabled="loading || !selected.length" @click="openPreview(true)">{{ label('reauditSelected') }} ({{ selected.length }})</button>
        <button type="button" class="btn btn-secondary" :disabled="loading || !total" @click="openPreview(false)">{{ label('reauditFilter') }} ({{ total }})</button>
      </div>
    </form>
    <p v-if="error" role="alert" class="text-red-600 dark:text-red-400">{{ error }}</p>
    <div class="card overflow-hidden">
      <div class="overflow-x-auto">
        <table class="w-full text-left text-sm">
          <thead class="bg-gray-50 text-gray-500 dark:bg-dark-900 dark:text-dark-400"><tr>
            <th class="p-3"><input type="checkbox" :aria-label="label('all')" :checked="rows.length > 0 && selected.length === rows.length" @change="selected = ($event.target as HTMLInputElement).checked ? rows.map(row => row.id) : []" /></th>
            <th class="p-3">{{ label('created') }}</th>
            <th class="p-3">{{ label('identity') }}</th>
            <th class="p-3">{{ label('platform') }} / {{ label('model') }}</th>
            <th class="p-3">{{ label('status') }} / {{ label('decision') }}</th>
            <th class="p-3">{{ label('targets') }}</th>
            <th class="p-3">{{ label('currentPrompt') }}</th>
            <th class="p-3">{{ label('auditDuration') }}</th>
            <th class="p-3">{{ label('detail') }}</th>
          </tr></thead>
          <tbody>
            <tr v-for="row in rows" :key="row.id" class="border-t border-gray-100 align-top dark:border-dark-700">
              <td class="p-3"><input v-model="selected" type="checkbox" :value="row.id" :aria-label="`#${row.id}`" /></td>
              <td class="whitespace-nowrap p-3">
                <p>{{ formatTime(row.created_at) }}</p>
                <p class="mt-1 text-xs text-gray-500">#{{ row.id }} · {{ label(row.current_run_kind) }} · {{ label('auditRound') }} {{ row.current_round }}</p>
                <p class="mt-1 max-w-64 whitespace-normal break-all text-xs text-gray-500">{{ label('requestID') }}: {{ row.request_id || '—' }}</p>
              </td>
              <td class="min-w-48 max-w-80 break-words p-3">
                <p>(#{{ row.user_id }})<span v-if="row.display_username" class="ml-1">{{ row.display_username }}</span></p>
                <p>{{ row.display_email }}</p>
                <p class="text-xs text-gray-500">{{ row.identity.api_key_name }} · {{ row.identity.group_name }}</p>
              </td>
              <td class="max-w-72 break-words p-3">
                <p>{{ row.platform }} · {{ row.model }}</p>
                <p class="mt-1 text-xs text-gray-500">{{ row.protocol }} · {{ label(row.mode) }}</p>
                <p v-if="row.orphan_continuation" class="mt-1 text-xs text-amber-700 dark:text-amber-400">{{ label('orphanContinuation') }}</p>
              </td>
              <td class="min-w-40 p-3">
                <p>{{ label(row.status) }}</p>
                <strong v-if="row.decision" :class="row.decision === 'block' ? 'text-red-600 dark:text-red-400' : row.decision === 'review' ? 'text-amber-700 dark:text-amber-400' : ''">{{ label(row.decision) }}</strong>
                <p v-if="row.trigger_kind" class="mt-1 text-xs text-gray-500">{{ label('trigger') }}: {{ label(row.trigger_kind) }}</p>
                <p v-if="row.last_error_code" class="mt-1 break-all text-xs text-red-600 dark:text-red-400"><span v-if="row.failure_stage">{{ label(row.failure_stage) }} · </span>{{ row.last_error_code }}<span v-if="row.last_error_message"> · {{ row.last_error_message }}</span></p>
                <p v-if="row.status === 'retry'" class="mt-1 text-xs">{{ label('nextRetry') }} {{ formatTime(row.next_attempt_at) }}</p>
                <p class="mt-1 text-xs">{{ label('attempts') }} {{ row.attempts }} / {{ row.max_attempts }}</p>
              </td>
              <td class="p-3 text-xs">
                <p>{{ label('targets') }}: {{ row.target_count }}</p>
                <p v-if="row.violation_counted">{{ label('violationCounted') }}</p>
                <p v-if="row.disable_counted">{{ label('disableCounted') }}</p>
              </td>
              <td class="p-3"><button class="btn btn-secondary btn-sm whitespace-nowrap" data-test="view-prompt" @click="openPrompts(row.id)">{{ label('viewPrompt') }}</button></td>
              <td class="whitespace-nowrap p-3">{{ formatMS(row.processing_ms) }}<span class="mt-1 block text-xs text-gray-500">{{ label('totalDuration') }} {{ formatMS(row.duration_ms) }}</span><span v-if="row.blocking_wait_ms !== null" class="mt-1 block text-xs text-gray-500">{{ label('blockingWait') }} {{ formatMS(row.blocking_wait_ms) }}</span></td>
              <td class="p-3"><button class="btn btn-secondary btn-sm whitespace-nowrap" @click="detailID = row.id">{{ label('detail') }}</button></td>
            </tr>
            <tr v-if="!loading && !rows.length"><td colspan="9" class="p-10 text-center text-gray-500">{{ label('empty') }}</td></tr>
          </tbody>
        </table>
      </div>
      <Pagination :page="page" :page-size="pageSize" :page-size-options="[20, 50, 100, 200]" :total="total" @update:page="changePage" @update:page-size="changeSize" />
    </div>
    <AuditDetail :id="detailID" :refresh-key="refreshKey" @close="detailID = null" @changed="load" @reaudit-created="emit('reaudit-created', $event)" />
    <BaseDialog :show="promptJobID !== null" :title="`${label('currentPrompt')} #${promptJobID ?? ''}`" width="wide" :close-on-click-outside="true" @close="promptJobID = null">
      <p v-if="promptLoading" role="status">{{ label('loading') }}</p>
      <div v-else class="space-y-4">
        <p class="text-xs text-gray-500 dark:text-dark-400">{{ label('currentPromptHint') }}</p>
        <article v-for="target in promptTargets" :key="target.order" class="space-y-2 rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
          <div class="flex flex-wrap items-center gap-3 text-sm">
            <strong>#{{ target.order }} · {{ label(target.kind) }}</strong>
            <span>{{ target.bytes }} B</span>
            <span v-if="target.resolution">{{ label('resolution') }}: {{ label('resolution_' + target.resolution) }}</span>
            <span v-if="target.decision">{{ label(target.decision) }}<template v-if="target.score !== null"> · {{ target.score }}</template></span>
            <button v-if="target.kind === 'instruction'" class="btn btn-secondary btn-sm" :disabled="promptTrusting" data-test="trust-target" @click="togglePromptTrust(target)">{{ label(target.trusted ? 'untrust' : 'trust') }}</button>
            <button class="btn btn-secondary btn-sm" data-test="copy-prompt" @click="copyPrompt(target)">{{ label('copyText') }}</button>
            <button class="btn btn-secondary btn-sm" data-test="download-prompt" @click="downloadPrompt(target, promptJobID)">{{ label('downloadText') }}</button>
          </div>
          <pre class="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-white p-3 text-xs dark:bg-dark-950">{{ target.text || '—' }}</pre>
          <p v-if="target.reason" class="text-xs text-gray-500 dark:text-dark-400">{{ target.reason }}</p>
        </article>
        <p v-if="!promptTargets.length" class="text-sm text-gray-500">{{ label('empty') }}</p>
      </div>
    </BaseDialog>
    <BaseDialog :show="previewRequest !== null" :title="label('preview')" width="wide" :show-close-button="!previewLoading" :close-on-escape="!previewLoading" :close-on-click-outside="!previewLoading" @close="closePreview">
      <div class="space-y-4">
        <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('previewHint') }}</p>
        <fieldset class="space-y-2">
          <legend class="text-sm font-medium">{{ label('reauditReuseMode') }}</legend>
          <label class="flex items-start gap-2 text-sm"><input v-model="draftReuse" type="radio" value="allow" @change="runPreview" /><span>{{ label('reuseAllow') }}<span class="block text-xs text-gray-500">{{ label('reuseAllowHint') }}</span></span></label>
          <label class="flex items-start gap-2 text-sm"><input v-model="draftReuse" type="radio" value="force" @change="runPreview" /><span>{{ label('reuseForce') }}<span class="block text-xs text-gray-500">{{ label('reuseForceHint') }}</span></span></label>
        </fieldset>
        <p v-if="previewLoading" role="status">{{ label('loading') }}</p>
        <template v-if="preview">
          <p>{{ label('matched') }}: {{ preview.matched }} · {{ label('ready') }}: {{ preview.ready }}</p>
          <div class="max-h-96 overflow-auto">
            <div v-for="item in previewItems" :key="item.job_id" class="border-t border-gray-100 py-3 text-sm dark:border-dark-700">
              <span>Job #{{ item.job_id }} → {{ label(item.status === 'requeued' ? 'submitted' : item.status) }}</span>
              <p class="text-gray-500 dark:text-dark-400">{{ item.reason }}</p>
            </div>
          </div>
          <Pagination :page="previewPage" :page-size="20" :total="preview.items.length" :show-page-size-selector="false" @update:page="previewPage = $event" />
          <button v-if="!submitted" class="btn btn-primary" :disabled="previewLoading || !preview.ready" @click="submit">{{ label('submitReaudit') }}</button>
          <p v-else role="status">{{ label('submitted') }}</p>
        </template>
      </div>
    </BaseDialog>
  </div>
</template>
