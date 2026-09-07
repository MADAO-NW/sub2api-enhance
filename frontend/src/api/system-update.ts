import {apiClient} from './client'
export interface BuildInfo {version:string;commit:string;date:string;build_type:string;schema_digest:string}
export interface UpdateStatus {current:BuildInfo;repository:string;supported:boolean;managed:boolean;restart_required:boolean;can_rollback:boolean;state:{phase:string;action:string;target_version:string;error:string;backup?:BuildInfo;updated_at:string}}
export interface UpdateCheck {current_version:string;latest_version:string;has_update:boolean;cached:boolean;release:{tag_name:string;name:string;body:string;html_url:string}}
export const systemUpdateAPI={
 async status(){return (await apiClient.get<UpdateStatus>('/admin/system/status')).data},
 async check(){return (await apiClient.get<UpdateCheck>('/admin/system/check-updates',{params:{force:true}})).data},
 async update(version:string){return (await apiClient.post('/admin/system/update',{version})).data},
 async rollback(version:string){return (await apiClient.post('/admin/system/rollback',{version})).data},
 async restart(){return (await apiClient.post('/admin/system/restart')).data}
}
