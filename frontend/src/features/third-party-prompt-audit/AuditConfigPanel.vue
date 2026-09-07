<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { thirdPartyPromptAuditAPI as api, type AuditConfig, type AuditModel, type Contract, type KeyUpdate, type ProbeResult, type SavedConfig } from '@/api/admin/third-party-prompt-audit'
import { getAll } from '@/api/admin/groups'
import type { AdminGroup } from '@/types'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { configUpdate, editableConfig, formatTime } from './viewModel'
import { useAuditLabels } from './labels'

const label = useAuditLabels()
const app = useAppStore()
const saved = ref<SavedConfig | null>(null)
const draft = ref<AuditConfig | null>(null)
const contract = ref<Contract | null>(null)
const groups = ref<AdminGroup[]>([])
const keys = reactive<Record<string, KeyUpdate>>({})
const probes = reactive<Record<string, ProbeResult>>({})
const probing = ref<string[]>([])
const probeInput = ref('')
const probeInputKind = ref('text')
const probeProtocol = ref('openai_responses')
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const dirty = computed(() => !!saved.value && !!draft.value && (JSON.stringify(draft.value) !== JSON.stringify(editableConfig(saved.value)) || Object.values(keys).some(key => key.action !== 'keep')))
const reviewPercent = computed({
  get: () => draft.value?.review_threshold == null ? '' : draft.value.review_threshold * 100,
  set: (value: number | '') => { if (draft.value) draft.value.review_threshold = value === '' ? null : value / 100 }
})
const blockPercent = computed({
  get: () => draft.value?.block_threshold == null ? '' : draft.value.block_threshold * 100,
  set: (value: number | '') => { if (draft.value) draft.value.block_threshold = value === '' ? null : value / 100 }
})
const probeExample = computed(() => {
  switch (probeProtocol.value) {
    case 'openai_chat_completions': return '{"messages":[{"role":"user","content":"Explain this project structure."}]}'
    case 'anthropic_messages': return '{"messages":[{"role":"user","content":"Explain this project structure."}]}'
    case 'gemini': return '{"contents":[{"role":"user","parts":[{"text":"Explain this project structure."}]}]}'
    case 'grok_media': return '{"prompt":"Describe a quiet garden."}'
    default: return '{"input":"Explain this project structure."}'
  }
})
const groupOptions = computed(() => {
  const values = groups.value.map(group => ({ id: group.id, name: group.name }))
  for (const id of draft.value?.group_ids ?? []) if (!values.some(group => group.id === id)) values.push({ id, name: `#${id}` })
  return values
})

