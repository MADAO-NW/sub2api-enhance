<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { thirdPartyPromptAuditAPI as api, type AuditFilter, type AuditRuntime, type AuditStats, type ClientChurn, type MetricsHour } from '@/api/admin/third-party-prompt-audit'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatMS, formatTime, timeRange, toLocalInput } from './viewModel'
import { useAuditLabels } from './labels'

const emit = defineEmits<{ (event: 'inspect-jobs', filter: AuditFilter): void }>()
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
const decisions = computed(() => ['pass', 'review', 'block'])
const actionStates = computed(() => Object.entries(stats.value?.action_by_state ?? {}))

async function load() {
  const range = timeRange(from.value, to.value)
  if (!range) { error.value = label('invalidRange'); return }
  loading.value = true
  error.value = ''
  const results = await Promise.allSettled([api.stats({ ...range, timezone, mode: mode.value || undefined }), api.runtime(), api.templateChurn()])
  if (results[0].status === 'fulfilled') stats.value = results[0].value
  if (results[1].status === 'fulfilled') runtime.value = results[1].value
  if (results[2].status === 'fulfilled') churn.value = results[2].value.items
  const historyResult = await Promise.allSettled([api.metricsHistory(7)])
  if (historyResult[0].status === 'fulfilled') history.value = historyResult[0].value.items
  error.value = results.filter(result => result.status === 'rejected').map(result => extractApiErrorMessage(result.reason, label('error'))).join(' · ')
  loading.value = false
}
async function refresh() {
  to.value = toLocalInput(new Date())
  await load()
}
// 每 5 秒按当前时间范围静默刷新；页面不可见或正在加载时跳过。
const autoTimer = window.setInterval(() => {
  if (!props.active || document.visibilityState !== 'visible' || loading.value) return
  void load()
}, 5000)
onUnmounted(() => window.clearInterval(autoTimer))
onMounted(load)
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
    <p v-if="runtime?.self_audit_nodes?.length" class="rounded-xl bg-amber-50 p-4 text-sm text-amber-800 dark:bg-amber-950/30 dark:text-amber-200" data-test="self-audit-warning">
      {{ label('selfAuditWarning') }}: {{ runtime.self_audit_nodes.join('、') }}
    </p>
    <p v-if="runtime?.redis?.check_error" class="rounded-xl bg-red-50 p-4 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-300" data-test="redis-check-error">
      {{ label('redisCheckFailed') }}: {{ runtime.redis.check_error }}
    </p>
    <template v-if="stats">
      <p class="text-sm text-gray-500 dark:text-dark-400">{{ formatTime(stats.from) }} — {{ formatTime(stats.to) }} · {{ stats.timezone }} · {{ label('mode') }}: {{ label(stats.mode || 'all') }}</p>
      <section class="card p-5" data-test="audit-overview-summary">
        <h2 class="text-lg font-semibold">{{ label('firstRequests') }}</h2>
        <p class="my-2 text-sm text-gray-500 dark:text-dark-400">{{ label('firstRequestsHint') }}</p>
        <div class="grid grid-cols-2 gap-3 md:grid-cols-4">
          <button class="rounded-xl bg-gray-50 p-4 text-left transition hover:bg-primary-50 dark:bg-dark-900 dark:hover:bg-dark-700" @click="emit('inspect-jobs', { from: stats.from, to: stats.to, mode: stats.mode || undefined })">
            <span class="block text-sm text-gray-500 dark:text-dark-400">{{ label('jobs') }}</span><strong class="mt-2 block text-2xl tabular-nums">{{ stats.jobs.toLocaleString() }}</strong>
          </button>
          <button class="rounded-xl bg-gray-50 p-4 text-left transition hover:bg-primary-50 dark:bg-dark-900 dark:hover:bg-dark-700" @click="emit('inspect-jobs', { from: stats.from, to: stats.to, run_kind: 'reaudit' })">
            <span class="block text-sm text-gray-500 dark:text-dark-400">{{ label('reauditJobs') }}</span><strong class="mt-2 block text-2xl tabular-nums">{{ stats.reaudit_jobs.toLocaleString() }}</strong>
          </button>
          <button class="rounded-xl bg-gray-50 p-4 text-left transition hover:bg-primary-50 dark:bg-dark-900 dark:hover:bg-dark-700" @click="emit('inspect-jobs', { from: stats.from, to: stats.to, status: 'failed' })">
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
      <div class="grid gap-5 xl:grid-cols-2">
        <section class="card p-5">
          <h2 class="text-lg font-semibold">{{ label('waitingJobs') }}</h2>
          <p class="my-2 text-sm text-gray-500 dark:text-dark-400">{{ label('waitingJobsHint') }}</p>
          <p class="text-sm">{{ label('waitingJobs') }}: {{ stats.waiting_jobs }} · {{ label('oldest') }}: {{ stats.waiting_since ? formatTime(stats.waiting_since) : label('noWaiting') }}</p>
        </section>
        <section class="card p-5">
          <h2 class="text-lg font-semibold">{{ label('actionStock') }}</h2>
          <p class="my-2 text-sm text-gray-500 dark:text-dark-400">{{ label('actionStockHint') }}</p>
          <p class="text-sm">{{ label('pending') }}: {{ stats.action_pending }}</p>
          <dl class="mt-3 flex flex-wrap gap-x-6 gap-y-2 text-sm">
            <div v-for="[state, count] in actionStates" :key="state"><dt class="inline text-gray-500">{{ label(state) }}: </dt><dd class="inline font-semibold">{{ count }}</dd></div>
            <p v-if="!actionStates.length">{{ label('empty') }}</p>
          </dl>
        </section>
        <section class="card p-5">
          <h2 class="text-lg font-semibold">{{ label('blockingWait') }}</h2>
          <p class="my-2 text-sm text-gray-500 dark:text-dark-400">{{ label('blockingWaitHint') }}</p>
          <p class="text-sm">{{ label('jobs') }}: {{ stats.blocking_jobs }} · {{ label('average') }}: {{ formatMS(stats.blocking_avg_ms) }} · P50: {{ formatMS(stats.blocking_p50_ms) }} · P95: {{ formatMS(stats.blocking_p95_ms) }} · {{ label('maximum') }}: {{ formatMS(stats.blocking_max_ms) }}</p>
          <p class="mt-2 text-sm text-gray-500 dark:text-dark-400">{{ label('jobDuration') }}: P50 {{ formatMS(stats.job_p50_ms) }} · P95 {{ formatMS(stats.job_p95_ms) }}</p>
          <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ label('queueWait') }}: P50 {{ formatMS(stats.queue_p50_ms) }} · P95 {{ formatMS(stats.queue_p95_ms) }} · {{ label('processing') }}: P50 {{ formatMS(stats.processing_p50_ms) }} · P95 {{ formatMS(stats.processing_p95_ms) }}</p>
        </section>
        <section class="card p-5" data-test="cache-miss">
          <h2 class="text-lg font-semibold">{{ label('cacheMissRate') }}</h2>
          <p class="my-2 text-sm text-gray-500 dark:text-dark-400">{{ label('cacheMissHint') }}</p>
          <div class="overflow-x-auto">
            <table class="w-full text-left text-sm">
              <thead class="text-gray-500 dark:text-dark-400"><tr><th class="p-2">{{ label('clientFingerprint') }}</th><th class="p-2">{{ label('targets') }}</th><th class="p-2">{{ label('modelCalls') }}</th><th class="p-2">{{ label('missRate') }}</th></tr></thead>
              <tbody>
                <tr v-for="item in stats.cache_miss" :key="`${item.client}:${item.kind}`" class="border-t border-gray-100 dark:border-dark-700">
                  <td class="p-2">{{ label(item.client) }} · {{ label(item.kind) }}</td><td class="p-2">{{ item.targets }}</td><td class="p-2">{{ item.model_calls }}</td>
                  <td class="p-2 font-semibold">{{ Math.round(item.miss_rate * 100) }}%</td>
                </tr>
                <tr v-if="!stats.cache_miss.length"><td colspan="4" class="p-6 text-center text-gray-500">{{ label('empty') }}</td></tr>
              </tbody>
            </table>
          </div>
        </section>
      </div>
    </template>
    <section v-if="history.length" class="card space-y-3 p-5" data-test="metrics-history">
      <h2 class="text-lg font-semibold">{{ label('metricsHistory') }}</h2>
      <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('metricsHistoryHint') }}</p>
      <div class="max-h-72 overflow-auto">
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
    </section>
    <section v-if="churn.length" class="card space-y-3 p-5" data-test="template-churn">
      <h2 class="text-lg font-semibold">{{ label('templateChurn') }}</h2>
      <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('templateChurnHint') }}</p>
      <table class="w-full text-left text-sm">
        <thead class="text-gray-500 dark:text-dark-400"><tr><th class="p-2">{{ label('clientFingerprint') }}</th><th class="p-2">{{ label('jobs') }}</th><th class="p-2">{{ label('distinctBlocks') }}</th><th class="p-2">{{ label('churnRate') }}</th></tr></thead>
        <tbody>
          <tr v-for="item in churn" :key="item.client" class="border-t border-gray-100 dark:border-dark-700">
            <td class="p-2">{{ label(item.client) }}</td><td class="p-2">{{ item.jobs }}</td><td class="p-2">{{ item.distinct }}</td>
            <td class="p-2 font-semibold">{{ Math.round(item.churn_rate * 100) }}%</td>
          </tr>
        </tbody>
      </table>
    </section>
    <section v-if="runtime?.retention" class="card space-y-3 p-5" data-test="retention-card">
      <h2 class="text-lg font-semibold">{{ label('retentionRun') }}</h2>
      <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('retentionRunHint') }}</p>
      <p class="text-sm">{{ formatTime(runtime.retention.finished_at) }} · {{ label('retentionDays') }} {{ runtime.retention.days }} · {{ runtime.retention.duration_ms }} ms</p>
      <p class="text-sm">{{ label('jobs') }} {{ runtime.retention.jobs }} · {{ label('reviews') }} {{ runtime.retention.reviews }} · {{ label('texts') }} {{ runtime.retention.texts }} · {{ label('probeAttempts') }} {{ runtime.retention.probe_attempts }}</p>
      <p v-if="runtime.retention.error" class="text-sm text-red-600 dark:text-red-400">{{ runtime.retention.error }}</p>
    </section>
    <section v-if="runtime" class="card space-y-4 p-5">
      <h2 class="text-lg font-semibold">{{ label('runtime') }}</h2>
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
      <section v-if="runtime.scheduler" class="space-y-3 border-t border-gray-100 pt-4 dark:border-dark-700" data-test="node-health">
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
      <details><summary class="cursor-pointer text-sm">{{ label('lastProbe') }}</summary>
        <p v-for="probe in runtime.probes" :key="probe.model_id" class="mt-2 break-words text-sm">{{ probe.model_id }} · {{ formatTime(probe.tested_at) }} · {{ probe.latency_ms }} ms · {{ probe.ok ? label('succeeded') : probe.error?.message }}</p>
      </details>
    </section>
  </div>
</template>
