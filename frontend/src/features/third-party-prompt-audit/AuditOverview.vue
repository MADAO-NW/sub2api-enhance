<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import {
  thirdPartyPromptAuditAPI as api, type AuditFilter, type AuditJob, type AuditRuntime, type AuditStats, type ClientChurn,
  type CostReport, type Inbox, type InboxCategoryKey, type MetricsHour, type RiskUser,
} from '@/api/admin/third-party-prompt-audit'
import { extractApiErrorMessage } from '@/utils/apiError'
import { categoryList, costSummary, formatMS, formatTime, timeRange, toLocalInput } from './viewModel'
import { useAuditLabels } from './labels'
import TrendChart from './TrendChart.vue'

const emit = defineEmits<{
  (event: 'inspect-jobs', filter: AuditFilter): void
  (event: 'open-inbox', category: InboxCategoryKey | ''): void
  (event: 'open-users', range: { from: string; to: string }): void
}>()
const props = withDefaults(defineProps<{ refreshKey?: number; active?: boolean }>(), { refreshKey: 0, active: true })
const label = useAuditLabels()
const from = ref(toLocalInput(new Date(Date.now() - 86400000)))
const to = ref(toLocalInput(new Date()))
const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone
const mode = ref('')
const loading = ref(false), error = ref('')
const stats = ref<AuditStats | null>(null), runtime = ref<AuditRuntime | null>(null)
const churn = ref<ClientChurn[]>([])
const history = ref<MetricsHour[]>([])
const inbox = ref<Inbox | null>(null)
const cost = ref<CostReport | null>(null)
const topUsers = ref<RiskUser[]>([])
const blockedJobs = ref<AuditJob[]>([])
const decisions = computed(() => ['pass', 'review', 'block'])
const actionStates = computed(() => Object.entries(stats.value?.action_by_state ?? {}))
const costTotals = computed(() => costSummary(cost.value))
const trendSeries = [{ key: 'jobs', name: label('jobs'), color: '#6366f1' }, { key: 'blocked', name: label('blocked'), color: '#ef4444' },
  { key: 'failed', name: label('failed'), color: '#f59e0b' }]
// 小时趋势：最近 48 个快照，标签只显示到小时。
const trendPoints = computed(() => history.value.slice(-48).map(item => ({
  label: new Date(item.hour).toLocaleString([], { month: '2-digit', day: '2-digit', hour: '2-digit' }),
  values: { jobs: item.jobs, blocked: item.blocked, failed: item.failed },
})))
// 类别分布来自最近 200 条阻断任务的 trigger_categories 聚合。
const categoryCounts = computed(() => {
  const counts = new Map<string, number>()
  for (const job of blockedJobs.value) {
    const categories = categoryList(job.trigger_categories)
    if (!categories.length) counts.set('', (counts.get('') ?? 0) + 1)
    for (const category of categories) counts.set(category, (counts.get(category) ?? 0) + 1)
  }
  return [...counts.entries()].sort((a, b) => b[1] - a[1]).map(([category, count]) => ({ category, count }))
})
// 客户端规则集健康度：未命中率与指令块变更率按客户端合并。
const rulesetHealth = computed(() => (stats.value?.cache_miss ?? []).map(item => ({
  ...item,
  churn: churn.value.find(entry => entry.client === item.client)?.churn_rate ?? null,
})))
const inboxCategories = computed(() => inbox.value?.categories ?? [])
// 运行诊断默认折叠，展开状态存本机。
const storageKey = 'enhance.third-party-prompt-audit.diagnostics'
const diagnosticsOpen = ref(false)
try { diagnosticsOpen.value = window.localStorage.getItem(storageKey) === '1' } catch { diagnosticsOpen.value = false }
watch(diagnosticsOpen, value => { try { window.localStorage.setItem(storageKey, value ? '1' : '0') } catch { /* 隐私模式下忽略存储失败 */ } })

