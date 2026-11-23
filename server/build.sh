#!/bin/sh
# 构建归途一体化镜像（三个组件共用一个镜像）
set -eu
cd "$(dirname "$0")"
VERSION="${HOMING_VERSION:-local}"
exec docker build \
  --build-arg "VERSION=${VERSION}" \
  -t "homing:${VERSION}" \
  .
