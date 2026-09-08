<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { thirdPartyPromptAuditAPI as api, type AuditFilter, type AuditRuntime, type AuditStats } from '@/api/admin/third-party-prompt-audit'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatMS, formatTime, sumCounts, timeRange, toLocalInput } from './viewModel'
import { useAuditLabels } from './labels'

const emit = defineEmits<{ (event: 'inspect-jobs', filter: AuditFilter): void }>()
const props = withDefaults(defineProps<{ refreshKey?: number }>(), { refreshKey: 0 })
const label = useAuditLabels()
const from = ref(toLocalInput(new Date(Date.now() - 86400000)))
const to = ref(toLocalInput(new Date()))
const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone
const mode = ref(''), modelID = ref(''), stage = ref('')
const loading = ref(false), error = ref('')
const stats = ref<AuditStats | null>(null), runtime = ref<AuditRuntime | null>(null)
const states = ['queued', 'processing', 'retry', 'done', 'failed', 'skipped']
const summaries = computed(() => stats.value ? [
  { title: 'formal', hint: 'completedHint', values: stats.value.formal, keys: ['pass', 'review', 'block', 'partial_failure'] },
  { title: 'reaudit', hint: 'completedHint', values: stats.value.reaudit, keys: ['pass', 'review', 'block', 'changed'] },
  { title: 'currentEvents', hint: 'eventsHint', values: stats.value.events, keys: ['pass', 'review', 'block'] },
  { title: 'gateway', hint: 'gatewayHint', values: stats.value.gateway, keys: ['continued', 'blocked_here', 'blocked_elsewhere', 'unavailable', 'not_observed'] },
  { title: 'actionWindow', hint: 'completedHint', values: stats.value.actions, keys: ['warning', 'disable', 'counter_reset'] }
] : [])

async function load() {
  const range = timeRange(from.value, to.value)
  if (!range) { error.value = label('invalidRange'); return }
  loading.value = true
  error.value = ''
  const results = await Promise.allSettled([api.stats({ ...range, timezone, mode: mode.value, model_id: modelID.value, stage: stage.value }), api.runtime()])
  if (results[0].status === 'fulfilled') stats.value = results[0].value
  if (results[1].status === 'fulfilled') runtime.value = results[1].value
  error.value = results.filter(result => result.status === 'rejected').map(result => extractApiErrorMessage(result.reason, label('error'))).join(' · ')
  loading.value = false
}
async function refresh() {
  to.value = toLocalInput(new Date())
  await load()
}
onMounted(load)
watch(() => props.refreshKey, () => { void refresh() })
</script>

