<script setup lang="ts">
import { onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import Pagination from '@/components/common/Pagination.vue'
import {
  thirdPartyPromptAuditAPI as api, type BreakerEntry, type SimilarBlock, type TrustEntry,
} from '@/api/admin/third-party-prompt-audit'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatTime } from './viewModel'
import { useAuditLabels } from './labels'
import TextDiff from './TextDiff.vue'

const props = withDefaults(defineProps<{ refreshKey?: number; active?: boolean; focusHash?: string }>(), { refreshKey: 0, active: true, focusHash: '' })
const emit = defineEmits<{ (event: 'changed'): void }>()
const label = useAuditLabels(), app = useAppStore()
const status = ref<'tripped' | 'trusted' | 'confirmed' | ''>('tripped')
const search = ref('')
const breakers = ref<BreakerEntry[]>([])
const breakerPage = ref(1), breakerTotal = ref(0)
const trusted = ref<TrustEntry[]>([])
const trustPage = ref(1), trustTotal = ref(0)
const loading = ref(false), pending = ref(''), error = ref('')
// 相似块按熔断块缓存；每个相似块只提供自己的信任/确认按钮，不做按相似度批量信任。
const similar = reactive<Record<string, SimilarBlock[]>>({})
const similarOpen = ref<string[]>([])
const similarSelected = reactive<Record<string, string>>({})
const similarLoading = ref('')

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
async function loadSimilar(hash: string) {
  similarLoading.value = hash
  try { similar[hash] = (await api.similarBlocks(hash)).items }
  catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { similarLoading.value = '' }
}
async function toggleSimilar(item: BreakerEntry) {
  if (similarOpen.value.includes(item.hash)) { similarOpen.value = similarOpen.value.filter(hash => hash !== item.hash); return }
  similarOpen.value = [...similarOpen.value, item.hash]
  if (!similar[item.hash]) await loadSimilar(item.hash)
}
function selectSimilar(hash: string, target: string) {
  similarSelected[hash] = similarSelected[hash] === target ? '' : target
}
function comparePreview(hash: string): SimilarBlock | null {
  const selected = similarSelected[hash]
  return selected ? (similar[hash] ?? []).find(item => item.hash === selected) ?? null : null
}
async function act(kind: 'trust' | 'confirm' | 'untrust', hash: string) {
  pending.value = hash + kind
  try {
    if (kind === 'trust') await api.trust(hash, '熔断列表手工信任')
    else if (kind === 'confirm') await api.confirmBreaker(hash)
    else await api.untrust(hash)
    app.showSuccess(label('saved'))
    emit('changed')
    delete similar[hash]
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
// 从待处理工作台跳到本页时，直接定位到该熔断块。
watch(() => props.focusHash, hash => {
  if (!hash) return
  status.value = ''
  search.value = hash
  if (!similarOpen.value.includes(hash)) {
    similarOpen.value = [...similarOpen.value, hash]
    if (!similar[hash]) void loadSimilar(hash)
  }
}, { immediate: true })
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
            <template v-for="item in breakers" :key="item.hash">
              <tr class="border-t border-gray-100 align-top dark:border-dark-700">
                <td class="max-w-xl p-2">
                  <p class="break-all font-mono text-xs text-gray-500">{{ item.hash }}</p>
                  <pre class="mt-1 max-h-32 overflow-auto whitespace-pre-wrap break-words text-xs">{{ item.preview }}</pre>
                </td>
                <td class="p-2">{{ label('breaker' + item.status.charAt(0).toUpperCase() + item.status.slice(1)) }}</td>
                <td class="p-2">{{ item.distinct_users }}<span v-if="item.user_ids?.length" class="mt-1 block text-xs text-gray-500">#{{ item.user_ids.join(' #') }}</span>
                  <button v-if="item.similar" type="button" class="mt-1 block text-xs text-amber-700 underline dark:text-amber-400" data-test="similar-toggle" @click="toggleSimilar(item)">
                    {{ label('similarBlocksExpand') }} {{ item.similar }}
                  </button></td>
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
              <tr v-if="similarOpen.includes(item.hash)" class="border-t border-gray-100 bg-gray-50 dark:border-dark-700 dark:bg-dark-900">
                <td colspan="5" class="space-y-3 p-4">
                  <p v-if="similarLoading === item.hash" role="status">{{ label('loading') }}</p>
                  <template v-else>
                    <p v-if="!(similar[item.hash] ?? []).length" class="text-sm text-gray-500">{{ label('similarEmpty') }}</p>
                    <div v-for="candidate in similar[item.hash] ?? []" :key="candidate.hash" class="rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                      <div class="flex flex-wrap items-center gap-3 text-sm">
                        <span class="break-all font-mono text-xs text-gray-500">{{ candidate.hash }}</span>
                        <span>{{ label('similarDistance') }}: {{ candidate.distance }}</span>
                        <span>{{ candidate.bytes }} B</span>
                        <span>{{ candidate.distinct_users }} · {{ label('distinctUsers') }}</span>
                        <span v-if="candidate.breaker_status">{{ label('breaker' + candidate.breaker_status.charAt(0).toUpperCase() + candidate.breaker_status.slice(1)) }}</span>
                        <span :class="candidate.trusted ? 'text-green-700 dark:text-green-400' : 'text-gray-500'">{{ label(candidate.trusted ? 'breakerTrusted' : 'similarNotTrusted') }}</span>
                        <button type="button" class="btn btn-ghost btn-sm" data-test="similar-compare" @click="selectSimilar(item.hash, candidate.hash)">{{ label('compareWithBreaker') }}</button>
                        <button v-if="!candidate.trusted && candidate.breaker_status !== 'confirmed'" class="btn btn-secondary btn-sm"
                          :disabled="pending === candidate.hash + 'trust'" data-test="similar-trust" @click="act('trust', candidate.hash)">{{ label('trust') }}</button>
                        <button v-if="candidate.breaker_status === 'tripped'" class="btn btn-secondary btn-sm"
                          :disabled="pending === candidate.hash + 'confirm'" data-test="similar-confirm" @click="act('confirm', candidate.hash)">{{ label('confirmViolation') }}</button>
                      </div>
                      <pre class="mt-2 max-h-32 overflow-auto whitespace-pre-wrap break-words text-xs" data-test="similar-preview">{{ candidate.preview }}</pre>
                      <div v-if="comparePreview(item.hash)?.hash === candidate.hash" class="mt-3" data-test="similar-diff">
                        <TextDiff :left="item.preview" :right="candidate.preview" />
                      </div>
                    </div>
                  </template>
                </td>
              </tr>
            </template>
            <tr v-if="!loading && !breakers.length"><td colspan="5" class="p-8 text-center text-gray-500">{{ label('empty') }}</td></tr>
          </tbody>
        </table>
      </div>
      <Pagination :page="breakerPage" :page-size="20" :page-size-options="[20, 50]" :total="breakerTotal" @update:page="changeBreakerPage" />
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
