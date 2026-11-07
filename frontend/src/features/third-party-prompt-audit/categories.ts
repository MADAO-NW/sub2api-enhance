import { reactive } from 'vue'
import { thirdPartyPromptAuditAPI as api } from '@/api/admin/third-party-prompt-audit'

// categoryNames 是政策里声明的类别编号到标题的映射，如 R07 → 恶意使用。
const categoryNames = reactive<Record<string, string>>({})
let pending: Promise<void> | null = null

// loadCategoryNames 只请求一次类别映射；接口不可用时保持空，界面继续显示编号。
export function loadCategoryNames(): Promise<void> {
  if (!pending) {
    pending = api.categories()
      .then(result => { for (const item of result.items) categoryNames[item.code] = item.name })
      .catch(() => { pending = null })
  }
  return pending
}

// categoryText 在编号后补上政策标题；政策未声明该编号时原样返回编号。
export function categoryText(code: string): string {
  return categoryNames[code] ? `${code} ${categoryNames[code]}` : code
}

// categoryName 只返回政策标题，供编辑类界面在编号输入框旁提示含义。
export function categoryName(code: string): string {
  return categoryNames[code] ?? ''
}

// categoryOptions 返回政策声明的类别（按编号排序），供列表筛选与规则下拉使用。
export function categoryOptions(): { code: string; name: string }[] {
  return Object.entries(categoryNames)
    .map(([code, name]) => ({ code, name }))
    .sort((left, right) => left.code.localeCompare(right.code))
}
