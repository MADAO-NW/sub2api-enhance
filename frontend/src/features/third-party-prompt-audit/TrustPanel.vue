<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from 'vue'
import Pagination from '@/components/common/Pagination.vue'
import {
  thirdPartyPromptAuditAPI as api, type BreakerEntry, type DrillResult, type TrustEntry,
} from '@/api/admin/third-party-prompt-audit'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatTime } from './viewModel'
import { useAuditLabels } from './labels'

const props = withDefaults(defineProps<{ refreshKey?: number; active?: boolean }>(), { refreshKey: 0, active: true })
const emit = defineEmits<{ (event: 'changed'): void }>()
const label = useAuditLabels(), app = useAppStore()
const status = ref<'tripped' | 'trusted' | 'confirmed' | ''>('tripped')
const search = ref('')
const breakers = ref<BreakerEntry[]>([])
const breakerPage = ref(1), breakerTotal = ref(0)
const trusted = ref<TrustEntry[]>([])
const trustPage = ref(1), trustTotal = ref(0)
const loading = ref(false), pending = ref(''), error = ref('')
// 熔断演练：用假哈希走一遍判定，不写任何业务表。
const drillHash = ref('')
const drill = ref<DrillResult | null>(null)
const drilling = ref(false)
async function runDrill() {
  if (drillHash.value.length !== 64) { app.showError(label('drillHashInvalid')); return }
  drilling.value = true
  try { drill.value = await api.drillBreaker(drillHash.value) }
  catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { drilling.value = false }
}

async function loadBreakers(silent = false) {
  if (!silent) { loading.value = true; error.value = '' }
  else error.value = ''
  try {
    const result = await api.breakerList(status.value, breakerPage.value, 20, search.value)
    breakers.value = result.items
    breakerTotal.value = result.total
  } catch (err) { if (!silent) error.value = extractApiErrorMessage(err, label('error')) }
  finally { if (!silent) loading.value = false }
}
async function loadTrust(silent = false) {
  try {
    const result = await api.trustList(trustPage.value, 20, search.value)
    trusted.value = result.items
    trustTotal.value = result.total
  } catch (err) { if (!silent) error.value = extractApiErrorMessage(err, label('error')) }
}
async function act(kind: 'trust' | 'confirm' | 'untrust', hash: string) {
  pending.value = hash + kind
  try {
    if (kind === 'trust') await api.trust(hash, '熔断列表手工信任')
    else if (kind === 'confirm') await api.confirmBreaker(hash)
    else await api.untrust(hash)
    app.showSuccess(label('saved'))
    emit('changed')
    await Promise.all([loadBreakers(), loadTrust()])
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { pending.value = '' }
}
function changeBreakerPage(value: number) { breakerPage.value = value; void loadBreakers() }
function changeTrustPage(value: number) { trustPage.value = value; void loadTrust() }
watch(status, () => { breakerPage.value = 1; void loadBreakers() })
let searchTimer = 0
watch(search, () => {
  window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => { breakerPage.value = 1; trustPage.value = 1; void loadBreakers(); void loadTrust() }, 300)
})
watch(() => props.refreshKey, () => { void loadBreakers(); void loadTrust() })
// 每 5 秒静默刷新熔断与信任列表；页面不可见、正在加载或有待完成操作时跳过。
const autoTimer = window.setInterval(() => {
  if (!props.active || document.visibilityState !== 'visible' || loading.value || pending.value) return
  void loadBreakers(true)
  void loadTrust(true)
}, 5000)
onUnmounted(() => window.clearInterval(autoTimer))
onMounted(() => { void loadBreakers(); void loadTrust() })
</script>

