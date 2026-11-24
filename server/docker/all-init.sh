#!/bin/sh
# 归途单容器模式的初始化（容器内 root UID，只带 NET_ADMIN + DAC_OVERRIDE）：
#   - 准备 /data 子目录（DAC_OVERRIDE 允许任意属主）
#   - 用 runit 监督 core / stun / admin / plumb / proxy 五个子进程
#   - 收到 TERM 时按反序停止，让 plumber 有机会清理 ip rule / iptables
#
# 这里不做任何 chown / mknod：/data 的所有权由宿主机映射目录决定，
# /dev/net/tun 由 compose 的 devices 映射提供。
set -eu

DATA="${DATA_DIR:-/data}"
export DATA_DIR="$DATA"
export HOMING_MODE=all

log() { printf '[homing-init] %s\n' "$*"; }

if ! mkdir -p "$DATA/stun" "$DATA/proxy" 2>/dev/null; then
  log "无法写入数据目录 $DATA（当前 uid=$(id -u)）；请检查映射目录权限"
  exit 1
fi

runsvdir_pid=""
stop_all() {
  # 反序停止：先撤出口规则，再停管理台/打洞，最后停 core。
  for name in plumb proxy admin stun core; do
    sv -w 10 force-stop "/etc/homing/services/$name" >/dev/null 2>&1 || true
  done
  if [ -n "$runsvdir_pid" ]; then
    kill "$runsvdir_pid" 2>/dev/null || true
  fi
}
trap 'log "stopping services"; stop_all; exit 0' INT TERM

log "single-container mode: data=$DATA (uid=$(id -u))"
runsvdir -P /etc/homing/services &
runsvdir_pid=$!
wait "$runsvdir_pid" || true
