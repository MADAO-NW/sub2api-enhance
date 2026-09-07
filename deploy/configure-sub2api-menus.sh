#!/usr/bin/env bash
# 在原版 Sub2API 设置中幂等维护两个增强菜单及其 SVG 图标。
set -euo pipefail

CONFIG_FILE=${1:-/etc/sub2api-enhance/sub2api-enhance.env}

# 审计菜单使用盾牌勾选图标。
PROMPT_AUDIT_ICON='<svg fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.5"><path stroke-linecap="round" stroke-linejoin="round" d="M9 12.75L11.25 15 15 9.75m-3-7.036A11.959 11.959 0 013.598 6 11.99 11.99 0 003 9.749c0 5.592 3.824 10.29 9 11.623 5.176-1.332 9-6.03 9-11.622 0-1.31-.21-2.571-.598-3.751h-.152c-3.196 0-6.1-1.248-8.25-3.285z"/></svg>'

# 额度跟随菜单使用时钟图标表示重置窗口。
QUOTA_FOLLOW_ICON='<svg fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.5"><path stroke-linecap="round" stroke-linejoin="round" d="M12 6v6h4.5m4.5 0a9 9 0 11-18 0 9 9 0 0118 0z"/></svg>'

read_config_value() {
    local key=$1 value
    value=$(awk -v key="$key" 'index($0,key"=")==1 {value=substr($0,length(key)+2)} END {print value}' "$CONFIG_FILE")
    value=${value#\"}
    value=${value%\"}
    printf '%s' "$value"
}

build_menu_payload() {
    local existing=$1 origin=$2
    jq -cn \
        --argjson items "$existing" \
        --arg origin "$origin" \
        --arg audit_icon "$PROMPT_AUDIT_ICON" \
        --arg quota_icon "$QUOTA_FOLLOW_ICON" '
      ($items | map(select(.id == "enhance-prompt-audit")) | first // {}) as $audit
      | ($items | map(select(.id == "enhance-quota-follow")) | first // {}) as $quota
      | ($items | map(select(.id != "enhance-prompt-audit" and .id != "enhance-quota-follow"))) as $base
      | ([$items[]? | (.sort_order // 0)] | max // 0) as $max
      | {custom_menu_items: ($base + [
          ($audit + {
            id: "enhance-prompt-audit",
            label: "第三方提示词审计",
            icon_svg: $audit_icon,
            url: ($origin + "/enhance/third-party-prompt-audit"),
            visibility: "admin",
            sort_order: ($audit.sort_order // ($max + 10))
          }),
          ($quota + {
            id: "enhance-quota-follow",
            label: "用户额度跟随",
            icon_svg: $quota_icon,
            url: ($origin + "/enhance/quota-follow"),
            visibility: "admin",
            sort_order: ($quota.sort_order // ($max + 20))
          })
        ])}
    '
}

configure_menus() {
    local internal_url public_origin admin_key settings existing payload verify
    [[ -r $CONFIG_FILE ]] || { echo "增强服务环境文件不可读：$CONFIG_FILE" >&2; return 1; }
    for tool in awk curl jq; do command -v "$tool" >/dev/null || { echo "缺少 $tool" >&2; return 1; }; done

    internal_url=$(read_config_value SUB2API_INTERNAL_URL)
    public_origin=$(read_config_value ENHANCE_PUBLIC_ORIGIN)
    admin_key=$(read_config_value SUB2API_ADMIN_API_KEY)
    internal_url=${internal_url%/}
    public_origin=${public_origin%/}
    [[ $internal_url =~ ^http://(127\.0\.0\.1|\[::1\]):[0-9]+$ ]] || { echo '原版内部地址必须是本机 HTTP 地址' >&2; return 1; }
    [[ $public_origin =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?$ ]] || { echo '增强服务公网地址必须是 HTTPS Origin' >&2; return 1; }
    [[ -n $admin_key && $admin_key != *$'\n'* && $admin_key != *$'\r'* && $admin_key != *'"'* && $admin_key != *'\'* ]] || { echo '原版管理员 API Key 格式无效' >&2; return 1; }

    settings=$(printf 'header = "x-api-key: %s"\n' "$admin_key" | curl -q --config - --fail --silent --show-error --max-time 30 "$internal_url/api/v1/admin/settings")
    existing=$(jq -ce '.data.custom_menu_items // [] | select(type == "array")' <<< "$settings")
    payload=$(build_menu_payload "$existing" "$public_origin")
    printf 'header = "x-api-key: %s"\n' "$admin_key" | curl -q --config - --fail --silent --show-error --max-time 30 \
        -X PUT -H 'Content-Type: application/json' --data-binary "$payload" "$internal_url/api/v1/admin/settings" >/dev/null

    verify=$(printf 'header = "x-api-key: %s"\n' "$admin_key" | curl -q --config - --fail --silent --show-error --max-time 30 "$internal_url/api/v1/admin/settings")
    jq -e --arg origin "$public_origin" '
      ([.data.custom_menu_items[] | select(.id == "enhance-prompt-audit" and .visibility == "admin" and .url == ($origin + "/enhance/third-party-prompt-audit") and (.icon_svg | startswith("<svg")))] | length == 1)
      and ([.data.custom_menu_items[] | select(.id == "enhance-quota-follow" and .visibility == "admin" and .url == ($origin + "/enhance/quota-follow") and (.icon_svg | startswith("<svg")))] | length == 1)
    ' <<< "$verify" >/dev/null
    unset admin_key settings existing payload verify
    echo 'Sub2API 增强菜单与图标已配置'
}

if [[ ${BASH_SOURCE[0]:-} == "$0" || -z ${BASH_SOURCE[0]:-} ]]; then configure_menus; fi
