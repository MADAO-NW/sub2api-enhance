import { describe, expect, it } from 'vitest'
import {
  categoryFilter, categoryList, collapseDiff, configUpdate, costSummary, editableConfig, jobQueryFilter, lineDiff, parseParameters, timeRange,
} from '../viewModel'
import type { CostReport, SavedConfig } from '@/api/admin/third-party-prompt-audit'

export const savedConfig = (): SavedConfig => ({
  mode: 'off', platforms: [], all_groups: true, group_ids: [], excluded_user_ids: [], audit_prompt: 'editable policy',
  models: [{ id: 'node-a', name: 'Node A', base_url: 'https://example.invalid', model: 'model', enabled: true, timeout_ms: 5000, max_concurrency: 4 }],
  review_threshold: null, block_threshold: null, aggregation: 'any_block', worker_count: 4,
  warning: { enabled: false, window: 0, limit: 0 }, disable: { enabled: false, limit: 0 }, user_rules: [], admin_email: '', retention_days: 30,
  category_rules: { R01: 'immediate_disable' }, escalate_on_warning: true,
  revision: 4, warning_rule_revision: 1, has_api_keys: { 'node-a': true }, updated_by: 1, updated_at: '2026-09-06T01:00:00Z', application_error: '', applied_revision: 1, instance_id: 'test-instance', model_defaults: { timeout_ms: 300000, max_concurrency: 4 },
  breaker_user_threshold: 2,
  rule_defaults: { review_threshold: 0.5, block_threshold: 0.8, warning_window: 10, warning_limit: 3, disable_limit: 5 }
})

describe('third-party audit configuration contract', () => {
  it('saves only editable policy and fields, without fixed protocol or server state', () => {
    const original = { ...savedConfig(), fixed_contract: 'read only', fixed_roles: 'fixed rules', default_policy: 'default' }
    const draft = editableConfig(original)
    draft.audit_prompt = 'new policy'
    expect(original.audit_prompt).toBe('editable policy')
    const request = configUpdate(draft, original.revision, {})
    expect(request.expected_revision).toBe(4)
    expect(request.config.audit_prompt).toBe('new policy')
    for (const key of ['revision', 'has_api_keys', 'fixed_contract', 'fixed_roles', 'default_policy', 'model_defaults', 'breaker_user_threshold']) expect(request.config).not.toHaveProperty(key)
    // 采集相关配置已下线，保留天数必须随可编辑配置一起保存。
    expect(request.config).not.toHaveProperty('capture_when_audit_off')
    expect(request.config).not.toHaveProperty('audit_scope')
    expect(request.config.retention_days).toBe(30)
    // 类别处置与自动加严只在全局配置中，随可编辑配置一起回传。
    expect(request.config.category_rules).toEqual({ R01: 'immediate_disable' })
    expect(request.config.escalate_on_warning).toBe(true)
  })
  it('does not send stale key text when keeping or clearing a key', () => {
    const draft = editableConfig(savedConfig())
    for (const action of ['keep', 'clear'] as const) expect(configUpdate(draft, 4, { 'node-a': { model_id: 'node-a', action, api_key: 'test-only-secret' } }).keys[0]).not.toHaveProperty('api_key')
    expect(configUpdate(draft, 4, { 'node-a': { model_id: 'node-a', action: 'replace', api_key: 'test-only-secret' } }).keys[0]?.api_key).toBe('test-only-secret')
  })
  it('rejects empty, reversed and invalid windows instead of silently changing the filter', () => {
    expect(timeRange('', '')).toBeNull()
    expect(timeRange('invalid', '2026-09-06T00:00')).toBeNull()
    expect(timeRange('2026-09-06T00:01', '2026-09-06T00:00')).toBeNull()
    const range = timeRange('2026-09-05T00:00', '2026-09-06T00:00')
    expect(range?.from.endsWith('Z')).toBe(true)
  })
})

describe('node advanced parameters', () => {
  it('accepts an empty box and a plain JSON object', () => {
    expect(parseParameters('')).toEqual({ value: {} })
    expect(parseParameters('  ')).toEqual({ value: {} })
    expect(parseParameters('{"temperature":0.2}')).toEqual({ value: { temperature: 0.2 } })
  })
  it('rejects non-object JSON and fixed request fields', () => {
    expect(parseParameters('[1,2]').error).toBe('nodeParametersInvalid')
    expect(parseParameters('not json').error).toBe('nodeParametersInvalid')
    expect(parseParameters('{"stream":true}')).toEqual({ error: 'nodeParametersReserved', field: 'stream' })
    expect(parseParameters('{"messages":[]}').field).toBe('messages')
    expect(parseParameters('{"model":"x"}').error).toBe('nodeParametersReserved')
  })
})

describe('job filters and categories', () => {
  it('looks up a job ID only with a # prefix', () => {
    expect(jobQueryFilter('')).toEqual({})
    expect(jobQueryFilter('#42')).toEqual({ ids: [42] })
    expect(jobQueryFilter('req-9')).toEqual({ request_id: 'req-9' })
    expect(jobQueryFilter('#abc')).toEqual({ ids: [0] })
  })
  it('accepts only R01–R99 categories', () => {
    expect(categoryFilter('')).toEqual({})
    expect(categoryFilter(' r7 ')).toEqual({ invalid: true })
    expect(categoryFilter('r07')).toEqual({ category: 'R07' })
    expect(categoryFilter('R107').invalid).toBe(true)
  })
  it('keeps empty categories out of the display list', () => {
    expect(categoryList(undefined)).toEqual([])
    expect(categoryList(['R07', ''])).toEqual(['R07'])
  })
})

