<script setup lang="ts">
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfigPolicySection from './ConfigPolicySection.vue'
import SamplesPanel from './SamplesPanel.vue'
import AuditModelNodes from './AuditModelNodes.vue'
import AuditModelEditor from './AuditModelEditor.vue'
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { thirdPartyPromptAuditAPI as api, type AuditConfig, type AuditModel, type AuditUser, type ConfigImpact, type Contract, type KeyUpdate, type SavedConfig, type UserRuleConfig } from '@/api/admin/third-party-prompt-audit'
import { getAll } from '@/api/admin/groups'
import type { AdminGroup } from '@/types'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { configUpdate, editableConfig, formatTime, nodeAPIFormatOptions } from './viewModel'
import { useAuditLabels } from './labels'
import { categoryOptions, categoryText, loadCategoryNames } from './categories'

const props = withDefaults(defineProps<{ refreshKey?: number; active?: boolean; focusNodeID?: string }>(), { refreshKey: 0, active: true, focusNodeID: '' })
const label = useAuditLabels()
const { t } = useI18n()
const app = useAppStore()
const saved = ref<SavedConfig | null>(null)
const draft = ref<AuditConfig | null>(null)
const contract = ref<Contract | null>(null)
const groups = ref<AdminGroup[]>([])
const users = ref<AuditUser[]>([])
const keys = reactive<Record<string, KeyUpdate>>({})
const modelOptions = reactive<Record<string, string[]>>({})
const modelListErrors = reactive<Record<string, string>>({})
const listingModels = ref<string[]>([])
// editingModelID 指向正在编辑的节点；保存到草稿前不影响配置。
const editingModelID = ref<string | null>(null)
const testingAdminEmail = ref(false)
const selectedRuleUserID = ref<number | null>(null)
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const editingModel = computed(() => draft.value?.models.find(model => model.id === editingModelID.value) ?? null)
const dirty = computed(() => !!saved.value && !!draft.value && (JSON.stringify(draft.value) !== JSON.stringify(editableConfig(saved.value)) || Object.values(keys).some(key => key.action !== 'keep')))
const ruleUsers = computed(() => users.value.filter(user => !(draft.value?.user_rules ?? []).some(rule => rule.user_id === user.id)))
const auditUser = (id: number) => users.value.find(user => user.id === id)
const userRoleLabel = (role: string) => label(role === 'admin' ? 'userAdmin' : role === 'user' ? 'userRegular' : role)
const userStatusLabel = (status: string) => label(status === 'active' ? 'userActive' : status === 'disabled' ? 'userDisabled' : status)
const reviewPercent = computed({
  get: () => draft.value?.review_threshold == null ? '' : draft.value.review_threshold * 100,
  set: (value: number | '') => { if (draft.value) draft.value.review_threshold = value === '' ? null : value / 100 }
})
const blockPercent = computed({
  get: () => draft.value?.block_threshold == null ? '' : draft.value.block_threshold * 100,
  set: (value: number | '') => { if (draft.value) draft.value.block_threshold = value === '' ? null : value / 100 }
})
// 停用阈值低于指令块熔断阈值时，被误判的共享指令块会先停用第一个用户。
const disableWarning = computed(() => {
  const threshold = saved.value?.breaker_user_threshold ?? 0
  if (!draft.value?.disable.enabled || threshold <= 0 || draft.value.disable.limit >= threshold) return ''
  return t('admin.thirdPartyPromptAudit.disableThresholdWarning', { threshold })
})

