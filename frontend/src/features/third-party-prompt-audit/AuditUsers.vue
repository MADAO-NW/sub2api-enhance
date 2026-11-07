<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Pagination from '@/components/common/Pagination.vue'
import {
  thirdPartyPromptAuditAPI as api, type ActionRecord, type AuditJob, type RiskUser, type RiskUserSort,
} from '@/api/admin/third-party-prompt-audit'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { categoryList, formatTime, timeRange, toLocalInput } from './viewModel'
import { useAuditLabels } from './labels'

const props = withDefaults(defineProps<{ refreshKey?: number; active?: boolean; initialRange?: { from: string; to: string } | null }>(), { initialRange: null, refreshKey: 0, active: true })
const emit = defineEmits<{ (event: 'inspect-jobs', filter: Record<string, unknown>): void }>()
const label = useAuditLabels(), app = useAppStore()
const from = ref(toLocalInput(new Date(Date.now() - 86400000)))
const to = ref(toLocalInput(new Date()))
const keyword = ref('')
const sort = ref<RiskUserSort>('blocks')
const rows = ref<RiskUser[]>([])
const page = ref(1), pageSize = ref(20), total = ref(0)
const loading = ref(false), error = ref('')
const expanded = ref<number[]>([])
// 展开行按用户缓存最近任务与处置动作，避免重复请求。
const userJobs = reactive<Record<number, AuditJob[]>>({})
const userActions = reactive<Record<number, ActionRecord[]>>({})
const detailLoading = ref<number | null>(null)
const busy = ref<number | null>(null)
const confirmDelete = ref<RiskUser | null>(null)
const range = computed(() => timeRange(from.value, to.value))

