<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { collapseDiff, lineDiff } from './viewModel'
import { useAuditLabels } from './labels'

const props = defineProps<{ left: string; right: string }>()
const label = useAuditLabels()
const { t } = useI18n()
const rows = computed(() => lineDiff(props.left ?? '', props.right ?? ''))
const changed = computed(() => rows.value.filter(row => row.changed).length)
// 相同的长段落折叠显示，差异行前后各保留 3 行上下文。
const display = computed(() => collapseDiff(rows.value))
</script>

<template>
  <div class="space-y-2" data-test="text-diff">
    <p class="text-xs text-gray-500 dark:text-dark-400">{{ label('diffHint') }} · {{ label('diffChanged') }}: {{ changed }}</p>
    <div class="max-h-72 overflow-auto rounded-lg border border-gray-200 dark:border-dark-700">
      <table class="w-full table-fixed text-left font-mono text-xs">
        <tbody>
          <template v-for="(row, index) in display" :key="index">
            <tr v-if="'skipped' in row" class="bg-gray-50 text-center text-gray-500 dark:bg-dark-800 dark:text-dark-400" data-test="diff-skipped">
              <td colspan="2" class="p-1">{{ t('admin.thirdPartyPromptAudit.diffSkipped', { count: row.skipped }) }}</td>
            </tr>
            <tr v-else :class="row.changed ? 'bg-amber-50 dark:bg-amber-950/30' : ''">
              <td class="w-1/2 whitespace-pre-wrap break-words border-r border-gray-200 p-1 align-top dark:border-dark-700">{{ row.left ?? '' }}</td>
              <td class="w-1/2 whitespace-pre-wrap break-words p-1 align-top">{{ row.right ?? '' }}</td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>
  </div>
</template>
