<script setup lang="ts">
import { onUnmounted, ref } from 'vue'
import CaptureRecords from '@/features/third-party-prompt-audit/CaptureRecords.vue'
import AuditOverview from '@/features/third-party-prompt-audit/AuditOverview.vue'
import AuditRecords from '@/features/third-party-prompt-audit/AuditRecords.vue'
import AuditConfigPanel from '@/features/third-party-prompt-audit/AuditConfigPanel.vue'
import { useAuditLabels } from '@/features/third-party-prompt-audit/labels'
import { thirdPartyPromptAuditAPI as api, type AuditFilter } from '@/api/admin/third-party-prompt-audit'
import SystemUpdatePanel from '@/features/system-update/SystemUpdatePanel.vue'

const label = useAuditLabels()
const requestedTab = new URLSearchParams(location.search).get('tab')
const initialTab = requestedTab && ['overview', 'jobs', 'captures', 'config'].includes(requestedTab) ? requestedTab : 'overview'
const active = ref(initialTab)
const visited = ref(initialTab === 'overview' ? ['overview'] : ['overview', initialTab])
const jobFilter = ref<AuditFilter>({})
const refreshKey = ref(0)
const trackedReaudits = new Map<number, string>()
let tracking = false
let disposed = false
function select(tab: string) { active.value = tab; if (!visited.value.includes(tab)) visited.value.push(tab) }
function inspectJobs(filter: AuditFilter) { jobFilter.value = filter; select('jobs') }
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
onUnmounted(() => { disposed = true })
</script>

<template>
  <main class="enhance-shell p-4 pt-3 sm:p-6 sm:pt-4">
    <div class="enhance-page space-y-6 pb-8">
      <header><div class="mb-2 flex flex-wrap items-center gap-3"><p class="text-sm font-semibold text-primary-600 dark:text-primary-400">{{ $t('nav.securityAudit') }}</p><SystemUpdatePanel /></div><h1 class="text-2xl font-bold tracking-tight sm:text-3xl">{{ label('title') }}</h1><p class="mt-3 text-gray-500 dark:text-dark-400">{{ label('description') }}</p></header>
      <nav class="flex flex-wrap gap-2" :aria-label="label('title')"><button v-for="tab in ['overview', 'jobs', 'captures', 'config']" :key="tab" class="rounded-xl px-5 py-3 text-sm font-medium transition" :class="active === tab ? 'bg-primary-600 text-white' : 'bg-white text-gray-600 hover:bg-primary-50 dark:bg-dark-800 dark:text-dark-300'" :aria-current="active === tab ? 'page' : undefined" @click="select(tab)">{{ label(tab) }}</button></nav>
      <AuditOverview v-show="active === 'overview'" :refresh-key="refreshKey" @inspect-jobs="inspectJobs" />
      <AuditRecords v-if="visited.includes('jobs')" v-show="active === 'jobs'" :initial-filter="jobFilter" :refresh-key="refreshKey" @reaudit-created="trackReaudits" />
      <CaptureRecords v-if="visited.includes('captures')" v-show="active === 'captures'" />
      <AuditConfigPanel v-if="visited.includes('config')" v-show="active === 'config'" />
    </div>
  </main>
</template>
