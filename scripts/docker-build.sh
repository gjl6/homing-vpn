#!/usr/bin/env bash
# 本地构建一体化镜像 homing（等价于 docker compose build）
set -euo pipefail
cd "$(dirname "$0")/.."

if [ ! -f .env ]; then
  echo "未找到 .env，先复制模板：cp .env.example .env 并修改"
  exit 1
fi

docker compose build "$@"
docker compose images
