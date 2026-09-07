#!/usr/bin/env bash
set -euo pipefail
# 只测试版本、校验和与解包边界；不安装依赖、不联网、不调用 systemd。
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
source "$repo_root/deploy/install.sh"
test_dir=$(mktemp -d)
trap 'rm -rf -- "$test_dir"' EXIT
validate_version v1.2.3
if validate_version 'v1.2.3;echo bad' 2>/dev/null; then echo '版本注入未拒绝'; exit 1; fi
printf 'fixture' > "$test_dir/sub2api-enhance"
(cd "$test_dir" && tar -czf package.tar.gz sub2api-enhance)
sha256sum "$test_dir/package.tar.gz" | awk '{print $1"  package.tar.gz"}' > "$test_dir/checksums.txt"
verify_archive "$test_dir/package.tar.gz" package.tar.gz "$test_dir/checksums.txt"
extract_member "$test_dir/package.tar.gz" sub2api-enhance "$test_dir/extracted"
cmp "$test_dir/sub2api-enhance" "$test_dir/extracted"
printf 'changed' >> "$test_dir/package.tar.gz"
if verify_archive "$test_dir/package.tar.gz" package.tar.gz "$test_dir/checksums.txt" 2>/dev/null; then echo '损坏安装包未拒绝'; exit 1; fi
ln -s /etc/passwd "$test_dir/linked"
(cd "$test_dir" && tar -czf link.tar.gz linked)
if extract_member "$test_dir/link.tar.gz" linked "$test_dir/unsafe" 2>/dev/null; then echo '链接未拒绝'; exit 1; fi
printf '安装脚本静态行为测试通过\n'
