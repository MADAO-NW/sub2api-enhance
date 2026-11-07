<script setup lang="ts">
import { computed } from 'vue'
import { useAuditLabels } from './labels'

const props = defineProps<{ prompt: string; defaultPolicy?: string }>()
const emit = defineEmits<{ (event: 'update:prompt', value: string): void }>()
const label = useAuditLabels()

// 旧版政策仍含已下线的审核阶段名，提示可一键换成新版默认政策。
const deprecated = computed(() => /audit_stage|intent_binding|effective_behavior|current_user/.test(props.prompt))
</script>

<template>
  <div class="space-y-2" data-test="policy-section">
    <p v-if="deprecated" class="rounded-lg bg-amber-50 p-3 text-sm text-amber-800 dark:bg-amber-950/30 dark:text-amber-200">
      {{ label('deprecatedPolicyHint') }}
      <button v-if="defaultPolicy" type="button" class="btn btn-secondary btn-sm ml-2" @click="emit('update:prompt', defaultPolicy)">{{ label('useDefaultPolicy') }}</button>
    </p>
    <label class="block space-y-2">
      <span class="text-sm font-medium">{{ label('policy') }}</span>
      <textarea :value="prompt" class="input min-h-96 font-mono text-sm" rows="18" required data-test="audit-policy" @input="emit('update:prompt', ($event.target as HTMLTextAreaElement).value)" />
    </label>
  </div>
</template>
