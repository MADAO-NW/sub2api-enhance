import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import Pagination from './Pagination.vue'

describe('Pagination', () => {
  it('supports first, last, specified page and page size changes', async () => {
    const wrapper = mount(Pagination, { props: { page: 2, pageSize: 20, total: 205, pageSizeOptions: [20, 50, 100, 200] } })
    await wrapper.get('button[aria-label="末页"]').trigger('click')
    expect(wrapper.emitted('update:page')?.at(-1)).toEqual([11])
    await wrapper.get('input[aria-label="指定页码"]').setValue('7')
    await wrapper.get('button[aria-label="跳转"]').trigger('click')
    expect(wrapper.emitted('update:page')?.at(-1)).toEqual([7])
    await wrapper.get('button[aria-label="首页"]').trigger('click')
    expect(wrapper.emitted('update:page')?.at(-1)).toEqual([1])
    await wrapper.get('select[aria-label="每页数量"]').setValue('100')
    expect(wrapper.emitted('update:pageSize')?.at(-1)).toEqual([100])
  })
})
