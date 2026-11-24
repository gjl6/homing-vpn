#!/bin/sh
# 在单容器（privileged）里启动不需要任何 capability 的子进程：
# 仍然以 root uid 运行（便于沿用现有 /data 权限），但清空 capability 集合。
# 用法：homing-nocaps <command> [args...]
set -eu

[ "$#" -gt 0 ] || { echo "usage: homing-nocaps <command> [args...]" >&2; exit 2; }
exec capsh --drop=all --shell=/bin/sh -- -c 'exec "$0" "$@"' "$@"
