<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Pagination from '@/components/common/Pagination.vue'
import {
  thirdPartyPromptAuditAPI as api, type AuditConfig, type PolicyReplay, type PolicyReplayResult, type Sample, type SavedConfig,
} from '@/api/admin/third-party-prompt-audit'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatTime } from './viewModel'
import { useAuditLabels } from './labels'

const props = withDefaults(defineProps<{ config?: AuditConfig | null; saved?: SavedConfig | null; active?: boolean; refreshKey?: number }>(), { config: null, saved: null, active: true, refreshKey: 0 })
const label = useAuditLabels(), app = useAppStore()
const samples = ref<Sample[]>([])
const kind = ref(''), expected = ref('')
const page = ref(1), pageSize = ref(20), total = ref(0)
const loading = ref(false), error = ref('')
const editingID = ref<number | null>(null), editingExpected = ref<'pass' | 'block'>('pass'), editingNote = ref('')
const busy = ref(false)
const confirmDelete = ref<Sample | null>(null)
const replays = ref<PolicyReplay[]>([])
const replayDetail = ref<PolicyReplay | null>(null)
const previous = ref<PolicyReplay | null>(null)
const replayLoading = ref(false)
const confirmReplay = ref(false)
const replaySamples = ref(0)
// enabledNodes 是本次回放每个样本会调用的节点数。
const enabledNodes = computed(() => (props.config?.models ?? []).filter(model => model.enabled).length)
const replayCalls = computed(() => replaySamples.value * enabledNodes.value)

