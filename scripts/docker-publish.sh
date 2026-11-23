#!/usr/bin/env bash
# 构建并推送一体化镜像 homing 到 Docker Hub（多架构）
#
# 用法：
#   1) .env 里设置：
#        HOMING_IMAGE_PREFIX=yourname/
#        HOMING_VERSION=1.0.0
#   2) docker login
#   3) bash scripts/docker-publish.sh
#
# 可选：
#   PLATFORMS=linux/amd64 bash scripts/docker-publish.sh       # 只发 amd64
#   PUSH_LATEST=0 bash scripts/docker-publish.sh               # 不打 latest 标签
set -euo pipefail
cd "$(dirname "$0")/.."

if [ ! -f .env ]; then
  echo "未找到 .env，先复制模板：cp .env.example .env 并修改"
  exit 1
fi

read_env() {
  grep -E "^[[:space:]]*$1=" .env 2>/dev/null | tail -n1 | cut -d= -f2- | tr -d '"'
}

PREFIX="$(read_env HOMING_IMAGE_PREFIX)"
VERSION="$(read_env HOMING_VERSION)"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
PUSH_LATEST="${PUSH_LATEST:-1}"

if [ -z "$PREFIX" ]; then
  echo "请先在 .env 设置 HOMING_IMAGE_PREFIX（例如 yourname/）"
  exit 1
fi
if [ -z "$VERSION" ]; then
  echo "请先在 .env 设置 HOMING_VERSION（例如 1.0.0）"
  exit 1
fi

PREFIX="${PREFIX%/}"

TAGS=(-t "${PREFIX}/homing:${VERSION}")
if [ "$PUSH_LATEST" = "1" ]; then
  TAGS+=(-t "${PREFIX}/homing:latest")
fi

echo ">>> ${PREFIX}/homing:${VERSION} (${PLATFORMS})"
docker buildx build \
  --platform "${PLATFORMS}" \
  --build-arg "VERSION=${VERSION}" \
  "${TAGS[@]}" \
  --push \
  server

echo
echo "完成：${PREFIX}/homing:${VERSION}"
