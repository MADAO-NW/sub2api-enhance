import { useI18n } from 'vue-i18n'

export function useAuditLabels() {
  const { t, te } = useI18n()
  return (value: string) => {
    const key = `admin.thirdPartyPromptAudit.${value}`
    return te(key) ? t(key) : value
  }
}
