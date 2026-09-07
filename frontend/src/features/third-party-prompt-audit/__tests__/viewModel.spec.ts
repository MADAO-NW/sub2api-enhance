import { describe, expect, it } from 'vitest'
import { configUpdate, editableConfig, timeRange } from '../viewModel'
import type { SavedConfig } from '@/api/admin/third-party-prompt-audit'

export const savedConfig = (): SavedConfig => ({
  mode: 'off', audit_scope: 'full_request', platforms: [], all_groups: true, group_ids: [], audit_prompt: 'editable policy',
  models: [{ id: 'node-a', name: 'Node A', base_url: 'https://example.invalid', model: 'model', enabled: true, timeout_ms: 5000 }],
  review_threshold: null, block_threshold: null, aggregation: 'any_block', worker_count: 4, store_pass_events: true,
  warning: { enabled: false, window: 0, limit: 0 }, disable: { enabled: false, limit: 0 }, admin_email: '',
  revision: 4, warning_rule_revision: 1, has_api_keys: { 'node-a': true }, updated_by: 1, updated_at: '2026-09-06T01:00:00Z', application_error: '', applied_revision: 1, instance_id: 'test-instance', model_defaults: { timeout_ms: 300000 }
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
    for (const key of ['revision', 'has_api_keys', 'fixed_contract', 'fixed_roles', 'default_policy', 'model_defaults']) expect(request.config).not.toHaveProperty(key)
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
