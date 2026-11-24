#!/bin/sh
# 单容器模式的打洞/中继 runner：
#   - 等 core 就绪后用 core 当前监听端口作为 -target
#   - settings.json 或 /data/core-port 变化时重启 gateway-stun（端口只在启动时生效）
set -eu

DATA="${DATA_DIR:-/data}"
STATUS_PORT="${STUN_STATUS_PORT:-51831}"
PUBLIC_HTTP="${STUN_PUBLIC_HTTP:-:18081}"
STUN_PORT_ENV="${STUN_PORT:-}"

log() { printf '[homing-stun] %s\n' "$*"; }
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

sig() {
  printf '%s:%s' \
    "$(stat -c '%Y' "$DATA/core-port" 2>/dev/null || echo -)" \
    "$(stat -c '%Y' "$DATA/settings.json" 2>/dev/null || echo -)"
}

while :; do
  s="$(sig)"
  core_port="$(/usr/local/bin/homing-core-port)"
  log "start (target 127.0.0.1:$core_port)"
  /usr/local/bin/gateway-stun \
    -target "127.0.0.1:$core_port" \
    -listen "$STUN_PORT_ENV" \
    -state-file "$DATA/stun/endpoint.json" \
    -config-file "$DATA/stun/config.json" \
    -http "127.0.0.1:$STATUS_PORT" \
    -public-http "$PUBLIC_HTTP" \
    -upnp &
  child=$!
  while kill -0 "$child" 2>/dev/null; do
    sleep 2
    if [ "$(sig)" != "$s" ]; then
      log "settings changed, restarting"
      kill -TERM "$child" 2>/dev/null || true
      break
    fi
  done
  wait "$child" 2>/dev/null || true
  child=""
  sleep 1
done
