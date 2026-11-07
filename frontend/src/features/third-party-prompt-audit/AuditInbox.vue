<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import Pagination from '@/components/common/Pagination.vue'
import {
  thirdPartyPromptAuditAPI as api, type ActionFilter, type ActionRecord, type Inbox, type InboxCategoryKey,
} from '@/api/admin/third-party-prompt-audit'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatTime } from './viewModel'
import { useAuditLabels } from './labels'

const props = withDefaults(defineProps<{ refreshKey?: number; active?: boolean; initialCategory?: InboxCategoryKey | '' }>(), { refreshKey: 0, active: true, initialCategory: '' })
const emit = defineEmits<{
  (event: 'inspect-jobs', filter: { ids: number[] }): void
  (event: 'open-trust', hash: string): void
  (event: 'open-config', nodeID: string): void
  (event: 'changed'): void
}>()
const label = useAuditLabels(), app = useAppStore()
const inbox = ref<Inbox | null>(null)
const loading = ref(false), error = ref(''), pending = ref('')
const selected = ref<InboxCategoryKey | ''>(props.initialCategory)
// 动作列表筛选：分类卡片点击后映射到对应状态。
const actionFilter = reactive<ActionFilter>({})
const actions = ref<ActionRecord[]>([])
const actionPage = ref(1), actionSize = ref(20), actionTotal = ref(0)
const actionLoading = ref(false)
const categories = computed(() => inbox.value?.categories ?? [])
// 分类卡片点击后同时过滤下方的处置动作列表。