// 类别处置规则：键为 R01 形式，值为 immediate_disable / notice_only。
const categoryRuleRows = computed(() => Object.entries(draft.value?.category_rules ?? {}).map(([code, disposition]) => ({ code, disposition })))
function addCategoryRule() {
  if (!draft.value) return
  // 优先使用政策已声明且尚未配置的类别；政策没有声明时退回 R01、R02…
  const rules = draft.value.category_rules
  const declared = categoryOptions().map(item => item.code)
  let code = declared.find(item => rules[item] === undefined)
  if (!code) {
    let index = 1
    let fallback = `R${String(index).padStart(2, '0')}`
    while (rules[fallback]) {
      index++
      fallback = `R${String(index).padStart(2, '0')}`
    }
    code = fallback
  }
  rules[code] = 'notice_only'
}
function updateCategoryCode(oldCode: string, event: Event) {
  const control = event.target as HTMLInputElement | HTMLSelectElement
  const next = control.value.trim().toUpperCase()
  if (!draft.value) return
  const collision = next !== oldCode && draft.value.category_rules[next] !== undefined
  if (!/^R\d{2}$/.test(next) || collision) {
    control.value = oldCode
    return
  }
  const disposition = draft.value.category_rules[oldCode]
  delete draft.value.category_rules[oldCode]
  draft.value.category_rules[next] = disposition
  control.value = next
}
function updateCategoryDisposition(code: string, event: Event) {
  if (!draft.value) return
  draft.value.category_rules[code] = (event.target as HTMLSelectElement).value
}
function removeCategoryRule(code: string) {
  if (draft.value) delete draft.value.category_rules[code]
}
const groupOptions = computed(() => {
  const values = groups.value.map(group => ({ id: group.id, name: group.name }))
  for (const id of draft.value?.group_ids ?? []) if (!values.some(group => group.id === id)) values.push({ id, name: `#${id}` })
  return values
})

