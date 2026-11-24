#!/bin/sh
# 单容器模式的 remote 出口 runner：
#   - 管理台写 /data/egress.json 和 /data/proxy/config.yaml
#   - enabled=true 且 mode=remote 时启动 mihomo，否则停掉
#   - 配置变化时重启 mihomo；状态写到 /data/proxy/status.json 供管理台读取
set -eu

DATA="${DATA_DIR:-/data}"
PROXY_DIR="$DATA/proxy"
CONF="$PROXY_DIR/config.yaml"
EGRESS_CFG="$DATA/egress.json"
STATUS="$PROXY_DIR/status.json"

log() { printf '[homing-proxy] %s\n' "$*"; }
child=""
stop_child() {
  if [ -n "$child" ] && kill -0 "$child" 2>/dev/null; then
    kill -TERM "$child" 2>/dev/null || true
    wait "$child" 2>/dev/null || true
  fi
  child=""
}

write_status() {
  running="$1"
  msg="$2"
  pid="${child:-0}"
  [ -n "$pid" ] || pid=0
  mkdir -p "$PROXY_DIR" 2>/dev/null || true
  msg="$(printf '%s' "$msg" | tr -d '"\\`~$' | tr '\n' ' ')"
  printf '{"running":%s,"pid":%s,"tun":"homing-proxy","updatedAt":"%s","message":"%s"}\n' \
    "$([ "$running" = "true" ] && echo true || echo false)" "$pid" \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$msg" > "$STATUS.tmp" 2>/dev/null \
    && mv -f "$STATUS.tmp" "$STATUS" 2>/dev/null || true
}

trap 'stop_child; write_status false "已停止"; exit 0' INT TERM

want_proxy() {
  [ -f "$CONF" ] || return 1
  grep -q '"enabled"[[:space:]]*:[[:space:]]*true' "$EGRESS_CFG" 2>/dev/null || return 1
  grep -q '"mode"[[:space:]]*:[[:space:]]*"remote"' "$EGRESS_CFG" 2>/dev/null || return 1
  return 0
}

conf_sig() {
  printf '%s:%s' \
    "$(stat -c '%Y' "$CONF" 2>/dev/null || echo -)" \
    "$(stat -c '%Y' "$EGRESS_CFG" 2>/dev/null || echo -)"
}

last_sig=""
write_status false "等待配置"
while :; do
  if want_proxy; then
    if [ ! -e /dev/net/tun ]; then
      log "缺少 /dev/net/tun：远程代理出口需要该设备（给容器加设备 /dev/net/tun，或保留 MKNOD 能力）"
      write_status false "缺少 /dev/net/tun：请给容器加设备 /dev/net/tun（或 MKNOD 能力）"
      sleep 2
      continue
    fi
    cur_sig="$(conf_sig)"
    if [ -z "$child" ] || ! kill -0 "$child" 2>/dev/null || [ "$cur_sig" != "$last_sig" ]; then
      stop_child
      log "启动本机透明代理"
      /usr/local/bin/mihomo -d "$PROXY_DIR" -f "$CONF" &
      child=$!
      last_sig="$cur_sig"
      sleep 1
      if kill -0 "$child" 2>/dev/null; then
        write_status true "本机代理运行中"
      else
        write_status false "本机代理启动失败"
      fi
    fi
  else
    if [ -n "$child" ]; then
      stop_child
      write_status false "未启用远程代理"
    fi
  fi
  sleep 2
done