async function load(silent = false) {
  if (!silent) loading.value = true
  const results = await Promise.allSettled([api.inbox()])
  if (results[0].status === 'fulfilled') inbox.value = results[0].value
  if (results[0].status === 'rejected' && !silent) error.value = extractApiErrorMessage(results[0].reason, label('error'))
  if (!silent) loading.value = false
  await loadActions(silent)
}
async function loadActions(silent = false) {
  if (!silent) actionLoading.value = true
  try {
    const result = await api.actions(actionFilter, actionPage.value, actionSize.value)
    actions.value = result.items
    actionTotal.value = result.total
    if (!silent) error.value = ''
  } catch (err) { if (!silent) error.value = extractApiErrorMessage(err, label('error')) }
  finally { if (!silent) actionLoading.value = false }
}
function selectCategory(key: InboxCategoryKey) {
  selected.value = selected.value === key ? '' : key
  delete actionFilter.execution_status
  delete actionFilter.notification_status
  if (selected.value === 'action_failed') actionFilter.execution_status = 'failed'
  if (selected.value === 'notification_failed') actionFilter.notification_status = 'failed'
  actionPage.value = 1
  void loadActions()
}
function applyFilter() { actionPage.value = 1; void loadActions() }
function resetFilter() {
  selected.value = ''
  for (const key of Object.keys(actionFilter)) delete actionFilter[key as keyof ActionFilter]
  actionPage.value = 1
  void loadActions()
}
async function retry(id: number) {
  pending.value = `retry-${id}`
  try {
    await api.retryAction(id)
    app.showSuccess(label('resumed'))
    emit('changed')
    await load()
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { pending.value = '' }
}
function openJob(jobID: number | null | undefined) {
  if (!jobID) return
  emit('inspect-jobs', { ids: [jobID] })
}
function changePage(value: number) { actionPage.value = value; void loadActions() }
function changeSize(value: number) { actionSize.value = value; actionPage.value = 1; void loadActions() }
watch(() => props.refreshKey, () => { void load() })
// 概览的“处置动作库存”数字会带着分类跳到本页，切换页签后仍要跟随。
watch(() => props.initialCategory, value => {
  if (!value) return
  selected.value = ''
  selectCategory(value)
}, { immediate: true })
// 沿用概览的 5 秒静默刷新；页面不可见、正在加载或有待完成操作时跳过。
const autoTimer = window.setInterval(() => {
  if (!props.active || document.visibilityState !== 'visible' || loading.value || pending.value) return
  void load(true)
}, 5000)
onUnmounted(() => window.clearInterval(autoTimer))
onMounted(() => { void load() })
</script>

<template>
  <div class="space-y-5">
    <p v-if="error" role="alert" class="text-red-600 dark:text-red-400">{{ error }}</p>
    <section class="card space-y-4 p-5">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 class="text-lg font-semibold">{{ label('inbox') }}</h2>
          <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ label('inboxHint') }}</p>
        </div>
        <p class="text-sm">{{ label('inboxTotal') }}: <strong class="text-xl tabular-nums">{{ inbox?.total ?? 0 }}</strong></p>
      </div>
      <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-5">
        <button v-for="category in categories" :key="category.key" type="button"
          class="rounded-xl p-4 text-left transition"
          :class="selected === category.key ? 'bg-primary-600 text-white' : 'bg-gray-50 hover:bg-primary-50 dark:bg-dark-900 dark:hover:bg-dark-700'"
          :data-test="`inbox-category-${category.key}`" @click="selectCategory(category.key)">
          <span class="block text-sm" :class="selected === category.key ? '' : 'text-gray-500 dark:text-dark-400'">{{ label(category.key) }}</span>
          <strong class="mt-2 block text-2xl tabular-nums">{{ category.count.toLocaleString() }}</strong>
        </button>
      </div>
      <div v-if="inbox?.failure_rate" class="rounded-xl bg-amber-50 p-4 text-sm text-amber-800 dark:bg-amber-950/30 dark:text-amber-200" data-test="inbox-failure-rate">
        {{ label('failure_rate') }} · {{ label('failureRateWindow') }} {{ inbox.failure_rate.window_minutes }} · {{ inbox.failure_rate.failed }} / {{ inbox.failure_rate.jobs }} · {{ Math.round(inbox.failure_rate.rate * 100) }}% · {{ label('failureRateThreshold') }} {{ Math.round(inbox.failure_rate.threshold * 100) }}%
      </div>
      <div v-for="category in categories" :key="`items-${category.key}`" class="space-y-2">
        <template v-if="category.items.length">
          <h3 class="text-sm font-medium">{{ label(category.key) }}</h3>
          <div v-for="item in category.items" :key="`${category.key}-${item.id ?? item.hash ?? item.occurred_at}`"
            class="flex flex-wrap items-center gap-3 rounded-lg border border-gray-100 p-3 text-sm dark:border-dark-700">
            <span class="text-gray-500 dark:text-dark-400">{{ formatTime(item.occurred_at) }}</span>
            <span v-if="item.id">#{{ item.id }}</span>
            <span v-if="item.action_type">{{ label(item.action_type) }}</span>
            <span v-if="item.username">{{ item.username }}<span v-if="item.user_id" class="text-gray-500"> (#{{ item.user_id }})</span></span>
            <span v-if="item.status">{{ item.status }}</span>
            <span v-if="item.detail" class="break-all text-gray-500 dark:text-dark-400">{{ item.detail }}</span>
            <button v-if="category.key === 'action_failed' || category.key === 'notification_failed'" class="btn btn-secondary btn-sm"
              :disabled="pending === `retry-${item.id}` || !item.id" data-test="inbox-retry" @click="retry(item.id ?? 0)">{{ label('retryNow') }}</button>
            <button v-if="item.job_id" class="btn btn-secondary btn-sm" data-test="inbox-view-job" @click="openJob(item.job_id)">{{ label('viewJob') }}</button>
            <button v-if="category.key === 'breaker_pending' && item.hash" class="btn btn-secondary btn-sm" @click="emit('open-trust', item.hash!)">{{ label('trustTab') }}</button>
            <button v-if="category.key === 'node_unhealthy' && item.detail" class="btn btn-secondary btn-sm" @click="emit('open-config', item.detail!)">{{ label('config') }}</button>
          </div>
        </template>
      </div>
      <p v-if="inbox && !inbox.total" class="text-sm text-gray-500 dark:text-dark-400">{{ label('noItems') }}</p>
    </section>
    <section class="card space-y-4 p-5" data-test="inbox-actions">
      <div>
        <h2 class="text-lg font-semibold">{{ label('actionList') }}</h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ label('actionListHint') }}</p>
      </div>
      <form class="flex flex-wrap items-end gap-3" @submit.prevent="applyFilter">
        <label class="space-y-1 text-sm"><span>{{ label('executionStatus') }}</span>
          <select v-model="actionFilter.execution_status" class="input">
            <option value="">{{ label('all') }}</option>
            <option v-for="status in ['pending', 'running', 'succeeded', 'failed', 'unknown']" :key="status" :value="status">{{ label(status) }}</option>
          </select>
        </label>
        <label class="space-y-1 text-sm"><span>{{ label('notificationStatus') }}</span>
          <select v-model="actionFilter.notification_status" class="input">
            <option value="">{{ label('all') }}</option>
            <option v-for="status in ['not_required', 'pending', 'sent', 'failed']" :key="status" :value="status">{{ label(status) }}</option>
          </select>
        </label>
        <label class="space-y-1 text-sm"><span>{{ label('actionType') }}</span>
          <select v-model="actionFilter.action_type" class="input">
            <option value="">{{ label('all') }}</option>
            <option v-for="type in ['warning', 'disable', 'counter_reset', 'blocking_notice', 'escalation', 'escalation_clear']" :key="type" :value="type">{{ label(type) }}</option>
          </select>
        </label>
        <label class="space-y-1 text-sm"><span>{{ label('userID') }}</span><input v-model.number="actionFilter.user_id" class="input w-28" type="number" min="1" /></label>
        <button class="btn btn-primary" :disabled="actionLoading">{{ label(actionLoading ? 'loading' : 'apply') }}</button>
        <button type="button" class="btn btn-secondary" data-test="inbox-reset" @click="resetFilter">{{ label('resetFilters') }}</button>
      </form>
      <div class="overflow-x-auto">
        <table class="w-full text-left text-sm">
          <thead class="text-gray-500 dark:text-dark-400"><tr>
            <th class="p-2">{{ label('created') }}</th>
            <th class="p-2">{{ label('user') }}</th>
            <th class="p-2">{{ label('actionType') }}</th>
            <th class="p-2">{{ label('executionStatus') }} / {{ label('notificationStatus') }}</th>
            <th class="p-2">{{ label('actions') }}</th>
          </tr></thead>
          <tbody>
            <tr v-for="action in actions" :key="action.id" class="border-t border-gray-100 align-top dark:border-dark-700">
              <td class="whitespace-nowrap p-2">{{ formatTime(action.requested_at) }}<span class="mt-1 block text-xs text-gray-500">#{{ action.id }}</span></td>
              <td class="p-2">{{ action.username || `#${action.user_id}` }}<span class="mt-1 block text-xs text-gray-500">#{{ action.user_id }}</span></td>
              <td class="p-2">{{ label(action.action_type) }}</td>
              <td class="p-2">{{ label(action.execution_status || 'unknown') }} / {{ label(action.notification_status) }}</td>
              <td class="space-y-2 p-2">
                <button class="btn btn-secondary btn-sm" :disabled="pending === `retry-${action.id}`" data-test="action-retry" @click="retry(action.id)">{{ label('retryNow') }}</button>
                <button v-if="action.job_id" class="btn btn-secondary btn-sm" @click="openJob(action.job_id)">{{ label('viewJob') }}</button>
              </td>
            </tr>
            <tr v-if="!actionLoading && !actions.length"><td colspan="5" class="p-8 text-center text-gray-500">{{ label('empty') }}</td></tr>
          </tbody>
        </table>
      </div>
      <Pagination :page="actionPage" :page-size="actionSize" :page-size-options="[20, 50]" :total="actionTotal"
        @update:page="changePage" @update:page-size="changeSize" />
    </section>
  </div>
</template>
