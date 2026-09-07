<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { apiClient } from '@/api/client'
import { useAppStore } from '@/stores/app'
import SystemUpdatePanel from '@/features/system-update/SystemUpdatePanel.vue'
const ready=ref(false), error=ref(''), app=useAppStore()
const props=defineProps<{bootstrapToken:string|null}>()
onMounted(async()=>{try{if(props.bootstrapToken)await apiClient.post('/auth/bootstrap',{token:props.bootstrapToken});await apiClient.get('/admin/session');ready.value=true}catch{error.value='管理员会话验证失败，请从原版后台重新打开此菜单。 / Please reopen this menu from sub2api.'}})
</script>
<template><div v-if="!ready" class="mx-auto max-w-xl p-12"><h1 class="text-2xl font-semibold">sub2api++</h1><p class="mt-5">{{ error || '正在验证原版管理员会话… / Verifying session…' }}</p></div><div v-else><div class="mx-auto flex max-w-[1600px] justify-end px-4 pt-4 sm:px-6"><SystemUpdatePanel/></div><RouterView/></div><div v-if="app.message" role="status" class="fixed bottom-6 right-6 z-[100] max-w-lg rounded-xl px-5 py-4 shadow-lg" :class="app.error?'bg-red-700 text-white':'bg-teal-700 text-white'">{{ app.message }}<button class="ml-4" @click="app.message=''">×</button></div></template>
