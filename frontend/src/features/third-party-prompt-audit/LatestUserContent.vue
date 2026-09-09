<script lang="ts">
// contentCache 按记录 ID 跨列表刷新复用已读取的不可变原文结果。
const contentCache = new Map<string, { content: string | null; unavailable_reason: string }>()
</script>

<script setup lang="ts">
import { onUnmounted, ref } from 'vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { thirdPartyPromptAuditAPI as api, type LatestUserContent } from '@/api/admin/third-party-prompt-audit'
import { extractApiErrorMessage } from '@/utils/apiError'
import { useAuditLabels } from './labels'

const props = defineProps<{ source: 'job' | 'capture'; id: number; variant: 'popover' | 'dialog' }>()
const label = useAuditLabels()
const open = ref(false)
const loading = ref(false)
const result = ref<LatestUserContent | null>(null)
const error = ref('')
const trigger = ref<HTMLElement | null>(null)
const popoverStyle = ref<Record<string, string>>({})
let hideTimer: ReturnType<typeof setTimeout> | undefined

async function load() {
  const key = `${props.source}:${props.id}`
  if (contentCache.has(key)) { result.value = contentCache.get(key)!; return }
  if (loading.value) return
  loading.value = true
  error.value = ''
  try {
    const value = props.source === 'job' ? await api.jobLatestUserContent(props.id) : await api.captureLatestUserContent(props.id)
    contentCache.set(key, value)
    result.value = value
  } catch (err) { error.value = extractApiErrorMessage(err, label('error')) }
  finally { loading.value = false }
}
function cancelHide() { if (hideTimer) clearTimeout(hideTimer); hideTimer = undefined }
function show() {
  cancelHide()
  if (props.variant === 'popover' && trigger.value) {
    const rect = trigger.value.getBoundingClientRect()
    const width = Math.min(384, window.innerWidth - 16)
    popoverStyle.value = { top: `${Math.max(8, Math.min(rect.bottom + 8, window.innerHeight - 304))}px`, left: `${Math.max(8, Math.min(rect.right - width, window.innerWidth - width - 8))}px`, width: `${width}px` }
  }
  open.value = true
  void load()
}
function hidePopover() {
  if (props.variant !== 'popover') return
  cancelHide()
  hideTimer = setTimeout(() => { open.value = false }, 100)
}
onUnmounted(cancelHide)
</script>

<template>
  <span v-if="variant === 'popover'" ref="trigger" class="inline-block" @mouseenter="show" @mouseleave="hidePopover" @focusin="show" @focusout="hidePopover">
    <button type="button" class="text-primary-600 underline" @click="show">{{ label('userContent') }}</button>
    <Teleport to="body">
      <span v-if="open" class="fixed z-[60] block max-w-[calc(100vw-1rem)] rounded-lg border border-gray-200 bg-white p-3 text-left shadow-xl dark:border-dark-700 dark:bg-dark-900" :style="popoverStyle" @mouseenter="cancelHide" @mouseleave="hidePopover">
        <span v-if="loading" class="text-sm">{{ label('loading') }}</span>
        <span v-else-if="error" class="text-sm text-red-600">{{ error }}</span>
        <pre v-else-if="result?.content" class="max-h-72 overflow-auto whitespace-pre-wrap break-words text-xs">{{ result.content }}</pre>
        <span v-else class="text-sm text-gray-500">{{ label(result?.unavailable_reason || 'user_content_not_found') }}</span>
      </span>
    </Teleport>
  </span>
  <template v-else>
    <button type="button" class="btn btn-secondary btn-sm whitespace-nowrap" @click="show">{{ label('viewUserContent') }}</button>
    <BaseDialog :show="open" :title="label('userContent')" :close-on-click-outside="true" @close="open = false">
      <p v-if="loading">{{ label('loading') }}</p>
      <p v-else-if="error" class="text-red-600">{{ error }}</p>
      <pre v-else-if="result?.content" class="max-h-[65vh] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-50 p-4 text-sm dark:bg-dark-950">{{ result.content }}</pre>
      <p v-else class="text-gray-500">{{ label(result?.unavailable_reason || 'user_content_not_found') }}</p>
    </BaseDialog>
  </template>
</template>
