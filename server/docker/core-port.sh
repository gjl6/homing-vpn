#!/bin/sh
# 输出当前隧道监听端口，供 stun runner 使用。
# 优先级：/data/core-port（管理台在单容器模式写入） > core API > 环境变量/默认值。
set -eu

DATA="${DATA_DIR:-/data}"
if [ -s "$DATA/core-port" ]; then
  port="$(cat "$DATA/core-port" 2>/dev/null || true)"
  case "$port" in
    ''|*[!0-9]*) ;;
    *) printf '%s\n' "$port"; exit 0 ;;
  esac
fi

body="$(wget -qO- "http://127.0.0.1:${CORE_API_PORT:-51821}/api/admin/interface" 2>/dev/null || true)"
port="$(printf '%s' "$body" | sed -n 's/.*"listenPort"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | tail -n 1)"
case "$port" in
  ''|*[!0-9]*) port="${CORE_PORT:-51820}" ;;
esac
printf '%s\n' "$port"
