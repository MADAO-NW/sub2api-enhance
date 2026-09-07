<script setup lang="ts">
import {computed,onBeforeUnmount,ref} from 'vue'
import {useI18n} from 'vue-i18n'
import {systemUpdateAPI as api,type UpdateStatus,type UpdateCheck} from '@/api/system-update'
import BaseDialog from '@/components/common/BaseDialog.vue'
import {extractApiErrorMessage} from '@/utils/apiError'
const {locale}=useI18n()
const en=computed(()=>locale.value.startsWith('en'))
const text=(zh:string,english:string)=>en.value?english:zh
const shown=ref(false),status=ref<UpdateStatus|null>(null),check=ref<UpdateCheck|null>(null),loading=ref(false),error=ref(''),notice=ref(''),confirmation=ref<'update'|'rollback'|'restart'|null>(null)
let poll:ReturnType<typeof setTimeout>|undefined
const running=computed(()=>['downloading','applying'].includes(status.value?.state.phase||''))
const phase=computed(()=>{
 const names:Record<string,[string,string]>={idle:['尚无更新操作','No update operation'],downloading:['正在准备发布包','Preparing release'],applying:['正在应用版本','Applying release'],ready:['版本已应用，等待重启','Applied; restart required'],completed:['版本操作已完成','Completed'],failed:['操作未完成，请检查原因','Operation incomplete']}
 const value=names[status.value?.state.phase||'idle'];return value?text(...value):status.value?.state.phase
})
async function refresh(){if(poll)clearTimeout(poll);status.value=await api.status();if(running.value&&shown.value){poll=setTimeout(()=>refresh().catch(e=>{error.value=extractApiErrorMessage(e,text('读取状态失败','Unable to read update status'))}),2000)}}
async function open(){shown.value=true;error.value='';notice.value='';loading.value=true;try{await refresh()}catch(e){error.value=extractApiErrorMessage(e,text('读取版本失败','Unable to read version'))}finally{loading.value=false}}
function close(){shown.value=false;confirmation.value=null;if(poll)clearTimeout(poll)}
async function checkVersion(){loading.value=true;error.value='';try{check.value=await api.check()}catch(e){error.value=extractApiErrorMessage(e,text('检查更新失败','Update check failed'))}finally{loading.value=false}}
async function execute(){const action=confirmation.value;if(!action)return;confirmation.value=null;loading.value=true;error.value='';try{
 if(action==='restart')await api.restart()
 else if(action==='update'){if(!check.value)return;await api.update(check.value.latest_version)}
 else {const version=status.value?.state.backup?.version;if(!version)return;await api.rollback(version)}
 if(action==='restart'){if(poll)clearTimeout(poll);notice.value=text('增强服务正在重启。原版会话令牌不会被持久保存，请稍后从原版后台重新打开此菜单。','Enhancement is restarting. Reopen this menu from sub2api afterward to renew the administrator session.');status.value=null}
 else{if(poll)clearTimeout(poll);await refresh()}
 }catch(e){error.value=extractApiErrorMessage(e,text('版本操作失败','Version operation failed'))}finally{loading.value=false}}
onBeforeUnmount(()=>{if(poll)clearTimeout(poll)})
</script>
<template>
 <button class="btn btn-secondary text-sm" data-test="open-update" @click="open">{{text('版本与更新','Version & updates')}}</button>
 <BaseDialog :show="shown" :title="text('增强服务版本与更新','Enhancement updates')" @close="close">
  <div class="space-y-4">
   <p class="text-sm text-gray-500">MADAO-NW/sub2api-enhance</p>
   <p class="rounded-lg bg-amber-50 p-3 text-sm text-amber-900 dark:bg-amber-950 dark:text-amber-200">{{text('更新将替换增强服务程序，重启时执行新增迁移并短暂中断增强代理连接。配置与数据保留；迁移集合不同时禁止直接恢复旧二进制。','Updates replace the enhancement binary. Restart applies new migrations and briefly interrupts enhancement proxy connections. Configuration and data remain. Binary rollback requires identical migrations.')}}</p>
   <p v-if="error" role="alert" class="text-red-600">{{error}}</p><p v-if="notice" role="status">{{notice}}</p>
   <template v-if="status">
    <dl class="space-y-2 text-sm"><div>{{text('当前版本','Current')}}：{{status.current.version}} · {{status.current.build_type}}</div><div>Commit：{{status.current.commit}}</div><div>{{text('构建时间','Built')}}：{{status.current.date}}</div><div>{{text('更新状态','Update state')}}：{{phase}} <span v-if="status.state.target_version">→ {{status.state.target_version}}</span></div></dl>
    <p v-if="!status.supported" class="text-sm text-gray-500">{{text('源码运行可检查版本；在线替换仅用于 Linux Release 脚本部署。','Source builds can check versions. In-place updates require a Linux Release installation.')}}</p>
    <p v-if="status.state.error" class="text-red-600">{{status.state.error}}</p>
    <div class="flex flex-wrap gap-3">
     <button class="btn btn-secondary" :disabled="loading||running" data-test="check-update" @click="checkVersion">{{text('检测更新','Check updates')}}</button>
     <button v-if="status.supported&&check?.has_update&&!status.restart_required" class="btn btn-primary" :disabled="loading||running" data-test="apply-update" @click="confirmation='update'">{{text('下载并应用更新','Download and apply')}}</button>
     <button v-if="status.supported&&status.managed" class="btn btn-secondary" :disabled="loading||running" @click="confirmation='restart'">{{text('重启增强服务','Restart enhancement')}}</button>
     <button v-if="status.can_rollback" class="btn btn-secondary" :disabled="loading||running" @click="confirmation='rollback'">{{text('恢复备份版本','Restore backup')}}</button>
    </div>
    <template v-if="check"><p>{{text('最新稳定版','Latest stable')}}：{{check.latest_version}} <span v-if="!check.has_update">· {{text('当前无可用升级','No upgrade available')}}</span></p><a :href="check.release.html_url" target="_blank" rel="noopener noreferrer" class="text-primary-600 underline">{{text('查看 GitHub Release','View GitHub Release')}}</a><pre class="max-h-72 overflow-auto whitespace-pre-wrap break-all text-sm">{{check.release.body}}</pre></template>
    <div v-if="confirmation" class="space-y-3 rounded-lg border p-4 dark:border-dark-700" data-test="update-confirmation"><p>{{confirmation==='update'?text('确认更新到','Update to'):confirmation==='rollback'?text('确认恢复备份','Restore backup'):text('确认重启增强服务','Restart enhancement')}} {{confirmation==='update'?check?.latest_version:confirmation==='rollback'?status.state.backup?.version:status.current.version}}？</p><div class="flex gap-3"><button class="btn btn-primary" data-test="confirm-update" @click="execute">{{text('确认执行','Confirm')}}</button><button class="btn btn-secondary" @click="confirmation=null">{{text('取消','Cancel')}}</button></div></div>
   </template>
  </div>
 </BaseDialog>
</template>
