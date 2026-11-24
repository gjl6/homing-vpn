#!/bin/sh
# 归途单容器模式（homing all）的进程 1 初始化：
#   - 准备 /data 子目录和 /dev/net/tun
#   - 用 runit 监督 core / stun / admin / plumb / proxy 五个子进程
#   - 收到 TERM 时按反序停止，让 plumber 有机会清理 ip rule / iptables
#
# 注意：这里不启动任何业务逻辑，只做一次性准备和信号转发。
# PID 1 是 tini（见 Dockerfile ENTRYPOINT），本脚本是 tini 的子进程。
set -eu

DATA="${DATA_DIR:-/data}"
export DATA_DIR="$DATA"
export HOMING_MODE=all

log() { printf '[homing-init] %s\n' "$*"; }

mkdir -p "$DATA"
# 数据目录可能是宿主机普通用户创建的（NAS 常见），先归 root 再建子目录：
# 容器已经没有 DAC_OVERRIDE，普通目录上直接 mkdir 会失败。
chown -R 0:0 "$DATA" 2>/dev/null || true
chmod -R u+rwX "$DATA" 2>/dev/null || true
mkdir -p "$DATA/stun" "$DATA/proxy"
# Docker 的 /dev 是 tmpfs，镜像里的设备节点会被覆盖；单容器里由 init 补一次。
mkdir -p /dev/net 2>/dev/null || true
[ -e /dev/net/tun ] || mknod /dev/net/tun c 10 200 2>/dev/null || true

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

log "single-container mode: data=$DATA"
runsvdir -P /etc/homing/services &
runsvdir_pid=$!
wait "$runsvdir_pid" || true
