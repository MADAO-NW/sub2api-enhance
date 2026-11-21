<script setup lang="ts">
import { computed } from 'vue'
import type { AuditModel } from '@/api/admin/third-party-prompt-audit'
import { useAuditLabels } from './labels'
import { nodeAPIFormatLabel, nodeHost, nodeInitial } from './viewModel'

const props = withDefaults(defineProps<{
  models: AuditModel[]
  hasKeys: Record<string, boolean>
  disabled?: boolean
}>(), { disabled: false })
const emit = defineEmits<{
  add: []
  edit: [id: string]
  toggle: [id: string, enabled: boolean]
  copy: [id: string]
  move: [id: string, direction: number]
  remove: [id: string]
  import: [file: File]
}>()
const label = useAuditLabels()
const enabledCount = computed(() => props.models.filter(model => model.enabled).length)

function formatName(model: AuditModel): string {
  return model.name || model.model || label('chooseModel')
}

function pickFile(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (file) emit('import', file)
}
</script>

<template>
  <section class="card space-y-4 p-5 sm:p-6" data-test="model-nodes">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div class="flex items-baseline gap-3">
        <h2 class="text-lg font-semibold">{{ label('models') }}</h2>
        <span class="text-sm text-gray-500 dark:text-dark-400" data-test="model-count">
          {{ label('nodeCount', { total: models.length, enabled: enabledCount }) }}
        </span>
      </div>
      <div class="flex flex-wrap gap-2">
        <label class="btn btn-secondary cursor-pointer">
          {{ label('importNodeFile') }}
          <input type="file" accept="application/json,.json" class="hidden" data-test="node-config-file" @change="pickFile" />
        </label>
        <button type="button" class="btn btn-primary" data-test="add-model" :disabled="disabled" @click="emit('add')">
          {{ label('addModel') }}
        </button>
      </div>
    </div>
    <p class="text-sm text-gray-500 dark:text-dark-400">{{ label('modelsHint') }}</p>

    <div v-if="models.length" class="space-y-3">
      <article
        v-for="(model, index) in models"
        :key="model.id"
        class="flex flex-wrap items-center gap-4 rounded-xl border border-gray-200 bg-white p-4 transition dark:border-dark-600 dark:bg-dark-800"
        :class="model.enabled ? '' : 'opacity-70'"
        data-test="model-card"
      >
        <div
          class="flex h-11 w-11 shrink-0 items-center justify-center rounded-full bg-gray-100 text-sm font-semibold text-gray-700 dark:bg-dark-700 dark:text-dark-200"
          aria-hidden="true"
        >{{ nodeInitial(model) }}</div>
        <div class="min-w-0 flex-1">
          <div class="flex flex-wrap items-center gap-2">
            <strong class="truncate text-sm font-semibold">{{ formatName(model) }}</strong>
            <span
              v-if="!model.enabled"
              class="rounded-full bg-gray-100 px-2 py-0.5 text-xs text-gray-500 dark:bg-dark-700 dark:text-dark-300"
            >{{ label('nodeDisabled') }}</span>
            <span
              v-if="!hasKeys[model.id]"
              class="rounded-full bg-amber-50 px-2 py-0.5 text-xs text-amber-700 dark:bg-amber-950/40 dark:text-amber-300"
            >{{ label('keyAbsent') }}</span>
          </div>
          <p class="mt-1 truncate text-xs text-gray-500 dark:text-dark-400">
            {{ nodeHost(model.base_url) }} · {{ nodeAPIFormatLabel(model.api_format) }}
            <template v-if="model.note"> · {{ model.note }}</template>
          </p>
        </div>
        <div class="flex items-center gap-2">
          <button
            type="button"
            role="switch"
            :aria-checked="model.enabled"
            :aria-label="label('nodeToggle')"
            class="relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition"
            :class="model.enabled ? 'bg-primary-600' : 'bg-gray-300 dark:bg-dark-600'"
            data-test="model-toggle"
            :disabled="disabled"
            @click="emit('toggle', model.id, !model.enabled)"
          >
            <span
              class="inline-block h-5 w-5 transform rounded-full bg-white shadow transition"
              :class="model.enabled ? 'translate-x-5' : 'translate-x-0.5'"
            />
          </button>
          <button type="button" class="btn btn-secondary btn-sm" data-test="edit-model" :disabled="disabled" @click="emit('edit', model.id)">
            {{ label('editNode') }}
          </button>
          <div class="flex items-center gap-1 text-xs text-gray-500 dark:text-dark-400">
            <button type="button" class="btn btn-ghost btn-sm" data-test="copy-model" :disabled="disabled" @click="emit('copy', model.id)">{{ label('copyNode') }}</button>
            <button type="button" class="btn btn-ghost btn-sm" :disabled="disabled || index === 0" @click="emit('move', model.id, -1)">{{ label('moveUp') }}</button>
            <button type="button" class="btn btn-ghost btn-sm" :disabled="disabled || index === models.length - 1" @click="emit('move', model.id, 1)">{{ label('moveDown') }}</button>
            <button type="button" class="btn btn-ghost btn-sm text-red-600" :disabled="disabled" @click="emit('remove', model.id)">{{ label('remove') }}</button>
          </div>
        </div>
      </article>
    </div>
    <div v-else class="rounded-xl border border-dashed border-gray-300 p-8 text-center dark:border-dark-600" data-test="model-empty">
      <p class="text-sm font-medium">{{ label('nodeEmpty') }}</p>
      <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ label('nodeEmptyHint') }}</p>
    </div>
  </section>
</template>
