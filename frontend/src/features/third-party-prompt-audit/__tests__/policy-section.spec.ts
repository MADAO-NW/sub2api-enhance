import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import ConfigPolicySection from '../ConfigPolicySection.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key.split('.').at(-1), te: () => true }) }))

describe('config policy section', () => {
  it('warns about retired stages and can switch to the default policy', async () => {
    const wrapper = mount(ConfigPolicySection, {
      props: { prompt: 'audit_stage=current_user\n只审最新 user', defaultPolicy: '新版默认政策' },
    })
    expect(wrapper.text()).toContain('deprecatedPolicyHint')
    await wrapper.get('button').trigger('click')
    expect(wrapper.emitted('update:prompt')?.[0]).toEqual(['新版默认政策'])
    wrapper.unmount()
  })

  it('does not warn for a v6 policy and emits typed edits', async () => {
    const wrapper = mount(ConfigPolicySection, { props: { prompt: 'target_kind=instruction 审核标准' } })
    expect(wrapper.text()).not.toContain('deprecatedPolicyHint')
    await wrapper.get('textarea').setValue('target_kind=user 新政策')
    expect(wrapper.emitted('update:prompt')?.at(-1)).toEqual(['target_kind=user 新政策'])
    wrapper.unmount()
  })
})
