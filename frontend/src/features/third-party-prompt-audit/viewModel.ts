import type { AuditConfig, ConfigUpdate, CostReport, KeyUpdate, SavedConfig } from '@/api/admin/third-party-prompt-audit'

export function editableConfig(value: SavedConfig): AuditConfig {
  const { mode, platforms, all_groups, group_ids, excluded_user_ids, audit_prompt, models, review_threshold, block_threshold,
	aggregation, worker_count, warning, disable, user_rules, admin_email, retention_days, category_rules, escalate_on_warning } = value
  return JSON.parse(JSON.stringify({ mode, platforms: platforms ?? [], all_groups, group_ids: group_ids ?? [], excluded_user_ids: excluded_user_ids ?? [], audit_prompt,
	models: models ?? [], review_threshold, block_threshold, aggregation, worker_count, warning, disable, user_rules: user_rules ?? [], admin_email,
	retention_days: retention_days ?? 30, category_rules: category_rules ?? {}, escalate_on_warning: escalate_on_warning === true })) as AuditConfig
}

export function configUpdate(config: AuditConfig, revision: number, keys: Record<string, KeyUpdate>, legacyConfirmed = false): ConfigUpdate {
  return { expected_revision: revision, config, legacy_policy_confirmed: legacyConfirmed, keys: config.models.map(model => {
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

// parametersText 把节点高级参数渲染成缩进 JSON 文本，供节点卡片就地编辑。
export function parametersText(parameters?: Record<string, unknown>): string {
  return JSON.stringify(parameters ?? {}, null, 2)
}

// parseParameters 就地校验高级参数：必须是 JSON 对象，且不能覆盖固定请求字段。
export function parseParameters(text: string): { value?: Record<string, unknown>; error?: string; field?: string } {
  if (!text.trim()) return { value: {} }
  let parsed: unknown
  try { parsed = JSON.parse(text) } catch { return { error: 'nodeParametersInvalid' } }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return { error: 'nodeParametersInvalid' }
  const value = parsed as Record<string, unknown>
  for (const reserved of ['model', 'messages', 'stream']) if (reserved in value) return { error: 'nodeParametersReserved', field: reserved }
  return { value }
}

// jobQueryFilter 把“任务 ID / 请求 ID”输入框拆成列表筛选：#开头按任务 ID，其余按请求 ID。
export function jobQueryFilter(value: string | undefined): { ids?: number[]; request_id?: string } {
  const text = (value ?? '').trim()
  if (!text) return {}
  if (!text.startsWith('#')) return { request_id: text }
  const id = Number(text.slice(1))
  return Number.isSafeInteger(id) && id > 0 ? { ids: [id] } : { ids: [0] }
}

// categoryFilter 只接受 R01–R99 形式，避免把自由文本直接当类别查询。
export function categoryFilter(value: string | undefined): { category?: string; invalid?: boolean } {
  const text = (value ?? '').trim().toUpperCase()
  if (!text) return {}
  if (!/^R\d{2}$/.test(text)) return { invalid: true }
  return { category: text }
}

// categoryList 返回可直接展示的类别；空数组表示未分类，由界面补“未分类”文案。
export function categoryList(categories?: string[] | null): string[] {
  return (categories ?? []).filter(item => !!item)
}

export interface CostNodeRow {
  model_id: string
  // node_name 优先取当前配置名称，配置已删除时回退到调用快照。
  node_name: string
  audit_calls: number
  repair_calls: number
  probe_calls: number
  input_tokens: number
  output_tokens: number
  unknown_usage: number
}

export interface CostReuseRow { kind: string; resolution: string; targets: number; share: number }

export interface CostSummary {
  nodes: CostNodeRow[]
  totals: { audit_calls: number; repair_calls: number; probe_calls: number; input_tokens: number; output_tokens: number; unknown_usage: number }
  reuse: CostReuseRow[]
  reuse_targets: number
  reuse_total_targets: number
}

// costSummary 按节点合并分桶数据：业务审核、格式修正与节点测试/健康探测分开计数，用量未知不按 0 计算。
export function costSummary(report: CostReport | null): CostSummary {
  const nodes = new Map<string, CostNodeRow>()
  const totals = { audit_calls: 0, repair_calls: 0, probe_calls: 0, input_tokens: 0, output_tokens: 0, unknown_usage: 0 }
  for (const bucket of report?.buckets ?? []) {
    const row = nodes.get(bucket.model_id) ?? { model_id: bucket.model_id, node_name: bucket.node_name || bucket.model_id,
      audit_calls: 0, repair_calls: 0, probe_calls: 0, input_tokens: 0, output_tokens: 0, unknown_usage: 0 }
    if (bucket.call_kind !== 'audit') {
      row.probe_calls += bucket.calls
      totals.probe_calls += bucket.calls
    } else if (bucket.stage === 'format_repair') {
      row.repair_calls += bucket.calls
      totals.repair_calls += bucket.calls
    } else {
      row.audit_calls += bucket.calls
      totals.audit_calls += bucket.calls
    }
    row.input_tokens += bucket.input_tokens
    row.output_tokens += bucket.output_tokens
    row.unknown_usage += bucket.unknown_usage
    totals.input_tokens += bucket.input_tokens
    totals.output_tokens += bucket.output_tokens
    totals.unknown_usage += bucket.unknown_usage
    nodes.set(bucket.model_id, row)
  }
  const reuseTargets = new Map<string, CostReuseRow>()
  let reuseTotal = 0, reuseSaved = 0
  for (const item of report?.reuse ?? []) {
    reuseTotal += item.targets
    if (item.resolution !== 'model') reuseSaved += item.targets
    const key = `${item.kind}:${item.resolution}`
    const row = reuseTargets.get(key) ?? { kind: item.kind, resolution: item.resolution, targets: 0, share: 0 }
    row.targets += item.targets
    reuseTargets.set(key, row)
  }
  const reuse = [...reuseTargets.values()].map(row => ({ ...row, share: reuseTotal ? row.targets / reuseTotal : 0 }))
  return { nodes: [...nodes.values()].sort((a, b) => a.model_id.localeCompare(b.model_id)), totals, reuse,
    reuse_targets: reuseSaved, reuse_total_targets: reuseTotal }
}

export interface DiffRow { left: string | null; right: string | null; changed: boolean }

// lineDiffLCSLimit 是中段 LCS 表格的最大单元数，超过后退化为逐行对照，避免二次方开销拖垮浏览器。
const lineDiffLCSLimit = 250000

// lineDiff 用行级 LCS 对齐两段文本；差异行两侧都保留原行，供界面并排高亮。
// 先裁掉相同的首尾行，只对中间不同的部分做 LCS：相似指令块通常只在局部插入或改动，
// 这样几十 KB 的全文也能精确对齐，不会因为总行数大而整体退化。
export function lineDiff(left: string, right: string): DiffRow[] {
  const a = (left ?? '').split('\n'), b = (right ?? '').split('\n')
  let start = 0
  while (start < a.length && start < b.length && a[start] === b[start]) start++
  let endA = a.length, endB = b.length
  while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) { endA--; endB-- }
  const same = (line: string): DiffRow => ({ left: line, right: line, changed: false })
  const rows: DiffRow[] = a.slice(0, start).map(same)
  const midA = a.slice(start, endA), midB = b.slice(start, endB)
  if (midA.length * midB.length > lineDiffLCSLimit) {
    const length = Math.max(midA.length, midB.length)
    for (let index = 0; index < length; index++) {
      const l = midA[index] ?? null, r = midB[index] ?? null
      rows.push({ left: l, right: r, changed: l !== r })
    }
  } else {
    const table: number[][] = Array.from({ length: midA.length + 1 }, () => new Array<number>(midB.length + 1).fill(0))
    for (let i = midA.length - 1; i >= 0; i--) {
      for (let j = midB.length - 1; j >= 0; j--) {
        table[i]![j] = midA[i] === midB[j] ? table[i + 1]![j + 1]! + 1 : Math.max(table[i + 1]![j]!, table[i]![j + 1]!)
      }
    }
    let i = 0, j = 0
    while (i < midA.length && j < midB.length) {
      if (midA[i] === midB[j]) { rows.push(same(midA[i]!)); i++; j++ }
      else if (table[i + 1]![j]! >= table[i]![j + 1]!) { rows.push({ left: midA[i]!, right: null, changed: true }); i++ }
      else { rows.push({ left: null, right: midB[j]!, changed: true }); j++ }
    }
    while (i < midA.length) { rows.push({ left: midA[i]!, right: null, changed: true }); i++ }
    while (j < midB.length) { rows.push({ left: null, right: midB[j]!, changed: true }); j++ }
  }
  return rows.concat(a.slice(endA).map(same))
}

export type DiffDisplayRow = DiffRow | { skipped: number }

// collapseDiff 把连续的相同行折叠成一行“N 行相同”，差异行前后各保留 context 行上下文，
// 让长指令块里的少量差异不被淹没在上千行相同内容里。
export function collapseDiff(rows: DiffRow[], context = 3): DiffDisplayRow[] {
  const keep = rows.map(row => row.changed)
  rows.forEach((row, index) => {
    if (!row.changed) return
    for (let offset = Math.max(0, index - context); offset <= Math.min(rows.length - 1, index + context); offset++) keep[offset] = true
  })
  const result: DiffDisplayRow[] = []
  let skipped = 0
  rows.forEach((row, index) => {
    if (keep[index]) {
      if (skipped) { result.push({ skipped }); skipped = 0 }
      result.push(row)
      return
    }
    skipped++
  })
  if (skipped) result.push({ skipped })
  return result
}
