<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { apiClient } from '@/api/client'
import { thirdPartyPromptAuditAPI as api, type AuditCapture, type AuditUser, type CaptureFilter, type RecoveryResult, type AuditBatch } from '@/api/admin/third-party-prompt-audit'
import AuditDetail from './AuditDetail.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import CaptureBody from './CaptureBody.vue'
import { useAuditLabels } from './labels'
import { formatTime } from './viewModel'
import LatestUserContent from './LatestUserContent.vue'
import Pagination from '@/components/common/Pagination.vue'

const props = withDefaults(defineProps<{ refreshKey?: number }>(), { refreshKey: 0 })
const emit = defineEmits<{ (event: 'recovery-created', ids: number[]): void }>()
const items = ref<AuditCapture[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const filters = reactive<CaptureFilter>({})
const applied = ref<CaptureFilter>({})
const from = ref(''), to = ref(''), advancedOpen = ref(false)
const selected = ref<AuditCapture | null>(null)
const error = ref('')
const app = useAppStore()
const label = useAuditLabels()
const forwardingLabel = (status: string) => label(`forwarding_${status}`)
const users = ref<AuditUser[]>([])
const userID = ref<number | null>(null)
const keyOptions = ref<{ id: number; name: string }[]>([])
const keyLoading = ref(false)
const recovering = ref(false)
const jobID = ref<number | null>(null)
const recoveryBlocked = computed(() => !selected.value || selected.value.snapshot_status !== 'complete')
const processing = computed(() => !!selected.value && (selected.value.processing_status === 'processing' || (['queued', 'retry'].includes(selected.value.processing_status) && selected.value.metadata?.mode === 'async' && selected.value.eligibility_status === 'passed')))
const batchOpen = ref(false), batchLoading = ref(false), batchSubmitted = ref(false), batchMode = ref<'failed' | 'awaiting'>('failed')
const batchResult = ref<RecoveryResult | null>(null), batchPage = ref(1)
const batchID = ref<number | null>(null), batchStatus = ref<string>(''), batchData = ref<AuditBatch | null>(null), batchPollTimer = ref<ReturnType<typeof setInterval> | null>(null)
const batchItems = computed(() => batchResult.value?.items.slice((batchPage.value - 1) * 20, batchPage.value * 20) ?? [])
watch(userID, async id => {
  manualKeyID.value = undefined
  keyOptions.value = []
  if (!id) { keyLoading.value = false; return }
  keyLoading.value = true
  try { const options = await api.userKeys(id); if (userID.value === id) keyOptions.value = options }
  catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { if (userID.value === id) keyLoading.value = false }
})
const manualKeyID = ref<number | undefined>()
const base = '/admin/third-party-prompt-audit/captures'

async function load() {
  try {
    const result = await api.captures(applied.value, page.value, pageSize.value)
    items.value = result.items
    total.value = result.total
    error.value = ''
  } catch (err) {
    error.value = extractApiErrorMessage(err, label('error'))
  }
}

function apply() {
  const next = Object.fromEntries(Object.entries(filters).filter(([, value]) => value !== '' && value != null)) as CaptureFilter
  if (from.value) next.from = new Date(from.value).toISOString()
  if (to.value) next.to = new Date(to.value).toISOString()
  if (next.from && next.to && new Date(next.from) >= new Date(next.to)) { error.value = label('invalidRange'); return }
  applied.value = next
  page.value = 1
  void load()
}

function changePage(value: number) { page.value = value; void load() }
function changeSize(value: number) { pageSize.value = value; page.value = 1; void load() }

async function openBatchRecovery(mode: 'failed' | 'awaiting' = 'failed') {
  batchMode.value = mode
  batchOpen.value = true
  batchLoading.value = true
  batchSubmitted.value = false
  batchResult.value = null
  batchPage.value = 1
  try { batchResult.value = batchMode.value === 'awaiting' ? await api.previewAwaitingReviews() : await api.previewRecoveries() }
  catch (err) { app.showError(extractApiErrorMessage(err, label('error'))); batchOpen.value = false }
  finally { batchLoading.value = false }
}

async function submitBatchRecovery() {
  const submittedMode = batchMode.value
  if (batchLoading.value || !batchResult.value?.ready) return
  batchLoading.value = true
  try {
    if (typeof api.createBatch === 'function') {
      const created = await api.createBatch({ batch_type: batchMode.value === 'awaiting' ? 'pending_review' : 'failed_recovery' })
      batchID.value = created.batch_id
      batchStatus.value = created.status
      batchData.value = { ...created, id: created.batch_id, batch_type: batchMode.value === 'awaiting' ? 'pending_review' : 'failed_recovery', requested_by: 0, processed: 0, created: 0, requeued: 0, resumed: 0, skipped: 0, failed: 0, started_at: null, finished_at: null, created_at: null, updated_at: null } as AuditBatch
      batchSubmitted.value = true
      if (batchPollTimer.value) clearInterval(batchPollTimer.value)
      batchPollTimer.value = setInterval(async () => {
        if (!batchID.value) return
        try { const current = await api.batch(batchID.value); batchData.value = current; batchStatus.value = current.status; if (['completed','failed'].includes(current.status)) { if (batchPollTimer.value) clearInterval(batchPollTimer.value); batchPollTimer.value = null; await load() } } catch { /* 下一轮继续 */ }
      }, 1500)
      app.showSuccess(`${label(submittedMode === 'awaiting' ? 'pendingReviewSubmitted' : 'recoverySubmitted')}: ${created.batch_id}`)
      return
    }
    if (batchMode.value === 'awaiting') { await api.createAwaitingReviews(); batchSubmitted.value = true; await load(); app.showSuccess(label('pendingReviewSubmitted')); return }
    batchResult.value = await api.createRecoveries(); batchSubmitted.value = true; batchPage.value = 1
    const ids = batchResult.value.items.flatMap(item => item.job_id && ['created', 'requeued', 'resumed', 'already_running'].includes(item.status) ? [item.job_id] : [])
    if (ids.length) emit('recovery-created', ids); await load(); app.showSuccess(`${label(submittedMode === 'awaiting' ? 'pendingReviewSubmitted' : 'recoverySubmitted')}: ${ids.length}`)
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { batchLoading.value = false }
}

function closeBatchRecovery() {
  if (batchLoading.value) return
  if (batchPollTimer.value) { clearInterval(batchPollTimer.value); batchPollTimer.value = null }
  batchOpen.value = false
  batchResult.value = null
  batchSubmitted.value = false
}

async function detail(id: number) {
  try {
    selected.value = await api.capture(id)
    userID.value = null; manualKeyID.value = undefined; keyOptions.value = []
    if (!selected.value.job_id && !selected.value.identity?.user_id && !recoveryBlocked.value && !processing.value) users.value = await api.users()
  }
  catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
}

async function reprocess(id: number) {
  if (recovering.value) return
  recovering.value = true
  try {
    await apiClient.post(`${base}/${id}/reprocess`, { api_key_id: manualKeyID.value })
    await detail(id)
    await load()
    app.showSuccess(label('captureResumed'))
  } catch (err) {
    app.showError(extractApiErrorMessage(err, label('error')))
  } finally { recovering.value = false }
}

onMounted(load)
onUnmounted(() => { if (batchPollTimer.value) clearInterval(batchPollTimer.value) })
watch(() => props.refreshKey, () => { void load() })
</script>

<template>
  <section class="card">
    <header class="mb-4 flex flex-wrap justify-between gap-3">
      <div>
        <h2 class="text-xl font-semibold">{{ label('captures') }}</h2>
        <p class="mt-2 text-sm text-gray-500">{{ label('captureHint') }}</p>
      </div>
      <div class="flex flex-wrap gap-2"><button class="btn btn-secondary" @click="openBatchRecovery('awaiting')">{{ label('processPendingReviews') }}</button><button class="btn btn-secondary" @click="openBatchRecovery('failed')">{{ label('recoverAllFailures') }}</button><button class="btn btn-secondary" @click="load">{{ label('refresh') }}</button></div>
    </header>
    <form class="mb-5 space-y-4 rounded-xl border border-gray-100 p-4 dark:border-dark-700" @submit.prevent="apply">
      <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <label class="space-y-1 text-sm"><span>{{ label('keyword') }}</span><input v-model="filters.keyword" class="input" /></label>
        <label class="space-y-1 text-sm"><span>{{ label('protocol') }}</span><input v-model="filters.protocol" class="input" /></label>
        <label class="space-y-1 text-sm"><span>{{ label('captureProcessing') }}</span><select v-model="filters.status" class="input"><option value="">{{ label('all') }}</option><option v-for="item in ['queued', 'processing', 'retry', 'done', 'failed', 'skipped', 'awaiting_review']" :key="item" :value="item">{{ label(item) }}</option></select></label>
        <label class="space-y-1 text-sm"><span>{{ label('forwardingStock') }}</span><select v-model="filters.forwarding_status" class="input"><option value="">{{ label('all') }}</option><option v-for="item in ['not_forwarded', 'started', 'response_started', 'complete', 'unknown', 'blocked']" :key="item" :value="item">{{ forwardingLabel(item) }}</option></select></label>
      </div>
      <div><button type="button" class="inline-flex w-fit items-center gap-1 text-sm" :aria-expanded="advancedOpen" @click="advancedOpen = !advancedOpen"><span aria-hidden="true">{{ advancedOpen ? '▾' : '▸' }}</span><span>{{ label('from') }} / {{ label('userID') }} / {{ label('captureIntegrity') }}</span></button>
        <div v-show="advancedOpen" class="mt-3 grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <label class="space-y-1 text-sm"><span>{{ label('from') }}</span><input v-model="from" type="datetime-local" class="input" /></label><label class="space-y-1 text-sm"><span>{{ label('to') }}</span><input v-model="to" type="datetime-local" class="input" /></label>
          <label v-for="field in [{ key: 'user_id', name: 'userID' }, { key: 'api_key_id', name: 'apiKeyID' }, { key: 'group_id', name: 'groupID' }] as const" :key="field.key" class="space-y-1 text-sm"><span>{{ label(field.name) }}</span><input :value="filters[field.key]" type="number" min="1" step="1" class="input" @input="filters[field.key] = ($event.target as HTMLInputElement).value === '' ? undefined : ($event.target as HTMLInputElement).valueAsNumber" /></label>
          <label class="space-y-1 text-sm"><span>{{ label('captureIntegrity') }}</span><select v-model="filters.snapshot_status" class="input"><option value="">{{ label('all') }}</option><option value="complete">{{ label('complete') }}</option><option value="incomplete">{{ label('incomplete') }}</option></select></label>
          <label class="space-y-1 text-sm"><span>{{ label('captureEligibility') }}</span><select v-model="filters.eligibility_status" class="input"><option value="">{{ label('all') }}</option><option v-for="item in ['passed', 'unknown', 'rejected', 'manual']" :key="item" :value="item">{{ label(item) }}</option></select></label>
        </div>
      </div>
      <button class="btn btn-primary">{{ label('apply') }}</button>
    </form>
    <p v-if="error" class="text-red-600">{{ error }}</p>
    <div class="overflow-auto">
      <table class="table min-w-[72rem]">
        <thead><tr><th>ID</th><th>{{ label('created') }}</th><th>{{ label('user') }}</th><th>{{ label('protocol') }}</th><th>{{ label('captureBytes') }}</th><th>{{ label('captureIntegrity') }}</th><th>{{ label('captureEligibility') }}</th><th>{{ label('captureProcessing') }}</th><th>{{ label('forwardingStock') }}</th><th>{{ label('operation') }}</th></tr></thead>
        <tbody>
          <tr v-for="item in items" :key="item.id">
            <td>{{ item.id }}</td><td class="whitespace-nowrap">{{ formatTime(item.created_at) }}</td><td class="min-w-44 break-words"><p v-if="item.identity?.user_id">(#{{ item.identity.user_id }})<span v-if="item.display_username" class="ml-1">{{ item.display_username }}</span></p><p v-else>{{ item.display_username || item.display_email || '—' }}</p><p v-if="item.display_email" class="text-xs text-gray-500">{{ item.display_email }}</p></td><td>{{ item.protocol }}</td><td>{{ item.body_bytes }}</td><td>{{ label(item.snapshot_status) }}</td><td>{{ label(item.eligibility_status) }}</td><td>{{ label(item.processing_status) }}</td><td>{{ item.forwarding_status ? forwardingLabel(item.forwarding_status) : '—' }}</td>
            <td class="space-x-2"><button class="btn btn-secondary btn-sm whitespace-nowrap" @click="detail(item.id)">{{ label('viewCapture') }}</button><LatestUserContent source="capture" :id="item.id" variant="dialog" /></td>
          </tr>
        </tbody>
      </table>
    </div>
    <Pagination class="mt-4" :page="page" :page-size="pageSize" :page-size-options="[20, 50, 100, 200]" :total="total" @update:page="changePage" @update:page-size="changeSize" />
    <BaseDialog :show="!!selected" :title="label('captureDetail')" :close-on-click-outside="true" @close="selected = null">
      <template v-if="selected">
        <CaptureBody :capture="selected" />
        <div class="mt-5 space-y-3">
          <div v-if="selected.job_id"><span>{{ label('linkedJob') }} #{{ selected.job_id }}</span> <button class="btn btn-secondary" @click="jobID = selected.job_id!">{{ label('viewJob') }}</button></div>
          <p v-else-if="recoveryBlocked">{{ label('captureCannotRecover') }}</p>
          <p v-else-if="processing">{{ label('captureProcessingWait') }}</p>
          <template v-else>
            <div v-if="!selected.identity?.user_id" class="flex flex-wrap gap-3">
              <select v-model="userID" class="input"><option :value="null">{{ label('selectRecoveryUser') }}</option><option v-for="user in users" :key="user.id" :value="user.id">{{ user.username }} (#{{ user.id }}) · {{ user.email }}</option></select>
              <select v-model="manualKeyID" class="input" :disabled="!userID || keyLoading"><option :value="undefined">{{ label('selectRecoveryKey') }}</option><option v-for="key in keyOptions" :key="key.id" :value="key.id">{{ key.name }} (#{{ key.id }})</option></select>
            </div>
            <p class="text-sm text-gray-500">{{ label('captureRecoveryHint') }}</p>
            <button class="btn btn-secondary" :disabled="recovering || (!selected.identity?.user_id && !manualKeyID)" @click="reprocess(selected.id)">{{ label(recovering ? 'loading' : 'reprocessCapture') }}</button>
          </template>
          <button class="text-primary-600 underline" :disabled="recovering" @click="detail(selected.id)">{{ label('refresh') }}</button>
        </div>
      </template>
    </BaseDialog>
    <BaseDialog :show="batchOpen" :title="label(batchMode === 'awaiting' ? 'processPendingReviews' : 'recoverAllFailures')" width="wide" :show-close-button="!batchLoading" :close-on-escape="!batchLoading" :close-on-click-outside="!batchLoading" @close="closeBatchRecovery">
      <div class="space-y-4">
        <p class="text-sm text-gray-500">{{ label(batchMode === 'awaiting' ? 'pendingReviewHint' : 'recoveryPreviewHint') }}</p>
        <p v-if="batchLoading" role="status">{{ label('loading') }}</p>
        <template v-if="batchResult"><p>{{ label('matched') }}: {{ batchResult.matched }} · {{ label('ready') }}: {{ batchResult.ready }}</p><div class="max-h-96 overflow-auto"><div v-for="item in batchItems" :key="`${item.capture_id}-${item.job_id || 0}`" class="border-t border-gray-100 py-3 text-sm dark:border-dark-700"><span>Capture #{{ item.capture_id }}<template v-if="item.job_id"> · Job #{{ item.job_id }}</template> → {{ label(item.action) }} · {{ label(item.status) }}</span><p v-if="item.reason" class="text-gray-500">{{ item.reason }}</p></div></div><Pagination :page="batchPage" :page-size="20" :total="batchResult.items.length" :show-page-size-selector="false" @update:page="batchPage = $event" /><button v-if="!batchSubmitted" class="btn btn-primary" :disabled="batchLoading || !batchResult.ready" @click="submitBatchRecovery">{{ label(batchMode === 'awaiting' ? 'submitPendingReviews' : 'submitRecovery') }}</button><p v-else role="status">{{ label(batchMode === 'awaiting' ? 'pendingReviewSubmitted' : 'recoverySubmitted') }}<template v-if="batchID"> · #{{ batchID }} · {{ batchStatus }}<template v-if="batchData"> · {{ label('processed') }}: {{ batchData.processed }}/{{ batchData.matched }} · {{ label('createdJobs') }}: {{ batchData.created }} · {{ label('requeued') }}: {{ batchData.requeued }} · {{ label('resumed') }}: {{ batchData.resumed }} · {{ label('skipped') }}: {{ batchData.skipped }} · {{ label('failed') }}: {{ batchData.failed }}</template></template></p></template>
      </div>
    </BaseDialog>
    <AuditDetail :id="jobID" @close="jobID = null" />
  </section>
</template>
