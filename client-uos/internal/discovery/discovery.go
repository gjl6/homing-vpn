// Package discovery 负责从 DNS TXT 公告板里发现网关的最新端点。
//
// 网关（gateway-stun）发布的 TXT 记录格式：
//
//	v1;v4=203.0.113.10:9612;v6=[2001:db8::10]:51820;ts=2026-11-20T05:09:32Z
//
// 因为家宽没有固定公网端口（STUN 打洞得到的端口会变），客户端必须靠它自动跟上。
package discovery

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"homing/client/internal/profile"
)

// Result 一次发现的结果。
type Result struct {
	Record string `json:"record"`
	V4     string `json:"v4"`
	V6     string `json:"v6"`
	TS     string `json:"ts"`
	Raw    string `json:"raw,omitempty"`
	Error  string `json:"error"`
}

// Mark 是 SO_MARK 值：与隧道策略路由的 fwmark 保持一致，
// 让 TXT 查询走物理网卡而不是隧道 —— 否则“隧道还没通 → DNS 也被塞进隧道 → 查不到 → 永远连不上”。
// 由 daemon 在启动时设为 tunnel.Table。
var Mark int

// Fetch 查询 TXT 记录。不需要 root 权限（非 root 时自动降级为普通解析）。
func Fetch(p *profile.Profile) Result {
	r := Result{Record: p.Discovery.Record}
	record := strings.TrimSpace(p.Discovery.Record)
	if record == "" {
		r.Error = "没有配置端点公告的 TXT 记录名（discovery.record）"
		return r
	}
	servers := dnsServers(p)
	var errs []string
	for _, server := range servers {
		txts, err := lookupTXT(record, server)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", server, err))
			continue
		}
		for _, txt := range txts {
			v4, v6, ts, ok := ParseTXT(txt)
			if !ok {
				continue
			}
			r.V4, r.V6, r.TS, r.Raw = v4, v6, ts, txt
			return r
		}
		errs = append(errs, server+": TXT 里没有可识别的 v1 记录")
	}
	r.Error = fmt.Sprintf("查询 TXT %s 失败（%s）", record, strings.Join(errs, "; "))
	return r
}

// dnsServers 决定用哪些 DNS 服务器查 TXT：
//  1. profile.discovery.dns 指定的；
//  2. /etc/resolv.conf 里的（跳过 127.x stub）；
//  3. 公共 DNS 兜底。
func dnsServers(p *profile.Profile) []string {
	var out []string
	if s := strings.TrimSpace(p.Discovery.DNS); s != "" {
		if !strings.Contains(s, ":") {
			s = net.JoinHostPort(s, "53")
		}
		out = append(out, s)
	}
	localNets := localNetworks()
	if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || strings.ToLower(fields[0]) != "nameserver" {
				continue
			}
			server := fields[1]
			if strings.HasPrefix(server, "127.") || strings.HasPrefix(server, "::1") {
				continue
			}
			// 隧道 DNS（私有地址且不在本机直连网段）在带 SO_MARK 绕隧道时是够不着的，
			// 直接跳过，避免每次发现都白等一轮超时。
			if ip := net.ParseIP(server); ip != nil && ip.To4() != nil && ip.IsPrivate() && !inLocalNets(ip, localNets) {
				continue
			}
			out = append(out, net.JoinHostPort(server, "53"))
		}
	}
	out = append(out, "223.5.5.5:53", "119.29.29.29:53")
	// 去重
	seen := map[string]bool{}
	uniq := out[:0]
	for _, s := range out {
		if !seen[s] {
			seen[s] = true
			uniq = append(uniq, s)
		}
	}
	return uniq
}

func lookupTXT(record, server string) ([]string, error) {
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			if Mark > 0 {
				d.Control = func(network, address string, raw syscall.RawConn) error {
					var sockErr error
					if err := raw.Control(func(fd uintptr) {
						// 失败就忽略（非 root 时没有 CAP_NET_ADMIN），退回普通 socket
						if e := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, Mark); e != nil {
							sockErr = nil
						}
					}); err != nil {
						return err
					}
					return sockErr
				}
			}
			return d.DialContext(ctx, "udp", server)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	txts, err := resolver.LookupTXT(ctx, record)
	if err != nil {
		return nil, err
	}
	return txts, nil
}

// ParseTXT 解析一条 TXT 内容。
func ParseTXT(txt string) (v4, v6, ts string, ok bool) {
	txt = strings.TrimSpace(txt)
	if txt == "" {
		return "", "", "", false
	}
	for _, part := range strings.Split(txt, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, found := strings.Cut(part, "=")
		if !found {
			// 可能是版本标记 v1
			if strings.HasPrefix(strings.ToLower(part), "v1") {
				ok = true
			}
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "v4":
			v4 = strings.TrimSpace(v)
			ok = true
		case "v6":
			v6 = strings.TrimSpace(v)
			ok = true
		case "ts":
			ts = strings.TrimSpace(v)
		}
	}
	return v4, v6, ts, ok
}

// Choose 按用户偏好挑一个可用端点：
// auto | v4 | v6 | v4,v6 | v6,v4 | v4-only | v6-only。
func Choose(p *profile.Profile, r Result) string {
	pref := strings.ToLower(strings.TrimSpace(p.Discovery.Preferred))
	switch pref {
	case "v4-only":
		return r.V4
	case "v6-only":
		return r.V6
	}
	order := []string{"v6", "v4"}
	switch pref {
	case "v4", "v4,v6":
		order = []string{"v4", "v6"}
	case "v6", "v6,v4":
		order = []string{"v6", "v4"}
	default:
		// auto：本机有全局 IPv6 才优先 v6
		if !HasGlobalIPv6() {
			order = []string{"v4", "v6"}
		}
	}
	for _, fam := range order {
		if fam == "v4" && r.V4 != "" {
			return r.V4
		}
		if fam == "v6" && r.V6 != "" {
			return r.V6
		}
	}
	return ""
}

// familyAllowed 判断当前偏好是否允许某个协议（仅 IPv4/仅 IPv6 用来过滤另一协议）。
func familyAllowed(p *profile.Profile, fam string) bool {
	switch strings.ToLower(strings.TrimSpace(p.Discovery.Preferred)) {
	case "v4-only":
		return fam == "v4"
	case "v6-only":
		return fam == "v6"
	}
	return true
}

// Alternate 给出“除了当前端点以外的另一个候选”，用于握手长期失败时轮换。
func Alternate(p *profile.Profile, r Result, current string) string {
	var candidates []string
	candidates = append(candidates, Choose(p, r))
	if familyAllowed(p, "v4") {
		candidates = append(candidates, r.V4)
	}
	if familyAllowed(p, "v6") {
		candidates = append(candidates, r.V6)
	}
	for _, c := range candidates {
		if c != "" && c != current {
			return c
		}
	}
	return ""
}

// HasGlobalIPv6 判断本机是否有可用的全局 IPv6 地址（排除 ULA/link-local/回环）。
func HasGlobalIPv6() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.To4() != nil {
			continue
		}
		ip := ipnet.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		if !ip.IsGlobalUnicast() {
			continue
		}
		// fc00::/7 是 ULA，不算可用的公网 IPv6
		if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
			continue
		}
		return true
	}
	return false
}

// localNetworks 返回本机各接口的直接网段（用来判断某个 DNS 服务器是否“直连可达”）。
func localNetworks() []*net.IPNet {
	var nets []*net.IPNet
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nets
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			nets = append(nets, ipnet)
		}
	}
	return nets
}

func inLocalNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}