async function load(silent = false) {
  const range = timeRange(from.value, to.value)
  if (!range) { error.value = label('invalidRange'); return }
  if (!silent) loading.value = true
  error.value = ''
  const core = Promise.allSettled([api.stats({ ...range, timezone, mode: mode.value || undefined }), api.runtime(), api.inbox()])
  const extra = silent ? null : Promise.allSettled([api.templateChurn(), api.metricsHistory(7), api.cost({ ...range, timezone, bucket: 'day' }),
    api.riskUsers({ ...range, sort: 'blocks' }, 1, 5), api.jobs({ ...range, decision: 'block' }, 1, 200)])
  const failures: string[] = []
  const [statsResult, runtimeResult, inboxResult] = await core
  if (statsResult.status === 'fulfilled') stats.value = statsResult.value
  else failures.push(extractApiErrorMessage(statsResult.reason, label('error')))
  if (runtimeResult.status === 'fulfilled') runtime.value = runtimeResult.value
  else failures.push(extractApiErrorMessage(runtimeResult.reason, label('error')))
  if (inboxResult.status === 'fulfilled') inbox.value = inboxResult.value
  else failures.push(extractApiErrorMessage(inboxResult.reason, label('error')))
  if (extra) {
    const [churnResult, historyResult, costResult, usersResult, blockedResult] = await extra
    if (churnResult.status === 'fulfilled') churn.value = churnResult.value.items
    else failures.push(extractApiErrorMessage(churnResult.reason, label('error')))
    if (historyResult.status === 'fulfilled') history.value = historyResult.value.items
    if (costResult.status === 'fulfilled') cost.value = costResult.value
    else failures.push(extractApiErrorMessage(costResult.reason, label('error')))
    if (usersResult.status === 'fulfilled') topUsers.value = usersResult.value.items
    if (blockedResult.status === 'fulfilled') blockedJobs.value = blockedResult.value.items
  }
  error.value = failures.join(' · ')
  if (!silent) loading.value = false
}
async function refresh() {
  to.value = toLocalInput(new Date())
  await load()
}
function inspect(filter: AuditFilter) { emit('inspect-jobs', filter) }
// 每 5 秒按当前时间范围静默刷新核心指标；页面不可见或正在加载时跳过。
const autoTimer = window.setInterval(() => {
  if (!props.active || document.visibilityState !== 'visible' || loading.value) return
  void load(true)
}, 5000)
onUnmounted(() => window.clearInterval(autoTimer))
onMounted(() => { void load() })
watch(() => props.refreshKey, () => { void refresh() })
</script>

