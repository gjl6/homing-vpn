#!/usr/bin/env bash
# 把旧版 core-data/、stun-data/ 迁移到统一 data/ 目录。
# 用法：在仓库根目录执行 bash scripts/migrate-data.sh
# 已存在的 data/ 文件不会被覆盖。
set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p data/stun

copy_if_absent() {
  local src="$1" dst="$2"
  if [ -e "$src" ] && [ ! -e "$dst" ]; then
    mkdir -p "$(dirname "$dst")"
    cp -a "$src" "$dst"
    echo "migrate: $src -> $dst"
  fi
}

copy_if_absent core-data/core.json data/core.json

for f in admin.json connect.json client-probe.json egress.json egress.env egress-status.json egress-clients; do
  copy_if_absent "stun-data/$f" "data/$f"
done

copy_if_absent stun-data/config.json data/stun/config.json
copy_if_absent stun-data/endpoint.json data/stun/endpoint.json
copy_if_absent stun-data/proxy data/proxy
copy_if_absent stun-data/backups data/backups

echo "完成。确认无误后可以删除 core-data/、stun-data/。"
