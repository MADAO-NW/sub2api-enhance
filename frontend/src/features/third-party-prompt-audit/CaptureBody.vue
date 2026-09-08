<script setup lang="ts">
import { computed } from 'vue'
import type { AuditCapture } from '@/api/admin/third-party-prompt-audit'
import { decodeCaptureBody, formatTime } from './viewModel'
import { useAuditLabels } from './labels'

const props = defineProps<{ capture: AuditCapture }>()
const label = useAuditLabels()
const body = computed(() => decodeCaptureBody(props.capture.raw_body))
</script>

<template>
  <div class="space-y-4">
    <dl class="grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
      <div><dt class="text-gray-500 dark:text-dark-400">ID</dt><dd>#{{ capture.id }}</dd></div>
      <div><dt class="text-gray-500 dark:text-dark-400">{{ label('protocol') }}</dt><dd>{{ capture.protocol }}</dd></div>
      <div><dt class="text-gray-500 dark:text-dark-400">{{ label('captureBytes') }}</dt><dd>{{ capture.body_bytes }}</dd></div>
      <div><dt class="text-gray-500 dark:text-dark-400">{{ label('created') }}</dt><dd>{{ formatTime(capture.created_at) }}</dd></div>
    </dl>
    <p v-if="capture.last_error_message" class="text-sm text-red-600 dark:text-red-400">{{ capture.last_error_message }}</p>
    <pre class="max-h-[60vh] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-950">{{ body || label('inputNotAvailable') }}</pre>
  </div>
</template>
