import {apiClient} from './client'
export interface GroupRule {group_id:number;enabled:boolean;observe_only:boolean;reset_daily_enabled:boolean;reset_weekly_enabled:boolean;epoch?:string;enabled_at?:string|null}
export interface FollowConfig {enabled:boolean;min_interval_minutes:number;max_interval_minutes:number;groups:GroupRule[]}
export interface SavedFollowConfig extends FollowConfig {revision:number;updated_by:number;updated_at:string}
export interface Account {id:number;name:string;type:string;is_shadow:boolean;parent_account_id?:number}
export interface Subscription {subscription_id:number;user_id:number;group_id:number;username:string;daily_usage_usd:string;weekly_usage_usd:string;monthly_usage_usd:string;expires_at:string}
export interface Discovery {group_id:number;group_name:string;accounts:Account[];subscriptions:Subscription[]}
export interface AccountState {account:Account;utilization:number|null;next_reset_at:string|null;candidate_reset_at:string|null;consumed_reset_at:string|null;baseline_rebased:boolean;observed_at:string|null;fetched_at:string|null;error:string}
export interface FollowRuntime {group_id:number;group_name:string;account_states:AccountState[];subscription_count:number;last_event_at:string|null;last_checked_at:string|null;next_check_at:string|null;last_error:string}
export interface ResetRecord {id:number;source:string;source_detail:string;action_type:string;evidence_type:string;user_id:number;username:string;group_id:number|null;subscription_id:number|null;batch_id:number|null;window:string|null;occurred_at:string;status:string;before_usage_usd:string|null;after_usage_usd:string|null;error_message:string}
export interface Batch {id:number;status:string;error_message:string;items:{subscription_id:number;username:string;status:string;error_message:string}[];[key:string]:unknown}
export interface RecordDetail {record:ResetRecord;batch?:Batch;[key:string]:unknown}
export interface FollowEvent {id:number;group_id:number|null;trigger_type:string;reset_at:string;status:string;subscription_count:number}
export interface ResetFilter {group_id?:number;source?:string;source_detail?:string;window?:string;status?:string;keyword?:string;from?:string;to?:string;page?:number;page_size?:number}
export interface ImmediateResetResult {event_id:number;group_id:number;status:string}
const base='/admin/quota-follow'
export const quotaFollowAPI={
 async events(page=1){return (await apiClient.get<{items:FollowEvent[];total:number}>(`${base}/events`,{params:{page,page_size:20}})).data},
 async event(id:number){return (await apiClient.get(`${base}/events/${id}`)).data},
 async config(){return (await apiClient.get<SavedFollowConfig>(`${base}/config`)).data},
 async save(config:FollowConfig,revision:number){return (await apiClient.put<SavedFollowConfig>(`${base}/config`,{config,expected_revision:revision})).data},
 async runtime(){return (await apiClient.get<FollowRuntime[]>(`${base}/runtime`)).data},
 async discovery(group_id:number){return (await apiClient.get<Discovery>(`${base}/discovery`,{params:{group_id}})).data},
 async resetNow(input:{group_id:number;expected_revision:number;request_id:string}){return (await apiClient.post<ImmediateResetResult>(`${base}/reset-now`,input)).data},
 async records(params:ResetFilter){return (await apiClient.get<{items:ResetRecord[];total:number}>(`${base}/reset-records`,{params})).data},
 async detail(id:number){return (await apiClient.get<RecordDetail>(`${base}/reset-records/${id}`)).data},
 async retry(id:number){return (await apiClient.post<ImmediateResetResult>(`${base}/batches/${id}/retry`)).data},
 async acknowledge(id:number,note:string){return (await apiClient.post(`${base}/batches/${id}/acknowledge`,{note})).data},
 async reconcile(id:number){return (await apiClient.post<Batch>(`${base}/batches/${id}/reconcile`)).data}
}
