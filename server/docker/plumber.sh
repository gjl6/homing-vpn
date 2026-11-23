#!/bin/sh
# 归途可选出口：按客户端把 10.8.0.0/24 的流量送进 mihomo TUN。
#
# 前提：homing-core 与本容器都使用 host 网络，core 不再做跨容器 NAT，
# 因此宿主机能看到真实的客户端源地址（10.8.0.x），可以按客户端分流：
#   /data/egress-clients 里列出的 IP（每行一个）→ 策略路由到 TUN
#   其它客户端 → 继续走 core 自己的 MASQUERADE / 直连
#
# 配置由管理台写入 /data/egress.env；本脚本启动时读取一次。
# 客户端列表每轮重新读取，管理台开关客户端后最多 INTERVAL 秒生效。
#
# IPv4 才走代理；IPv6 保持 home 侧直连（mihomo TUN 当前 IPv4-only）。
set -eu

CONFIG_FILE="${EGRESS_CONFIG:-/data/egress.env}"
STATUS_FILE="${EGRESS_STATUS:-/data/egress-status.json}"

EGRESS_ENABLED=0
EGRESS_MODE=tun
MIHOMO_TUN=mihomo-gw
MIHOMO_TABLE=100
CLIENT_NET=10.8.0.0/24
CLIENTS_FILE="${EGRESS_CLIENTS_FILE:-/data/egress-clients}"
LAN_CIDRS="10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16"
EGRESS_DNS_LISTEN=""
EGRESS_DNS_PORT=0
LEGACY_MARK="${MIHOMO_MARK:-0x2024}"
COMMENT="homing-mihomo"
INTERVAL="${PLUMB_INTERVAL:-10}"
OLD_RP=""
APPLIED=""
CLIENT_COUNT=0

if [ -f "$CONFIG_FILE" ]; then
  # shellcheck disable=SC1090
  . "$CONFIG_FILE"
fi
EGRESS_ENABLED="${EGRESS_ENABLED:-0}"
EGRESS_MODE="${EGRESS_MODE:-tun}"
MIHOMO_TUN="${MIHOMO_TUN:-mihomo-gw}"
MIHOMO_TABLE="${MIHOMO_TABLE:-100}"
CLIENT_NET="${CLIENT_NET:-10.8.0.0/24}"
CLIENTS_FILE="${EGRESS_CLIENTS_FILE:-$CLIENTS_FILE}"
LAN_CIDRS="${LAN_CIDRS:-10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16}"
EGRESS_DNS_LISTEN="${EGRESS_DNS_LISTEN:-}"
EGRESS_DNS_PORT="${EGRESS_DNS_PORT:-0}"
TUN="$MIHOMO_TUN"
TABLE="$MIHOMO_TABLE"
DNS_PORT="$EGRESS_DNS_PORT"
# mihomo 的 TUN 栈只接受源地址在 TUN 子网内的包；客户端源地址必须
# SNAT 成 TUN 对端地址，否则包进了 TUN 但 mihomo 不会处理。
TUN_SRC="${MIHOMO_TUN_SRC:-}"
# 只有 remote 模式才有本机 DNS；tun 模式的 DNS 由宿主机 mihomo 自己处理
if [ "$EGRESS_MODE" != "remote" ]; then
  DNS_PORT=0
fi

log() { printf '[homing-egress] %s\n' "$*"; }