async function load() {
  const current = range.value
  if (!current) { error.value = label('invalidRange'); return }
  loading.value = true
  error.value = ''
  try {
    const result = await api.riskUsers({ ...current, sort: sort.value, keyword: keyword.value || undefined }, page.value, pageSize.value)
    rows.value = result.items
    total.value = result.total
  } catch (err) { error.value = extractApiErrorMessage(err, label('error')) }
  finally { loading.value = false }
}
function apply() { page.value = 1; expanded.value = []; void load() }
function reset() {
  from.value = toLocalInput(new Date(Date.now() - 86400000))
  to.value = toLocalInput(new Date())
  keyword.value = ''
  sort.value = 'blocks'
  page.value = 1
  expanded.value = []
  void load()
}
async function toggle(row: RiskUser) {
  if (expanded.value.includes(row.user_id)) { expanded.value = expanded.value.filter(id => id !== row.user_id); return }
  expanded.value = [...expanded.value, row.user_id]
  if (userJobs[row.user_id] || detailLoading.value === row.user_id) return
  detailLoading.value = row.user_id
  try {
    const [jobs, actions] = await Promise.all([
      api.jobs({ user_id: row.user_id }, 1, 20),
      api.actions({ user_id: row.user_id }, 1, 20),
    ])
    userJobs[row.user_id] = jobs.items
    userActions[row.user_id] = actions.items
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { detailLoading.value = null }
}
function inspectJobs(row: RiskUser) {
  emit('inspect-jobs', { user_id: row.user_id, ...(range.value ?? {}) })
}
async function exportData(row: RiskUser) {
  busy.value = row.user_id
  try {
    const blob = await api.exportUserData(row.user_id)
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = `user-${row.user_id}-third-party-prompt-audit.csv`
    link.click()
    URL.revokeObjectURL(url)
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { busy.value = null }
}
async function removeData() {
  const row = confirmDelete.value
  if (!row) return
  busy.value = row.user_id
  try {
    const result = await api.deleteUserData(row.user_id)
    app.showSuccess(`${label('userDataDeleted')}: ${result.deleted_jobs}`)
    confirmDelete.value = null
    delete userJobs[row.user_id]
    delete userActions[row.user_id]
    await load()
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { busy.value = null }
}
async function enableReset(row: RiskUser) {
  busy.value = row.user_id
  try {
    const result = await api.enableAndReset(row.user_id)
    app.showSuccess(`${label('actionSubmitted')} #${result.action_id}`)
    await load()
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { busy.value = null }
}
async function clearEscalation(row: RiskUser) {
  busy.value = row.user_id
  try {
    await api.clearEscalation(row.user_id)
    app.showSuccess(label('escalationCleared'))
    await load()
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { busy.value = null }
}
function changePage(value: number) { page.value = value; void load() }
function changeSize(value: number) { pageSize.value = value; page.value = 1; void load() }
watch(sort, () => apply())
// 从概览“风险用户 Top 5”跳转时带上概览的时间范围。
watch(() => props.initialRange, value => {
  if (!value) return
  from.value = toLocalInput(new Date(value.from))
  to.value = toLocalInput(new Date(value.to))
  page.value = 1
  void load()
}, { immediate: true })
watch(() => props.refreshKey, () => { void load() })
// 沿用其他页签的 5 秒静默刷新；页面不可见、正在加载或展开明细时跳过。
const autoTimer = window.setInterval(() => {
  if (!props.active || document.visibilityState !== 'visible' || loading.value || busy.value || detailLoading.value) return
  void load()
}, 5000)
onUnmounted(() => window.clearInterval(autoTimer))
onMounted(() => { void load() })
</script>

<template>
  <div class="space-y-5">
    <form class="card flex flex-wrap items-end gap-3 p-5" @submit.prevent="apply">
      <label class="space-y-1 text-sm"><span>{{ label('from') }}</span><input v-model="from" class="input" type="datetime-local" required /></label>
      <label class="space-y-1 text-sm"><span>{{ label('to') }}</span><input v-model="to" class="input" type="datetime-local" required /></label>
      <label class="min-w-56 flex-1 space-y-1 text-sm"><span>{{ label('keyword') }}</span><input v-model="keyword" class="input" :placeholder="label('keyword')" data-test="risk-keyword" /></label>
      <label class="space-y-1 text-sm"><span>{{ label('sortBy') }}</span>
        <select v-model="sort" class="input" data-test="risk-sort">
          <option v-for="item in ['blocks', 'violations', 'last_block_at']" :key="item" :value="item">{{ label(`sort_${item}`) }}</option>
        </select>
      </label>
      <button class="btn btn-primary" :disabled="loading">{{ label(loading ? 'loading' : 'apply') }}</button>
      <button type="button" class="btn btn-secondary" data-test="risk-reset" @click="reset">{{ label('resetFilters') }}</button>
    </form>
    <p v-if="error" role="alert" class="text-red-600 dark:text-red-400">{{ error }}</p>
    <section class="card space-y-3 p-5">
      <div>
        <h2 class="text-lg font-semibold">{{ label('userRisk') }}</h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ label('userRiskHint') }}</p>
      </div>
      <div class="overflow-x-auto">
        <table class="w-full text-left text-sm">
          <thead class="bg-gray-50 text-gray-500 dark:bg-dark-900 dark:text-dark-400"><tr>
            <th class="p-3">{{ label('user') }}</th>
            <th class="p-3">{{ label('accountStatus') }}</th>
            <th class="p-3">{{ label('jobs') }}</th>
            <th class="p-3">{{ label('blocks') }}</th>
            <th class="p-3">{{ label('reviewCount') }}</th>
            <th class="p-3">{{ label('violations') }}</th>
            <th class="p-3">{{ label('lastBlockAt') }}</th>
            <th class="p-3">{{ label('category') }}</th>
            <th class="p-3">{{ label('detail') }}</th>
          </tr></thead>
          <tbody>
            <template v-for="row in rows" :key="row.user_id">
              <tr class="border-t border-gray-100 align-top dark:border-dark-700">
                <td class="min-w-48 max-w-72 break-words p-3">
                  <p>(#{{ row.user_id }}) {{ row.username || '—' }}</p>
                  <p class="text-xs text-gray-500">{{ row.email }}</p>
                </td>
                <td class="p-3">{{ label(row.role === 'admin' ? 'userAdmin' : 'userRegular') }} · {{ label(row.status === 'active' ? 'userActive' : 'userDisabled') }}
                  <span v-if="row.escalated" class="mt-1 block text-xs text-amber-700 dark:text-amber-400">{{ label('escalated') }}</span>
                </td>
                <td class="p-3 tabular-nums">{{ row.jobs.toLocaleString() }}</td>
                <td class="p-3 tabular-nums">{{ row.blocks.toLocaleString() }}</td>
                <td class="p-3 tabular-nums">{{ row.reviews.toLocaleString() }}</td>
                <td class="p-3 tabular-nums">{{ row.violations.toLocaleString() }}<span v-if="row.pending_actions" class="mt-1 block text-xs text-amber-700 dark:text-amber-400">{{ label('pendingActions') }} {{ row.pending_actions }}</span></td>
                <td class="whitespace-nowrap p-3">{{ formatTime(row.last_block_at) }}</td>
                <td class="p-3 text-xs">
                  <span v-for="category in categoryList(row.categories)" :key="category" class="mr-1 inline-block rounded bg-gray-100 px-2 py-0.5 dark:bg-dark-700">{{ category }}</span>
                  <span v-if="!categoryList(row.categories).length" class="text-gray-500">{{ label('uncategorized') }}</span>
                </td>
                <td class="p-3"><button class="btn btn-secondary btn-sm whitespace-nowrap" :data-test="`risk-toggle-${row.user_id}`" @click="toggle(row)">{{ label(expanded.includes(row.user_id) ? 'collapse' : 'expand') }}</button></td>
              </tr>
              <tr v-if="expanded.includes(row.user_id)" class="border-t border-gray-100 bg-gray-50 dark:border-dark-700 dark:bg-dark-900">
                <td colspan="9" class="space-y-4 p-4">
                  <div class="grid gap-3 text-sm md:grid-cols-3">
                    <p>{{ label('accountStatus') }}: {{ label(row.status === 'active' ? 'userActive' : 'userDisabled') }}</p>
                    <p>{{ label('disableViolationCount') }}: {{ row.disable_violation_count }}</p>
                    <p>{{ label('lastCounterReset') }}: {{ formatTime(row.disable_reset_at) }}</p>
                    <p>{{ label('escalated') }}: {{ label(row.escalated ? 'escalated' : 'notEscalated') }}</p>
                    <p>{{ label('pendingActions') }}: {{ row.pending_actions }}</p>
                  </div>
                  <div class="flex flex-wrap gap-2">
                    <button class="btn btn-secondary btn-sm" @click="inspectJobs(row)">{{ label('allJobsOfUser') }}</button>
                    <button class="btn btn-secondary btn-sm" :disabled="busy === row.user_id" data-test="risk-export" @click="exportData(row)">{{ label('exportUserData') }}</button>
                    <button class="btn btn-secondary btn-sm" :disabled="busy === row.user_id" data-test="risk-delete" @click="confirmDelete = row">{{ label('deleteUserData') }}</button>
                    <button class="btn btn-secondary btn-sm" :disabled="busy === row.user_id || row.pending_actions > 0" data-test="risk-enable-reset" @click="enableReset(row)">{{ label('enableAndReset') }}</button>
                    <button v-if="row.escalated" class="btn btn-secondary btn-sm" :disabled="busy === row.user_id" data-test="risk-clear-escalation" @click="clearEscalation(row)">{{ label('clearEscalation') }}</button>
                  </div>
                  <p v-if="detailLoading === row.user_id" role="status">{{ label('loading') }}</p>
                  <div v-else class="grid gap-5 xl:grid-cols-2">
                    <section class="space-y-2">
                      <h3 class="text-sm font-medium">{{ label('recentJobs') }}</h3>
                      <p v-for="job in userJobs[row.user_id] ?? []" :key="job.id" class="flex flex-wrap gap-2 text-xs">
                        <span>#{{ job.id }}</span><span>{{ formatTime(job.created_at) }}</span><span>{{ label(job.status) }}</span>
                        <span v-if="job.decision">{{ label(job.decision) }}</span>
                      </p>
                      <p v-if="!(userJobs[row.user_id] ?? []).length" class="text-xs text-gray-500">{{ label('empty') }}</p>
                    </section>
                    <section class="space-y-2">
                      <h3 class="text-sm font-medium">{{ label('recentActions') }}</h3>
                      <p v-for="action in userActions[row.user_id] ?? []" :key="action.id" class="flex flex-wrap gap-2 text-xs">
                        <span>#{{ action.id }}</span><span>{{ label(action.action_type) }}</span><span>{{ label(action.execution_status || 'unknown') }}</span>
                        <span>{{ formatTime(action.requested_at) }}</span>
                      </p>
                      <p v-if="!(userActions[row.user_id] ?? []).length" class="text-xs text-gray-500">{{ label('empty') }}</p>
                    </section>
                  </div>
                </td>
              </tr>
            </template>
            <tr v-if="!loading && !rows.length"><td colspan="9" class="p-10 text-center text-gray-500">{{ label('empty') }}</td></tr>
          </tbody>
        </table>
      </div>
      <Pagination :page="page" :page-size="pageSize" :page-size-options="[20, 50, 100]" :total="total" @update:page="changePage" @update:page-size="changeSize" />
    </section>
    <BaseDialog :show="confirmDelete !== null" :title="label('deleteUserData')" :close-on-click-outside="true" @close="confirmDelete = null">
      <p class="text-sm text-red-700 dark:text-red-300">{{ label('deleteUserDataHint') }}</p>
      <p class="mt-2 text-sm text-gray-500 dark:text-dark-400">{{ label('deleteUserDataKeeps') }}</p>
      <template #footer><div class="flex gap-3"><button class="btn btn-primary" :disabled="busy !== null" data-test="confirm-delete-user" @click="removeData">{{ label('confirmSave') }}</button><button class="btn btn-secondary" @click="confirmDelete = null">{{ label('cancelChanges') }}</button></div></template>
    </BaseDialog>
  </div>
</template>