function restore() {
  if (!saved.value) return
  draft.value = editableConfig(saved.value)
  for (const id of Object.keys(keys)) delete keys[id]
  for (const model of draft.value.models) keys[model.id] = { model_id: model.id, action: 'keep', api_key: '' }
}
async function load() {
  const preserveDraft = dirty.value
  loading.value = true
  error.value = ''
  const results = await Promise.allSettled([api.getConfig(), api.getContract(), getAll()])
  const [configResult, contractResult, groupsResult] = results
  if (configResult.status === 'fulfilled' && !preserveDraft) { saved.value = configResult.value; restore() }
  if (contractResult.status === 'fulfilled') contract.value = contractResult.value
  if (groupsResult.status === 'fulfilled') groups.value = groupsResult.value
  const failures = results.filter(result => result.status === 'rejected')
  if (failures.length) error.value = failures.map(result => extractApiErrorMessage(result.reason, label('error'))).join(' · ')
  loading.value = false
}
function addModel() {
  if (!draft.value || !saved.value) return
  const id = crypto.randomUUID()
  draft.value.models.push({ id, name: '', enabled: true, base_url: '', model: '', timeout_ms: saved.value.model_defaults.timeout_ms })
  keys[id] = { model_id: id, action: 'replace', api_key: '' }
}
function moveModel(index: number, direction: number) {
  const models = draft.value?.models
  if (!models || index + direction < 0 || index + direction >= models.length) return
  const removed = models.splice(index, 1)[0]
  if (removed) models.splice(index + direction, 0, removed)
}
function removeModel(id: string) {
  if (!draft.value) return
  draft.value.models = draft.value.models.filter(model => model.id !== id)
  delete keys[id]
  delete probes[id]
}
async function save() {
  if (!draft.value || !saved.value) return
  saving.value = true
  try {
    const result = await api.saveConfig(configUpdate(draft.value, saved.value.revision, keys))
    saved.value = result
    restore()
    app.showSuccess(label('saved'))
  } catch (err) { app.showError(extractApiErrorMessage(err, label('invalidConfig'))) }
  finally { saving.value = false }
}
async function probe(model: AuditModel) {
  if (!draft.value || probing.value.includes(model.id)) return
  probing.value.push(model.id)
  try {
    const input = probeInput.value
    const key = keys[model.id]
    probes[model.id] = await api.probe({ model, audit_prompt: draft.value.audit_prompt, ...(probeInputKind.value === 'json' ? { protocol: probeProtocol.value } : {}), input, input_kind: probeInputKind.value as 'text' | 'json',
      key_action: key?.action ?? 'keep', ...(key?.action === 'replace' ? { api_key: key.api_key } : {}) })
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { probing.value = probing.value.filter(id => id !== model.id) }
}
onMounted(load)
</script>

<template>
  <div class="space-y-5 pb-6">
    <p v-if="loading" role="status">{{ label('loading') }}</p>
    <div v-if="error" class="rounded-xl bg-red-50 p-4 text-red-700 dark:bg-red-950/30 dark:text-red-300" role="alert">
      {{ error }} <button type="button" class="btn btn-secondary ml-3" @click="load">{{ label('refresh') }}</button>
    </div>
    <form v-if="draft && saved" class="space-y-5" @submit.prevent="save">
      <div v-if="saved.application_error" class="rounded-xl bg-amber-50 p-4 text-amber-800 dark:bg-amber-950/30 dark:text-amber-200" role="alert">
        {{ label('applicationError') }} · {{ saved.application_error }}
      </div>
      <fieldset :disabled="saving || loading" class="space-y-5">
        <section class="card p-5 sm:p-6">
          <h2 class="mb-2 text-lg font-semibold">{{ label('selection') }}</h2>
          <p class="mb-5 text-sm text-gray-500 dark:text-dark-400">{{ label('globalHint') }}</p>
          <div class="grid gap-5 md:grid-cols-2 xl:grid-cols-3">
            <label class="space-y-2"><span class="text-sm font-medium">{{ label('mode') }}</span>
              <select v-model="draft.mode" class="input"><option v-for="mode in ['off', 'async', 'blocking']" :key="mode" :value="mode">{{ label(mode) }}</option></select>
            </label>
            <label class="space-y-2"><span class="text-sm font-medium">{{ label('scope') }}</span>
              <select v-model="draft.audit_scope" class="input"><option value="full_request">{{ label('full_request') }}</option><option value="current_turn">{{ label('current_turn') }}</option></select>
            </label>
            <label class="space-y-2"><span class="text-sm font-medium">{{ label('worker') }}</span><input v-model.number="draft.worker_count" class="input" type="number" min="1" step="1" required /></label>
          </div>
          <p class="mt-3 text-sm text-gray-500 dark:text-dark-400">{{ label('scopeHint') }}</p>
          <p class="mt-2 text-sm text-gray-500 dark:text-dark-400">{{ label('scopeExample') }}</p>
          <p class="mt-2 text-sm text-gray-500 dark:text-dark-400">{{ label('workerHint') }}</p>
          <fieldset class="mt-5"><legend class="mb-2 text-sm font-medium">{{ label('platforms') }}</legend>
            <div class="flex flex-wrap gap-4"><label v-for="platform in ['openai', 'anthropic', 'gemini', 'antigravity', 'grok']" :key="platform" class="flex items-center gap-2 text-sm"><input v-model="draft.platforms" type="checkbox" :value="platform" />{{ platform }}</label></div>
          </fieldset>
          <label class="my-4 flex items-center gap-2 text-sm"><input v-model="draft.all_groups" type="checkbox" />{{ label('allGroups') }}</label>
          <fieldset v-if="!draft.all_groups"><legend class="mb-2 text-sm font-medium">{{ label('groups') }}</legend><div class="flex max-h-48 flex-wrap gap-4 overflow-y-auto"><label v-for="group in groupOptions" :key="group.id" class="flex items-center gap-2 text-sm"><input v-model="draft.group_ids" type="checkbox" :value="group.id" />{{ group.name }} (#{{ group.id }})</label></div></fieldset>
          <label class="mt-5 flex items-center gap-2 text-sm"><input v-model="draft.store_pass_events" type="checkbox" />{{ label('storePass') }}</label>
          <p class="mt-2 text-sm text-gray-500 dark:text-dark-400">{{ label('storeHint') }}</p>
        </section>

        <section class="card p-5 sm:p-6">
          <div class="flex flex-wrap items-center justify-between gap-3"><h2 class="text-lg font-semibold">{{ label('policy') }}</h2><button v-if="contract" type="button" class="btn btn-secondary" @click="draft.audit_prompt = contract.default_policy">{{ label('defaultPolicy') }}</button></div><p class="mb-5 mt-2 text-sm text-gray-500 dark:text-dark-400">{{ label('promptHint') }}</p>
          <div class="grid gap-6 xl:grid-cols-2">
            <label class="block space-y-2"><span class="text-sm font-medium">{{ label('policy') }}</span><textarea v-model="draft.audit_prompt" class="input min-h-96 font-mono text-sm" rows="18" required data-test="audit-policy" /></label>
            <div v-if="contract" class="min-w-0 space-y-2"><h3 class="text-sm font-medium">{{ label('contract') }} · {{ contract.version }}</h3><pre class="max-h-96 overflow-auto whitespace-pre-wrap rounded-xl bg-gray-50 p-4 text-sm dark:bg-dark-900" data-test="fixed-contract">{{ contract.output_contract }}</pre></div>
          </div>
        </section>

        <section class="card space-y-5 p-5 sm:p-6">
          <div class="flex items-center justify-between gap-4"><h2 class="text-lg font-semibold">{{ label('models') }}</h2><button type="button" class="btn btn-secondary" @click="addModel">{{ label('addModel') }}</button></div>
          <div v-for="(model, index) in draft.models" :key="model.id" class="space-y-4 rounded-xl border border-gray-200 p-4 dark:border-dark-600">
            <div class="flex justify-between gap-3"><label class="flex items-center gap-2"><input v-model="model.enabled" type="checkbox" />#{{ index + 1 }} · {{ label('enabled') }}</label><div class="flex gap-2"><button type="button" class="btn btn-ghost" :disabled="index === 0" @click="moveModel(index, -1)">{{ label('moveUp') }}</button><button type="button" class="btn btn-ghost" :disabled="index === draft.models.length - 1" @click="moveModel(index, 1)">{{ label('moveDown') }}</button><button type="button" class="btn btn-ghost text-red-600" @click="removeModel(model.id)">{{ label('remove') }}</button></div></div>
            <div class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
              <label class="space-y-2"><span class="text-sm">{{ label('name') }}</span><input v-model="model.name" class="input" required /></label>
              <label class="space-y-2"><span class="text-sm">{{ label('baseURL') }}</span><input v-model="model.base_url" class="input" type="url" required /></label>
              <label class="space-y-2"><span class="text-sm">{{ label('model') }}</span><input v-model="model.model" class="input" required /></label>
              <label class="space-y-2"><span class="text-sm">{{ label('timeout') }}</span><input v-model.number="model.timeout_ms" class="input" type="number" min="1" step="1" required /></label>
            </div>
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('timeoutHint') }}</p>
            <div v-if="keys[model.id]" class="grid gap-4 md:grid-cols-2">
              <label class="space-y-2"><span class="text-sm">{{ label('credential') }} · {{ label(saved.has_api_keys[model.id] ? 'keyPresent' : 'keyAbsent') }}</span><select v-model="keys[model.id]!.action" class="input"><option v-for="action in ['keep', 'replace', 'clear']" :key="action" :value="action">{{ label(action) }}</option></select></label>
              <label v-if="keys[model.id]?.action === 'replace'" class="space-y-2"><span class="text-sm">{{ label('replace') }}</span><input v-model="keys[model.id]!.api_key" class="input" type="password" autocomplete="new-password" required /></label>
            </div>
            <p class="text-xs text-gray-500 dark:text-dark-400">{{ label('keyHint') }} · {{ label('modelID') }}: {{ model.id }}</p>
            <button type="button" class="btn btn-secondary" :disabled="probing.includes(model.id)" @click="probe(model)">{{ label(probing.includes(model.id) ? 'loading' : 'probe') }}</button>
            <div v-if="probes[model.id]" class="rounded-lg bg-gray-50 p-3 text-sm dark:bg-dark-900" role="status">
              <span>{{ formatTime(probes[model.id]!.tested_at) }} · {{ probes[model.id]!.latency_ms }} ms · #{{ probes[model.id]!.attempt_id }}</span>
              <p v-if="probes[model.id]?.result">{{ label('score') }} {{ probes[model.id]!.result!.confidence }} · {{ probes[model.id]!.result!.reason }}</p>
              <p v-if="probes[model.id]?.error" class="text-red-600 dark:text-red-400">{{ probes[model.id]!.error!.message }} · {{ probes[model.id]!.error!.code }}</p>
            </div>
          </div>
          <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('probeHint') }}</p>
          <label class="block space-y-2"><span class="text-sm">{{ label('probeInputKind') }}</span><select v-model="probeInputKind" class="input"><option value="text">{{ label('textInput') }}</option><option value="json">{{ label('jsonInput') }}</option></select></label>
          <label v-if="probeInputKind === 'json'" class="block space-y-2"><span class="text-sm">Protocol</span><select v-model="probeProtocol" class="input" data-test="probe-protocol"><option v-for="protocol in ['openai_responses', 'openai_chat_completions', 'anthropic_messages', 'gemini', 'grok_media']" :key="protocol">{{ protocol }}</option></select></label>
          <p class="text-sm text-gray-500 dark:text-dark-400">{{ label(probeInputKind === 'json' ? 'jsonInputHint' : 'textInputHint') }}</p>
          <pre v-if="probeInputKind === 'json'" class="overflow-auto whitespace-pre-wrap rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-900">{{ probeExample }}</pre>
          <label class="block space-y-2"><span class="text-sm">{{ label('probeInput') }}</span><textarea v-model="probeInput" class="input font-mono text-sm" rows="3" :placeholder="label('defaultProbeInput')" /></label>
        </section>

        <section class="card space-y-4 p-5 sm:p-6">
          <h2 class="text-lg font-semibold">{{ label('thresholds') }}</h2><p class="text-sm">{{ label('enabledNodes') }}: {{ draft.models.filter(model => model.enabled).length }} · {{ label('blockVotes') }}: {{ draft.aggregation === 'any_block' ? 1 : draft.aggregation === 'all_block' ? draft.models.filter(model => model.enabled).length : Math.floor(draft.models.filter(model => model.enabled).length / 2) + 1 }}</p><p class="text-sm text-gray-500 dark:text-dark-400">{{ label('thresholdHint') }}</p>
          <div class="grid gap-4 md:grid-cols-3">
            <label class="space-y-2"><span class="text-sm">{{ label('reviewThreshold') }} (%)</span><input v-model.number="reviewPercent" class="input" type="number" min="0" max="100" step="any" :required="draft.mode !== 'off'" data-test="review-threshold" /></label>
            <label class="space-y-2"><span class="text-sm">{{ label('blockThreshold') }} (%)</span><input v-model.number="blockPercent" class="input" type="number" min="0" max="100" step="any" :required="draft.mode !== 'off'" data-test="block-threshold" /></label>
            <label class="space-y-2"><span class="text-sm">{{ label('aggregation') }}</span><select v-model="draft.aggregation" class="input"><option v-for="strategy in ['any_block', 'majority_block', 'all_block']" :key="strategy" :value="strategy">{{ label(strategy) }}</option></select></label>
          </div><p class="text-sm text-gray-500 dark:text-dark-400">{{ label('aggregationHint') }}</p>
          <div v-if="draft.review_threshold !== null && draft.block_threshold !== null" class="rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-900" data-test="threshold-ranges">
            <p>{{ label('rawThresholds') }}: {{ draft.review_threshold }} / {{ draft.block_threshold }}</p>
            <p>{{ label('joint') }}: {{ label('pass') }} &lt; {{ draft.review_threshold }} · {{ label('review') }} [{{ draft.review_threshold }}, {{ draft.block_threshold }}) · {{ label('block') }} ≥ {{ draft.block_threshold }}</p>
            <p v-if="draft.review_threshold === draft.block_threshold" class="mt-2 text-amber-700 dark:text-amber-400">{{ label('emptyReviewRange') }}</p>
            <p v-if="draft.review_threshold === 1 && draft.block_threshold === 1" class="mt-2 text-amber-700 dark:text-amber-400">{{ label('fullScoreOnly') }}</p>
          </div>
        </section>

        <section class="card space-y-4 p-5 sm:p-6">
          <h2 class="text-lg font-semibold">{{ label('actions') }}</h2><p class="text-sm text-gray-500 dark:text-dark-400">{{ label('actionHint') }}</p>
          <div class="grid gap-6 md:grid-cols-2">
            <fieldset class="space-y-4"><label class="flex items-center gap-2"><input v-model="draft.warning.enabled" type="checkbox" />{{ label('warning') }}</label>
              <div v-if="draft.warning.enabled" class="grid grid-cols-2 gap-4"><label class="space-y-2"><span class="text-sm">{{ label('warningWindow') }}</span><input v-model.number="draft.warning.window" class="input" type="number" min="1" step="1" required /></label><label class="space-y-2"><span class="text-sm">{{ label('warningLimit') }}</span><input v-model.number="draft.warning.limit" class="input" type="number" min="1" :max="draft.warning.window" step="1" required /></label></div>
            </fieldset>
            <fieldset class="space-y-4"><label class="flex items-center gap-2"><input v-model="draft.disable.enabled" type="checkbox" />{{ label('disable') }}</label><label v-if="draft.disable.enabled" class="block space-y-2"><span class="text-sm">{{ label('disableLimit') }}</span><input v-model.number="draft.disable.limit" class="input" type="number" min="1" step="1" required /></label></fieldset>
          </div><label class="block space-y-2"><span class="text-sm">{{ label('adminEmail') }}</span><input v-model="draft.admin_email" class="input" type="email" :required="draft.warning.enabled || draft.disable.enabled" /></label>
        </section>
      </fieldset>
      <div class="sticky bottom-4 flex flex-wrap items-center justify-between gap-3 rounded-xl border border-gray-200 bg-white/95 p-4 shadow-lg backdrop-blur dark:border-dark-600 dark:bg-dark-800/95">
        <span class="text-sm">{{ label('revision') }}: {{ saved.revision }} / {{ saved.applied_revision }} · {{ formatTime(saved.updated_at) }} · {{ saved.instance_id }} <span v-if="dirty" class="ml-2 text-amber-700 dark:text-amber-400">{{ label('dirty') }}</span></span>
        <div class="flex gap-2"><button type="button" class="btn btn-secondary" :disabled="saving || !dirty" @click="restore">{{ label('reset') }}</button><button type="submit" class="btn btn-primary" :disabled="saving || !dirty" data-test="save-config">{{ label(saving ? 'loading' : 'save') }}</button></div>
      </div>
    </form>
  </div>
</template>