<template>
  <div class="space-y-5">
    <p v-if="error" role="alert" class="text-red-600 dark:text-red-400">{{ error }}</p>
    <section class="card space-y-4 p-5">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 class="text-lg font-semibold">{{ label('breakerList') }}</h2>
          <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ label('breakerHint') }}</p>
        </div>
        <label class="flex items-center gap-2 text-sm"><span>{{ label('search') }}</span>
          <input v-model="search" class="input w-56" :placeholder="label('searchHint')" data-test="breaker-search" />
        </label>
        <label class="flex items-center gap-2 text-sm"><span>{{ label('status') }}</span>
          <select v-model="status" class="input w-auto">
            <option value="tripped">{{ label('breakerTripped') }}</option>
            <option value="trusted">{{ label('breakerTrusted') }}</option>
            <option value="confirmed">{{ label('breakerConfirmed') }}</option>
            <option value="">{{ label('all') }}</option>
          </select>
        </label>
      </div>
      <div class="overflow-x-auto">
        <table class="w-full text-left text-sm">
          <thead class="text-gray-500 dark:text-dark-400"><tr>
            <th class="p-2">{{ label('instructionBlock') }}</th>
            <th class="p-2">{{ label('status') }}</th>
            <th class="p-2">{{ label('distinctUsers') }}</th>
            <th class="p-2">{{ label('trippedAt') }}</th>
            <th class="p-2">{{ label('actions') }}</th>
          </tr></thead>
          <tbody>
            <tr v-for="item in breakers" :key="item.hash" class="border-t border-gray-100 align-top dark:border-dark-700">
              <td class="max-w-xl p-2">
                <p class="break-all font-mono text-xs text-gray-500">{{ item.hash }}</p>
                <pre class="mt-1 max-h-32 overflow-auto whitespace-pre-wrap break-words text-xs">{{ item.preview }}</pre>
              </td>
              <td class="p-2">{{ label('breaker' + item.status.charAt(0).toUpperCase() + item.status.slice(1)) }}</td>
              <td class="p-2">{{ item.distinct_users }}<span v-if="item.user_ids?.length" class="mt-1 block text-xs text-gray-500">#{{ item.user_ids.join(' #') }}</span><span v-if="item.similar" class="mt-1 block text-xs text-amber-700">{{ label('similarBlocks') }}: {{ item.similar }}</span></td>
              <td class="whitespace-nowrap p-2">{{ formatTime(item.tripped_at) }}</td>
              <td class="space-y-2 p-2">
                <template v-if="item.status === 'tripped'">
                  <button class="btn btn-secondary btn-sm" :disabled="pending === item.hash + 'trust'" @click="act('trust', item.hash)">{{ label('trust') }}</button>
                  <button class="btn btn-secondary btn-sm" :disabled="pending === item.hash + 'confirm'" @click="act('confirm', item.hash)">{{ label('confirmViolation') }}</button>
                </template>
                <button v-else-if="item.status === 'trusted'" class="btn btn-secondary btn-sm" :disabled="pending === item.hash + 'untrust'" @click="act('untrust', item.hash)">{{ label('untrust') }}</button>
                <span v-else class="text-xs text-gray-500">{{ label('breakerConfirmedHint') }}</span>
              </td>
            </tr>
            <tr v-if="!loading && !breakers.length"><td colspan="5" class="p-8 text-center text-gray-500">{{ label('empty') }}</td></tr>
          </tbody>
        </table>
      </div>
      <Pagination :page="breakerPage" :page-size="20" :page-size-options="[20, 50]" :total="breakerTotal" @update:page="changeBreakerPage" />
    </section>
    <section class="card space-y-3 p-5" data-test="breaker-drill">
      <h2 class="text-lg font-semibold">{{ label('breakerDrill') }}</h2>
      <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('breakerDrillHint') }}</p>
      <div class="flex flex-wrap items-end gap-3">
        <label class="space-y-2 text-sm"><span>{{ label('instructionBlock') }}</span><input v-model="drillHash" class="input w-96 font-mono" :placeholder="label('drillHashPlaceholder')" /></label>
        <button class="btn btn-secondary" :disabled="drilling || drillHash.length !== 64" @click="runDrill">{{ label(drilling ? 'loading' : 'runDrill') }}</button>
      </div>
      <div v-if="drill" class="space-y-1 text-sm">
        <p>{{ label('threshold') }}: {{ drill.threshold }} · {{ drill.tripped ? label('drillTripped') : label('drillNotTripped') }}</p>
        <p v-for="(step, index) in drill.steps" :key="index">{{ step }}</p>
      </div>
    </section>
    <section class="card space-y-4 p-5">
      <div>
        <h2 class="text-lg font-semibold">{{ label('trustList') }}</h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ label('trustHint') }}</p>
      </div>
      <div class="overflow-x-auto">
        <table class="w-full text-left text-sm">
          <thead class="text-gray-500 dark:text-dark-400"><tr>
            <th class="p-2">{{ label('instructionBlock') }}</th>
            <th class="p-2">{{ label('trustedBy') }}</th>
            <th class="p-2">{{ label('reason') }}</th>
            <th class="p-2">{{ label('created') }}</th>
            <th class="p-2">{{ label('actions') }}</th>
          </tr></thead>
          <tbody>
            <tr v-for="item in trusted" :key="item.hash" class="border-t border-gray-100 align-top dark:border-dark-700">
              <td class="max-w-xl p-2">
                <p class="break-all font-mono text-xs text-gray-500">{{ item.hash }}</p>
                <pre class="mt-1 max-h-32 overflow-auto whitespace-pre-wrap break-words text-xs">{{ item.preview }}</pre>
              </td>
              <td class="p-2">{{ item.trusted_by }}</td>
              <td class="p-2">{{ item.reason }}</td>
              <td class="whitespace-nowrap p-2">{{ formatTime(item.created_at) }}</td>
              <td class="p-2"><button class="btn btn-secondary btn-sm" :disabled="pending === item.hash + 'untrust'" @click="act('untrust', item.hash)">{{ label('untrust') }}</button></td>
            </tr>
            <tr v-if="!trusted.length"><td colspan="5" class="p-8 text-center text-gray-500">{{ label('empty') }}</td></tr>
          </tbody>
        </table>
      </div>
      <Pagination :page="trustPage" :page-size="20" :page-size-options="[20, 50]" :total="trustTotal" @update:page="changeTrustPage" />
    </section>
  </div>
</template>
