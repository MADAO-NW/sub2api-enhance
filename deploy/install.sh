#!/usr/bin/env bash
# 基于 sub2api 的脚本安装流程适配：只管理 sub2api-enhance 的文件和 systemd 服务。
set -euo pipefail

GITHUB_REPO='MADAO-NW/sub2api-enhance'
INSTALL_DIR='/opt/sub2api-enhance'
SERVICE_NAME='sub2api-enhance'
CONFIG_DIR='/etc/sub2api-enhance'
UPDATE_STAGE=''
# 与原版更新器一致的发布包体积边界。
MAX_DOWNLOAD_SIZE=$((500*1024*1024))

validate_version() {
    [[ $1 =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo '版本必须为 v主版本.次版本.修订号' >&2; return 1; }
}

# 继承原版独立 UPDATE_GITHUB_TOKEN 绑定，只给 api.github.com 发送凭据。
github_curl() {
    local url=$1
    shift
    if [[ -n ${UPDATE_GITHUB_TOKEN:-} && $url == https://api.github.com/* ]]; then
        if [[ $UPDATE_GITHUB_TOKEN == *$'\n'* || $UPDATE_GITHUB_TOKEN == *$'\r'* || $UPDATE_GITHUB_TOKEN == *'"'* || $UPDATE_GITHUB_TOKEN == *'\'* ]]; then
            echo 'UPDATE_GITHUB_TOKEN 格式无效' >&2; return 1
        fi
        printf 'header = "Authorization: Bearer %s"\n' "$UPDATE_GITHUB_TOKEN" | UPDATE_GITHUB_TOKEN= GITHUB_TOKEN= GH_TOKEN= curl -q --config - --globoff --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 300 --max-filesize "$MAX_DOWNLOAD_SIZE" "$url" "$@"
    else
        UPDATE_GITHUB_TOKEN= GITHUB_TOKEN= GH_TOKEN= curl -q --globoff --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 300 --max-filesize "$MAX_DOWNLOAD_SIZE" "$url" "$@"
    fi
}

# jq 只解析已下载的 GitHub 元数据，不以文本替换拼接执行代码。
prepare_release() {
    local requested=$1 arch=$2 endpoint version archive archive_url checksum_url
    endpoint="https://api.github.com/repos/$GITHUB_REPO/releases/latest"
    if [[ -n $requested ]]; then validate_version "$requested"; endpoint="https://api.github.com/repos/$GITHUB_REPO/releases/tags/$requested"; fi
    github_curl "$endpoint" -H 'Accept: application/vnd.github+json' -o "$UPDATE_STAGE/release-api.json"
    version=$(jq -er 'select(.draft == false and .prerelease == false) | .tag_name' "$UPDATE_STAGE/release-api.json")
    validate_version "$version"
    [[ -z $requested || $requested == "$version" ]] || { echo '标签与返回版本不一致' >&2; return 1; }
    archive="sub2api-enhance_${version#v}_linux_${arch}.tar.gz"
    archive_url=$(jq -er --arg name "$archive" '[.assets[] | select(.name == $name)] | select(length == 1) | .[0].url' "$UPDATE_STAGE/release-api.json")
    checksum_url=$(jq -er '[.assets[] | select(.name == "checksums.txt")] | select(length == 1) | .[0].url' "$UPDATE_STAGE/release-api.json")
    for address in "$archive_url" "$checksum_url"; do
        [[ $address =~ ^https://api\.github\.com/repos/MADAO-NW/sub2api-enhance/releases/assets/[0-9]+$ ]] || { echo '发布资源不属于增强仓库' >&2; return 1; }
    done
    github_curl "$archive_url" -H 'Accept: application/octet-stream' -o "$UPDATE_STAGE/$archive"
    github_curl "$checksum_url" -H 'Accept: application/octet-stream' -o "$UPDATE_STAGE/checksums.txt"
    verify_archive "$UPDATE_STAGE/$archive" "$archive" "$UPDATE_STAGE/checksums.txt"
    extract_member "$UPDATE_STAGE/$archive" sub2api-enhance "$UPDATE_STAGE/sub2api-enhance"
    extract_member "$UPDATE_STAGE/$archive" release.json "$UPDATE_STAGE/release.json"
    extract_member "$UPDATE_STAGE/$archive" deploy/.env.example "$UPDATE_STAGE/env.example"
    extract_member "$UPDATE_STAGE/$archive" deploy/sub2api-enhance.service "$UPDATE_STAGE/sub2api-enhance.service"
    if [[ $(tar -tzf "$UPDATE_STAGE/$archive" | awk '$0 == "deploy/configure-sub2api-menus.sh" {n++} END {print n+0}') == 1 ]]; then
        extract_member "$UPDATE_STAGE/$archive" deploy/configure-sub2api-menus.sh "$UPDATE_STAGE/configure-sub2api-menus.sh"
    fi
    jq -e --arg version "${version#v}" '.build_type == "release" and .version == $version and (.schema_digest | test("^[0-9a-f]{64}$"))' "$UPDATE_STAGE/release.json" >/dev/null
    chmod 0755 "$UPDATE_STAGE/sub2api-enhance"
    [[ $("$UPDATE_STAGE/sub2api-enhance" --version) == "${version#v}" ]] || { echo '二进制版本与标签不一致' >&2; return 1; }
    [[ $("$UPDATE_STAGE/sub2api-enhance" --schema-digest) == "$(jq -er '.schema_digest' "$UPDATE_STAGE/release.json")" ]] || { echo '迁移指纹不一致' >&2; return 1; }
    printf '%s\n' "已下载并校验 $version ($arch)"
}

verify_archive() {
    local archive_path=$1 archive_name=$2 checksum_path=$3 expected actual
    expected=$(awk -v name="$archive_name" '$2 == name || $2 == "*"name {print $1}' "$checksum_path")
    [[ $expected =~ ^[0-9a-f]{64}$ ]] || { echo '缺少唯一有效的 SHA256 校验项' >&2; return 1; }
    actual=$(sha256sum "$archive_path"); actual=${actual%% *}
    [[ $actual == "$expected" ]] || { echo '安装包 SHA256 校验失败，未修改服务' >&2; return 1; }
}

# 仅输出精确命名的单个常规文件，不将归档直接解压到安装目录。
extract_member() {
    local archive=$1 member=$2 output=$3 listing count
    count=$(tar -tzf "$archive" | awk -v name="$member" '$0 == name {n++} END {print n+0}')
    [[ $count == 1 ]] || { echo "发布包文件缺失或重复：$member" >&2; return 1; }
    listing=$(tar -tvzf "$archive" -- "$member")
    [[ $listing == -* ]] || { echo "发布包成员不是常规文件：$member" >&2; return 1; }
    tar -xOzf "$archive" -- "$member" > "$output"
}

health_check() {
    local bind host port attempt
    bind=$(awk -F= '$1 == "ENHANCE_LISTEN" {print substr($0,index($0,"=")+1)}' "$CONFIG_DIR/sub2api-enhance.env" | tail -1)
    bind=${bind#\"}; bind=${bind%\"}
    [[ -n $bind ]] || bind='127.0.0.1:18081'
    host=${bind%:*}; port=${bind##*:}
    [[ $port =~ ^[0-9]+$ ]] || return 1
    case "$host" in ''|0.0.0.0) host=127.0.0.1;; '[::]') host='[::1]';; esac
    for attempt in {1..30}; do
        if curl -q --noproxy '*' --fail --silent --max-time 1 "http://$host:$port/health" | jq -e --arg version "$1" '.service == "sub2api++" and .status == "ok" and .version == $version' >/dev/null; then return 0; fi
        sleep 1
    done
    echo '增强服务健康检查未通过。保留备份；数据库不会自动降级，请查看 journalctl。' >&2
    return 1
}

main() {
    local command=${1:-install} version='' arch current candidate schema_current schema_candidate
    [[ $# == 0 ]] || shift
    if [[ $command == --help || $command == -h ]]; then
        echo '用法：install.sh [install|upgrade|rollback] [-v v1.2.3]'; return
    fi
    case "$command" in install|upgrade|rollback) ;; *) echo '未知安装命令' >&2; return 1;; esac
    while [[ $# -gt 0 ]]; do
        case "$1" in -v|--version) [[ $# -ge 2 ]] || return 1; version=$2; shift 2;; *) echo '未知安装参数' >&2; return 1;; esac
    done
    [[ -z $version ]] || validate_version "$version"
    [[ $(uname -s) == Linux && $EUID == 0 ]] || { echo '请在 Linux 服务器上以 root/sudo 执行' >&2; return 1; }
    case "$(uname -m)" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; *) echo '只支持 amd64/arm64' >&2; return 1;; esac
    for tool in curl tar sha256sum jq flock systemctl install getent useradd sort; do command -v "$tool" >/dev/null || { echo "缺少 $tool，请先安装系统依赖" >&2; return 1; }; done
    if [[ $command != install && ! -x $INSTALL_DIR/sub2api-enhance ]]; then echo '尚未安装增强服务' >&2; return 1; fi
    if [[ $command == install && -e $INSTALL_DIR/sub2api-enhance ]]; then echo '已安装，请使用 upgrade' >&2; return 1; fi
    getent passwd "$SERVICE_NAME" >/dev/null || useradd --system --user-group --home-dir "$INSTALL_DIR" --shell /usr/sbin/nologin "$SERVICE_NAME"
    install -d -m 0750 -o "$SERVICE_NAME" -g "$SERVICE_NAME" "$INSTALL_DIR"
    install -d -m 0750 -o root -g "$SERVICE_NAME" "$CONFIG_DIR"
    touch "$INSTALL_DIR/.update.lock"; chown "$SERVICE_NAME:$SERVICE_NAME" "$INSTALL_DIR/.update.lock"; chmod 0600 "$INSTALL_DIR/.update.lock"
    exec 9>"$INSTALL_DIR/.update.lock"
    flock -n 9 || { echo '已有安装或在线更新正在执行' >&2; return 1; }
    UPDATE_STAGE=$(mktemp -d "$INSTALL_DIR/.sub2api-enhance-update-XXXXXX")
    trap 'if [[ -n $UPDATE_STAGE ]]; then rm -rf -- "$UPDATE_STAGE"; fi' EXIT
    if [[ $command == rollback && -z $version ]]; then
        [[ -x $INSTALL_DIR/sub2api-enhance.backup ]] || { echo '没有可用备份' >&2; return 1; }
        cp "$INSTALL_DIR/sub2api-enhance.backup" "$UPDATE_STAGE/sub2api-enhance"
    else
        prepare_release "$version" "$arch"
    fi
    candidate=$("$UPDATE_STAGE/sub2api-enhance" --version)
    if [[ $command != install ]]; then
        current=$("$INSTALL_DIR/sub2api-enhance" --version)
        if [[ $candidate == "$current" ]]; then echo '已是该版本'; return; fi
        if [[ $command == rollback || $(printf '%s\n%s\n' "$candidate" "$current" | sort -V | head -1) == "$candidate" ]]; then
            schema_current=$("$INSTALL_DIR/sub2api-enhance" --schema-digest)
            schema_candidate=$("$UPDATE_STAGE/sub2api-enhance" --schema-digest)
            [[ $schema_current == "$schema_candidate" ]] || { echo '迁移集合不同，不能仅回退二进制；需要修复版本或经确认的数据库恢复' >&2; return 1; }
        fi
        # 校验与兼容检查完成后才停止增强服务，原 sub2api 不受该命令管理。
        "$INSTALL_DIR/sub2api-enhance" --release-info > "$UPDATE_STAGE/previous-info.json"
        jq -n --slurpfile previous "$UPDATE_STAGE/previous-info.json" \
            --arg action "script_$command" --arg target "$candidate" \
            --arg hash "$(sha256sum "$UPDATE_STAGE/sub2api-enhance" | awk '{print $1}')" \
            --arg backup_hash "$(sha256sum "$INSTALL_DIR/sub2api-enhance" | awk '{print $1}')" \
            '{phase:"applying",action:$action,target_version:$target,target_hash:$hash,backup:$previous[0],backup_hash:$backup_hash,error:""}' > "$UPDATE_STAGE/state.json"
        chown "$SERVICE_NAME:$SERVICE_NAME" "$UPDATE_STAGE/state.json"
        chmod 0600 "$UPDATE_STAGE/state.json"
        systemctl stop "$SERVICE_NAME"
        cp "$INSTALL_DIR/sub2api-enhance" "$UPDATE_STAGE/previous"
        chown "$SERVICE_NAME:$SERVICE_NAME" "$UPDATE_STAGE/previous"
        mv -f "$UPDATE_STAGE/previous" "$INSTALL_DIR/sub2api-enhance.backup"
        mv -f "$UPDATE_STAGE/state.json" "$INSTALL_DIR/.update-state.json"
    fi
    chown "$SERVICE_NAME:$SERVICE_NAME" "$UPDATE_STAGE/sub2api-enhance"
    chmod 0755 "$UPDATE_STAGE/sub2api-enhance"
    mv -f "$UPDATE_STAGE/sub2api-enhance" "$INSTALL_DIR/sub2api-enhance"
    if [[ -f $UPDATE_STAGE/configure-sub2api-menus.sh ]]; then
        install -m 0755 -o root -g root "$UPDATE_STAGE/configure-sub2api-menus.sh" "$INSTALL_DIR/configure-sub2api-menus.sh"
    fi
    if [[ $command == install ]]; then
        install -m 0640 -o root -g "$SERVICE_NAME" "$UPDATE_STAGE/env.example" "$CONFIG_DIR/sub2api-enhance.env"
        install -m 0644 "$UPDATE_STAGE/sub2api-enhance.service" "/etc/systemd/system/$SERVICE_NAME.service"
        systemctl daemon-reload
        systemctl enable "$SERVICE_NAME"
        echo "安装完成。填写 $CONFIG_DIR/sub2api-enhance.env 后执行：systemctl start $SERVICE_NAME"
        echo '首次启动会自动创建增强表；无需新建管理员账号，页面复用原版管理员会话。'
    else
        systemctl start "$SERVICE_NAME"
        health_check "$candidate"
        echo "增强服务已切换到 $candidate；配置和业务数据保留。"
    fi
}

if [[ ${BASH_SOURCE[0]:-} == "$0" || -z ${BASH_SOURCE[0]:-} ]]; then main "$@"; fi