<template>
  <div class="space-y-5">
    <form class="card flex flex-wrap items-end gap-3 p-4" @submit.prevent="refresh">
      <label class="space-y-2"><span class="text-sm">{{ label('from') }}</span><input v-model="from" class="input" type="datetime-local" required /></label>
      <label class="space-y-2"><span class="text-sm">{{ label('to') }}</span><input v-model="to" class="input" type="datetime-local" required /></label>
      <label class="space-y-2"><span class="text-sm">{{ label('mode') }}</span><select v-model="mode" class="input"><option value="">{{ label('all') }}</option><option value="async">{{ label('async') }}</option><option value="blocking">{{ label('blocking') }}</option></select></label>
      <button type="submit" class="btn btn-primary" :disabled="loading">{{ label(loading ? 'loading' : 'refresh') }}</button><span class="py-2 text-xs text-gray-500 dark:text-dark-400">{{ label('timezone') }}: {{ timezone }}</span>
    </form>
    <p v-if="error" class="rounded-xl bg-red-50 p-4 text-red-700 dark:bg-red-950/30 dark:text-red-300" role="alert">{{ error }}</p>
    <template v-if="stats">
      <section class="card grid gap-5 md:grid-cols-3"><div v-for="(values,title) in {captureStock:stats.capture_stock,forwardingStock:stats.forwarding_stock,accountExecutionStock:stats.action_execution_stock}" :key="title"><h2 class="font-semibold">{{label(title)}}</h2><p class="my-2 text-xs text-gray-500">{{label('allTimeStock')}}</p><p v-for="(count,status) in values" :key="status" class="text-sm">{{label(status)}} · {{count}}</p></div></section>
      <p class="text-sm text-gray-500 dark:text-dark-400">{{ formatTime(stats.from) }} — {{ formatTime(stats.to) }} · {{ stats.timezone }} · {{ label('mode') }}: {{ label(stats.mode || 'all') }} · {{ label('asOf') }}: {{ formatTime(stats.as_of) }}</p>
      <section class="card p-5">
        <h2 class="text-lg font-semibold">{{ label('stock') }}</h2><p class="my-2 text-sm text-gray-500 dark:text-dark-400">{{ label('stockHint') }}</p>
        <div class="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
          <button v-for="state in states" :key="state" class="rounded-xl bg-gray-50 p-4 text-left transition hover:bg-primary-50 dark:bg-dark-900 dark:hover:bg-dark-700" @click="emit('inspect-jobs', { status: state, mode: stats.mode || undefined })"><span class="block text-sm text-gray-500 dark:text-dark-400">{{ label(state) }}</span><strong class="mt-2 block text-2xl tabular-nums">{{ (stats.stock[state] ?? 0).toLocaleString() }}</strong></button>
        </div>
        <p class="mt-4 text-sm">{{ label('active') }}: {{ (stats.stock.queued ?? 0) + (stats.stock.processing ?? 0) + (stats.stock.retry ?? 0) }} · {{ label('waitingSlot') }}: {{ stats.waiting_for_slot }} · {{ label('oldest') }}: {{ stats.oldest_waiting_at ? formatTime(stats.oldest_waiting_at) : label('noWaiting') }}</p>
      </section>
      <section class="card p-5">
        <h2 class="text-lg font-semibold">{{ label('cohort') }}</h2><p class="my-2 text-sm text-gray-500 dark:text-dark-400">{{ label('cohortHint') }}</p>
        <p class="mb-4 text-sm">{{ label('received') }}: {{ stats.received }} + {{ label('reauditsCreated') }}: {{ stats.reaudits_created }} = {{ sumCounts(stats.cohort) }}</p>
        <div class="flex flex-wrap gap-4"><button v-for="state in states" :key="state" class="text-sm text-primary-700 dark:text-primary-400" @click="emit('inspect-jobs', { from: stats.from, to: stats.to, status: state, mode: stats.mode || undefined })">{{ label(state) }} {{ stats.cohort[state] ?? 0 }}</button></div>
      </section>
      <div class="grid gap-5 xl:grid-cols-2">
        <section v-for="group in summaries" :key="group.title" class="card p-5">
          <h2 class="text-lg font-semibold">{{ label(group.title) }}</h2><p class="my-2 text-sm text-gray-500 dark:text-dark-400">{{ label(group.hint) }}</p>
          <dl class="mt-4 flex flex-wrap gap-x-8 gap-y-4"><div v-for="key in group.keys" :key="key"><dt class="text-sm text-gray-500 dark:text-dark-400">{{ label(key) }}</dt><dd class="mt-1 text-xl font-semibold tabular-nums">{{ (group.values[key] ?? 0).toLocaleString() }}</dd></div></dl>
          <p v-if="group.title === 'actionWindow' && runtime" class="mt-3 text-xs text-gray-500">{{ label('warning') }}: {{ label(runtime.warning_enabled ? 'enabled' : 'notEnabled') }} · {{ label('disable') }}: {{ label(runtime.disable_enabled ? 'enabled' : 'notEnabled') }}</p>
          <div v-if="group.title === 'formal'" class="mt-5 border-t border-gray-100 pt-4 text-sm dark:border-dark-700"><strong>{{ label('unclassified') }}: {{ sumCounts(stats.failures) }}</strong><span v-for="(count, failureStage) in stats.failures" :key="failureStage" class="ml-4">{{ label(failureStage) }} {{ count }}</span></div>
          <p v-if="group.title === 'gateway'" class="mt-5 text-sm">{{ label('latency') }}: {{ formatMS(stats.gateway_latency.p50_ms) }} / {{ formatMS(stats.gateway_latency.p95_ms) }} · {{ label('samples') }}: {{ stats.gateway_latency.count }}</p>
        </section>
        <section class="card p-5"><h2 class="text-lg font-semibold">{{ label('reuse') }}</h2>
          <dl class="mt-4 space-y-3 text-sm"><div><dt>{{ label('rounds') }}</dt><dd class="font-semibold">{{ stats.evaluation_rounds }}</dd></div><div><dt>{{ label('wholeReuse') }}</dt><dd class="font-semibold">{{ stats.reuse.whole_hits }} / {{ stats.reuse.whole_lookups }}</dd></div><div><dt>{{ label('segmentReuse') }}</dt><dd class="font-semibold">{{ stats.reuse.segment_hits }} / {{ stats.reuse.segment_lookups }}</dd></div><div><dt>{{ label('withinJob') }}</dt><dd class="font-semibold">{{ stats.reuse.within_job_hits }}</dd></div><div><dt>{{ label('taskLatency') }}</dt><dd>{{ formatMS(stats.task_latency.p50_ms) }} / {{ formatMS(stats.task_latency.p95_ms) }} · n={{ stats.task_latency.count }}</dd></div></dl>
        </section>
      </div>
      <section class="card p-5">
        <h2 class="text-lg font-semibold">{{ label('calls') }}</h2><p class="my-2 text-sm text-gray-500 dark:text-dark-400">{{ label('callHint') }}</p>
        <form class="mb-4 flex flex-wrap items-end gap-3" @submit.prevent="load"><label class="space-y-2 text-sm"><span>{{ label('modelID') }}</span><input v-model="modelID" class="input" /></label><label class="space-y-2 text-sm"><span>{{ label('stage') }}</span><select v-model="stage" class="input"><option value="">{{ label('all') }}</option><option v-for="item in ['segment', 'joint', 'format_repair', 'probe']" :key="item" :value="item">{{ label(item) }}</option></select></label><button class="btn btn-secondary" :disabled="loading">{{ label('apply') }}</button></form>
        <div class="overflow-x-auto"><table class="w-full text-left text-sm"><thead class="text-gray-500 dark:text-dark-400"><tr><th class="p-2">{{ label('modelID') }}</th><th class="p-2">{{ label('source') }} / {{ label('stage') }}</th><th class="p-2">{{ label('total') }}</th><th class="p-2">{{ label('httpSuccess') }} / {{ label('validResult') }}</th><th class="p-2">{{ label('failed') }} / {{ label('unknown') }} / {{ label('inFlight') }}</th><th class="p-2">{{ label('latency') }}</th></tr></thead><tbody><tr v-for="call in stats.calls" :key="`${call.model_id}:${call.call_kind}:${call.stage}`" class="border-t border-gray-100 dark:border-dark-700"><td class="max-w-64 break-all p-2">{{ call.model_id }}</td><td class="p-2">{{ label(call.call_kind) }} / {{ label(call.stage) }}</td><td class="p-2">{{ call.total }}</td><td class="p-2">{{ call.http_success }} / {{ call.valid_result }}</td><td class="p-2"><span>{{ call.failed }} / {{ call.unknown }} / {{ call.in_flight }}</span><details v-if="Object.keys(call.errors).length" class="mt-1"><summary>{{ label('failure') }}</summary><p v-for="(count, code) in call.errors" :key="code">{{ code }}: {{ count }}</p></details></td><td class="p-2">{{ formatMS(call.latency.p50_ms) }} / {{ formatMS(call.latency.p95_ms) }} · n={{ call.latency.count }}</td></tr><tr v-if="!stats.calls.length"><td colspan="6" class="p-8 text-center text-gray-500">{{ label('empty') }}</td></tr></tbody></table></div>
      </section>
      <div class="grid gap-5 lg:grid-cols-3"><section v-for="item in [{ title: 'notificationStock', values: stats.notification_stock }, { title: 'deliveryStock', values: stats.delivery_stock }, { title: 'authCacheStock', values: stats.auth_cache_stock }]" :key="item.title" class="card p-5"><h2 class="font-semibold">{{ label(item.title) }}</h2><dl class="mt-4 space-y-2 text-sm"><div v-for="(count, state) in item.values" :key="state" class="flex justify-between gap-4"><dt>{{ label(state) }}</dt><dd class="font-semibold">{{ count }}</dd></div><p v-if="!Object.keys(item.values).length">{{ label('empty') }}</p></dl></section></div>
    </template>
    <section v-if="runtime" class="card space-y-4 p-5">
      <h2 class="text-lg font-semibold">{{ label('runtime') }}</h2><p class="text-sm text-gray-500 dark:text-dark-400">{{ label('runtimeHint') }}</p>
      <p class="break-all text-xs text-gray-500 dark:text-dark-400">{{ label('instance') }}: {{ runtime.instance_id }} / {{ formatTime(runtime.started_at) }} · {{ label('asOf') }}: {{ formatTime(runtime.as_of) }}</p>
      <dl class="grid gap-4 text-sm sm:grid-cols-2 lg:grid-cols-4"><div><dt>{{ label('status') }}</dt><dd class="font-semibold">{{ label(runtime.running ? 'running' : 'stopped') }} · {{ label(runtime.mode) }}</dd></div><div><dt>{{ label('revision') }}</dt><dd class="font-semibold">{{ runtime.expected_revision }} / {{ runtime.revision }}</dd></div><div><dt>{{ label('worker') }}</dt><dd class="font-semibold">{{ runtime.active_workers }} / {{ runtime.worker_capacity }}</dd></div><div><dt>{{ label('database') }}</dt><dd class="font-semibold">{{ runtime.database_ok ? 'OK' : runtime.database_error }}</dd></div><div><dt>{{ label('inputFailures') }}</dt><dd class="font-semibold">{{ runtime.input_persist_failures }}</dd></div><div><dt>{{ label('lastSuccess') }}</dt><dd>{{ formatTime(runtime.last_success) }}</dd></div></dl>
      <p v-if="runtime.config_error" class="text-sm text-amber-700 dark:text-amber-400">{{ label('applicationError') }}: {{ runtime.config_error }}</p>
      <p v-if="runtime.last_error" class="text-sm text-red-600 dark:text-red-400">{{ label('lastError') }}: {{ formatTime(runtime.last_error.at) }} · {{ runtime.last_error.message }} · {{ runtime.last_error.code }}</p>
      <p class="text-sm">{{ label('evaluationLatency') }}: {{ formatMS(runtime.evaluation_latency.p50_ms) }} / {{ formatMS(runtime.evaluation_latency.p95_ms) }} · {{ label('samples') }}: {{ runtime.evaluation_latency.count }} / {{ runtime.evaluation_latency.capacity }} · {{ formatTime(runtime.evaluation_latency.from) }} — {{ formatTime(runtime.evaluation_latency.to) }}</p>
      <details><summary class="cursor-pointer text-sm">{{ label('lastProbe') }}</summary><p v-for="probe in runtime.probes" :key="probe.model_id" class="mt-2 break-words text-sm">{{ probe.model_id }} · {{ formatTime(probe.tested_at) }} · {{ probe.latency_ms }} ms · {{ probe.ok ? label('succeeded') : probe.error?.message }}</p></details>
    </section>
  </div>
</template>
