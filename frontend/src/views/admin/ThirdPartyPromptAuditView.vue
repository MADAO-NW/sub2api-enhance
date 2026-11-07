<script setup lang="ts">
import { onMounted, onUnmounted, reactive, ref } from 'vue'
import { loadCategoryNames } from '@/features/third-party-prompt-audit/categories'
import AuditOverview from '@/features/third-party-prompt-audit/AuditOverview.vue'
import AuditInbox from '@/features/third-party-prompt-audit/AuditInbox.vue'
import AuditRecords from '@/features/third-party-prompt-audit/AuditRecords.vue'
import AuditUsers from '@/features/third-party-prompt-audit/AuditUsers.vue'
import AuditConfigPanel from '@/features/third-party-prompt-audit/AuditConfigPanel.vue'
import TrustPanel from '@/features/third-party-prompt-audit/TrustPanel.vue'
import { useAuditLabels } from '@/features/third-party-prompt-audit/labels'
import { thirdPartyPromptAuditAPI as api, type AuditFilter, type InboxCategoryKey } from '@/api/admin/third-party-prompt-audit'
import SystemUpdatePanel from '@/features/system-update/SystemUpdatePanel.vue'

const label = useAuditLabels()
const tabs = ['overview', 'inbox', 'jobs', 'users', 'trust', 'config'] as const
// 支持 ?tab= 深链；管理员邮件链接用 ?tab=jobs&job_id=<id> 直接打开任务详情。
const query = new URLSearchParams(location.search)
const requestedTab = query.get('tab')
const initialTab = requestedTab && (tabs as readonly string[]).includes(requestedTab) ? requestedTab : 'overview'
const requestedJobID = Number(query.get('job_id'))
const active = ref(initialTab)
const visited = ref<string[]>(initialTab === 'overview' ? ['overview'] : ['overview', initialTab])
const jobFilter = ref<AuditFilter>({})
const jobDetailID = ref<number | null>(Number.isSafeInteger(requestedJobID) && requestedJobID > 0 ? requestedJobID : null)
const refreshKey = ref(0)
const tabRefresh = reactive<Record<string, number>>({ overview: 0, inbox: 0, jobs: 0, users: 0, trust: 0, config: 0 })
// 页签角标取待处理工作台的总数，沿用自适应轮询的 5 秒节奏。
const inboxTotal = ref(0)
const inboxCategory = ref<InboxCategoryKey | ''>('')
const trustFocus = ref('')
const configFocusNode = ref('')
const usersRange = ref<{ from: string; to: string } | null>(null)
const trackedReaudits = new Map<number, string>()
let tracking = false
let disposed = false

function select(tab: string, refresh = true) {
  active.value = tab
  if (!visited.value.includes(tab)) visited.value.push(tab)
  if (refresh) tabRefresh[tab] = (tabRefresh[tab] ?? 0) + 1
}
function inspectJobs(filter: AuditFilter) {
  jobDetailID.value = null
  jobFilter.value = filter
  select('jobs', false)
}
function openInbox(category: InboxCategoryKey | '') {
  inboxCategory.value = category
  select('inbox', false)
}
function openTrust(hash: string) {
  trustFocus.value = hash
  select('trust', false)
}
function openConfigNode(nodeID: string) {
  configFocusNode.value = nodeID
  select('config', false)
}
function openUsers(range: { from: string; to: string }) {
  usersRange.value = range
  select('users', false)
}
function refreshAll() { refreshKey.value++ }
async function trackReaudits(ids: number[]) {
  for (const id of ids) if (!trackedReaudits.has(id)) trackedReaudits.set(id, '')
  refreshKey.value++
  if (tracking) return
  tracking = true
  try {
    while (!disposed && trackedReaudits.size) {
      await new Promise(resolve => window.setTimeout(resolve, 2000))
      if (disposed) break
      const currentIDs = [...trackedReaudits.keys()]
      const result = await api.jobs({ ids: currentIDs }, 1, currentIDs.length)
      let changed = false
      const found = new Set<number>()
      for (const job of result.items) {
        found.add(job.id)
        if (trackedReaudits.get(job.id) !== job.status) changed = true
        trackedReaudits.set(job.id, job.status)
        if (['done', 'failed', 'skipped'].includes(job.status)) trackedReaudits.delete(job.id)
      }
      for (const id of currentIDs) if (!found.has(id)) { trackedReaudits.delete(id); changed = true }
      if (changed) refreshKey.value++
    }
  } catch {
    trackedReaudits.clear()
    refreshKey.value++
  } finally { tracking = false }
}
// 待处理角标不单独轮询：概览与待处理页签各自按 30 秒节奏加载 /inbox，并把总数交给角标。
// 类别映射只取一次：政策里声明的 “R07 标题” 用于在所有页面给类别编号补名称。
onMounted(() => { void loadCategoryNames() })
onUnmounted(() => {
  disposed = true
})
</script>