# ---------- 状态文件（供管理台读取） ----------
write_status() {
  local applied="$1" msg="$2" ready="false"
  tun_ready && ready="true"
  msg="$(printf '%s' "$msg" | tr -d '"\\`~$' | tr '\n' ' ')"
  mkdir -p "$(dirname "$STATUS_FILE")" 2>/dev/null || true
  printf '{"enabled":%d,"mode":"%s","tun":"%s","dnsPort":%d,"clients":%d,"tunReady":%s,"applied":%s,"updatedAt":"%s","message":"%s"}\n' \
    "$([ "$EGRESS_ENABLED" = "1" ] && echo 1 || echo 0)" "$EGRESS_MODE" "$TUN" "${DNS_PORT:-0}" "${CLIENT_COUNT:-0}" \
    "$ready" "$applied" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$msg" > "$STATUS_FILE.tmp" 2>/dev/null \
    && mv -f "$STATUS_FILE.tmp" "$STATUS_FILE" 2>/dev/null || true
}

# ---------- ip rule / iptables 幂等封装 ----------
rule_has() { ip -4 rule show 2>/dev/null | grep -F "$1" >/dev/null 2>&1; }
rule_add() {
  local spec="$1"; shift
  rule_has "$spec" && return 0
  ip -4 rule add "$@" 2>/dev/null || true
  rule_has "$spec"
}
rule_del_all() {
  local spec="$1"; shift
  local n=0
  while rule_has "$spec" && [ "$n" -lt 8 ]; do
    ip -4 rule del "$@" 2>/dev/null || break
    n=$((n + 1))
  done
}

ipt_has() { iptables -w "$@" >/dev/null 2>&1; }
# 调用方式：ipt_del_all -t nat -D POSTROUTING <rule-spec>
# 每轮 -D 成功即删掉一条，失败说明没有了。
ipt_del_all() {
  local n=0
  while [ "$n" -lt 8 ] && iptables -w "$@" >/dev/null 2>&1; do
    n=$((n + 1))
  done
}

tun_ready() { ip link show "$TUN" >/dev/null 2>&1; }

tun_source_ip() {
  ip -4 addr show dev "$TUN" 2>/dev/null | awk '
    /inet / {
      split($2, a, "/"); split(a[1], o, ".");
      o[4] = o[4] + 1;
      printf "%s.%s.%s.%d", o[1], o[2], o[3], o[4];
      exit
    }'
}

# ---------- 客户端列表 ----------
read_clients() {
  [ -f "$CLIENTS_FILE" ] || { printf ''; return 0; }
  awk '
    { sub(/#.*/, ""); gsub(/[ \t\r]/, "") }
    /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ { print }
  ' "$CLIENTS_FILE" 2>/dev/null | sort -u | tr '\n' ' ' | sed 's/[[:space:]]*$//'
}

# 删除 table 100 上所有 from 规则（table 100 专用于本出口的客户端分流）。
flush_table_rules() {
  ip -4 rule show 2>/dev/null | awk -v t="lookup $TABLE" 'index($0, t) { sub(/^[0-9]+:[[:space:]]*/, ""); print }' | \
    while IFS= read -r spec; do
      [ -n "$spec" ] || continue
      # shellcheck disable=SC2086
      set -- $spec
      ip -4 rule del "$@" 2>/dev/null || true
    done
}

cleanup_dns_rules() {
  iptables -w -t nat -S PREROUTING 2>/dev/null | grep -F -- "$COMMENT-dns" | \
    sed 's/^-A PREROUTING /-D PREROUTING /' | while IFS= read -r rule; do
      # shellcheck disable=SC2086
      eval "iptables -w -t nat $rule" 2>/dev/null || true
    done
}

apply_client_rules() {
  desired="$1"
  flush_table_rules
  cleanup_dns_rules
  for ip in $desired; do
    ip -4 rule add from "$ip" lookup "$TABLE" pref 100 2>/dev/null || true
    if [ "$DNS_PORT" != "0" ]; then
      for proto in udp tcp; do
        iptables -w -t nat -A PREROUTING -s "$ip" -p "$proto" --dport 53 \
          -m comment --comment "$COMMENT-dns" -j REDIRECT --to-ports "$DNS_PORT" 2>/dev/null || true
      done
    fi
  done
  APPLIED="$desired"
  if [ -n "$desired" ]; then
    CLIENT_COUNT=$(printf '%s\n' "$desired" | wc -w | tr -d ' ')
  else
    CLIENT_COUNT=0
  fi
  log "代理客户端: ${desired:-无}（${CLIENT_COUNT} 台）"
}

# ---------- 规则应用 / 清理 ----------
rules_applied() {
  ip -4 route show table "$TABLE" 2>/dev/null | grep -F "default dev $TUN" >/dev/null 2>&1 || return 1
  rule_has "to $CLIENT_NET lookup main" || return 1
  if [ -n "$TUN_SRC" ]; then
    ipt_has -t nat -C POSTROUTING -s "$CLIENT_NET" -o "$TUN" \
      -m comment --comment "$COMMENT-snat" -j SNAT --to-source "$TUN_SRC" || return 1
  fi
  ipt_has -C FORWARD -s "$CLIENT_NET" -o "$TUN" -m comment --comment "$COMMENT" -j ACCEPT || return 1
  ipt_has -C FORWARD -d "$CLIENT_NET" -i "$TUN" -m comment --comment "$COMMENT" -j ACCEPT || return 1
  for ip in $APPLIED; do
    rule_has "from $ip lookup $TABLE" || return 1
    if [ "$DNS_PORT" != "0" ]; then
      ipt_has -t nat -C PREROUTING -s "$ip" -p udp --dport 53 \
        -m comment --comment "$COMMENT-dns" -j REDIRECT --to-ports "$DNS_PORT" || return 1
      ipt_has -t nat -C PREROUTING -s "$ip" -p tcp --dport 53 \
        -m comment --comment "$COMMENT-dns" -j REDIRECT --to-ports "$DNS_PORT" || return 1
    fi
  done
  return 0
}

setup_routes() {
  ip route replace default dev "$TUN" table "$TABLE" 2>/dev/null || true

  # 隧道自身网段（10.8.0.0/24）必须直连，不能送进 TUN，
  # 否则客户端连 10.8.0.1 / 访问其它客户端都会被代理掉。
  rule_add "to $CLIENT_NET lookup main" to "$CLIENT_NET" lookup main pref 85

  # 私网/局域网直连，不进 mihomo（所有客户端通用）
  local cidr
  for cidr in $(printf '%s' "$LAN_CIDRS" | tr ',' ' '); do
    [ -n "$cidr" ] || continue
    rule_add "to $cidr lookup main" to "$cidr" lookup main pref 90
  done

  if [ -z "$TUN_SRC" ]; then
    TUN_SRC="$(tun_source_ip)"
  fi
  if [ -n "$TUN_SRC" ]; then
    ipt_has -t nat -C POSTROUTING -s "$CLIENT_NET" -o "$TUN" \
      -m comment --comment "$COMMENT-snat" -j SNAT --to-source "$TUN_SRC" \
      || iptables -w -t nat -I POSTROUTING 1 -s "$CLIENT_NET" -o "$TUN" \
           -m comment --comment "$COMMENT-snat" -j SNAT --to-source "$TUN_SRC" 2>/dev/null || true
  fi

  # FORWARD 双向放行
  ipt_has -C FORWARD -s "$CLIENT_NET" -o "$TUN" -m comment --comment "$COMMENT" -j ACCEPT \
    || iptables -w -I FORWARD 1 -s "$CLIENT_NET" -o "$TUN" \
         -m comment --comment "$COMMENT" -j ACCEPT 2>/dev/null || true
  ipt_has -C FORWARD -d "$CLIENT_NET" -i "$TUN" -m comment --comment "$COMMENT" -j ACCEPT \
    || iptables -w -I FORWARD 1 -d "$CLIENT_NET" -i "$TUN" \
         -m comment --comment "$COMMENT" -j ACCEPT 2>/dev/null || true

  echo 1 > /proc/sys/net/ipv4/ip_forward 2>/dev/null || true
  if [ -w "/proc/sys/net/ipv4/conf/$TUN/rp_filter" ]; then
    if [ -z "$OLD_RP" ]; then
      OLD_RP="$(cat "/proc/sys/net/ipv4/conf/$TUN/rp_filter" 2>/dev/null || true)"
    fi
    echo 0 > "/proc/sys/net/ipv4/conf/$TUN/rp_filter" 2>/dev/null || true
  fi
  return 0
}

cleanup_legacy_rules() {
  # 兼容旧版（core 在 bridge 网络时）留下的 mark / from CORE_NET 规则。
  iptables -w -t mangle -S PREROUTING 2>/dev/null | grep -F -- "$COMMENT-wg" | \
    sed 's/^-A PREROUTING /-D PREROUTING /' | while IFS= read -r rule; do
      # shellcheck disable=SC2086
      eval "iptables -w -t mangle $rule" 2>/dev/null || true
    done
  rule_del_all "fwmark $LEGACY_MARK lookup main" fwmark "$LEGACY_MARK" lookup main
}

cleanup_routes() {
  flush_table_rules
  cleanup_dns_rules
  rule_del_all "to $CLIENT_NET lookup main" to "$CLIENT_NET" lookup main
  local cidr
  for cidr in $(printf '%s' "$LAN_CIDRS" | tr ',' ' '); do
    [ -n "$cidr" ] || continue
    rule_del_all "to $cidr lookup main" to "$cidr" lookup main
  done
  ip route del default dev "$TUN" table "$TABLE" 2>/dev/null || true

  if [ -n "$TUN_SRC" ]; then
    ipt_del_all -t nat -D POSTROUTING -s "$CLIENT_NET" -o "$TUN" \
      -m comment --comment "$COMMENT-snat" -j SNAT --to-source "$TUN_SRC"
  fi
  iptables -w -t nat -S POSTROUTING 2>/dev/null | grep -F -- "$COMMENT-snat" | \
    sed 's/^-A POSTROUTING /-D POSTROUTING /' | while IFS= read -r rule; do
      # shellcheck disable=SC2086
      eval "iptables -w -t nat $rule" 2>/dev/null || true
    done
  ipt_del_all -D FORWARD -d "$CLIENT_NET" -i "$TUN" -m comment --comment "$COMMENT" -j ACCEPT
  ipt_del_all -D FORWARD -s "$CLIENT_NET" -o "$TUN" -m comment --comment "$COMMENT" -j ACCEPT
  cleanup_legacy_rules
  if [ -n "$OLD_RP" ] && [ -w "/proc/sys/net/ipv4/conf/$TUN/rp_filter" ]; then
    echo "$OLD_RP" > "/proc/sys/net/ipv4/conf/$TUN/rp_filter" 2>/dev/null || true
    OLD_RP=""
  fi
  APPLIED=""
  CLIENT_COUNT=0
  return 0
}

cleanup() {
  log "清理 mihomo 出口规则"
  cleanup_routes || true
  write_status false "已停止"
  exit 0
}
trap cleanup INT TERM

if [ "$EGRESS_ENABLED" != "1" ]; then
  log "未启用 mihomo 出口（配置: $CONFIG_FILE）"
  cleanup_routes || true
  write_status false "未启用"
fi

log "启用出口 mode=$EGRESS_MODE clientNet=$CLIENT_NET tun=$TUN tunSrc=$TUN_SRC table=$TABLE dnsPort=$DNS_PORT clientsFile=$CLIENTS_FILE"

while :; do
  if [ "$EGRESS_ENABLED" != "1" ]; then
    cleanup_routes || true
    write_status false "未启用"
  elif tun_ready; then
    setup_routes || true
    desired="$(read_clients)"
    if [ "$desired" != "$APPLIED" ]; then
      apply_client_rules "$desired" || true
    fi
    if rules_applied; then
      write_status true "策略路由已应用（${CLIENT_COUNT} 台代理客户端）"
    else
      write_status false "规则未就绪"
    fi
  else
    cleanup_routes || true
    write_status false "等待 mihomo TUN $TUN 出现"
  fi
  sleep "$INTERVAL"
done
