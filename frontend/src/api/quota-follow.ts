import {apiClient} from './client'
export interface FollowConfig {enabled:boolean;group_id:number|null;reset_weekly_enabled:boolean;reset_daily_enabled:boolean;min_interval_minutes:number;max_interval_minutes:number;observe_only:boolean}
export interface SavedFollowConfig extends FollowConfig {revision:number;epoch:string;enabled_at:string|null;updated_by:number;updated_at:string}
export interface Account {id:number;name:string;type:string}
export interface User {id:number;username:string;email:string}
export interface AccountState {account:Account;utilization:number|null;next_reset_at:string|null;candidate_reset_at:string|null;baseline_rebased?:boolean;observed_at:string|null;suspected_drop:boolean;error:string}
export interface FollowRuntime {carryover_error:string;account_states:AccountState[];accounts:Account[];last_event_at:string|null;last_checked_at:string|null;next_check_at:string|null;last_error:string;paused_revision:number|null;redis_status:string;collector_error:string;original_timezone:string}
export interface QuotaSnapshot {daily_usage_usd:string;weekly_usage_usd:string;daily_window_start:string|null;weekly_window_start:string|null;observed_at:string}
export interface ResetRecord {action_type:string;before_usage_usd?:string|null;after_usage_usd?:string|null;accounts?:Account[];id:number;source:string;source_detail:string;evidence_type:string;user_id:number;username:string;window:string|null;occurred_at:string;detected_at:string;status:string;event_id:number|null;delivery_id:number|null;audit_log_id:number|null;request_id:string;error_message:string;before_snapshot?:QuotaSnapshot|null;after_snapshot?:QuotaSnapshot|null;evidence?:unknown}
export interface ResetFilter {source?:string;source_detail?:string;window?:string;status?:string;keyword?:string;from?:string;to?:string;page?:number;page_size?:number}
export interface RecordDetail {record:ResetRecord;carryover?:{id:number;status:string;cache_status:string;error_message:string;[key:string]:unknown};event?:{accounts:AccountState[];reset_at:string;[key:string]:unknown};delivery?:{id:number;status:string;database_status:string;cache_status:string;http_status:number|null;error_message:string;request_body:string;response_body:string;before_snapshot:QuotaSnapshot|null;after_snapshot:QuotaSnapshot|null;[key:string]:unknown}}
export interface ImmediateResetItem {user_id:number;username:string;window:'daily'|'weekly';request_id:string;status:string;http_status:number;error:string}
export interface ImmediateResetResult {group_id:number;windows:('daily'|'weekly')[];total:number;succeeded:number;non_success:number;items:ImmediateResetItem[]}
const base='/admin/quota-follow'
export const quotaFollowAPI={
 async repairCache(id:number){return (await apiClient.post(`${base}/reset-records/${id}/repair-cache`)).data},
 async events(page=1){return (await apiClient.get<{items:{id:number;reset_at:string;status:string}[];total:number}>(`${base}/events`,{params:{page,page_size:20}})).data},
 async event(id:number){return (await apiClient.get(`${base}/events/${id}`)).data},
 async config(){return (await apiClient.get<SavedFollowConfig>(`${base}/config`)).data},
 async save(config:FollowConfig,revision:number){return (await apiClient.put<SavedFollowConfig>(`${base}/config`,{config,expected_revision:revision})).data},
 async runtime(){return (await apiClient.get<FollowRuntime>(`${base}/runtime`)).data},
 async discovery(){return (await apiClient.get<{accounts:Account[];users:User[]}>(`${base}/discovery`)).data},
 async resetNow(input:{group_id:number;windows:('daily'|'weekly')[]}){return (await apiClient.post<ImmediateResetResult>(`${base}/reset-now`,input)).data},
 async records(params:ResetFilter){return (await apiClient.get<{items:ResetRecord[];total:number}>(`${base}/reset-records`,{params})).data},
 async detail(id:number){return (await apiClient.get<RecordDetail>(`${base}/reset-records/${id}`)).data},
 async reconcile(id:number){return (await apiClient.post(`${base}/deliveries/${id}/reconcile`)).data}
}
