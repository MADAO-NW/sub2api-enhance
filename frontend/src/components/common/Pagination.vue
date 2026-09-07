<script setup lang="ts">
import {computed} from 'vue'
const props=defineProps<{page:number;pageSize:number;total:number}>()
const emit=defineEmits<{'update:page':[value:number];'update:pageSize':[value:number];change:[value:number];pageChange:[value:number];pageSizeChange:[value:number]}>()
const pages=computed(()=>Math.max(1,Math.ceil(props.total/props.pageSize)))
function go(page:number){emit('update:page',page);emit('change',page);emit('pageChange',page)}
</script>
<template><nav class="flex items-center justify-between border-t p-4 dark:border-dark-700"><span>{{ total }} · {{ page }} / {{ pages }}</span><div class="flex gap-3"><button class="btn btn-secondary" :disabled="page<=1" @click="go(page-1)">←</button><button class="btn btn-secondary" :disabled="page>=pages" @click="go(page+1)">→</button></div></nav></template>