<template>
  <div class="space-y-5">
    <form class="card flex flex-wrap items-end gap-3 p-4" @submit.prevent="refresh">
      <label class="space-y-2"><span class="text-sm">{{ label('from') }}</span><input v-model="from" class="input" type="datetime-local" required /></label>
      <label class="space-y-2"><span class="text-sm">{{ label('to') }}</span><input v-model="to" class="input" type="datetime-local" required /></label>
      <label class="space-y-2"><span class="text-sm">{{ label('mode') }}</span>
        <select v-model="mode" class="input"><option value="">{{ label('all') }}</option><option value="async">{{ label('async') }}</option><option value="blocking">{{ label('blocking') }}</option></select>
      </label>
      <button type="submit" class="btn btn-primary" :disabled="loading">{{ label(loading ? 'loading' : 'refresh') }}</button>
      <span class="py-2 text-xs text-gray-500 dark:text-dark-400">{{ label('timezone') }}: {{ timezone }}</span>
    </form>
    <p v-if="error" class="rounded-xl bg-red-50 p-4 text-red-700 dark:bg-red-950/30 dark:text-red-300" role="alert">{{ error }}</p>
    <p v-if="runtime?.redis?.check_error" class="rounded-xl bg-red-50 p-4 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-300" data-test="redis-check-error">
      {{ label('redisCheckFailed') }}: {{ runtime.redis.check_error }}
    </p>
    <template v-if="stats">
      <p class="text-sm text-gray-500 dark:text-dark-400">{{ formatTime(stats.from) }} — {{ formatTime(stats.to) }} · {{ stats.timezone }} · {{ label('mode') }}: {{ label(stats.mode || 'all') }}</p>
      <section class="card p-5" data-test="audit-overview-summary">
        <h2 class="text-lg font-semibold">{{ label('riskOverview') }}</h2>
        <p class="my-2 text-sm text-gray-500 dark:text-dark-400">{{ label('firstRequestsHint') }}</p>
        <div class="grid grid-cols-2 gap-3 md:grid-cols-4">
          <button class="rounded-xl bg-gray-50 p-4 text-left transition hover:bg-primary-50 dark:bg-dark-900 dark:hover:bg-dark-700" @click="inspect({ from: stats.from, to: stats.to, mode: stats.mode || undefined })">
            <span class="block text-sm text-gray-500 dark:text-dark-400">{{ label('jobs') }}</span><strong class="mt-2 block text-2xl tabular-nums">{{ stats.jobs.toLocaleString() }}</strong>
          </button>
          <button class="rounded-xl bg-gray-50 p-4 text-left transition hover:bg-primary-50 dark:bg-dark-900 dark:hover:bg-dark-700" @click="inspect({ from: stats.from, to: stats.to, run_kind: 'reaudit' })">
            <span class="block text-sm text-gray-500 dark:text-dark-400">{{ label('reauditJobs') }}</span><strong class="mt-2 block text-2xl tabular-nums">{{ stats.reaudit_jobs.toLocaleString() }}</strong>
          </button>
          <button class="rounded-xl bg-gray-50 p-4 text-left transition hover:bg-primary-50 dark:bg-dark-900 dark:hover:bg-dark-700" @click="inspect({ from: stats.from, to: stats.to, status: 'failed' })">
            <span class="block text-sm text-gray-500 dark:text-dark-400">{{ label('failed') }}</span><strong class="mt-2 block text-2xl tabular-nums">{{ stats.failed_jobs.toLocaleString() }}</strong>
          </button>
          <div class="rounded-xl bg-gray-50 p-4 text-left dark:bg-dark-900">
            <span class="block text-sm text-gray-500 dark:text-dark-400">{{ label('orphanContinuation') }}</span><strong class="mt-2 block text-2xl tabular-nums">{{ stats.orphan_jobs.toLocaleString() }}</strong>
          </div>
        </div>
        <dl class="mt-5 flex flex-wrap gap-x-8 gap-y-4">
          <div v-for="item in decisions" :key="item">
            <dt class="text-sm text-gray-500 dark:text-dark-400">{{ label(item) }}</dt>
            <dd class="mt-1 text-xl font-semibold tabular-nums">{{ (stats.decisions[item] ?? 0).toLocaleString() }}</dd>
          </div>
        </dl>
      </section>
      <section class="card space-y-4 p-5" data-test="pending-summary">
        <div>
          <h2 class="text-lg font-semibold">{{ label('pendingSummary') }}</h2>
          <p class="mt-2 text-sm text-gray-500 dark:text-dark-400">{{ label('pendingSummaryHint') }}</p>
        </div>
        <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-5">
          <button v-for="category in inboxCategories" :key="category.key" class="rounded-xl bg-gray-50 p-4 text-left transition hover:bg-primary-50 dark:bg-dark-900 dark:hover:bg-dark-700"
            :data-test="`overview-inbox-${category.key}`" @click="emit('open-inbox', category.key)">
            <span class="block text-sm text-gray-500 dark:text-dark-400">{{ label(category.key) }}</span>
            <strong class="mt-2 block text-2xl tabular-nums">{{ category.count.toLocaleString() }}</strong>
          </button>
        </div>
        <div v-if="stats.action_by_state && Object.keys(stats.action_by_state).length" class="border-t border-gray-100 pt-4 dark:border-dark-700">
          <h3 class="text-sm font-medium">{{ label('actionStock') }}</h3>
          <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ label('actionStockHint') }}</p>
          <dl class="mt-2 flex flex-wrap gap-x-6 gap-y-2 text-sm">
            <div v-for="[state, count] in actionStates" :key="state">
              <dt class="inline text-gray-500">{{ label(state) }}: </dt>
              <dd class="inline font-semibold">
                <button class="underline" :data-test="`overview-action-${state}`" @click="emit('open-inbox', state === 'failed' ? 'action_failed' : '')">{{ count }}</button>
              </dd>
            </div>
          </dl>
        </div>
      </section>
      <section v-if="topUsers.length" class="card space-y-3 p-5" data-test="risk-users-top">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div>
            <h2 class="text-lg font-semibold">{{ label('riskUsersTop') }}</h2>
            <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ label('riskUsersTopHint') }}</p>
          </div>
          <button class="btn btn-secondary" data-test="overview-open-users" @click="emit('open-users', { from: stats.from, to: stats.to })">{{ label('all') }}</button>
        </div>
        <table class="w-full text-left text-sm">
          <thead class="text-gray-500 dark:text-dark-400"><tr>
            <th class="p-2">{{ label('user') }}</th><th class="p-2">{{ label('jobs') }}</th><th class="p-2">{{ label('blocks') }}</th>
            <th class="p-2">{{ label('violations') }}</th><th class="p-2">{{ label('category') }}</th>
          </tr></thead>
          <tbody>
            <tr v-for="user in topUsers" :key="user.user_id" class="border-t border-gray-100 dark:border-dark-700">
              <td class="p-2">(#{{ user.user_id }}) {{ user.username || '—' }}</td>
              <td class="p-2 tabular-nums">{{ user.jobs }}</td><td class="p-2 tabular-nums">{{ user.blocks }}</td>
              <td class="p-2 tabular-nums">{{ user.violations }}</td>
              <td class="p-2 text-xs">{{ categoryList(user.categories).join(', ') || label('uncategorized') }}</td>
            </tr>
          </tbody>
        </table>
      </section>
      <section v-if="cost" class="card space-y-4 p-5" data-test="audit-cost">
        <div>
          <h2 class="text-lg font-semibold">{{ label('costCard') }}</h2>
          <p class="mt-2 text-sm text-gray-500 dark:text-dark-400">{{ label('costHint') }}</p>
        </div>
        <div class="overflow-x-auto">
          <table class="w-full text-left text-sm">
            <thead class="text-gray-500 dark:text-dark-400"><tr>
              <th class="p-2">{{ label('costNode') }}</th><th class="p-2">{{ label('costAuditCalls') }}</th><th class="p-2">{{ label('costRepairCalls') }}</th>
              <th class="p-2">{{ label('costProbeCalls') }}</th><th class="p-2">{{ label('costInputTokens') }}</th><th class="p-2">{{ label('costOutputTokens') }}</th>
              <th class="p-2">{{ label('costUnknownUsage') }}</th>
            </tr></thead>
            <tbody>
              <tr v-for="row in costTotals.nodes" :key="row.model_id" class="border-t border-gray-100 dark:border-dark-700">
                <td class="p-2">{{ row.node_name || row.model_id }}</td>
                <td class="p-2 tabular-nums">{{ row.audit_calls.toLocaleString() }}</td>
                <td class="p-2 tabular-nums">{{ row.repair_calls.toLocaleString() }}</td>
                <td class="p-2 tabular-nums">{{ row.probe_calls.toLocaleString() }}</td>
                <td class="p-2 tabular-nums">{{ row.input_tokens.toLocaleString() }}</td>
                <td class="p-2 tabular-nums">{{ row.output_tokens.toLocaleString() }}</td>
                <td class="p-2 tabular-nums">{{ row.unknown_usage.toLocaleString() }}</td>
              </tr>
              <tr v-if="!costTotals.nodes.length"><td colspan="7" class="p-6 text-center text-gray-500">{{ label('costEmpty') }}</td></tr>
            </tbody>
          </table>
        </div>
        <dl class="flex flex-wrap gap-x-8 gap-y-3 text-sm">
          <div><dt class="text-gray-500 dark:text-dark-400">{{ label('costReuseSummary') }}</dt>
            <dd class="font-semibold tabular-nums">{{ costTotals.reuse_targets.toLocaleString() }} / {{ costTotals.reuse_total_targets.toLocaleString() }}<span v-if="costTotals.reuse_total_targets" class="ml-1 text-gray-500">({{ Math.round(costTotals.reuse_targets / costTotals.reuse_total_targets * 100) }}%)</span></dd></div>
          <div v-for="row in costTotals.reuse" :key="`${row.kind}:${row.resolution}`">
            <dt class="text-gray-500 dark:text-dark-400">{{ label(row.kind) }} · {{ label('resolution_' + row.resolution) }}</dt>
            <dd class="font-semibold tabular-nums">{{ row.targets.toLocaleString() }} · {{ Math.round(row.share * 100) }}%</dd>
          </div>
        </dl>
      </section>
      <section v-if="categoryCounts.length" class="card space-y-3 p-5" data-test="category-distribution">
        <div>
          <h2 class="text-lg font-semibold">{{ label('categoryDistribution') }}</h2>
          <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ label('categoryDistributionHint') }}</p>
        </div>
        <div class="flex flex-wrap gap-2">
          <button v-for="item in categoryCounts" :key="item.category" class="rounded-xl bg-gray-50 px-4 py-3 text-left transition hover:bg-primary-50 dark:bg-dark-900 dark:hover:bg-dark-700"
            @click="inspect(item.category ? { from: stats.from, to: stats.to, decision: 'block', category: item.category } : { from: stats.from, to: stats.to, decision: 'block' })">
            <span class="block text-sm">{{ item.category || label('uncategorized') }}</span>
            <strong class="mt-1 block text-xl tabular-nums">{{ item.count.toLocaleString() }}</strong>
          </button>
        </div>
      </section>
      <section v-if="history.length" class="card space-y-3 p-5" data-test="metrics-history">
        <div>
          <h2 class="text-lg font-semibold">{{ label('trendChart') }}</h2>
          <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ label('metricsHistoryHint') }}</p>
        </div>
        <TrendChart :points="trendPoints" :series="trendSeries" />
        <details>
          <summary class="cursor-pointer text-sm">{{ label('viewData') }}</summary>
          <div class="mt-3 max-h-72 overflow-auto">
            <table class="w-full text-left text-sm">
              <thead class="text-gray-500 dark:text-dark-400"><tr><th class="p-2">{{ label('hour') }}</th><th class="p-2">{{ label('jobs') }}</th><th class="p-2">{{ label('blocked') }}</th><th class="p-2">{{ label('failed') }}</th><th class="p-2">{{ label('average') }}</th></tr></thead>
              <tbody>
                <tr v-for="item in history.slice(-48).reverse()" :key="item.hour" class="border-t border-gray-100 dark:border-dark-700">
                  <td class="p-2">{{ formatTime(item.hour) }}</td><td class="p-2">{{ item.jobs }}</td><td class="p-2">{{ item.blocked }}</td>
                  <td class="p-2">{{ item.failed }}</td><td class="p-2">{{ formatMS(item.avg_blocking_ms) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </details>
      </section>
      <section class="card space-y-4 p-5" data-test="run-diagnostics">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div>
            <h2 class="text-lg font-semibold">{{ label('runDiagnostics') }}</h2>
            <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ label('runDiagnosticsHint') }}</p>
          </div>
          <button class="btn btn-secondary" data-test="toggle-diagnostics" :aria-expanded="diagnosticsOpen" @click="diagnosticsOpen = !diagnosticsOpen">{{ label(diagnosticsOpen ? 'collapse' : 'expand') }}</button>
        </div>
        <div v-if="diagnosticsOpen" class="space-y-5">
          <section class="space-y-2">
            <h3 class="font-semibold">{{ label('waitingJobs') }}</h3>
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('waitingJobsHint') }}</p>
            <p class="text-sm">{{ label('waitingJobs') }}: {{ stats.waiting_jobs }} · {{ label('oldest') }}: {{ stats.waiting_since ? formatTime(stats.waiting_since) : label('noWaiting') }}</p>
          </section>
          <section class="space-y-2">
            <h3 class="font-semibold">{{ label('blockingWait') }}</h3>
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('blockingWaitHint') }}</p>
            <p class="text-sm">{{ label('jobs') }}: {{ stats.blocking_jobs }} · {{ label('average') }}: {{ formatMS(stats.blocking_avg_ms) }} · P50: {{ formatMS(stats.blocking_p50_ms) }} · P95: {{ formatMS(stats.blocking_p95_ms) }} · {{ label('maximum') }}: {{ formatMS(stats.blocking_max_ms) }}</p>
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('jobDuration') }}: P50 {{ formatMS(stats.job_p50_ms) }} · P95 {{ formatMS(stats.job_p95_ms) }}</p>
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('queueWait') }}: P50 {{ formatMS(stats.queue_p50_ms) }} · P95 {{ formatMS(stats.queue_p95_ms) }} · {{ label('processing') }}: P50 {{ formatMS(stats.processing_p50_ms) }} · P95 {{ formatMS(stats.processing_p95_ms) }}</p>
          </section>
          <section v-if="runtime?.retention" class="space-y-2" data-test="retention-card">
            <h3 class="font-semibold">{{ label('retentionRun') }}</h3>
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('retentionRunHint') }}</p>
            <p class="text-sm">{{ formatTime(runtime.retention.finished_at) }} · {{ label('retentionDays') }} {{ runtime.retention.days }} · {{ runtime.retention.duration_ms }} ms</p>
            <p class="text-sm">{{ label('jobs') }} {{ runtime.retention.jobs }} · {{ label('reviews') }} {{ runtime.retention.reviews }} · {{ label('texts') }} {{ runtime.retention.texts }} · {{ label('probeAttempts') }} {{ runtime.retention.probe_attempts }}</p>
            <p v-if="runtime.retention.error" class="text-sm text-red-600 dark:text-red-400">{{ runtime.retention.error }}</p>
          </section>
          <section v-if="runtime" class="space-y-3">
            <h3 class="font-semibold">{{ label('runtime') }}</h3>
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('runtimeHint') }}</p>
            <p class="break-all text-xs text-gray-500 dark:text-dark-400">{{ label('instance') }}: {{ runtime.instance_id }} / {{ formatTime(runtime.started_at) }} · {{ label('asOf') }}: {{ formatTime(runtime.as_of) }}</p>
            <dl class="grid gap-4 text-sm sm:grid-cols-2 lg:grid-cols-4">
              <div><dt>{{ label('status') }}</dt><dd class="font-semibold">{{ label(runtime.running ? 'running' : 'stopped') }} · {{ label(runtime.mode) }}</dd></div>
              <div><dt>{{ label('revision') }}</dt><dd class="font-semibold">{{ runtime.expected_revision }} / {{ runtime.revision }}</dd></div>
              <div><dt>{{ label('worker') }}</dt><dd class="font-semibold">{{ runtime.active_workers }} / {{ runtime.worker_capacity }}</dd></div>
              <div><dt>{{ label('database') }}</dt><dd class="font-semibold">{{ runtime.database_ok ? 'OK' : runtime.database_error }}</dd></div>
              <div><dt>{{ label('inputFailures') }}</dt><dd class="font-semibold">{{ runtime.intake_failures }}</dd></div>
              <div><dt>{{ label('lastSuccess') }}</dt><dd>{{ formatTime(runtime.last_success) }}</dd></div>
            </dl>
            <p v-if="runtime.config_error" class="text-sm text-amber-700 dark:text-amber-400">{{ label('applicationError') }}: {{ runtime.config_error }}</p>
            <p v-if="runtime.last_error" class="text-sm text-red-600 dark:text-red-400">{{ label('lastError') }}: {{ formatTime(runtime.last_error.at) }} · {{ runtime.last_error.message }} · {{ runtime.last_error.code }}</p>
            <p v-if="runtime.redis" class="text-sm" :class="runtime.redis.ok ? '' : 'text-red-600 dark:text-red-400'">Redis: {{ runtime.redis.status }} · TTL {{ runtime.redis.ttl_seconds }}s<span v-if="runtime.redis.error"> · {{ runtime.redis.error }}</span></p>
            <p class="text-sm">{{ label('evaluationLatency') }}: {{ formatMS(runtime.evaluation_latency.p50_ms) }} / {{ formatMS(runtime.evaluation_latency.p95_ms) }} · {{ label('samples') }}: {{ runtime.evaluation_latency.count }} / {{ runtime.evaluation_latency.capacity }}</p>
          </section>
          <section v-if="runtime?.scheduler" class="space-y-3 border-t border-gray-100 pt-4 dark:border-dark-700" data-test="node-health">
            <h3 class="font-semibold">{{ label('nodeHealth') }}</h3>
            <p class="text-xs text-gray-500">{{ label('nodeHealthHint') }}</p>
            <p class="text-sm">{{ label('backpressuredTotal') }}: {{ runtime.scheduler.backpressured_total }} · {{ label('blockingWaiters') }}: {{ runtime.scheduler.blocking_waiters }}</p>
            <div class="overflow-x-auto">
              <table class="w-full text-left text-sm">
                <thead class="text-gray-500"><tr><th class="p-2">{{ label('modelID') }}</th><th class="p-2">{{ label('health') }}</th><th class="p-2">{{ label('nodeLoad') }}</th><th class="p-2">{{ label('waiting') }}</th><th class="p-2">{{ label('failureCount') }}</th><th class="p-2">{{ label('latency') }}</th><th class="p-2">{{ label('nextProbe') }}</th></tr></thead>
                <tbody>
                  <tr v-for="node in runtime.scheduler.nodes" :key="node.model_id" class="border-t border-gray-100 dark:border-dark-700">
                    <td class="p-2">{{ node.model_id }}</td><td class="p-2">{{ label(node.health) }}</td>
                    <td class="p-2">{{ node.active }} / {{ node.max_concurrency }} · {{ Math.round(node.load_ratio * 100) }}%</td>
                    <td class="p-2">{{ node.eligible_waiters }}</td>
                    <td class="p-2">{{ node.consecutive_failures }}<span v-if="node.last_error_code" class="block text-xs text-red-600">{{ node.last_error_code }}</span></td>
                    <td class="p-2">{{ formatMS(node.latency_ewma_ms) }}</td><td class="p-2">{{ formatTime(node.next_probe_at) }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </section>
          <section class="space-y-3 border-t border-gray-100 pt-4 dark:border-dark-700" data-test="cache-miss">
            <h3 class="font-semibold">{{ label('clientRulesetHealth') }}</h3>
            <p class="text-xs text-gray-500">{{ label('clientRulesetHealthHint') }}</p>
            <div class="overflow-x-auto">
              <table class="w-full text-left text-sm">
                <thead class="text-gray-500 dark:text-dark-400"><tr>
                  <th class="p-2">{{ label('clientFingerprint') }}</th><th class="p-2">{{ label('targetKind') }}</th><th class="p-2">{{ label('targetCount') }}</th>
                  <th class="p-2">{{ label('realCalls') }}</th><th class="p-2">{{ label('missRate') }}</th><th class="p-2">{{ label('churnRate') }}</th>
                </tr></thead>
                <tbody>
                  <tr v-for="item in rulesetHealth" :key="`${item.client}:${item.kind}`" class="border-t border-gray-100 dark:border-dark-700">
                    <td class="p-2">{{ label(item.client) }}</td><td class="p-2">{{ label(item.kind) }}</td><td class="p-2 tabular-nums">{{ item.targets }}</td>
                    <td class="p-2 tabular-nums">{{ item.model_calls }}</td><td class="p-2 font-semibold">{{ Math.round(item.miss_rate * 100) }}%</td>
                    <td class="p-2 font-semibold">{{ item.churn === null ? '—' : `${Math.round(item.churn * 100)}%` }}</td>
                  </tr>
                  <tr v-if="!rulesetHealth.length"><td colspan="6" class="p-6 text-center text-gray-500">{{ label('empty') }}</td></tr>
                </tbody>
              </table>
            </div>
          </section>
          <section class="space-y-3 border-t border-gray-100 pt-4 dark:border-dark-700" data-test="category-recognition">
            <h3 class="font-semibold">{{ label('categoryRecognition') }}</h3>
            <p class="text-xs text-gray-500">{{ label('categoryRecognitionHint') }}</p>
            <div class="overflow-x-auto">
              <table class="w-full text-left text-sm">
                <thead class="text-gray-500 dark:text-dark-400"><tr>
                  <th class="p-2">{{ label('node') }}</th><th class="p-2">{{ label('targetCount') }}</th>
                  <th class="p-2">{{ label('recognized') }}</th><th class="p-2">{{ label('recognitionRate') }}</th>
                </tr></thead>
                <tbody>
                  <tr v-for="item in stats?.category_recognition ?? []" :key="item.model_id" class="border-t border-gray-100 dark:border-dark-700">
                    <td class="p-2">{{ item.model_id }}</td><td class="p-2 tabular-nums">{{ item.total }}</td>
                    <td class="p-2 tabular-nums">{{ item.recognized }}</td><td class="p-2 font-semibold">{{ Math.round(item.rate * 100) }}%</td>
                  </tr>
                  <tr v-if="!(stats?.category_recognition ?? []).length"><td colspan="4" class="p-6 text-center text-gray-500">{{ label('empty') }}</td></tr>
                </tbody>
              </table>
            </div>
          </section>
          <details v-if="runtime?.probes?.length">
            <summary class="cursor-pointer text-sm">{{ label('lastProbe') }}</summary>
            <p v-for="probe in runtime.probes" :key="probe.model_id" class="mt-2 break-words text-sm">{{ probe.model_id }} · {{ formatTime(probe.tested_at) }} · {{ probe.latency_ms }} ms · {{ probe.ok ? label('succeeded') : probe.error?.message }}</p>
          </details>
        </div>
      </section>
    </template>
  </div>
</template>