function restore() {
  if (!saved.value) return
  draft.value = editableConfig(saved.value)
  for (const id of Object.keys(keys)) delete keys[id]
  for (const id of Object.keys(modelOptions)) delete modelOptions[id]
  for (const id of Object.keys(modelListErrors)) delete modelListErrors[id]
  editingModelID.value = null
  for (const model of draft.value.models) {
    keys[model.id] = { model_id: model.id, action: 'keep', api_key: '' }
    modelOptions[model.id] = model.model ? [model.model] : []
  }
}
// importNodeFile 只读取节点固定字段；文件只读本地选择，不发起请求。
async function importNodeFile(file: File) {
  if (!draft.value) return
  let raw: unknown
  try { raw = JSON.parse(await file.text()) } catch { app.showError(label('nodeConfigInvalidJSON')); return }
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) { app.showError(label('nodeConfigInvalidJSON')); return }
  const value = raw as Record<string, unknown>
  const baseURL = typeof value.base_url === 'string' ? value.base_url.trim() : ''
  const modelName = typeof value.model === 'string' ? value.model.trim() : ''
  if (!baseURL || !modelName) { app.showError(label('nodeConfigRequired')); return }
  const timeout = Number(value.timeout_ms)
  const concurrency = Number(value.max_concurrency)
  if ((value.timeout_ms !== undefined && (!Number.isFinite(timeout) || timeout <= 0)) ||
      (value.max_concurrency !== undefined && (!Number.isFinite(concurrency) || concurrency <= 0))) {
    app.showError(label('nodeConfigNumbers'))
    return
  }
  const id = crypto.randomUUID()
  const format = typeof value.api_format === 'string' && nodeAPIFormatOptions.some(item => item.value === value.api_format)
    ? value.api_format as AuditModel['api_format'] : 'openai_chat_completions'
  const model: AuditModel = {
    id, name: modelName, model: modelName, base_url: baseURL, enabled: value.enabled !== false,
    timeout_ms: timeout > 0 ? Math.trunc(timeout) : saved.value?.model_defaults.timeout_ms ?? 300000,
    max_concurrency: concurrency > 0 ? Math.trunc(concurrency) : saved.value?.model_defaults.max_concurrency ?? 10,
    note: typeof value.note === 'string' ? value.note.slice(0, 200) : '',
    allow_self_audit: value.allow_self_audit === true, api_format: format,
  }
  draft.value.models.push(model)
  keys[id] = { model_id: id, action: 'keep', api_key: '' }
  modelOptions[id] = [modelName]
  editingModelID.value = id
}
// applyModel 把编辑弹窗的局部副本写回草稿，取消编辑不污染配置。
function applyModel(value: AuditModel, key: KeyUpdate) {
  if (!draft.value) return
  const index = draft.value.models.findIndex(model => model.id === value.id)
  if (index < 0) return
  draft.value.models[index] = value
  keys[value.id] = key
  modelOptions[value.id] = Array.from(new Set([value.model, ...(modelOptions[value.id] ?? [])]))
  delete modelListErrors[value.id]
  // 名称统一在写回草稿后重建，重名节点与后端保存结果保持一致。
  syncModelNames()
  editingModelID.value = null
}
// copyModel 复制节点配置（不复制凭据）：新 id 紧随原节点插入并直接进入编辑（补填 Key）。
function copyModel(id: string) {
  if (!draft.value) return
  const index = draft.value.models.findIndex(model => model.id === id)
  const source = draft.value.models[index]
  if (!source) return
  const copyID = crypto.randomUUID()
  const copy = JSON.parse(JSON.stringify(source)) as AuditModel
  copy.id = copyID
  draft.value.models.splice(index + 1, 0, copy)
  keys[copyID] = { model_id: copyID, action: 'keep', api_key: '' }
  modelOptions[copyID] = source.model ? [source.model] : []
  syncModelNames()
  editingModelID.value = copyID
}
async function load(force = false) {
	const preserveDraft = dirty.value && !force
  loading.value = true
  error.value = ''
  const results = await Promise.allSettled([api.getConfig(), api.getContract(), getAll(), api.users()])
  const [configResult, contractResult, groupsResult, usersResult] = results
  if (configResult.status === 'fulfilled' && !preserveDraft) { saved.value = configResult.value; restore() }
  if (contractResult.status === 'fulfilled') contract.value = contractResult.value
  if (groupsResult.status === 'fulfilled') groups.value = groupsResult.value
  if (usersResult.status === 'fulfilled') users.value = usersResult.value
  const failures = results.filter(result => result.status === 'rejected')
  if (failures.length) error.value = failures.map(result => extractApiErrorMessage(result.reason, label('error'))).join(' · ')
  loading.value = false
}
// syncModelNames 让节点名称始终跟随模型名；同模型按顺序追加 -1、-2，与后端保存规则一致。
function syncModelNames() {
  if (!draft.value) return
  const counts = new Map<string, number>()
  for (const model of draft.value.models) {
    const name = model.model.trim()
    if (!name) {
      model.name = ''
      continue
    }
    const count = counts.get(name) ?? 0
    model.name = count > 0 ? `${name}-${count}` : name
    counts.set(name, count + 1)
  }
}
function addModel() {
  if (!draft.value || !saved.value) return
  const id = crypto.randomUUID()
  const model: AuditModel = { id, name: '', note: '', enabled: true, api_format: 'openai_chat_completions', base_url: '', model: '', timeout_ms: saved.value.model_defaults.timeout_ms, max_concurrency: saved.value.model_defaults.max_concurrency }
  draft.value.models.push(model)
  keys[id] = { model_id: id, action: 'keep', api_key: '' }
  modelOptions[id] = []
  editingModelID.value = id
}
async function listModels(model: AuditModel, key: KeyUpdate) {
	if (!model.base_url || listingModels.value.includes(model.id)) return
  listingModels.value.push(model.id)
  delete modelListErrors[model.id]
  try {
    const existing = modelOptions[model.id] ?? []
    const result = await api.listModels({ model_id: model.id, base_url: model.base_url, timeout_ms: model.timeout_ms,
      api_format: model.api_format,
      key_action: key?.action === 'replace' ? 'replace' : 'keep', ...(key?.action === 'replace' ? { api_key: key.api_key } : {}) })
    modelOptions[model.id] = Array.from(new Set([...existing, model.model, ...result.models].filter(Boolean)))
  } catch (err) { modelListErrors[model.id] = extractApiErrorMessage(err, label('modelListFailed')); app.showError(modelListErrors[model.id]!) }
  finally { listingModels.value = listingModels.value.filter(id => id !== model.id) }
}
watch(() => draft.value?.mode, mode => {
  if (!draft.value || !saved.value || mode === 'off') return
  if (draft.value.review_threshold == null) draft.value.review_threshold = saved.value.rule_defaults.review_threshold
  if (draft.value.block_threshold == null) draft.value.block_threshold = saved.value.rule_defaults.block_threshold
})
watch(() => draft.value?.warning.enabled, enabled => {
  if (!draft.value || !saved.value || !enabled) return
  if (draft.value.warning.window < 1) draft.value.warning.window = saved.value.rule_defaults.warning_window
  if (draft.value.warning.limit < 1) draft.value.warning.limit = saved.value.rule_defaults.warning_limit
})
watch(() => draft.value?.disable.enabled, enabled => {
  if (draft.value && saved.value && enabled && draft.value.disable.limit < 1) draft.value.disable.limit = saved.value.rule_defaults.disable_limit
})
watch(() => draft.value?.models.map(model => model.model), syncModelNames, { deep: true })
// 从待处理工作台跳转时直接打开该节点的编辑弹窗。
watch(() => props.focusNodeID, id => {
  if (!id || !draft.value?.models.some(model => model.id === id)) return
  editingModelID.value = id
  void nextTick(() => document.querySelector<HTMLElement>('[data-test="model-nodes"]')?.scrollIntoView({ block: 'center' }))
}, { immediate: true })
function moveModel(id: string, direction: number) {
  const models = draft.value?.models
  const index = models?.findIndex(model => model.id === id) ?? -1
  if (!models || index + direction < 0 || index + direction >= models.length) return
  const removed = models.splice(index, 1)[0]
  if (removed) models.splice(index + direction, 0, removed)
}
function removeModel(id: string) {
  if (!draft.value) return
  const model = draft.value.models.find(item => item.id === id)
  if (model && !window.confirm(label('confirmRemoveNode', { name: model.model || model.name || id }))) return
  draft.value.models = draft.value.models.filter(model => model.id !== id)
  delete keys[id]
  delete modelOptions[id]
  delete modelListErrors[id]
  if (editingModelID.value === id) editingModelID.value = null
  syncModelNames()
}
function toggleModel(id: string, enabled: boolean) {
  const model = draft.value?.models.find(item => item.id === id)
  if (model) model.enabled = enabled
}
// 专属规则只覆盖处理方式与两个阈值；聚合、提醒与停用一律跟随全局。
function addUserRule() {
  if (!draft.value || !saved.value || !selectedRuleUserID.value) return
  const user = auditUser(selectedRuleUserID.value)
  if (!user || draft.value.user_rules.some(rule => rule.user_id === user.id)) return
  draft.value.user_rules.push({
    user_id: user.id,
    mode: draft.value.mode === 'blocking' ? 'blocking' : 'async',
    review_threshold: draft.value.review_threshold ?? saved.value.rule_defaults.review_threshold,
    block_threshold: draft.value.block_threshold ?? saved.value.rule_defaults.block_threshold,
  })
  selectedRuleUserID.value = null
}
function removeUserRule(userID: number) {
  if (draft.value) draft.value.user_rules = draft.value.user_rules.filter(rule => rule.user_id !== userID)
}
function updateRuleThreshold(rule: UserRuleConfig, field: 'review_threshold' | 'block_threshold', event: Event) {
  const value = Number((event.target as HTMLInputElement).value)
  rule[field] = Number.isFinite(value) ? value / 100 : 0
}
async function testAdminEmail() {
  if (!saved.value?.admin_email || draft.value?.admin_email !== saved.value.admin_email || testingAdminEmail.value) return
  testingAdminEmail.value = true
  try {
    await api.testAdminEmail()
    app.showSuccess(label('adminEmailTested'))
  } catch (err) { app.showError(extractApiErrorMessage(err, label('error'))) }
  finally { testingAdminEmail.value = false }
}
// 保存前列出将要变更的字段，并估算这次保存对全库审核缓存的影响。
const showDiff = ref(false)
const pendingChanges = ref<string[]>([])
const impact = ref<ConfigImpact | null>(null)
const impactLoading = ref(false)
const impactFailed = ref(false)
const impactMessage = computed(() => impact.value && impact.value.instruction_changed
  ? t('admin.thirdPartyPromptAudit.cacheImpactInstruction', { hours: impact.value.window_hours, count: impact.value.recent_instruction_blocks })
  : '')
