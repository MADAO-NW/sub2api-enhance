<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { apiClient } from '@/api/client'
import { thirdPartyPromptAuditAPI as api, type AuditCapture } from '@/api/admin/third-party-prompt-audit'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import CaptureBody from './CaptureBody.vue'
import { useAuditLabels } from './labels'

const items = ref<AuditCapture[]>([])
const total = ref(0)
const page = ref(1)
const selected = ref<AuditCapture | null>(null)
const error = ref('')
const app = useAppStore()
const label = useAuditLabels()
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
  try { selected.value = await api.capture(id) }
  catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
}

async function reprocess(id: number) {
  try {
    await apiClient.post(`${base}/${id}/reprocess`, { api_key_id: manualKeyID.value })
    await detail(id)
    await load()
    app.showSuccess(label('captureResumed'))
  } catch (err) {
    app.showError(extractApiErrorMessage(err, label('error')))
  }
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
        <thead><tr><th>ID</th><th>{{ label('protocol') }}</th><th>{{ label('captureBytes') }}</th><th>{{ label('captureIntegrity') }}</th><th>{{ label('captureEligibility') }}</th><th>{{ label('captureProcessing') }}</th><th>{{ label('forwardingStock') }}</th><th>{{ label('operation') }}</th></tr></thead>
        <tbody>
          <tr v-for="item in items" :key="item.id">
            <td>{{ item.id }}</td><td>{{ item.protocol }}</td><td>{{ item.body_bytes }}</td><td>{{ label(item.snapshot_status) }}</td><td>{{ label(item.eligibility_status) }}</td><td>{{ label(item.processing_status) }}</td><td>{{ label(item.forwarding_status) }}</td>
            <td><button class="btn btn-secondary btn-sm whitespace-nowrap" @click="detail(item.id)">{{ label('viewCapture') }}</button></td>
          </tr>
        </tbody>
      </table>
    </div>
    <footer class="mt-4 flex items-center justify-end gap-3"><span>{{ total }} · {{ page }}</span><button class="btn btn-secondary" :disabled="page <= 1" @click="page--; load()">←</button><button class="btn btn-secondary" :disabled="page * 20 >= total" @click="page++; load()">→</button></footer>
    <BaseDialog :show="!!selected" :title="label('captureDetail')" :close-on-click-outside="true" @close="selected = null">
      <template v-if="selected">
        <CaptureBody :capture="selected" />
        <div class="mt-5 flex flex-wrap gap-3">
          <input v-model.number="manualKeyID" class="input max-w-xs" type="number" min="1" :placeholder="label('manualKeyHint')">
          <button class="btn btn-secondary" @click="reprocess(selected.id)">{{ label('reprocessCapture') }}</button>
        </div>
      </template>
    </BaseDialog>
  </section>
</template>
