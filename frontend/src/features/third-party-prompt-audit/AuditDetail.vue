<script setup lang="ts">
defineOptions({ name: 'AuditDetail' })
import { computed, ref, watch } from 'vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { thirdPartyPromptAuditAPI as api, type AuditCapture, type AuditEvent, type JobDetail } from '@/api/admin/third-party-prompt-audit'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatMS, formatTime } from './viewModel'
import { useAuditLabels } from './labels'
import NodeDecision from './NodeDecision.vue'
import CaptureBody from './CaptureBody.vue'

const props = withDefaults(defineProps<{ id: number | null; source: 'events' | 'jobs'; refreshKey?: number }>(), { refreshKey: 0 })
const emit = defineEmits<{ (event: 'close'): void; (event: 'changed'): void; (event: 'reaudit-created', ids: number[]): void }>()
const label = useAuditLabels(), app = useAppStore()
const detail = ref<JobDetail | null>(null), event = ref<AuditEvent | null>(null)
const loading = ref(false), pending = ref(false), error = ref('')
const capture = ref<AuditCapture | null>(null), captureLoading = ref(false)
const selectedReauditID = ref<number | null>(null)
let generation = 0
const job = computed(() => detail.value?.job)
const pretty = (value: unknown) => JSON.stringify(value, null, 2)
async function load() {
  const current = ++generation
  if (!props.id) { detail.value = null; event.value = null; capture.value = null; return }
  loading.value = true
  error.value = ''
  detail.value = null
  event.value = null
  try {
    if (props.source === 'events') {
      const result = await api.event(props.id)
      if (current === generation) { detail.value = result.detail; event.value = result.event }
    } else {
      const result = await api.job(props.id)
      if (current === generation) detail.value = result
    }
  } catch (err) { if (current === generation) error.value = extractApiErrorMessage(err, label('error')) }
  finally { if (current === generation) loading.value = false }
}
function close() { capture.value = null; emit('close') }
async function copy(value: unknown) {
  try { await navigator.clipboard.writeText(typeof value === 'string' ? value : pretty(value)); app.showSuccess(label('copied')) }
  catch { app.showError(label('error')) }
}
async function restore(kind: 'result' | 'action', id: number) {
  pending.value = true
  try {
    if (kind === 'result') await api.resume(id)
    else await api.retryAction(id)
    app.showSuccess(label('resumed'))
    emit('changed')
    await load()
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { pending.value = false }
}
async function showCapture() {
  if (!job.value?.capture_id) return
  captureLoading.value = true
  try { capture.value = await api.capture(job.value.capture_id) }
  catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { captureLoading.value = false }
}
async function reaudit() {
  if (!props.id) return
  pending.value = true
  try {
    const result = await api.reaudit({ source: props.source, filter: { ids: [props.id] } })
    const ids = result.items.flatMap(item => item.job_id && ['requeued', 'already_running'].includes(item.status) ? [item.job_id] : [])
    if (ids.length) {
      const created = result.items.filter(item => item.status === 'requeued').length
      app.showSuccess(created ? `${label('createdJobs')}: ${created}` : label('already_running'))
      emit('reaudit-created', ids)
      emit('changed')
      await load()
    } else {
      app.showError(result.items[0]?.reason || label('noReauditAvailable'))
    }
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { pending.value = false }
}
watch(() => [props.id, props.source, props.refreshKey], load, { immediate: true })
</script>

<template>
  <BaseDialog :show="id !== null" :title="`${label('detail')} #${id ?? ''}`" width="full" :close-on-click-outside="true" @close="close">
    <p v-if="loading" role="status">{{ label('loading') }}</p><p v-if="error" role="alert" class="text-red-600 dark:text-red-400">{{ error }}</p>
    <div v-if="detail && job" class="space-y-6">
      <div class="flex flex-wrap gap-3"><button v-if="job.capture_id" class="btn btn-secondary" :disabled="captureLoading" @click="showCapture">{{ label(captureLoading ? 'loading' : 'viewCapture') }}</button><button class="btn btn-secondary" :disabled="pending" @click="reaudit">{{ label(pending ? 'loading' : 'reaudit') }}</button></div>
      <section class="space-y-3">
        <div class="flex flex-wrap gap-3"><strong>Job #{{ job.id }}</strong><span>{{ label(job.current_run_kind || job.run_kind) }} · {{ label(job.status) }}</span><span>{{ label('auditRound') }} {{ job.audit_round }}</span><span>{{ label(job.execution_mode) }}</span><span>{{ label(job.gateway_result) }}</span></div>
        <p class="break-words text-sm">{{ job.identity.username }} · {{ job.identity.user_email }} · {{ job.identity.api_key_name }} · {{ job.identity.group_name }}</p>
        <p class="break-all text-sm text-gray-500 dark:text-dark-400">{{ job.request_id }} · {{ job.identity.endpoint }} · {{ job.protocol }} · {{ job.requested_model }}</p>
        <p class="break-all text-sm text-gray-500 dark:text-dark-400">Conversation: {{ job.conversation_key || label('conversationUnavailable') }}</p>
        <p class="text-sm">{{ label('created') }}: {{ formatTime(job.created_at) }} · {{ label('attempts') }}: {{ job.attempts }} / {{ job.max_attempts }} · {{ label('inputStatus') }}: {{ label(job.snapshot_status) }}</p>
        <p v-if="job.source_job_id" class="text-sm">{{ label('source') }} Job #{{ job.source_job_id }} · {{ label('userID') }} {{ job.requested_by }}</p>
        <p v-if="event" class="text-sm">{{ label('originalDecision') }}: {{ event.original ? label(event.original.decision) : '—' }} → {{ label('latestDecision') }}: {{ event.latest ? label(event.latest.decision) : '—' }}</p>
        <p v-if="detail.outcome" class="text-sm"><strong>{{ label('decision') }}: {{ label(detail.outcome.decision) }}</strong><span v-if="detail.outcome.partial_failure"> · {{ label('partial_failure') }}</span><span v-if="detail.outcome.source_outcome_id"> · {{ label('sourceReference') }} #{{ detail.outcome.source_outcome_id }}</span></p>
        <div v-if="job.last_error_message" class="rounded-xl bg-red-50 p-3 text-sm dark:bg-red-950/30"><p class="text-red-700 dark:text-red-300">{{ label(job.failure_stage) }} · {{ job.last_error_code }}</p><pre class="mt-2 whitespace-pre-wrap break-words">{{ job.last_error_message }}</pre></div>
        <button v-if="job.status === 'failed' && job.failure_stage === 'result_persist' && job.result_checkpoint" class="btn btn-secondary" :disabled="pending" @click="restore('result', job.id)">{{ label('resume') }}</button>
      </section>
      <section class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-700">
        <div class="flex flex-wrap justify-between gap-3"><h3 class="font-semibold">{{ label('rawInput') }}</h3><button v-if="detail.input_json" class="btn btn-secondary btn-sm" @click="copy(detail.input_json)">{{ label('copy') }}</button></div>
        <p v-if="!detail.input_json" class="text-amber-700 dark:text-amber-400">{{ label('inputNotAvailable') }}</p>
        <template v-else>
          <div v-if="detail.input_parts.length" class="space-y-2"><h4 class="text-sm font-medium">{{ label('manifest') }}</h4><details v-for="part in detail.input_parts" :key="part.order" class="rounded-lg border border-gray-100 p-3 text-sm dark:border-dark-700"><summary class="cursor-pointer break-words">#{{ part.order }} · {{ part.source_role }} → {{ part.policy_role }} · {{ label(part.turn_scope === 'active' ? 'activeInstruction' : part.turn_scope) }} · {{ part.selected ? label('selected') : label('notSelected') }} · {{ part.source_path }}</summary><pre class="mt-3 max-h-96 overflow-auto whitespace-pre-wrap break-words">{{ part.content.map(block => block.text).join('\n') || label('emptyContentNotCalled') }}</pre></details></div>
          <details v-if="detail.audit_target"><summary class="cursor-pointer text-sm">{{ label('actualAuditTarget') }}</summary><pre class="mt-3 max-h-[36rem] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-50 p-4 text-xs dark:bg-dark-900">{{ pretty(detail.audit_target) }}</pre></details>
          <p v-if="detail.non_text?.length" class="text-sm text-amber-700 dark:text-amber-400">{{ label('notText') }}: {{ detail.non_text.map(part => `${part.source_path} (${part.type})`).join(' · ') }}</p>
          <details><summary class="cursor-pointer text-sm">JSON · {{ label('rawInput') }}</summary><pre class="mt-3 max-h-[36rem] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-50 p-4 text-xs dark:bg-dark-900">{{ detail.input_json }}</pre></details>
        </template>
      </section>
      <section class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-700"><h3 class="font-semibold">{{ label('modelsDetail') }}</h3>
        <article v-for="model in detail.outcome?.models ?? job.result_checkpoint?.models ?? []" :key="model.model_id" class="space-y-3 rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
          <strong>{{ model.model_name || model.model_id }}</strong>
          <NodeDecision :model="model" :config="detail.outcome?.decision_config ?? job.decision_config" />
          <p class="text-sm">{{ label('basis') }}: {{ label(model.basis) }} <span v-if="model.reused"> · {{ label(model.segments.some(segment => segment.reuse_kind === 'inflight') ? 'inflight' : 'full_evaluation') }}</span></p><p v-if="model.skipped" class="text-sm text-gray-500">{{ label(model.skip_reason || 'aggregation_decided') }}</p><p class="whitespace-pre-wrap break-words text-sm">{{ model.reason }}</p><p v-if="model.error" class="text-sm text-red-600 dark:text-red-400">{{ model.error.message }} · {{ model.error.code }}</p>
          <details v-for="part in model.segments" :key="part.order" class="text-sm"><summary class="cursor-pointer">#{{ part.order }} · {{ part.result.source_role }} · {{ label(part.reuse_kind) }} · {{ part.result.confidence }} · {{ part.source_path }}</summary><p class="mt-2 whitespace-pre-wrap break-words">{{ part.result.reason }}</p><p class="mt-1 text-xs">Segment #{{ part.result.id }} · Attempt #{{ part.result.source_attempt_id }}</p></details>
        </article>
      </section>
      <section class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-700"><h3 class="font-semibold">{{ label('callsDetail') }}</h3>
        <details v-for="attempt in detail.attempts" :key="attempt.id" class="rounded-lg border border-gray-100 p-3 text-sm dark:border-dark-700"><summary class="cursor-pointer break-words">#{{ attempt.id }} · {{ label('auditRound') }} {{ attempt.audit_round }} · Job #{{ attempt.job_id }} · {{ attempt.model_snapshot.name || attempt.model_id }} · {{ label(attempt.stage) }} · {{ label(attempt.status) }} · HTTP {{ attempt.http_status ?? '—' }} · {{ formatMS(attempt.latency_ms) }}</summary>
          <p class="mt-3">{{ formatTime(attempt.dispatch_started_at) }} — {{ formatTime(attempt.finished_at) }} · Token {{ attempt.input_tokens ?? '—' }} / {{ attempt.output_tokens ?? '—' }} · {{ label('attempts') }} {{ attempt.evaluation_round }}</p><p v-if="attempt.repair_of_attempt_id">{{ label('format_repair') }} ← #{{ attempt.repair_of_attempt_id }}</p>
          <pre v-if="attempt.error_message" class="my-3 whitespace-pre-wrap break-words text-red-600 dark:text-red-400">{{ attempt.error_code }} · {{ attempt.error_message }}</pre>
          <div class="my-3 flex justify-between"><strong>{{ label('rawResponse') }}</strong><button v-if="attempt.raw_response !== null" class="btn btn-secondary btn-sm" @click="copy(attempt.raw_response)">{{ label('copy') }}</button></div><pre class="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-900">{{ attempt.raw_response ?? '—' }}</pre>
          <details class="mt-3"><summary>Request metadata</summary><pre class="mt-2 overflow-auto whitespace-pre-wrap break-words text-xs">{{ pretty(attempt.request_metadata) }}</pre></details>
        </details><p v-if="!detail.attempts.length" class="text-sm text-gray-500">{{ label('empty') }}</p>
      </section>
      <section class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-700"><h3 class="font-semibold">{{ label('actionsDetail') }}</h3>
        <div v-if="detail.enforcement" class="rounded-lg bg-gray-50 p-3 text-sm dark:bg-dark-900"><p>{{ label('warning') }}: {{ label(detail.enforcement.warning_reason) }} · {{ detail.enforcement.window_violations }} / {{ detail.enforcement.window_size }}</p><p class="mt-1">{{ label('disable') }}: {{ label(detail.enforcement.disable_reason) }} · {{ label('disableViolationCount') }} {{ detail.enforcement.disable_violation_count }}</p></div>
        <article v-for="action in detail.actions" :key="action.id" class="space-y-3 rounded-lg border border-gray-100 p-3 text-sm dark:border-dark-700"><p>#{{ action.id }} · {{ label(action.action_type) }} · {{ label(action.execution_status || 'unknown') }} · {{ formatTime(action.applied_at) }} · {{ label(action.notification_status) }}</p><details><summary class="cursor-pointer">{{ label('detail') }}</summary><pre class="mt-2 overflow-auto whitespace-pre-wrap break-words text-xs">{{ pretty({ rule: action.rule_snapshot, transition: action.business_snapshot }) }}</pre></details><details v-if="action.attempt_history?.length"><summary>{{label('accountAttempts')}}</summary><pre class="overflow-auto whitespace-pre-wrap text-xs">{{pretty(action.attempt_history)}}</pre></details><p>{{ label('authCacheStock') }}: {{ label(action.auth_cache_status) }} {{ action.auth_cache_error }}</p><div v-for="delivery in action.deliveries" :key="delivery.recipient + delivery.kind"><p>{{ delivery.recipient }} · {{ label(delivery.status) }} · {{ delivery.last_error }}</p><details><summary>{{ label('attempts') }}: {{ delivery.attempts.length }}</summary><pre class="overflow-auto whitespace-pre-wrap text-xs">{{ pretty(delivery.attempts) }}</pre></details></div><button v-if="action.notification_status === 'failed' || action.execution_status === 'failed' || action.execution_status === 'unknown'" class="btn btn-secondary" :disabled="pending" @click="restore('action', action.id)">{{ label('retryAction') }}</button></article>
      </section>
      <details class="border-t border-gray-200 pt-5 dark:border-dark-700"><summary class="cursor-pointer font-semibold">{{ label('policySnapshot') }}</summary><p class="mt-2 text-sm text-gray-500">{{ label('snapshotHint') }}</p><pre class="mt-3 overflow-auto whitespace-pre-wrap break-words text-xs">{{ pretty(job.config_snapshot) }}</pre></details>
      <section v-if="detail.rounds?.length" class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-700"><h3 class="font-semibold">{{ label('roundHistory') }}</h3><article v-for="round in detail.rounds" :key="round.id" class="rounded-lg border border-gray-100 p-3 text-sm dark:border-dark-700"><p><strong>{{ label('auditRound') }} {{ round.audit_round }}</strong> · {{ label(round.run_kind) }} · {{ label(round.decision) }} · {{ formatMS(round.duration_ms) }}</p><p class="mt-1 text-gray-500">{{ formatTime(round.started_at) }} — {{ formatTime(round.finished_at) }}<span v-if="round.requested_by"> · {{ label('userID') }} {{ round.requested_by }}</span></p><details class="mt-2"><summary class="cursor-pointer">{{ label('modelsDetail') }}</summary><pre class="mt-2 overflow-auto whitespace-pre-wrap break-words text-xs">{{ pretty(round.models) }}</pre></details></article></section>
      <section v-if="detail.reaudits.length" class="space-y-2"><h3 class="font-semibold">{{ label('legacyReaudits') }}</h3><p v-for="child in detail.reaudits" :key="child.id" class="text-sm"><button class="text-primary-600 underline" type="button" @click="selectedReauditID = child.id">#{{ child.id }}</button> · {{ label(child.status) }} · {{ formatTime(child.created_at) }} · {{ child.last_error_message }}</p></section>
    </div>
    <BaseDialog :show="!!capture" :title="label('captureDetail')" :close-on-click-outside="true" @close="capture = null"><CaptureBody v-if="capture" :capture="capture" /></BaseDialog>
    <AuditDetail v-if="selectedReauditID !== null" :id="selectedReauditID" source="jobs" @close="selectedReauditID = null" @changed="load" @reaudit-created="emit('reaudit-created', $event)" />
  </BaseDialog>
</template>
