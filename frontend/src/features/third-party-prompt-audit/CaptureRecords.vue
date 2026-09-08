<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { apiClient } from '@/api/client'
import { thirdPartyPromptAuditAPI as api, type AuditCapture, type AuditUser } from '@/api/admin/third-party-prompt-audit'
import AuditDetail from './AuditDetail.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import CaptureBody from './CaptureBody.vue'
import { useAuditLabels } from './labels'
import { formatTime } from './viewModel'

const items = ref<AuditCapture[]>([])
const total = ref(0)
const page = ref(1)
const selected = ref<AuditCapture | null>(null)
const error = ref('')
const app = useAppStore()
const label = useAuditLabels()
const users = ref<AuditUser[]>([])
const userID = ref<number | null>(null)
const keyOptions = ref<{ id: number; name: string }[]>([])
const keyLoading = ref(false)
const recovering = ref(false)
const jobID = ref<number | null>(null)
const recoveryBlocked = computed(() => !selected.value || selected.value.snapshot_status !== 'complete')
const processing = computed(() => !!selected.value && (selected.value.processing_status === 'processing' || (['queued', 'retry'].includes(selected.value.processing_status) && selected.value.metadata?.mode === 'async' && selected.value.eligibility_status === 'passed')))
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
    const { data } = await apiClient.get(base, { params: { page: page.value, page_size: 20 } })
    items.value = data.items
    total.value = data.total
    error.value = ''
  } catch (err) {
    error.value = extractApiErrorMessage(err, label('error'))
  }
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
</script>

<template>
  <section class="card">
    <header class="mb-4 flex justify-between">
      <div>
        <h2 class="text-xl font-semibold">{{ label('captures') }}</h2>
        <p class="mt-2 text-sm text-gray-500">{{ label('captureHint') }}</p>
      </div>
      <button class="btn btn-secondary" @click="load">{{ label('refresh') }}</button>
    </header>
    <p v-if="error" class="text-red-600">{{ error }}</p>
    <div class="overflow-auto">
      <table class="table">
        <thead><tr><th>ID</th><th>{{ label('created') }}</th><th>{{ label('protocol') }}</th><th>{{ label('captureBytes') }}</th><th>{{ label('captureIntegrity') }}</th><th>{{ label('captureEligibility') }}</th><th>{{ label('captureProcessing') }}</th><th>{{ label('forwardingStock') }}</th><th>{{ label('operation') }}</th></tr></thead>
        <tbody>
          <tr v-for="item in items" :key="item.id">
            <td>{{ item.id }}</td><td class="whitespace-nowrap">{{ formatTime(item.created_at) }}</td><td>{{ item.protocol }}</td><td>{{ item.body_bytes }}</td><td>{{ label(item.snapshot_status) }}</td><td>{{ label(item.eligibility_status) }}</td><td>{{ label(item.processing_status) }}</td><td>{{ label(item.forwarding_status) }}</td>
            <td><button class="btn btn-secondary btn-sm whitespace-nowrap" @click="detail(item.id)">{{ label('viewCapture') }}</button></td>
          </tr>
        </tbody>
      </table>
    </div>
    <footer class="mt-4 flex items-center justify-end gap-3"><span>{{ total }} · {{ page }}</span><button class="btn btn-secondary" :disabled="page <= 1" @click="page--; load()">←</button><button class="btn btn-secondary" :disabled="page * 20 >= total" @click="page++; load()">→</button></footer>
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
    <AuditDetail :id="jobID" source="jobs" @close="jobID = null" />
  </section>
</template>