// 旧版政策需显式勾选确认，后端才接受保存。
const legacyConfirmed = ref(false)
const legacyPolicy = computed(() => !!draft.value && /audit_stage|intent_binding|effective_behavior/.test(draft.value.audit_prompt))
const diffLabels: Record<string, string> = {
  mode: '运行模式', retention_days: '记录保留天数', audit_prompt: '审核政策', worker_count: 'Worker 数',
  review_threshold: '复核阈值', block_threshold: '阻断阈值', aggregation: '聚合规则', admin_email: '管理员邮箱',
  platforms: '平台范围', all_groups: '全部分组', group_ids: '分组', excluded_user_ids: '不审核用户',
  models: '审核节点', user_rules: '用户规则', warning: '提醒规则', disable: '停用规则',
  category_rules: '违规类别处置', escalate_on_warning: '提醒触发时自动加严',
}
function describeDiff(): string[] {
  if (!draft.value || !saved.value) return []
  const before = editableConfig(saved.value) as unknown as Record<string, unknown>
  const after = draft.value as unknown as Record<string, unknown>
  const changes: string[] = []
  for (const key of Object.keys(diffLabels)) {
    if (JSON.stringify(before[key]) !== JSON.stringify(after[key])) changes.push(diffLabels[key])
  }
  for (const [id, update] of Object.entries(keys)) {
    if (update.action !== 'keep') changes.push(`节点凭据 ${id}：${update.action === 'replace' ? '替换' : '清除'}`)
  }
  return changes
}
async function save() {
  if (!draft.value || !saved.value) return
  pendingChanges.value = describeDiff()
  if (pendingChanges.value.length === 0) return
  legacyConfirmed.value = false
  showDiff.value = true
  impact.value = null
  impactFailed.value = false
  impactLoading.value = true
  try { impact.value = await api.configImpact(draft.value) }
  catch { impactFailed.value = true }
  finally { impactLoading.value = false }
}
async function confirmSave() {
  showDiff.value = false
  if (!draft.value || !saved.value) return
  saving.value = true
  try {
    const result = await api.saveConfig(configUpdate(draft.value, saved.value.revision, keys, legacyConfirmed.value))
    saved.value = result
    restore()
    app.showSuccess(label('saved'))
  } catch (err) { app.showError(extractApiErrorMessage(err, label('invalidConfig'))) }
  finally { saving.value = false }
}
// 每 5 秒同步一次服务端配置；正在编辑或有未保存修改时跳过，避免覆盖草稿。
async function refreshQuiet() {
  if (!props.active || dirty.value || saving.value || loading.value || document.visibilityState !== 'visible') return
  try {
    const [configResult, contractResult] = await Promise.all([api.getConfig(), api.getContract()])
    saved.value = configResult
    contract.value = contractResult
    if (!dirty.value) restore()
  } catch { /* 静默刷新失败不打扰当前操作 */ }
}
const autoTimer = window.setInterval(() => { void refreshQuiet() }, 5000)
onUnmounted(() => window.clearInterval(autoTimer))
// 类别规则的下拉需要政策里的编号与标题；先取一次再加载配置（模块内已缓存）。
onMounted(() => { void loadCategoryNames(); void load() })
watch(() => props.refreshKey, () => { void load(true) })
</script>

