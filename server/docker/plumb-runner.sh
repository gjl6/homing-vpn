#!/bin/sh
# 单容器模式的出口 plumber runner：
#   - homing-plumber 只在启动时 source /data/egress.env；
#   - 管理台改出口配置后，这里检测 egress.env 变化并重启 plumber，
#     等价于分容器模式下“重建 homing-egress 容器”。
set -eu

DATA="${DATA_DIR:-/data}"
CFG="$DATA/egress.env"

log() { printf '[homing-plumb] %s\n' "$*"; }
child=""
stop_child() {
  if [ -n "$child" ] && kill -0 "$child" 2>/dev/null; then
    kill -TERM "$child" 2>/dev/null || true
    wait "$child" 2>/dev/null || true
  fi
  child=""
}
trap 'stop_child; exit 0' INT TERM

until wget -q -O /dev/null "http://127.0.0.1:${CORE_API_PORT:-51821}/healthz" 2>/dev/null; do
  sleep 1
done

sig() { stat -c '%Y' "$CFG" 2>/dev/null || echo -; }

while :; do
  s="$(sig)"
  /usr/local/bin/homing-plumber &
  child=$!
  while kill -0 "$child" 2>/dev/null; do
    sleep 2
    if [ "$(sig)" != "$s" ]; then
      log "egress config changed, restarting plumber"
      kill -TERM "$child" 2>/dev/null || true
      break
    fi
  done
  wait "$child" 2>/dev/null || true
  child=""
  sleep 1
done
