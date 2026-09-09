<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref } from 'vue'
import { apiClient, setBootstrapToken, reloadMenu } from '@/api/client'
import { useAppStore } from '@/stores/app'

const ready = ref(false)
const error = ref('')
const app = useAppStore()
const props = defineProps<{ bootstrapToken: string | null }>()
const loginRequired = ref(false)
function requireLogin() { loginRequired.value = true }
onBeforeUnmount(() => window.removeEventListener('enhance-login-required', requireLogin))

onMounted(async () => {
  setBootstrapToken(props.bootstrapToken)
  window.addEventListener('enhance-login-required', requireLogin)
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
    <div v-else class="mx-auto max-w-xl p-6"><p class="text-red-600 dark:text-red-400">{{ error }}</p><button class="btn btn-secondary mt-3" @click="reloadMenu">重新连接 / Reconnect</button></div>
  </div>
  <div v-else>
    <div v-if="loginRequired" role="alert" class="m-4 rounded-lg bg-amber-50 p-4 text-amber-900">原版登录可能已过期，请重新连接；未保存的修改请先留存。 / Reconnect to verify your login. Preserve unsaved changes first.<button class="btn btn-secondary ml-3" @click="reloadMenu">重新连接 / Reconnect</button></div>
    <RouterView />
  </div>
  <div v-if="app.message" role="status" class="fixed bottom-6 right-6 z-[100] max-w-lg rounded-xl px-5 py-4 shadow-lg" :class="app.error ? 'bg-red-700 text-white' : 'bg-teal-700 text-white'">
    {{ app.message }}<button class="ml-4" @click="app.dismiss">×</button>
  </div>
</template>
