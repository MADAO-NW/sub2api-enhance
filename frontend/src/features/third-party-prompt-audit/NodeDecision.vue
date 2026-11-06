<script setup lang="ts">
import type { NodeResult } from '@/api/admin/third-party-prompt-audit'
import { useAuditLabels } from './labels'

defineProps<{ node: NodeResult }>()
const label = useAuditLabels()
</script>

<template>
  <div class="space-y-1 text-sm" data-test="node-decision">
    <p class="font-medium">{{ node.model_name || node.model_id }}</p>
    <p v-if="node.skipped" class="text-gray-500">{{ label(node.skip_reason || 'aggregation_decided') }}</p>
    <p v-else-if="node.error" class="text-red-600 dark:text-red-400">{{ label('unclassified') }} · {{ node.error.message }} · {{ node.error.code }}</p>
    <template v-else-if="node.confidence != null">
      <p>{{ label('targetScore') }}: {{ node.confidence }}</p>
      <p class="whitespace-pre-wrap break-words">{{ node.reason }}</p>
      <p v-if="node.attempt_id" class="text-xs text-gray-500">{{ label('sourceReference') }} · Attempt #{{ node.attempt_id }}</p>
      <p v-if="node.dispatch" class="text-xs text-gray-500">{{ label('dispatch') }} #{{ node.dispatch.order }} · {{ label(node.dispatch.health) }} · {{ node.dispatch.active }} / {{ node.dispatch.max_concurrency }} · {{ label(node.dispatch.reason) }}</p>
    </template>
    <p v-else>{{ label('noValidScore') }}</p>
  </div>
</template>