<template>
  <main class="enhance-shell p-4 pt-3 sm:p-6 sm:pt-4">
    <div class="enhance-page space-y-6 pb-8">
      <header>
        <div class="mb-2 flex flex-wrap items-center gap-3"><p class="text-sm font-semibold text-primary-600 dark:text-primary-400">{{ $t('nav.securityAudit') }}</p><SystemUpdatePanel /></div>
        <h1 class="text-2xl font-bold tracking-tight sm:text-3xl">{{ label('title') }}</h1>
        <p class="mt-3 text-gray-500 dark:text-dark-400">{{ label('description') }}</p>
      </header>
      <nav class="flex flex-wrap gap-2" :aria-label="label('title')">
        <button v-for="tab in tabs" :key="tab" class="rounded-xl px-5 py-3 text-sm font-medium transition"
          :class="active === tab ? 'bg-primary-600 text-white' : 'bg-white text-gray-600 hover:bg-primary-50 dark:bg-dark-800 dark:text-dark-300'"
          :aria-current="active === tab ? 'page' : undefined" @click="select(tab)">
          {{ label(tab === 'trust' ? 'trustTab' : tab) }}<span v-if="tab === 'inbox' && inboxTotal" class="ml-2 rounded-full bg-red-600 px-2 py-0.5 text-xs text-white" data-test="inbox-badge">{{ inboxTotal }}</span>
        </button>
      </nav>
      <AuditOverview v-show="active === 'overview'" :active="active === 'overview'" :refresh-key="tabRefresh.overview + refreshKey"
        @inspect-jobs="inspectJobs" @open-inbox="openInbox" @open-users="openUsers" @inbox-loaded="inboxTotal = $event" />
      <AuditInbox v-if="visited.includes('inbox')" v-show="active === 'inbox'" :active="active === 'inbox'" :initial-category="inboxCategory"
        :refresh-key="tabRefresh.inbox + refreshKey" @inspect-jobs="inspectJobs" @open-trust="openTrust" @open-config="openConfigNode" @changed="refreshAll" @loaded="inboxTotal = $event" />
      <AuditRecords v-if="visited.includes('jobs')" v-show="active === 'jobs'" :initial-filter="jobFilter" :initial-job-id="jobDetailID"
        :active="active === 'jobs'" :refresh-key="tabRefresh.jobs + refreshKey" @reaudit-created="trackReaudits" />
      <AuditUsers v-if="visited.includes('users')" v-show="active === 'users'" :active="active === 'users'" :initial-range="usersRange"
        :refresh-key="tabRefresh.users + refreshKey" @inspect-jobs="inspectJobs" />
      <TrustPanel v-if="visited.includes('trust')" v-show="active === 'trust'" :active="active === 'trust'" :focus-hash="trustFocus"
        :refresh-key="tabRefresh.trust + refreshKey" @changed="refreshAll" />
      <AuditConfigPanel v-if="visited.includes('config')" v-show="active === 'config'" :active="active === 'config'" :focusNodeID="configFocusNode"
        :refresh-key="tabRefresh.config" />
    </div>
  </main>
</template>
