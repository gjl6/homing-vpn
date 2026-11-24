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

# 远程代理出口的 mihomo TUN 需要 /dev/net/tun：
# - compose 部署用 devices 映射，设备节点由 Docker 提供；
# - NAS 图形界面一般没有“设备”栏，但默认能力里带 MKNOD，这里兜底创建一次。
if [ ! -e /dev/net/tun ]; then
  mkdir -p /dev/net 2>/dev/null || true
  if mknod /dev/net/tun c 10 200 2>/dev/null; then
    chmod 666 /dev/net/tun 2>/dev/null || true
    log "已创建 /dev/net/tun（远程代理出口可用）"
  else
    log "未找到 /dev/net/tun 且无权创建：远程代理出口不可用；请加设备 /dev/net/tun 或 MKNOD 能力"
  fi
fi

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