async function loadSamples() {
  loading.value = true
  error.value = ''
  try {
    const result = await api.samples({ kind: kind.value || undefined, expected: expected.value || undefined }, page.value, pageSize.value)
    samples.value = result.items
    total.value = result.total
  } catch (err) { error.value = extractApiErrorMessage(err, label('error')) }
  finally { loading.value = false }
}
async function loadReplays() {
  try { replays.value = (await api.replays(20)).items }
  catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
}
function startEdit(sample: Sample) {
  editingID.value = sample.id
  editingExpected.value = sample.expected
  editingNote.value = sample.note
}
async function saveSample(id: number) {
  busy.value = true
  try {
    await api.updateSample(id, { expected: editingExpected.value, note: editingNote.value })
    app.showSuccess(label('saved'))
    editingID.value = null
    await loadSamples()
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { busy.value = false }
}
async function removeSample() {
  const sample = confirmDelete.value
  if (!sample) return
  busy.value = true
  try {
    await api.deleteSample(sample.id)
    app.showSuccess(label('sampleDeleted'))
    confirmDelete.value = null
    await loadSamples()
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { busy.value = false }
}
// 执行前用未过滤的样本总数与启用节点数估算调用量。
async function openReplayConfirm() {
  if (!props.config) return
  try { replaySamples.value = (await api.samples({}, 1, 1)).total }
  catch { replaySamples.value = total.value }
  confirmReplay.value = true
}
async function runReplay() {
  if (!props.config) return
  busy.value = true
  try {
    const replay = await api.startReplay(props.config, [])
    confirmReplay.value = false
    app.showSuccess(label('replayStarted'))
    await loadReplays()
    await openReplay(replay.id)
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { busy.value = false }
}
async function openReplay(id: number) {
  replayLoading.value = true
  replayDetail.value = null
  previous.value = null
  try {
    replayDetail.value = await api.replay(id)
    // 上一次回放用于逐样本对比：取比当前更早的最近一次已完成回放。
    const current = replayDetail.value
    const older = replays.value
      .filter(item => item.id !== current.id && item.status === 'done' && new Date(item.created_at) <= new Date(current.created_at))
      .sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime())[0]
    if (older) previous.value = await api.replay(older.id)
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { replayLoading.value = false }
}
const groups = computed(() => {
  const results = replayDetail.value?.results ?? []
  return {
    newMisjudge: results.filter(item => !item.error && item.expected === 'pass' && item.actual === 'block'),
    newMissed: results.filter(item => !item.error && item.expected === 'block' && item.actual === 'pass'),
    consistent: results.filter(item => !item.error && item.actual === item.expected),
    failed: results.filter(item => !!item.error),
  }
})
const previousResults = computed(() => new Map<number, PolicyReplayResult>((previous.value?.results ?? []).map(item => [item.sample_id, item])))
function comparison(item: PolicyReplayResult): string {
  const before = previousResults.value.get(item.sample_id)
  if (!before) return label('replayPreviousMissing')
  return before.actual === item.actual ? label('replaySame') : label('replayChanged')
}
function changePage(value: number) { page.value = value; void loadSamples() }
function changeSize(value: number) { pageSize.value = value; page.value = 1; void loadSamples() }
function resetFilter() { kind.value = ''; expected.value = ''; page.value = 1; void loadSamples() }
watch(() => props.refreshKey, () => { void loadSamples(); void loadReplays() })
// 运行中的回放每 3 秒刷新一次进度；结束后刷新历史列表。
const pollTimer = window.setInterval(async () => {
  if (!props.active || document.visibilityState !== 'visible' || !replayDetail.value || replayDetail.value.status !== 'running') return
  try {
    replayDetail.value = await api.replay(replayDetail.value.id)
    if (replayDetail.value.status !== 'running') await loadReplays()
  } catch { /* 进度刷新失败不打断当前视图 */ }
}, 3000)
onUnmounted(() => window.clearInterval(pollTimer))
onMounted(() => { void loadSamples(); void loadReplays() })
</script>

<template>
  <div class="space-y-5">
    <section class="space-y-4" data-test="samples-panel">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h3 class="font-semibold">{{ label('sampleSet') }}</h3>
          <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ label('samplesHint') }}</p>
        </div>
        <div class="flex flex-wrap items-end gap-2">
          <label class="space-y-1 text-sm"><span>{{ label('sampleKind') }}</span>
            <select v-model="kind" class="input w-auto" data-test="sample-kind" @change="changePage(1)">
              <option value="">{{ label('all') }}</option>
              <option value="instruction">{{ label('instruction') }}</option>
              <option value="user">{{ label('user') }}</option>
            </select>
          </label>
          <label class="space-y-1 text-sm"><span>{{ label('sampleExpected') }}</span>
            <select v-model="expected" class="input w-auto" data-test="sample-expected" @change="changePage(1)">
              <option value="">{{ label('all') }}</option>
              <option value="pass">{{ label('expectedPass') }}</option>
              <option value="block">{{ label('expectedBlock') }}</option>
            </select>
          </label>
          <button type="button" class="btn btn-secondary" @click="resetFilter">{{ label('resetFilters') }}</button>
        </div>
      </div>
      <p v-if="error" role="alert" class="text-red-600 dark:text-red-400">{{ error }}</p>
      <div class="overflow-x-auto">
        <table class="w-full text-left text-sm">
          <thead class="text-gray-500 dark:text-dark-400"><tr>
            <th class="p-2">{{ label('hash') }}</th>
            <th class="p-2">{{ label('samplePreview') }}</th>
            <th class="p-2">{{ label('sampleKind') }}</th>
            <th class="p-2">{{ label('sampleExpected') }}</th>
            <th class="p-2">{{ label('sampleNote') }}</th>
            <th class="p-2">{{ label('sampleSourceJob') }}</th>
            <th class="p-2">{{ label('actions') }}</th>
          </tr></thead>
          <tbody>
            <tr v-for="sample in samples" :key="sample.id" class="border-t border-gray-100 align-top dark:border-dark-700">
              <td class="max-w-40 break-all p-2 font-mono text-xs text-gray-500">{{ sample.hash }}</td>
              <td class="max-w-96 p-2"><pre class="max-h-24 overflow-auto whitespace-pre-wrap break-words text-xs">{{ sample.preview }}</pre></td>
              <td class="p-2">{{ label(sample.kind) }}</td>
              <td class="p-2">
                <select v-if="editingID === sample.id" v-model="editingExpected" class="input" data-test="sample-edit-expected">
                  <option value="pass">{{ label('expectedPass') }}</option>
                  <option value="block">{{ label('expectedBlock') }}</option>
                </select>
                <span v-else>{{ label(sample.expected === 'pass' ? 'expectedPass' : 'expectedBlock') }}</span>
              </td>
              <td class="p-2">
                <input v-if="editingID === sample.id" v-model="editingNote" class="input" maxlength="500" data-test="sample-edit-note" />
                <span v-else class="break-words">{{ sample.note || '—' }}</span>
              </td>
              <td class="whitespace-nowrap p-2">{{ sample.source_job_id ? `#${sample.source_job_id}` : '—' }}<span class="mt-1 block text-xs text-gray-500">{{ formatTime(sample.updated_at) }}</span></td>
              <td class="space-y-2 p-2">
                <template v-if="editingID === sample.id">
                  <button type="button" class="btn btn-secondary btn-sm" :disabled="busy" data-test="sample-save" @click="saveSample(sample.id)">{{ label('saveSample') }}</button>
                  <button type="button" class="btn btn-ghost btn-sm" :disabled="busy" @click="editingID = null">{{ label('cancelChanges') }}</button>
                </template>
                <template v-else>
                  <button type="button" class="btn btn-secondary btn-sm" data-test="sample-edit" @click="startEdit(sample)">{{ label('editSample') }}</button>
                  <button type="button" class="btn btn-secondary btn-sm" data-test="sample-delete" @click="confirmDelete = sample">{{ label('deleteSample') }}</button>
                </template>
              </td>
            </tr>
            <tr v-if="!loading && !samples.length"><td colspan="7" class="p-6 text-center text-gray-500">{{ label('samplesEmpty') }}</td></tr>
          </tbody>
        </table>
      </div>
      <Pagination :page="page" :page-size="pageSize" :page-size-options="[20, 50]" :total="total" @update:page="changePage" @update:page-size="changeSize" />
    </section>
    <section class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-700" data-test="replay-panel">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h3 class="font-semibold">{{ label('policyReplay') }}</h3>
          <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ label('policyReplayHint') }}</p>
        </div>
        <button type="button" class="btn btn-primary" :disabled="busy || !config" data-test="start-replay" @click="openReplayConfirm">{{ label('startReplay') }}</button>
      </div>
      <div class="overflow-x-auto">
        <table class="w-full text-left text-sm">
          <thead class="text-gray-500 dark:text-dark-400"><tr>
            <th class="p-2">#</th>
            <th class="p-2">{{ label('status') }}</th>
            <th class="p-2">{{ label('replaySamples') }}</th>
            <th class="p-2">{{ label('created') }}</th>
            <th class="p-2">{{ label('actions') }}</th>
          </tr></thead>
          <tbody>
            <tr v-for="item in replays" :key="item.id" class="border-t border-gray-100 dark:border-dark-700">
              <td class="p-2">#{{ item.id }}</td>
              <td class="p-2">{{ label(`replayStatus_${item.status}`) }}</td>
              <td class="p-2">{{ item.sample_ids.length }}</td>
              <td class="whitespace-nowrap p-2">{{ formatTime(item.created_at) }}<span class="mt-1 block text-xs text-gray-500">{{ item.finished_at ? formatTime(item.finished_at) : '—' }}</span></td>
              <td class="p-2"><button type="button" class="btn btn-secondary btn-sm" data-test="view-replay" @click="openReplay(item.id)">{{ label('replayDetail') }}</button></td>
            </tr>
            <tr v-if="!replays.length"><td colspan="5" class="p-6 text-center text-gray-500">{{ label('replayNoHistory') }}</td></tr>
          </tbody>
        </table>
      </div>
    </section>
    <BaseDialog :show="confirmReplay" :title="label('replayConfirmTitle')" :close-on-click-outside="!busy" @close="confirmReplay = false">
      <p class="text-sm">{{ label('replayConfirm') }}</p>
      <dl class="mt-4 flex flex-wrap gap-x-8 gap-y-3 text-sm">
        <div><dt class="text-gray-500">{{ label('replaySamples') }}</dt><dd class="text-lg font-semibold tabular-nums">{{ replaySamples }}</dd></div>
        <div><dt class="text-gray-500">{{ label('replayNodes') }}</dt><dd class="text-lg font-semibold tabular-nums">{{ enabledNodes }}</dd></div>
        <div><dt class="text-gray-500">{{ label('replayCallCount') }}</dt><dd class="text-lg font-semibold tabular-nums" data-test="replay-calls">{{ replayCalls }}</dd></div>
      </dl>
      <template #footer><div class="flex gap-3"><button type="button" class="btn btn-primary" :disabled="busy || !enabledNodes" data-test="confirm-replay" @click="runReplay">{{ label('startReplay') }}</button><button type="button" class="btn btn-secondary" @click="confirmReplay = false">{{ label('cancelChanges') }}</button></div></template>
    </BaseDialog>
    <BaseDialog :show="replayDetail !== null" :title="`${label('replayDetail')} #${replayDetail?.id ?? ''}`" width="wide" :close-on-click-outside="true" @close="replayDetail = null">
      <p v-if="replayLoading" role="status">{{ label('loading') }}</p>
      <div v-else-if="replayDetail" class="space-y-4">
        <p class="break-all text-sm text-gray-500 dark:text-dark-400">{{ label('status') }}: {{ label(`replayStatus_${replayDetail.status}`) }} · {{ label('created') }}: {{ formatTime(replayDetail.created_at) }} · {{ label('samples') }}: {{ replayDetail.sample_ids.length }}</p>
        <p v-if="replayDetail.error_message" class="rounded-xl bg-red-50 p-3 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-300" role="alert">{{ replayDetail.error_message }}</p>
        <section v-for="group in [
          { key: 'replayNewMisjudge', items: groups.newMisjudge }, { key: 'replayNewMissed', items: groups.newMissed },
          { key: 'replayConsistent', items: groups.consistent }, { key: 'replayErrors', items: groups.failed },
        ]" :key="group.key" class="space-y-2" :data-test="`replay-group-${group.key}`">
          <h3 class="text-sm font-medium">{{ label(group.key) }} ({{ group.items.length }})</h3>
          <div v-for="item in group.items" :key="item.sample_id" class="rounded-lg border border-gray-100 p-3 text-sm dark:border-dark-700">
            <p class="flex flex-wrap gap-3">
              <span>{{ label('replaySample') }} #{{ item.sample_id }}</span>
              <span>{{ label('sampleExpected') }}: {{ label(item.expected === 'pass' ? 'expectedPass' : 'expectedBlock') }}</span>
              <span>{{ label('replayActual') }}: {{ label(item.actual || 'unknown') }}</span>
              <span v-if="item.score !== null">{{ label('replayScore') }}: {{ item.score }}</span>
              <span v-if="item.categories.length">{{ label('category') }}: {{ item.categories.join(', ') }}</span>
              <span class="text-xs text-gray-500">{{ label('replayComparedToPrevious') }}: {{ comparison(item) }}</span>
            </p>
            <p v-if="item.error" class="text-red-600 dark:text-red-400">{{ item.error }}</p>
            <details v-if="item.nodes.length" class="mt-2"><summary class="cursor-pointer text-xs">{{ label('replayNodeResults') }}</summary>
              <p v-for="node in item.nodes" :key="node.model_id" class="mt-1 break-words text-xs">{{ node.model_name || node.model_id }} · {{ label(node.decision || 'unknown') }} · {{ node.score ?? '—' }} · {{ node.reason }}<span v-if="node.error"> · {{ node.error }}</span></p>
            </details>
          </div>
          <p v-if="!group.items.length" class="text-xs text-gray-500">{{ label('empty') }}</p>
        </section>
      </div>
    </BaseDialog>
    <BaseDialog :show="confirmDelete !== null" :title="label('deleteSample')" :close-on-click-outside="!busy" @close="confirmDelete = null">
      <p class="text-sm">{{ label('confirmDeleteSample') }}</p>
      <template #footer><div class="flex gap-3"><button type="button" class="btn btn-primary" :disabled="busy" data-test="confirm-delete-sample" @click="removeSample">{{ label('deleteSample') }}</button><button type="button" class="btn btn-secondary" @click="confirmDelete = null">{{ label('cancelChanges') }}</button></div></template>
    </BaseDialog>
  </div>
</template>
