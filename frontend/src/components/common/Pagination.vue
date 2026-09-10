<script setup lang="ts">
import { computed, ref, watch } from 'vue'

const props = withDefaults(defineProps<{
  page: number
  pageSize: number
  total: number
  pageSizeOptions?: number[]
  showPageSizeSelector?: boolean
}>(), {
  pageSizeOptions: () => [20, 50, 100, 200],
  showPageSizeSelector: true,
})
const emit = defineEmits<{
  'update:page': [value: number]
  'update:pageSize': [value: number]
  change: [value: number]
  pageChange: [value: number]
  pageSizeChange: [value: number]
}>()
const pages = computed(() => Math.max(1, Math.ceil(props.total / props.pageSize)))
const targetPage = ref(props.page)

watch(() => props.page, value => { targetPage.value = value })
watch(pages, value => {
  if (targetPage.value > value) targetPage.value = value
})

function go(value: number) {
  const page = Math.min(pages.value, Math.max(1, Math.trunc(value || 1)))
  targetPage.value = page
  if (page === props.page) return
  emit('update:page', page)
  emit('change', page)
  emit('pageChange', page)
}

function changeSize(event: Event) {
  const value = Number((event.target as HTMLSelectElement).value)
  emit('update:pageSize', value)
  emit('pageSizeChange', value)
}
</script>

<template>
  <nav class="flex flex-wrap items-center justify-between gap-3 border-t p-4 dark:border-dark-700">
    <span>{{ total }} · {{ page }} / {{ pages }}</span>
    <div class="flex flex-wrap items-center gap-2">
      <select v-if="showPageSizeSelector" class="input w-auto min-w-20" :value="pageSize" aria-label="每页数量" @change="changeSize">
        <option v-for="option in pageSizeOptions" :key="option" :value="option">{{ option }}</option>
      </select>
      <button class="btn btn-secondary" :disabled="page <= 1" aria-label="首页" @click="go(1)">«</button>
      <button class="btn btn-secondary" :disabled="page <= 1" aria-label="上一页" @click="go(page - 1)">←</button>
      <input v-model.number="targetPage" class="input w-20" type="number" min="1" :max="pages" aria-label="指定页码" @keyup.enter="go(targetPage)" />
      <button class="btn btn-secondary" aria-label="跳转" @click="go(targetPage)">跳转</button>
      <button class="btn btn-secondary" :disabled="page >= pages" aria-label="下一页" @click="go(page + 1)">→</button>
      <button class="btn btn-secondary" :disabled="page >= pages" aria-label="末页" @click="go(pages)">»</button>
    </div>
  </nav>
</template>
