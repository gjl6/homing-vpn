#!/bin/sh
# 在容器里按指定列表删除 capability bounding set，然后 exec 目标命令。
# 用法：homing-capdrop <英文逗号分隔的 capability 名> <command> [args...]
# 例：homing-capdrop chown,setpcap /usr/local/bin/homing-core ...
#
# 注意：容器本身必须带 CAP_SETPCAP，capsh 才能修改 bounding set。
set -eu

[ "$#" -gt 1 ] || { echo "usage: homing-capdrop <caps> <command> [args...]" >&2; exit 2; }
drop="$1"
shift
exec capsh --drop="$drop" --shell=/bin/sh -- -c 'exec "$0" "$@"' "$@"
