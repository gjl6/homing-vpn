#!/bin/sh
# 归途一体化镜像入口：
#   homing core  [homing-core 参数]
#   homing stun  [gateway-stun 参数]
#   homing admin [gateway-admin 参数]
#   homing plumb          # 策略路由
#   homing proxy          # remote 出口的本机透明代理（mihomo）
#   homing all            # 单容器模式：runit 监督以上全部子进程
# 其它情况直接 exec 传入命令，方便 docker run --entrypoint 调试。
set -e

case "$1" in
  all)
    shift
    exec /usr/local/bin/homing-init "$@"
    ;;
  core)
    shift
    # 多容器模式下 core 不再是 privileged：先把宿主机用户创建的 /data 归 root，
    # 再由 homing-capdrop 把 capability 降到只剩 NET_ADMIN。
    if [ -d /data ]; then
      chown -R 0:0 /data 2>/dev/null || true
      chmod -R u+rwX /data 2>/dev/null || true
    fi
    exec /usr/local/bin/homing-capdrop cap_chown,cap_setpcap /usr/local/bin/homing-core "$@"
    ;;
  stun)
    shift
    exec /usr/local/bin/gateway-stun "$@"
    ;;
  admin)
    shift
    exec /usr/local/bin/gateway-admin "$@"
    ;;
  plumb|egress)
    shift
    exec /usr/local/bin/homing-plumber "$@"
    ;;
  proxy)
    # remote 出口的本机透明代理；配置由管理台写入 /data/proxy/config.yaml
    shift
    # Docker 的 /dev 是 tmpfs，镜像里的设备节点会被覆盖，运行时补一次
    mkdir -p /dev/net 2>/dev/null || true
    [ -e /dev/net/tun ] || mknod /dev/net/tun c 10 200 2>/dev/null || true
    exec /usr/local/bin/mihomo -d /data/proxy -f /data/proxy/config.yaml "$@"
    ;;
  *)
    exec "$@"
    ;;
esac
