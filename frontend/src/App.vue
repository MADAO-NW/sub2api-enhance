<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { apiClient } from '@/api/client'
import { useAppStore } from '@/stores/app'

const ready = ref(false)
const error = ref('')
const app = useAppStore()
const props = defineProps<{ bootstrapToken: string | null }>()

onMounted(async () => {
  try {
    if (props.bootstrapToken) {
      // Bootstrap 已完成原版管理员校验并签发增强会话，无需再次串行验证。
      await apiClient.post('/auth/bootstrap', { token: props.bootstrapToken })
    } else {
      await apiClient.get('/admin/session')
    }
    ready.value = true
  } catch {
    error.value = '管理员会话验证失败，请从原版后台重新打开此菜单。 / Please reopen this menu from sub2api.'
  }
})
</script>

<template>
  <div v-if="!ready" class="min-h-16" aria-live="polite">
    <span v-if="!error" class="sr-only">正在验证管理员会话</span>
    <p v-else class="mx-auto max-w-xl p-12 text-red-600 dark:text-red-400">{{ error }}</p>
  </div>
  <div v-else>
    <RouterView />
  </div>
  <div v-if="app.message" role="status" class="fixed bottom-6 right-6 z-[100] max-w-lg rounded-xl px-5 py-4 shadow-lg" :class="app.error ? 'bg-red-700 text-white' : 'bg-teal-700 text-white'">
    {{ app.message }}<button class="ml-4" @click="app.message = ''">×</button>
  </div>
</template>
