<script setup lang="ts">
import { ref } from 'vue'
import CaptureRecords from '@/features/third-party-prompt-audit/CaptureRecords.vue'
import AuditOverview from '@/features/third-party-prompt-audit/AuditOverview.vue'
import AuditRecords from '@/features/third-party-prompt-audit/AuditRecords.vue'
import AuditConfigPanel from '@/features/third-party-prompt-audit/AuditConfigPanel.vue'
import { useAuditLabels } from '@/features/third-party-prompt-audit/labels'
import type { AuditFilter } from '@/api/admin/third-party-prompt-audit'

const label = useAuditLabels()
const active = ref('overview')
const visited = ref(['overview'])
const jobFilter = ref<AuditFilter>({})
function select(tab: string) { active.value = tab; if (!visited.value.includes(tab)) visited.value.push(tab) }
function inspectJobs(filter: AuditFilter) { jobFilter.value = filter; select('jobs') }
</script>

<template>
  <main class="p-4 sm:p-6">
    <div class="mx-auto max-w-[1600px] space-y-6 pb-8">
      <header><p class="mb-2 text-sm font-semibold text-primary-600 dark:text-primary-400">{{ $t('nav.securityAudit') }}</p><h1 class="text-2xl font-bold tracking-tight sm:text-3xl">{{ label('title') }}</h1><p class="mt-3 text-gray-500 dark:text-dark-400">{{ label('description') }}</p></header>
      <nav class="flex flex-wrap gap-2" :aria-label="label('title')"><button v-for="tab in ['overview', 'events', 'jobs', 'captures', 'config']" :key="tab" class="rounded-xl px-5 py-3 text-sm font-medium transition" :class="active === tab ? 'bg-primary-600 text-white' : 'bg-white text-gray-600 hover:bg-primary-50 dark:bg-dark-800 dark:text-dark-300'" :aria-current="active === tab ? 'page' : undefined" @click="select(tab)">{{ label(tab) }}</button></nav>
      <AuditOverview v-show="active === 'overview'" @inspect-jobs="inspectJobs" />
      <AuditRecords v-if="visited.includes('events')" v-show="active === 'events'" source="events" />
      <AuditRecords v-if="visited.includes('jobs')" v-show="active === 'jobs'" source="jobs" :initial-filter="jobFilter" />
      <CaptureRecords v-if="visited.includes('captures')" v-show="active === 'captures'" />
      <AuditConfigPanel v-if="visited.includes('config')" v-show="active === 'config'" />
    </div>
  </main>
</template>
