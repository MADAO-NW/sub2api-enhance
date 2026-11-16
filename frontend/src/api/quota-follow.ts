import {apiClient} from './client'
export interface FollowConfig {enabled:boolean;monitor_account_id:number}
export interface SavedFollowConfig extends FollowConfig {epoch:string;enabled_at:string|null;revision:number;updated_by:number;updated_at:string}
export interface MonitorAccount {id:number;name:string;platform:string;type:string;parent_account_id:number|null;status:string;schedulable:boolean;deleted:boolean;group_count:number}
export interface MonitorCandidate extends MonitorAccount {problems:string[];valid:boolean}
export interface GroupCount {group_id:number;group_name:string;count:number}
export interface Discovery {monitor_accounts:MonitorCandidate[];subscription_count:number;group_counts:GroupCount[]}
export interface FollowRuntime {epoch:string;monitor_account_id:number;account_name:string;upstream_account_id:string;plan_type:string;baseline_reset_at:string|null;awaiting_window:boolean;used_percent:number|null;fetched_at:string|null;last_checked_at:string|null;next_check_at:string|null;last_reset_at:string|null;last_event_id:number|null;last_error:string;paused_revision:number|null}
export interface Subscription {subscription_id:number;user_id:number;group_id:number;username:string;daily_usage_usd:string;weekly_usage_usd:string;monthly_usage_usd:string;expires_at:string}
export interface ResetRecord {id:number;source:string;source_detail:string;action_type:string;evidence_type:string;user_id:number;username:string;group_id:number|null;subscription_id:number|null;batch_id:number|null;window:string|null;occurred_at:string;status:string;before_usage_usd:string|null;after_usage_usd:string|null;error_message:string}
export interface Batch {id:number;status:string;error_message:string;trigger_type:string;items:{subscription_id:number;username:string;status:string;error_message:string}[];[key:string]:unknown}
export interface RecordDetail {record:ResetRecord;batch?:Batch;[key:string]:unknown}
export interface FollowEvent {id:number;group_id:number|null;trigger_type:string;reset_at:string;status:string;subscription_count:number}
export interface ResetFilter {group_id?:number;source?:string;source_detail?:string;window?:string;status?:string;keyword?:string;from?:string;to?:string;page?:number;page_size?:number}
export interface ImmediateResetResult {event_id:number;status:string}
const base='/admin/quota-follow'
export const quotaFollowAPI={
 async events(page=1){return (await apiClient.get<{items:FollowEvent[];total:number}>(`${base}/events`,{params:{page,page_size:20}})).data},
 async event(id:number){return (await apiClient.get(`${base}/events/${id}`)).data},
 async config(){return (await apiClient.get<SavedFollowConfig>(`${base}/config`)).data},
 async save(config:FollowConfig,revision:number){return (await apiClient.put<SavedFollowConfig>(`${base}/config`,{...config,expected_revision:revision})).data},
 async runtime(){return (await apiClient.get<FollowRuntime>(`${base}/runtime`)).data},
 async discovery(){return (await apiClient.get<Discovery>(`${base}/discovery`)).data},
 async resetNow(input:{expected_revision:number;request_id:string}){return (await apiClient.post<ImmediateResetResult>(`${base}/reset-now`,input)).data},
 async records(params:ResetFilter){return (await apiClient.get<{items:ResetRecord[];total:number}>(`${base}/reset-records`,{params})).data},
 async detail(id:number){return (await apiClient.get<RecordDetail>(`${base}/reset-records/${id}`)).data},
 async retry(id:number){return (await apiClient.post<ImmediateResetResult>(`${base}/batches/${id}/retry`)).data},
 async acknowledge(id:number,note:string){return (await apiClient.post(`${base}/batches/${id}/acknowledge`,{note})).data},
 async reconcile(id:number){return (await apiClient.post<Batch>(`${base}/batches/${id}/reconcile`)).data}
}