<template>
  <div class="space-y-5 pb-6">
    <p v-if="loading" role="status">{{ label('loading') }}</p>
    <div v-if="error" class="rounded-xl bg-red-50 p-4 text-red-700 dark:bg-red-950/30 dark:text-red-300" role="alert">
      {{ error }} <button type="button" class="btn btn-secondary ml-3" @click="load()">{{ label('refresh') }}</button>
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
			<label class="space-y-2"><span class="text-sm font-medium">{{ label('worker') }}</span><input v-model.number="draft.worker_count" class="input" type="number" min="1" step="1" required /></label>
            <label class="space-y-2"><span class="text-sm font-medium">{{ label('retentionDays') }}</span><input v-model.number="draft.retention_days" class="input" type="number" min="1" max="365" step="1" required /><span class="text-xs text-gray-500">{{ label('retentionHint') }}</span></label>
          </div>
          <p class="mt-3 text-sm text-gray-500 dark:text-dark-400">{{ label('scopeHint') }}</p>
          <p class="mt-2 text-sm text-gray-500 dark:text-dark-400">{{ label('scopeExample') }}</p>
          <p class="mt-2 text-sm text-gray-500 dark:text-dark-400">{{ label('workerHint') }}</p>
          <fieldset class="mt-5"><legend class="mb-2 text-sm font-medium">{{ label('platforms') }}</legend>
            <div class="flex flex-wrap gap-4"><label v-for="platform in ['openai', 'anthropic', 'gemini', 'antigravity', 'grok']" :key="platform" class="flex items-center gap-2 text-sm"><input v-model="draft.platforms" type="checkbox" :value="platform" />{{ platform }}</label></div>
          </fieldset>
          <label class="my-4 flex items-center gap-2 text-sm"><input v-model="draft.all_groups" type="checkbox" />{{ label('allGroups') }}</label>
          <fieldset v-if="!draft.all_groups"><legend class="mb-2 text-sm font-medium">{{ label('groups') }}</legend><div class="flex max-h-48 flex-wrap gap-4 overflow-y-auto"><label v-for="group in groupOptions" :key="group.id" class="flex items-center gap-2 text-sm"><input v-model="draft.group_ids" type="checkbox" :value="group.id" />{{ group.name }} (#{{ group.id }})</label></div></fieldset>
          <fieldset class="mt-5"><legend class="mb-2 text-sm font-medium">{{ label('excludedUsers') }}</legend><p class="mb-3 text-xs text-gray-500 dark:text-dark-400">{{ label('excludedUsersHint') }}</p><div class="grid max-h-52 gap-2 overflow-y-auto rounded-lg border border-gray-200 p-3 dark:border-dark-700 sm:grid-cols-2 xl:grid-cols-3"><label v-for="user in users" :key="user.id" class="flex items-start gap-2 text-sm"><input v-model="draft.excluded_user_ids" class="mt-1" type="checkbox" :value="user.id" /><span class="min-w-0"><span class="block break-words">{{ user.username }} (#{{ user.id }})</span><span class="block break-all text-xs text-gray-500">{{ user.email }} · {{ userRoleLabel(user.role) }} · {{ userStatusLabel(user.status) }}</span></span></label><p v-if="!users.length" class="text-sm text-gray-500">{{ label('empty') }}</p></div></fieldset>
        </section>

        <AuditModelNodes
          :models="draft.models"
          :has-keys="saved.has_api_keys"
          :disabled="saving || loading"
          @add="addModel"
          @edit="editingModelID = $event"
          @toggle="toggleModel"
          @copy="copyModel"
          @move="moveModel"
          @remove="removeModel"
          @import="importNodeFile"
        />

        <section class="card p-5 sm:p-6">
          <div class="flex flex-wrap items-center justify-between gap-3"><h2 class="text-lg font-semibold">{{ label('policy') }}</h2><button v-if="contract" type="button" class="btn btn-secondary" @click="draft.audit_prompt = contract.default_policy">{{ label('defaultPolicy') }}</button></div><p class="mb-5 mt-2 text-sm text-gray-500 dark:text-dark-400">{{ label('promptHint') }}</p>
          <div class="grid gap-6 xl:grid-cols-2">
            <ConfigPolicySection v-model:prompt="draft.audit_prompt" :default-policy="contract?.default_policy" />
            <div v-if="contract" class="min-w-0 space-y-2"><h3 class="text-sm font-medium">{{ label('contract') }} · {{ contract.version }}</h3><pre class="max-h-96 overflow-auto whitespace-pre-wrap rounded-xl bg-gray-50 p-4 text-sm dark:bg-dark-900" data-test="fixed-contract">{{ contract.output_contract }}</pre></div>
          </div>
        </section>
        <section class="card p-5 sm:p-6" data-test="samples-section">
          <SamplesPanel :config="draft" :saved="saved" :active="active" :refresh-key="refreshKey" />
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
            <p>{{ label('current_user') }}: {{ label('pass') }} &lt; {{ draft.review_threshold }} · {{ label('review') }} [{{ draft.review_threshold }}, {{ draft.block_threshold }}) · {{ label('block') }} ≥ {{ draft.block_threshold }}</p>
            <p v-if="draft.review_threshold === draft.block_threshold" class="mt-2 text-amber-700 dark:text-amber-400">{{ label('emptyReviewRange') }}</p>
            <p v-if="draft.review_threshold === 1 && draft.block_threshold === 1" class="mt-2 text-amber-700 dark:text-amber-400">{{ label('fullScoreOnly') }}</p>
          </div>
        </section>

        <section class="card space-y-4 p-5 sm:p-6">
          <h2 class="text-lg font-semibold">{{ label('actions') }}</h2><p class="text-sm text-gray-500 dark:text-dark-400">{{ label('actionHint') }}</p>
          <div class="grid gap-6 md:grid-cols-2">
            <fieldset class="space-y-4"><label class="flex items-center gap-2"><input v-model="draft.warning.enabled" type="checkbox" />{{ label('warning') }}</label>
              <div v-if="draft.warning.enabled" class="space-y-3"><div class="grid grid-cols-2 gap-4"><label class="space-y-2"><span class="text-sm">{{ label('warningWindow') }}</span><input v-model.number="draft.warning.window" class="input" type="number" min="1" step="1" required /></label><label class="space-y-2"><span class="text-sm">{{ label('warningLimit') }}</span><input v-model.number="draft.warning.limit" class="input" type="number" min="1" :max="draft.warning.window" step="1" required /></label></div><p class="text-xs text-gray-500 dark:text-dark-400">{{ label('warningWindowHint') }}</p></div>
            </fieldset>
            <fieldset class="space-y-4"><label class="flex items-center gap-2"><input v-model="draft.disable.enabled" type="checkbox" />{{ label('disable') }}</label><label v-if="draft.disable.enabled" class="block space-y-2"><span class="text-sm">{{ label('disableLimit') }}</span><input v-model.number="draft.disable.limit" class="input" type="number" min="1" step="1" required /></label>
              <p v-if="disableWarning" class="text-xs text-amber-700 dark:text-amber-400" data-test="disable-threshold-warning">{{ disableWarning }}</p>
            </fieldset>
            <div class="space-y-3 md:col-span-2" data-test="category-rules">
              <label class="flex items-center gap-2"><input v-model="draft.escalate_on_warning" type="checkbox" />{{ label('escalateOnWarning') }}</label>
              <p class="text-xs text-gray-500 dark:text-dark-400">{{ label('escalateOnWarningHint') }}</p>
              <div class="flex items-center justify-between"><h4 class="font-semibold">{{ label('categoryRules') }}</h4><button type="button" class="btn btn-secondary" data-test="add-category-rule" @click="addCategoryRule">{{ label('addCategoryRule') }}</button></div>
              <p class="text-xs text-gray-500 dark:text-dark-400">{{ label('categoryRulesHint') }}</p>
              <div v-for="row in categoryRuleRows" :key="row.code" class="flex flex-wrap items-center gap-3">
                <select v-if="categoryOptions().length" :value="row.code" class="input w-64" aria-label="R01" @change="updateCategoryCode(row.code, $event)">
                  <option v-for="item in categoryOptions()" :key="item.code" :value="item.code">{{ categoryText(item.code) }}</option>
                  <option v-if="!categoryOptions().some(item => item.code === row.code)" :value="row.code">{{ row.code }}</option>
                </select>
                <input v-else :value="row.code" class="input w-24" maxlength="3" aria-label="R01" @change="updateCategoryCode(row.code, $event)" />
                <select :value="row.disposition" class="input w-56" @change="updateCategoryDisposition(row.code, $event)">
                  <option value="immediate_disable">{{ label('dispositionImmediateDisable') }}</option>
                  <option value="notice_only">{{ label('dispositionNoticeOnly') }}</option>
                </select>
                <button type="button" class="btn btn-ghost text-red-600" @click="removeCategoryRule(row.code)">{{ label('removeCategoryRule') }}</button>
              </div>
            </div>
          </div><div class="flex flex-wrap items-end gap-3"><label class="min-w-72 flex-1 space-y-2"><span class="text-sm">{{ label('adminEmail') }}</span><input v-model="draft.admin_email" class="input" type="email" /><span class="block text-xs text-gray-500 dark:text-dark-400">{{ label('adminEmailHint') }}</span></label><button type="button" class="btn btn-secondary self-end" data-test="test-admin-email" :disabled="saving || loading || testingAdminEmail || !saved.admin_email || draft.admin_email !== saved.admin_email" @click="testAdminEmail">{{ label(testingAdminEmail ? 'loading' : 'testAdminEmail') }}</button></div><p class="text-xs text-gray-500 dark:text-dark-400">{{ label('testAdminEmailHint') }}</p>
          <div class="border-t border-gray-200 pt-5 dark:border-dark-700">
            <h3 class="font-semibold">{{ label('userRules') }}</h3><p class="mb-4 mt-2 text-sm text-gray-500 dark:text-dark-400">{{ label('userRulesHint') }}</p>
            <div class="flex flex-wrap items-end gap-3"><label class="min-w-72 flex-1 space-y-2"><span class="text-sm">{{ label('userColumn') }}</span><select v-model="selectedRuleUserID" class="input"><option :value="null">{{ label('chooseRuleUser') }}</option><option v-for="user in ruleUsers" :key="user.id" :value="user.id">{{ user.username }} (#{{ user.id }}) · {{ user.email }} · {{ userRoleLabel(user.role) }}</option></select></label><button type="button" class="btn btn-secondary" :disabled="!selectedRuleUserID" @click="addUserRule">{{ label('addUserRule') }}</button></div>
            <div class="mt-4 space-y-4">
              <section v-for="rule in draft.user_rules" :key="rule.user_id" class="rounded-xl border border-gray-200 p-4 dark:border-dark-600">
                <div class="flex flex-wrap items-start justify-between gap-3"><div><strong>{{ auditUser(rule.user_id)?.username || `#${rule.user_id}` }} (#{{ rule.user_id }})</strong><p class="text-xs text-gray-500">{{ auditUser(rule.user_id)?.email }} · {{ userRoleLabel(auditUser(rule.user_id)?.role || '') }}</p></div><button type="button" class="btn btn-ghost text-red-600" @click="removeUserRule(rule.user_id)">{{ label('removeUserRule') }}</button></div>
                <div class="mt-4 grid gap-4 md:grid-cols-3"><label class="space-y-2"><span class="text-sm">{{ label('userMode') }}</span><select v-model="rule.mode" class="input"><option value="async">{{ label('async') }}</option><option value="blocking">{{ label('blocking') }}</option></select></label><label class="space-y-2"><span class="text-sm">{{ label('reviewThreshold') }} (%)</span><input :value="rule.review_threshold * 100" class="input" type="number" min="0" max="100" step="any" required @input="updateRuleThreshold(rule, 'review_threshold', $event)" /></label><label class="space-y-2"><span class="text-sm">{{ label('blockThreshold') }} (%)</span><input :value="rule.block_threshold * 100" class="input" type="number" min="0" max="100" step="any" required @input="updateRuleThreshold(rule, 'block_threshold', $event)" /></label></div>
              </section>
            </div>
          </div>
        </section>
      </fieldset>
      <div class="sticky bottom-4 flex flex-wrap items-center justify-between gap-3 rounded-xl border border-gray-200 bg-white/95 p-4 shadow-lg backdrop-blur dark:border-dark-600 dark:bg-dark-800/95">
        <span class="text-sm">{{ label('revision') }}: {{ saved.revision }} / {{ saved.applied_revision }} · {{ formatTime(saved.updated_at) }} · {{ saved.instance_id }} <span v-if="dirty" class="ml-2 text-amber-700 dark:text-amber-400">{{ label('dirty') }}</span></span>
        <div class="flex gap-2"><button type="button" class="btn btn-secondary" :disabled="saving || !dirty" @click="restore">{{ label('reset') }}</button><button type="submit" class="btn btn-primary" :disabled="saving || !dirty" data-test="save-config">{{ label(saving ? 'loading' : 'save') }}</button></div>
      </div>
    </form>
    <BaseDialog :show="showDiff" :title="label('confirmChanges')" :close-on-click-outside="true" @close="showDiff = false">
      <p class="mb-3 text-sm text-gray-500 dark:text-dark-400">{{ label('confirmChangesHint') }}</p>
      <ul class="list-disc space-y-1 pl-5 text-sm"><li v-for="item in pendingChanges" :key="item">{{ item }}</li></ul>
      <p v-if="impactLoading" class="mt-3 text-sm" role="status">{{ label('loading') }}</p>
      <p v-else-if="impactFailed" class="mt-3 text-sm text-amber-700 dark:text-amber-400" data-test="cache-impact-failed">{{ label('cacheImpactFailed') }}</p>
      <div v-else-if="impact" class="mt-3 space-y-2 rounded-lg bg-amber-50 p-3 text-sm text-amber-800 dark:bg-amber-950/30 dark:text-amber-200" data-test="cache-impact">
        <p v-if="impactMessage" class="font-medium">{{ impactMessage }}</p>
        <p v-if="impact.user_changed">{{ label('cacheImpactUser') }}</p>
      </div>
      <label v-if="legacyPolicy" class="mt-3 flex items-start gap-2 text-sm text-amber-800 dark:text-amber-200"><input v-model="legacyConfirmed" type="checkbox" class="mt-1" />{{ label('legacyPolicyConfirm') }}</label>
      <template #footer><div class="flex gap-3"><button class="btn btn-primary" :disabled="saving || (legacyPolicy && !legacyConfirmed)" @click="confirmSave">{{ label('confirmSave') }}</button><button class="btn btn-secondary" @click="showDiff = false">{{ label('cancelChanges') }}</button></div></template>
    </BaseDialog>
    <AuditModelEditor
      v-if="saved && draft"
      :show="editingModel !== null"
      :model="editingModel"
      :saved="saved"
      :key-state="editingModel ? keys[editingModel.id] : undefined"
      :model-options="editingModel ? modelOptions[editingModel.id] ?? [] : []"
      :list-error="editingModel ? modelListErrors[editingModel.id] ?? '' : ''"
      :list-loading="editingModel ? listingModels.includes(editingModel.id) : false"
      :audit-prompt="draft.audit_prompt"
      @close="editingModelID = null"
      @apply="applyModel"
      @list-models="listModels"
    />
  </div>
</template>
