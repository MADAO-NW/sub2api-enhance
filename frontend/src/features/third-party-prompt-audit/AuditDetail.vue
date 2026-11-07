<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { thirdPartyPromptAuditAPI as api, type AuditAction, type JobDetail, type JobTarget, type ModelAttempt, type SampleExpected } from '@/api/admin/third-party-prompt-audit'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { categoryList, formatMS, formatTime } from './viewModel'
import { useAuditLabels } from './labels'
import NodeDecision from './NodeDecision.vue'

const props = withDefaults(defineProps<{ id: number | null; refreshKey?: number }>(), { refreshKey: 0 })
const emit = defineEmits<{ (event: 'close'): void; (event: 'changed'): void; (event: 'reaudit-created', ids: number[]): void }>()
const label = useAuditLabels(), app = useAppStore()
const detail = ref<JobDetail | null>(null)
const loading = ref(false), pending = ref(false), error = ref('')
const sampling = ref('')
let generation = 0
const job = computed(() => detail.value?.job)
const pretty = (value: unknown) => JSON.stringify(value, null, 2)
function attemptRequest(attempt: ModelAttempt) {
  if (!attempt.request_metadata || typeof attempt.request_metadata !== 'object' || Array.isArray(attempt.request_metadata)) return null
  return (attempt.request_metadata as Record<string, unknown>).request_body ?? null
}
async function load() {
  const current = ++generation
  if (!props.id) { detail.value = null; return }
  loading.value = true
  error.value = ''
  detail.value = null
  try {
    const result = await api.job(props.id)
    if (current !== generation) return
    detail.value = result
  } catch (err) {
    if (current === generation) error.value = extractApiErrorMessage(err, label('error'))
  } finally {
    if (current === generation) loading.value = false
  }
}
function close() { emit('close') }
async function copy(value: unknown) {
  try { await navigator.clipboard.writeText(typeof value === 'string' ? value : pretty(value)); app.showSuccess(label('copied')) }
  catch { app.showError(label('error')) }
}
async function retryAction(action: AuditAction) {
  pending.value = true
  try {
    await api.retryAction(action.id)
    app.showSuccess(label('resumed'))
    emit('changed')
    await load()
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { pending.value = false }
}
async function toggleTrust(target: JobTarget) {
  pending.value = true
  try {
    if (target.trusted) await api.untrust(target.hash)
    else await api.trust(target.hash, `任务 #${props.id} 详情手工信任`)
    app.showSuccess(label('saved'))
    emit('changed')
    await load()
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { pending.value = false }
}
// 加入误判样本集：hash 与类型由后端按任务目标解析，这里只提交任务、目标序号与期望结论。
async function addSample(target: JobTarget, expected: SampleExpected) {
  if (!props.id) return
  sampling.value = `${target.order}:${expected}`
  try {
    await api.createSample({ job_id: props.id, target_order: target.order, expected })
    target.sampled = true
    app.showSuccess(label('sampleAdded'))
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { sampling.value = '' }
}
async function reaudit() {
  if (!props.id) return
  pending.value = true
  try {
    const result = await api.reaudit({ filter: { ids: [props.id] } })
    const ids = result.items.flatMap(item => ['requeued', 'already_running'].includes(item.status) ? [item.job_id] : [])
    if (ids.length) {
      app.showSuccess(label('createdJobs'))
      emit('reaudit-created', ids)
      emit('changed')
      await load()
    } else {
      app.showError(result.items[0]?.reason || label('noReauditAvailable'))
    }
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { pending.value = false }
}
// 轮次历史里 fresh 显示为“重新审核”，旧的 allow/force 保留原文案。
function reuseLabel(mode: string): string {
  return mode === 'fresh' ? 'reuseFresh' : mode === 'force' ? 'reuseForce' : 'reuseAllow'
}
watch(() => [props.id, props.refreshKey], load, { immediate: true })
</script>

<template>
  <BaseDialog :show="id !== null" :title="`${label('detail')} #${id ?? ''}`" width="full" :close-on-click-outside="true" @close="close">
    <p v-if="loading" role="status">{{ label('loading') }}</p>
    <p v-if="error" role="alert" class="text-red-600 dark:text-red-400">{{ error }}</p>
    <div v-if="detail && job" class="space-y-6">
      <div class="flex flex-wrap items-center gap-3">
        <button class="btn btn-secondary" :disabled="pending" @click="reaudit">{{ label(pending ? 'loading' : 'reaudit') }}</button>
      </div>
      <section class="space-y-3">
        <div class="flex flex-wrap gap-3">
          <strong>Job #{{ job.id }}</strong>
          <span>{{ label(job.current_run_kind) }} · {{ label(job.status) }}</span>
          <span>{{ label('auditRound') }} {{ job.current_round }}</span>
          <span>{{ label(job.mode) }}</span>
          <strong v-if="job.decision" :class="job.decision === 'block' ? 'text-red-600 dark:text-red-400' : job.decision === 'review' ? 'text-amber-700 dark:text-amber-400' : ''">{{ label(job.decision) }}</strong>
        </div>
        <p class="break-words text-sm">{{ job.identity.username }} · {{ job.identity.user_email }} · {{ job.identity.api_key_name }} · {{ job.identity.group_name }}</p>
        <p class="break-all text-sm text-gray-500 dark:text-dark-400">{{ label('requestID') }}: {{ job.request_id || '—' }} · {{ job.identity.endpoint }} · {{ job.protocol }} · {{ job.model }}</p>
        <p class="text-sm">{{ label('category') }}: <span v-for="category in categoryList(job.trigger_categories)" :key="category" class="mr-2 inline-block rounded bg-gray-100 px-2 py-0.5 text-xs dark:bg-dark-700">{{ category }}</span><span v-if="!categoryList(job.trigger_categories).length" class="text-gray-500">{{ label('uncategorized') }}</span></p>
        <p class="text-sm">{{ label('created') }}: {{ formatTime(job.created_at) }} · {{ label('attempts') }}: {{ job.attempts }} / {{ job.max_attempts }} · {{ label('clientFingerprint') }}: {{ job.client_fingerprint }}</p>
        <p v-if="job.orphan_continuation" class="text-sm text-amber-700 dark:text-amber-400">{{ label('orphanContinuation') }}</p>
        <p class="text-sm">{{ label('enforcement') }}: {{ job.enforcement_eligible ? label('eligible') : label('notEligible') }}<span v-if="job.violation_counted"> · {{ label('violationCounted') }}</span><span v-if="job.disable_counted"> · {{ label('disableCounted') }}</span></p>
        <div v-if="job.last_error_message" class="rounded-xl bg-red-50 p-3 text-sm dark:bg-red-950/30">
          <p class="text-red-700 dark:text-red-300">{{ label(job.failure_stage) }} · {{ job.last_error_code }}</p>
          <pre class="mt-2 whitespace-pre-wrap break-words">{{ job.last_error_message }}</pre>
        </div>
      </section>
      <section class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-700">
        <h3 class="font-semibold">{{ label('targets') }}</h3>
        <p class="text-xs text-gray-500 dark:text-dark-400">{{ label('targetsHint') }}</p>
        <article v-for="target in detail.targets" :key="target.order" class="space-y-3 rounded-xl bg-gray-50 p-4 dark:bg-dark-900">
          <div class="flex flex-wrap items-center gap-3 text-sm">
            <strong>#{{ target.order }} · {{ label(target.kind) }}</strong>
            <span>{{ target.bytes }} B</span>
            <span class="break-all font-mono text-xs text-gray-500">{{ target.hash }}</span>
            <span v-if="target.resolution">{{ label('resolution') }}: {{ label('resolution_' + target.resolution) }}</span>
            <strong v-if="target.decision" :class="target.decision === 'block' ? 'text-red-600 dark:text-red-400' : target.decision === 'review' ? 'text-amber-700 dark:text-amber-400' : ''">{{ label(target.decision) }}</strong>
            <span v-if="target.sampled" class="text-xs text-primary-600 dark:text-primary-400" data-test="target-sampled">{{ label('sampledMark') }}</span>
            <button v-if="target.kind === 'instruction'" class="btn btn-secondary btn-sm" :disabled="pending" @click="toggleTrust(target)">{{ label(target.trusted ? 'untrust' : 'trust') }}</button>
            <button class="btn btn-secondary btn-sm" :disabled="sampling === `${target.order}:pass`" data-test="detail-sample-pass" @click="addSample(target, 'pass')">{{ label('addSamplePass') }}</button>
            <button class="btn btn-secondary btn-sm" :disabled="sampling === `${target.order}:block`" data-test="detail-sample-block" @click="addSample(target, 'block')">{{ label('addSampleBlock') }}</button>
          </div>
          <p v-if="target.error_code" class="text-sm text-red-600 dark:text-red-400">{{ target.error_code }} · {{ target.error_message }}</p>
          <details open><summary class="cursor-pointer text-sm font-medium">{{ label('auditedText') }}</summary>
            <pre class="mt-2 max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-white p-3 text-xs dark:bg-dark-950">{{ target.text || '—' }}</pre>
          </details>
          <div v-if="target.review" class="space-y-2">
            <p class="text-sm">{{ label('score') }}: {{ target.review.score ?? '—' }} · {{ target.review.reason }}</p>
            <div v-for="node in target.review.nodes" :key="node.model_id" class="rounded-lg border border-gray-100 p-3 text-sm dark:border-dark-700">
              <NodeDecision :node="node" />
            </div>
          </div>
        </article>
        <p v-if="!detail.targets.length" class="text-sm text-gray-500">{{ label('empty') }}</p>
      </section>
      <section class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-700">
        <h3 class="font-semibold">{{ label('actionsDetail') }}</h3>
        <article v-for="action in detail.actions" :key="action.id" class="space-y-3 rounded-lg border border-gray-100 p-3 text-sm dark:border-dark-700">
          <p>#{{ action.id }} · {{ label(action.action_type) }} · {{ label(action.execution_status || 'unknown') }} · {{ formatTime(action.applied_at) }} · {{ label(action.notification_status) }}</p>
          <details><summary class="cursor-pointer">{{ label('detail') }}</summary><pre class="mt-2 overflow-auto whitespace-pre-wrap break-words text-xs">{{ pretty({ rule: action.rule_snapshot, transition: action.business_snapshot }) }}</pre></details>
          <div v-for="delivery in action.deliveries" :key="delivery.recipient + delivery.kind">
            <p>{{ delivery.recipient }} · {{ label(delivery.status) }} · {{ delivery.last_error }}</p>
          </div>
          <button v-if="['failed', 'unknown'].includes(action.execution_status || '') || action.notification_status === 'failed'" class="btn btn-secondary" :disabled="pending" @click="retryAction(action)">{{ label('retryAction') }}</button>
        </article>
        <p v-if="!detail.actions.length" class="text-sm text-gray-500">{{ label('empty') }}</p>
      </section>
      <section v-if="detail.rounds.length" class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-700">
        <h3 class="font-semibold">{{ label('roundHistory') }}</h3>
        <article v-for="round in detail.rounds" :key="round.round" class="rounded-lg border border-gray-100 p-3 text-sm dark:border-dark-700">
          <p><strong>{{ label('auditRound') }} {{ round.round }}</strong> · {{ label(round.run_kind) }} · {{ label(round.decision) }} · {{ label(reuseLabel(round.reuse_mode)) }} · {{ label('trigger') }}: {{ round.trigger_kind ? label(round.trigger_kind) : '—' }}<span v-if="round.trigger_categories?.length"> · {{ categoryList(round.trigger_categories).join(', ') }}</span></p>
          <p class="mt-1 text-gray-500">{{ formatTime(round.started_at) }} — {{ formatTime(round.finished_at) }}<span v-if="round.requested_by"> · {{ label('userID') }} {{ round.requested_by }}</span><span v-if="round.config_revision"> · {{ label('revision') }} {{ round.config_revision }}</span></p>
        </article>
      </section>
      <section class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-700">
        <div class="flex flex-wrap justify-between gap-3"><h3 class="font-semibold">{{ label('callsDetail') }}</h3></div>
        <p v-if="!detail.attempts.length" class="text-sm text-gray-500">{{ label('callsInJobHint') }}</p>
        <details v-for="attempt in detail.attempts" :key="attempt.id" class="rounded-lg border border-gray-100 p-3 text-sm dark:border-dark-700">
          <summary class="cursor-pointer break-words">#{{ attempt.id }} · {{ label('auditRound') }} {{ attempt.audit_round }} · {{ attempt.model_snapshot.name || attempt.model_id }} · {{ label(attempt.stage) }} · {{ label(attempt.status) }} · HTTP {{ attempt.http_status ?? '—' }} · {{ formatMS(attempt.latency_ms) }}</summary>
          <pre v-if="attempt.error_message" class="my-3 whitespace-pre-wrap break-words text-red-600 dark:text-red-400">{{ attempt.error_code }} · {{ attempt.error_message }}</pre>
          <details v-if="attemptRequest(attempt)" class="my-3"><summary class="cursor-pointer font-medium">{{ label('actualModelRequest') }}</summary><pre class="mt-2 max-h-[36rem] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-900">{{ pretty(attemptRequest(attempt)) }}</pre></details>
          <div class="my-3 flex justify-between"><strong>{{ label('rawResponse') }}</strong><button v-if="attempt.raw_response !== null" class="btn btn-secondary btn-sm" @click="copy(attempt.raw_response)">{{ label('copy') }}</button></div>
          <pre class="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-900">{{ attempt.raw_response ?? '—' }}</pre>
        </details>
      </section>
      <details class="border-t border-gray-200 pt-5 dark:border-dark-700">
        <summary class="cursor-pointer font-semibold">{{ label('policySnapshot') }}</summary>
        <p class="mt-2 text-sm text-gray-500">{{ label('snapshotHint') }}</p>
        <pre class="mt-3 max-h-96 overflow-auto whitespace-pre-wrap break-words text-xs">{{ pretty(job.config_revision ? { revision: job.config_revision, mode: job.mode, skill: job.client_fingerprint } : null) }}</pre>
      </details>
    </div>
  </BaseDialog>
</template>
