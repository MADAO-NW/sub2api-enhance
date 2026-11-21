import { useI18n } from 'vue-i18n'

export function useAuditLabels() {
  const { t, te } = useI18n()
  return (value: string, params?: Record<string, unknown>) => {
    const key = `admin.thirdPartyPromptAudit.${value}`
    return te(key) ? t(key, params ?? {}) : value
  }
}
