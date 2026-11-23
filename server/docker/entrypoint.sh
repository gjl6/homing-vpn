#!/bin/sh
# 归途一体化镜像入口：
#   homing core  [homing-core 参数]
#   homing stun  [gateway-stun 参数]
#   homing admin [gateway-admin 参数]
#   homing plumb          # 策略路由
#   homing proxy          # remote 出口的本机透明代理（mihomo）
# 其它情况直接 exec 传入命令，方便 docker run --entrypoint 调试。
set -e

case "$1" in
  core)
    shift
    exec /usr/local/bin/homing-core "$@"
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
