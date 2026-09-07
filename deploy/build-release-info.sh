#!/usr/bin/env bash
set -euo pipefail
# 发布元数据由同一份代码生成，读取版本不加载配置或访问数据库。
release_version=${1:?version required}
release_commit=${2:?commit required}
release_date=${3:?date required}
[[ $release_version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || exit 1
[[ $release_commit =~ ^[0-9a-f]+$ ]] || exit 1
[[ $release_date =~ ^[0-9TZ:+.-]+$ ]] || exit 1
mkdir -p .release-artifacts
(cd backend && CGO_ENABLED=0 GOTOOLCHAIN=go1.27.0 go run -ldflags "-X main.Version=$release_version -X main.Commit=$release_commit -X main.Date=$release_date -X main.BuildType=release" ./cmd/server --release-info) > .release-artifacts/release.json
