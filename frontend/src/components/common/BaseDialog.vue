<script setup lang="ts">
import {watch,onUnmounted,ref,nextTick} from 'vue'
const props=withDefaults(defineProps<{show:boolean;title:string;width?:string;closeOnEscape?:boolean;closeOnClickOutside?:boolean;showCloseButton?:boolean}>(),{closeOnEscape:true,closeOnClickOutside:false,showCloseButton:true})
const emit=defineEmits<{close:[]}>(),dialog=ref<HTMLElement|null>(null)
let previous:HTMLElement|null=null
watch(()=>props.show,async show=>{if(show){previous=document.activeElement as HTMLElement;document.body.classList.add('modal-open');await nextTick();dialog.value?.focus()}else{document.body.classList.remove('modal-open');previous?.focus()}},{immediate:true})
onUnmounted(()=>document.body.classList.remove('modal-open'))
function onKey(e:KeyboardEvent){if(e.key==='Escape'&&props.closeOnEscape)emit('close');if(e.key==='Tab'){const nodes=dialog.value?.querySelectorAll<HTMLElement>('button,input,select,textarea,a[href],[tabindex="0"]');if(!nodes?.length){e.preventDefault();return}const first=nodes[0]!,last=nodes[nodes.length-1]!;if(e.shiftKey&&(document.activeElement===first||document.activeElement===dialog.value)){e.preventDefault();last.focus()}else if(!e.shiftKey&&document.activeElement===last){e.preventDefault();first.focus()}}}
</script>
<template><Teleport to="body"><div v-if="show" data-test="dialog-backdrop" class="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4" @click.self="closeOnClickOutside&&emit('close')"><section ref="dialog" role="dialog" aria-modal="true" :aria-label="title" tabindex="-1" class="max-h-[90vh] w-full max-w-6xl overflow-auto rounded-2xl bg-white p-6 shadow-xl dark:bg-dark-900" @keydown="onKey"><header class="mb-5 flex items-center justify-between"><h2 class="text-xl font-semibold">{{ title }}</h2><button v-if="showCloseButton" aria-label="关闭 / Close" @click="emit('close')">×</button></header><slot/><footer class="mt-5"><slot name="footer"/></footer></section></div></Teleport></template>
