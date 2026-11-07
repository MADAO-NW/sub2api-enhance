<script setup lang="ts">
import { computed } from 'vue'
import { lineDiff } from './viewModel'
import { useAuditLabels } from './labels'

const props = defineProps<{ left: string; right: string }>()
const label = useAuditLabels()
const rows = computed(() => lineDiff(props.left ?? '', props.right ?? ''))
const changed = computed(() => rows.value.filter(row => row.changed).length)
</script>

<template>
  <div class="space-y-2" data-test="text-diff">
    <p class="text-xs text-gray-500 dark:text-dark-400">{{ label('diffHint') }} · {{ label('diffChanged') }}: {{ changed }}</p>
    <div class="max-h-72 overflow-auto rounded-lg border border-gray-200 dark:border-dark-700">
      <table class="w-full table-fixed text-left font-mono text-xs">
        <tbody>
          <tr v-for="(row, index) in rows" :key="index" :class="row.changed ? 'bg-amber-50 dark:bg-amber-950/30' : ''">
            <td class="w-1/2 whitespace-pre-wrap break-words border-r border-gray-200 p-1 align-top dark:border-dark-700">{{ row.left ?? '' }}</td>
            <td class="w-1/2 whitespace-pre-wrap break-words p-1 align-top">{{ row.right ?? '' }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