describe('audit cost summary', () => {
  const report: CostReport = {
    from: '2026-09-05T00:00:00Z', to: '2026-09-06T00:00:00Z', bucket: 'day', timezone: 'Asia/Shanghai',
    buckets: [
      { bucket: '2026-09-05T00:00:00Z', model_id: 'node-a', node_name: '节点 A', call_kind: 'audit', stage: 'audit', calls: 6, succeeded: 6, input_tokens: 100, output_tokens: 20, unknown_usage: 1, p50_latency_ms: 800 },
      { bucket: '2026-09-05T00:00:00Z', model_id: 'node-a', node_name: '节点 A', call_kind: 'audit', stage: 'format_repair', calls: 2, succeeded: 2, input_tokens: 10, output_tokens: 5, unknown_usage: 0, p50_latency_ms: 700 },
      { bucket: '2026-09-05T00:00:00Z', model_id: 'node-a', node_name: '节点 A', call_kind: 'probe', stage: 'probe', calls: 3, succeeded: 3, input_tokens: 7, output_tokens: 1, unknown_usage: 0, p50_latency_ms: 600 },
      { bucket: '2026-09-05T00:00:00Z', model_id: 'node-b', node_name: '', call_kind: 'health_probe', stage: 'probe', calls: 1, succeeded: 0, input_tokens: 0, output_tokens: 0, unknown_usage: 0, p50_latency_ms: null },
    ],
    reuse: [{ kind: 'instruction', resolution: 'cache', targets: 8 }, { kind: 'instruction', resolution: 'trust', targets: 2 }, { kind: 'user', resolution: 'model', targets: 5 }],
  }
  it('keeps repair, probe and business calls apart and counts unknown usage', () => {
    const summary = costSummary(report)
    const node = summary.nodes.find(item => item.model_id === 'node-a')!
    expect(node.audit_calls).toBe(6)
    expect(node.repair_calls).toBe(2)
    expect(node.probe_calls).toBe(3)
    expect(node.input_tokens).toBe(117)
    expect(node.unknown_usage).toBe(1)
    expect(summary.totals.probe_calls).toBe(4)
    expect(summary.totals.audit_calls).toBe(6)
  })
  it('reports reused targets and their share', () => {
    const summary = costSummary(report)
    expect(summary.reuse_targets).toBe(10)
    expect(summary.reuse_total_targets).toBe(15)
    const cache = summary.reuse.find(item => item.resolution === 'cache')!
    expect(cache.share).toBeCloseTo(8 / 15)
  })
  it('handles an empty report', () => {
    expect(costSummary(null).nodes).toEqual([])
    expect(costSummary(null).reuse_total_targets).toBe(0)
  })
})

describe('line diff', () => {
  it('marks unchanged lines and pairs changed ones', () => {
    const rows = lineDiff('a\nb\nc', 'a\nB\nc')
    expect(rows.filter(row => !row.changed).map(row => row.left)).toEqual(['a', 'c'])
    expect(rows.filter(row => row.changed)).toEqual([
      { left: 'b', right: null, changed: true },
      { left: null, right: 'B', changed: true },
    ])
  })
  it('keeps inserted and removed lines on their own side', () => {
    const rows = lineDiff('one', 'one\ntwo')
    expect(rows).toEqual([{ left: 'one', right: 'one', changed: false }, { left: null, right: 'two', changed: true }])
    expect(lineDiff('a\nb', 'a')).toEqual([{ left: 'a', right: 'a', changed: false }, { left: 'b', right: null, changed: true }])
  })
  it('treats identical texts as fully unchanged', () => {
    expect(lineDiff('x\ny', 'x\ny').every(row => !row.changed)).toBe(true)
  })
  it('aligns a small change inside a long text instead of degrading to line-by-line', () => {
    const body = Array.from({ length: 800 }, (_, index) => `line ${index}`)
    const changedBody = [...body.slice(0, 10), 'injected', ...body.slice(10)]
    const rows = lineDiff(body.join('\n'), changedBody.join('\n'))
    expect(rows.filter(row => row.changed)).toEqual([{ left: null, right: 'injected', changed: true }])
    expect(rows).toHaveLength(801)
  })
  it('collapses long unchanged runs and keeps context around changes', () => {
    const rows = lineDiff(Array.from({ length: 20 }, (_, index) => `l${index}`).join('\n'),
      [...Array.from({ length: 20 }, (_, index) => `l${index}`), 'tail'].join('\n'))
    const display = collapseDiff(rows, 2)
    expect(display[0]).toEqual({ skipped: 18 })
    expect(display.slice(1)).toEqual([
      { left: 'l18', right: 'l18', changed: false }, { left: 'l19', right: 'l19', changed: false },
      { left: null, right: 'tail', changed: true },
    ])
  })
})
