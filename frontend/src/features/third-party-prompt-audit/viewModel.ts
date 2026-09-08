import type { AuditConfig, ConfigUpdate, KeyUpdate, SavedConfig } from '@/api/admin/third-party-prompt-audit'

export function editableConfig(value: SavedConfig): AuditConfig {
  const { mode, audit_scope, platforms, all_groups, group_ids, audit_prompt, models, review_threshold, block_threshold,
    aggregation, worker_count, store_pass_events, warning, disable, admin_email } = value
  return JSON.parse(JSON.stringify({ mode, audit_scope, platforms: platforms ?? [], all_groups, group_ids: group_ids ?? [], audit_prompt,
    models: models ?? [], review_threshold, block_threshold, aggregation, worker_count, store_pass_events, warning, disable, admin_email })) as AuditConfig
}

export function configUpdate(config: AuditConfig, revision: number, keys: Record<string, KeyUpdate>): ConfigUpdate {
  return { expected_revision: revision, config, keys: config.models.map(model => {
    const key = keys[model.id] ?? { model_id: model.id, action: 'keep' as const }
    return { model_id: model.id, action: key.action, ...(key.action === 'replace' ? { api_key: key.api_key } : {}) }
  }) }
}

export function toLocalInput(date: Date): string {
  return new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16)
}

export function timeRange(from: string, to: string): { from: string; to: string } | null {
  const start = new Date(from), end = new Date(to)
  if (!from || !to || !Number.isFinite(start.getTime()) || !Number.isFinite(end.getTime()) || start >= end) return null
  return { from: start.toISOString(), to: end.toISOString() }
}

export function formatTime(value: string | null | undefined): string {
  return value ? new Date(value).toLocaleString() : '—'
}

export function formatMS(value: number | null | undefined): string {
  return value == null ? '—' : `${Math.round(value).toLocaleString()} ms`
}

export function sumCounts(value: Record<string, number>): number { return Object.values(value).reduce((sum, count) => sum + count, 0) }

export function decodeCaptureBody(value: string | undefined): string {
  if (!value) return ''
  const bytes = Uint8Array.from(atob(value), character => character.charCodeAt(0))
  try { return new TextDecoder('utf-8', { fatal: true }).decode(bytes) }
  catch { return value }
}
