// homing-core —— 归途自研隧道核心（替代 隧道核心）
//
// 职责：
//  1. 管理内核 隧道 接口（默认 homing0）：服务端密钥、peer、监听端口；
//  2. 配 NAT/转发（iptables v4 + ip6tables NAT66 + sysctl）；
//  3. 提供客户端管理 API（供 homing-admin 管理台使用）：
//     /api/client*、/api/admin/interface、/api/admin/userconfig（兼容原管理台调用形状）；
//  4. 数据持久化：/data/core.json（0600，含服务端私钥与各客户端私钥）。
//
// 用法：
//
//	homing-core -api 127.0.0.1:51821 -data /data/core.json -iface homing0 -port 51820
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// ---------------- 数据模型 ----------------

type Client struct {
	ID                  int      `json:"id"`
	Name                string   `json:"name"`
	PrivateKey          string   `json:"privateKey"`
	PublicKey           string   `json:"publicKey"`
	PresharedKey        string   `json:"presharedKey"`
	IPv4                string   `json:"ipv4"`
	IPv6                string   `json:"ipv6"`
	Enabled             bool     `json:"enabled"`
	PersistentKeepalive int      `json:"persistentKeepalive"`
	DNS                 []string `json:"dns"`
	AllowedIPs          []string `json:"allowedIps,omitempty"`
	// ProxyEgress 表示该客户端是否走宿主机 mihomo 出口。
	// core 只保存/下发这个标记，实际分流由 homing-plumber 按源地址完成。
	ProxyEgress bool `json:"proxyEgress,omitempty"`
	// ProxySelfService 表示是否允许客户端通过隧道内自助接口切换 ProxyEgress。
	ProxySelfService bool  `json:"proxySelfService,omitempty"`
	CreatedAt        int64 `json:"createdAt"`
}

type userConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type coreData struct {
	PrivateKey string     `json:"privateKey"`
	PublicKey  string     `json:"publicKey"`
	ListenPort int        `json:"listenPort"`
	IPv4CIDR   string     `json:"ipv4Cidr"`
	IPv6CIDR   string     `json:"ipv6Cidr"`
	AddressV4  string     `json:"addressV4"`
	AddressV6  string     `json:"addressV6"`
	MTU        int        `json:"mtu"`
	DNS        []string   `json:"dns"`
	OutIface   string     `json:"outIface"`
	Clients    []Client   `json:"clients"`
	NextID     int        `json:"nextId"`
	UserConfig userConfig `json:"userConfig"`
}

type peerStat struct {
	Endpoint  string
	Handshake time.Time
	Rx        int64
	Tx        int64
}

// ---------------- 全局 ----------------

var (
	apiAddr        = flag.String("api", "127.0.0.1:51821", "管理 API 监听地址")
	dataPath       = flag.String("data", "/data/core.json", "数据文件")
	iface          = flag.String("iface", "homing0", "WireGuard 接口名")
	outIface       = flag.String("out", "", "出口网卡（默认自动探测默认路由网卡）")
	v4cidr         = flag.String("v4cidr", "", "隧道 IPv4 网段（留空自动使用 10.8.0.0/24）")
	v6cidr         = flag.String("v6cidr", "fdcc:ad94:bacf:61a4::cafe:0/112", "隧道 IPv6 网段")
	addrV4         = flag.String("addr4", "", "服务端 IPv4 地址（留空按 -v4cidr 推导网络地址+1）")
	addrV6         = flag.String("addr6", "fdcc:ad94:bacf:61a4::cafe:1/112", "服务端 IPv6 地址")
	mtu            = flag.Int("mtu", 1420, "MTU")
	dnsList        = flag.String("dns", "", "下发给客户端的 DNS，逗号分隔（留空自动探测默认网关）")
	listen         = flag.Int("port", 51820, "WireGuard 监听端口")
	probePort      = flag.Int("probe-port", 51824, "隧道内 UDP 探针回显端口（0=关闭）")
	verbose        = flag.Bool("verbose", false, "打印详细日志")
	reconcileEvery = flag.Duration("reconcile", 10*time.Second, "周期校准间隔（0=只校准一次）")

	dataMu sync.Mutex
	data   *coreData
	// configDirty=true 时才重配 peer（周期校准不碰 peer，避免 ReplacePeers 重置会话导致周期性丢包）
	configDirtyMu sync.Mutex
	configDirty   = true

	statMu sync.Mutex
	stats  = map[string]peerStat{}
)

const (
	// defaultTunnelIPv4CIDR 是未通过 -v4cidr/env 指定时的隧道 IPv4 网段。
	defaultTunnelIPv4CIDR = "10.8.0.0/24"
	// fallbackDNS 是自动探测默认网关失败时下发给客户端的兜底 DNS。
	fallbackDNS = "223.5.5.5"
)

// ---------------- 持久化 ----------------

func dataFile() string { return *dataPath }

func loadData() error {
	if b, err := os.ReadFile(dataFile()); err == nil {
		var d coreData
		if err := json.Unmarshal(b, &d); err != nil {
			return fmt.Errorf("解析 %s 失败: %w", dataFile(), err)
		}
		data = &d
	} else {
		// 新部署：生成服务端密钥
		priv, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			return err
		}
		data = &coreData{
			PrivateKey: priv.String(),
			PublicKey:  priv.PublicKey().String(),
			ListenPort: *listen,
			IPv6CIDR:   *v6cidr,
			AddressV6:  *addrV6,
			MTU:        *mtu,
			NextID:     1,
		}
	}
	if data.ListenPort == 0 {
		data.ListenPort = *listen
	}
	if data.MTU == 0 {
		data.MTU = *mtu
	}
	// 下面只补空值：已有 core.json 中用户配置过的值不会被覆盖。
	if data.IPv4CIDR == "" {
		if *v4cidr != "" {
			data.IPv4CIDR = *v4cidr
		} else {
			data.IPv4CIDR = defaultTunnelIPv4CIDR
		}
	}
	if data.IPv6CIDR == "" {
		data.IPv6CIDR = *v6cidr
	}
	if data.AddressV4 == "" {
		if *addrV4 != "" {
			data.AddressV4 = *addrV4
		} else {
			// 未显式指定时按 -v4cidr/实际网段推导网络地址+1，如 10.8.0.1/24。
			data.AddressV4 = addressFromCIDR(data.IPv4CIDR)
		}
	}
	if data.AddressV6 == "" {
		data.AddressV6 = *addrV6
	}
	if len(data.DNS) == 0 {
		if *dnsList != "" {
			data.DNS = splitList(*dnsList)
		} else if gw := detectDefaultGateway(); gw != "" {
			data.DNS = []string{gw}
		} else {
			data.DNS = []string{fallbackDNS}
		}
	}
	if data.NextID <= 0 {
		data.NextID = 1
	}
	for _, c := range data.Clients {
		if c.ID >= data.NextID {
			data.NextID = c.ID + 1
		}
	}
	return nil
}

func saveData() error {
	dataMu.Lock()
	defer dataMu.Unlock()
	b, _ := json.MarshalIndent(data, "", "  ")
	_ = os.MkdirAll(dirOf(dataFile()), 0o755)
	tmp := dataFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dataFile())
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ---------------- 默认值探测 ----------------

// addressFromCIDR 返回 IPv4 网段的第一个可用地址（网络地址+1），形如 10.8.0.1/24。
func addressFromCIDR(cidr string) string {
	_, n, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return ""
	}
	base, ok := ipv4Uint32(n.IP)
	if !ok {
		return ""
	}
	ones, bits := n.Mask.Size()
	if bits != 32 || ones >= 32 {
		return ""
	}
	return fmt.Sprintf("%s/%d", ipv4String(base+1), ones)
}

// ipv4Uint32 把 IPv4 地址转成大端序 uint32，便于做网段偏移计算。
func ipv4Uint32(ip net.IP) (uint32, bool) {
	ip4 := ip.To4()
	if ip4 == nil {
		return 0, false
	}
	return binary.BigEndian.Uint32(ip4), true
}

// ipv4String 把大端序 uint32 还原成点分十进制 IPv4 地址。
func ipv4String(v uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// detectDefaultGateway 从 /proc/net/route 解析默认路由的网关 IPv4 地址。
// 不依赖 shell；非 host 网络（例如 bridge 网络没有默认路由）或读取失败时返回 ""。
func detectDefaultGateway() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	best, bestMetric := "", -1
	for i, line := range strings.Split(string(b), "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		// 字段：Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
		if len(f) < 8 {
			continue
		}
		// 默认路由：Destination 与 Mask 都是 0.0.0.0。
		if f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(f[3], 16, 32)
		if err != nil || flags&0x1 == 0 { // RTF_UP
			continue
		}
		gw, ok := routeIPv4(f[2])
		if !ok || gw == "0.0.0.0" {
			continue
		}
		metric := 0
		if m, err := strconv.Atoi(f[6]); err == nil {
			metric = m
		}
		if bestMetric < 0 || metric < bestMetric {
			best, bestMetric = gw, metric
		}
	}
	return best
}

// routeIPv4 解析 /proc/net/route 中小端序十六进制表示的 IPv4 字段，如 0101A8C0 -> 192.168.1.1。
func routeIPv4(s string) (string, bool) {
	if len(s) != 8 {
		return "", false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d.%d.%d.%d", byte(v), byte(v>>8), byte(v>>16), byte(v>>24)), true
}

// ---------------- 系统配置（ip / iptables / sysctl） ----------------

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func detectOutIface() string {
	if *outIface != "" {
		return *outIface
	}
	out, err := run("sh", "-c", "ip route show default 2>/dev/null | awk '{print $5; exit}'")
	if err == nil && out != "" {
		return out
	}
	return "eth0"
}

func ensureLink() error {
	if _, err := run("ip", "link", "show", "dev", *iface); err == nil {
		return nil
	}
	if _, err := run("ip", "link", "add", "dev", *iface, "type", "wireguard"); err != nil {
		return fmt.Errorf("创建 %s 失败（内核无 wireguard 模块？）: %w", *iface, err)
	}
	log.Printf("已创建接口 %s", *iface)
	return nil
}

func configureDevice() error {
	priv, err := wgtypes.ParseKey(data.PrivateKey)
	if err != nil {
		return fmt.Errorf("服务端私钥无效: %w", err)
	}
	port := data.ListenPort
	cfg := wgtypes.Config{PrivateKey: &priv, ListenPort: &port, ReplacePeers: true}
	for _, c := range data.Clients {
		if !c.Enabled {
			continue
		}
		pub, err := wgtypes.ParseKey(c.PublicKey)
		if err != nil {
			continue
		}
		pc := wgtypes.PeerConfig{PublicKey: pub, ReplaceAllowedIPs: true}
		if c.PresharedKey != "" {
			if psk, err := wgtypes.ParseKey(c.PresharedKey); err == nil {
				pc.PresharedKey = &psk
			}
		}
		if c.PersistentKeepalive > 0 {
			ka := time.Duration(c.PersistentKeepalive) * time.Second
			pc.PersistentKeepaliveInterval = &ka
		}
		if _, n, err := net.ParseCIDR(c.IPv4 + "/32"); err == nil {
			pc.AllowedIPs = append(pc.AllowedIPs, *n)
		}
		if _, n, err := net.ParseCIDR(c.IPv6 + "/128"); err == nil {
			pc.AllowedIPs = append(pc.AllowedIPs, *n)
		}
		cfg.Peers = append(cfg.Peers, pc)
	}
	c, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("连接内核 WireGuard 失败: %w", err)
	}
	defer c.Close()
	return c.ConfigureDevice(*iface, cfg)
}

func ensureAddrs() {
	run("ip", "addr", "add", data.AddressV4, "dev", *iface) // 已存在会报错，忽略
	run("ip", "addr", "add", data.AddressV6, "dev", *iface)
	run("ip", "link", "set", "dev", *iface, "mtu", strconv.Itoa(data.MTU), "up")
}

func ensureIPT(bin, table, chain string, spec []string) {
	base := []string{"-w"}
	if table != "" {
		base = append(base, "-t", table)
	}
	check := append(append([]string{}, base...), "-C", chain)
	check = append(check, spec...)
	if _, err := run(bin, check...); err == nil {
		return
	}
	add := append(append([]string{}, base...), "-A", chain)
	add = append(add, spec...)
	if out, err := run(bin, add...); err != nil {
		log.Printf("%s -A %s 失败: %v %s", bin, chain, err, out)
	}
}

// cleanupOwnedRules 删除上一次运行遗留的、带 homing-core 注释的规则。
// 宿主机网络模式下 iptables 规则会留在宿主命名空间，启动时先清一遍，
// 避免端口/网段变化后旧规则一直挂着。
func cleanupOwnedRules() {
	refs := []struct{ bin, table, chain string }{
		{"iptables", "filter", "INPUT"},
		{"iptables", "filter", "FORWARD"},
		{"iptables", "nat", "POSTROUTING"},
		{"ip6tables", "filter", "FORWARD"},
		{"ip6tables", "nat", "POSTROUTING"},
	}
	for _, ref := range refs {
		args := []string{"-w"}
		if ref.table != "" {
			args = append(args, "-t", ref.table)
		}
		args = append(args, "-S", ref.chain)
		out, err := run(ref.bin, args...)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			if !strings.HasPrefix(line, "-A ") || !strings.Contains(line, "homing-core-") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			del := []string{"-w"}
			if ref.table != "" {
				del = append(del, "-t", ref.table)
			}
			del = append(del, "-D", ref.chain)
			del = append(del, fields[2:]...)
			_, _ = run(ref.bin, del...)
		}
	}
}

func ensureNAT() {
	out := detectOutIface()
	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644)
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0o644)
	_ = os.WriteFile("/proc/sys/net/ipv4/conf/all/src_valid_mark", []byte("1"), 0o644)

	port := strconv.Itoa(data.ListenPort)
	ensureIPT("iptables", "nat", "POSTROUTING", []string{
		"-s", data.IPv4CIDR, "-o", out,
		"-m", "comment", "--comment", "homing-core-nat", "-j", "MASQUERADE",
	})
	ensureIPT("iptables", "filter", "INPUT", []string{
		"-p", "udp", "--dport", port,
		"-m", "comment", "--comment", "homing-core-in", "-j", "ACCEPT",
	})
	ensureIPT("iptables", "filter", "FORWARD", []string{
		"-i", *iface, "-m", "comment", "--comment", "homing-core-fwd", "-j", "ACCEPT",
	})
	ensureIPT("iptables", "filter", "FORWARD", []string{
		"-o", *iface, "-m", "comment", "--comment", "homing-core-fwd", "-j", "ACCEPT",
	})

	// IPv6 NAT66
	ensureIPT("ip6tables", "nat", "POSTROUTING", []string{
		"-s", data.IPv6CIDR, "-o", out,
		"-m", "comment", "--comment", "homing-core-nat6", "-j", "MASQUERADE",
	})
	ensureIPT("ip6tables", "filter", "FORWARD", []string{
		"-i", *iface, "-m", "comment", "--comment", "homing-core-fwd6", "-j", "ACCEPT",
	})
	ensureIPT("ip6tables", "filter", "FORWARD", []string{
		"-o", *iface, "-m", "comment", "--comment", "homing-core-fwd6", "-j", "ACCEPT",
	})
}

func markConfigDirty() {
	configDirtyMu.Lock()
	configDirty = true
	configDirtyMu.Unlock()
}

func takeConfigDirty() bool {
	configDirtyMu.Lock()
	d := configDirty
	configDirty = false
	configDirtyMu.Unlock()
	return d
}

func reconcile() error {
	if err := ensureLink(); err != nil {
		return err
	}
	if takeConfigDirty() {
		if err := configureDevice(); err != nil {
			markConfigDirty() // 下次重试
			return err
		}
	}
	ensureAddrs()
	ensureNAT()
	refreshStats()
	return nil
}

func refreshStats() {
	c, err := wgctrl.New()
	if err != nil {
		return
	}
	defer c.Close()
	dev, err := c.Device(*iface)
	if err != nil {
		return
	}
	statMu.Lock()
	defer statMu.Unlock()
	stats = map[string]peerStat{}
	for _, p := range dev.Peers {
		st := peerStat{Rx: p.ReceiveBytes, Tx: p.TransmitBytes, Handshake: p.LastHandshakeTime}
		if p.Endpoint != nil {
			st.Endpoint = p.Endpoint.String()
		}
		stats[p.PublicKey.String()] = st
	}
}

// ---------------- HTTP API ----------------

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func allowedIPsOf(c Client) []string {
	if len(c.AllowedIPs) > 0 {
		return c.AllowedIPs
	}
	// 兼容旧 core.json：没有该字段的客户端按全流量处理
	return []string{"0.0.0.0/0", "::/0"}
}

func clientJSON(c Client) map[string]interface{} {
	statMu.Lock()
	st := stats[c.PublicKey]
	statMu.Unlock()
	hs := ""
	if !st.Handshake.IsZero() {
		hs = st.Handshake.UTC().Format(time.RFC3339)
	}
	return map[string]interface{}{
		"id": c.ID, "name": c.Name, "enabled": c.Enabled,
		"ipv4Address": c.IPv4, "ipv6Address": c.IPv6,
		"latestHandshakeAt": hs, "endpoint": st.Endpoint,
		"transferRx": st.Rx, "transferTx": st.Tx,
		"persistentKeepalive": c.PersistentKeepalive,
		"dns":                 c.DNS, "allowedIps": allowedIPsOf(c),
		"proxyEgress":      c.ProxyEgress,
		"proxySelfService": c.ProxySelfService,
		"publicKey":        c.PublicKey,
	}
}

func findClient(id int) *Client {
	for i := range data.Clients {
		if data.Clients[i].ID == id {
			return &data.Clients[i]
		}
	}
	return nil
}

func nextIPs() (string, string) {
	used := map[int]bool{}
	for _, c := range data.Clients {
		if i := lastOctet(c.IPv4); i > 0 {
			used[i] = true
		}
	}
	// 网段以 data.IPv4CIDR / data.IPv6CIDR 为准（管理台可以改隧道网段，
	// 改完后新建的客户端必须落在新网段里）。
	base4 := uint32(0)
	if _, n, err := net.ParseCIDR(data.IPv4CIDR); err == nil {
		if v, ok := ipv4Uint32(n.IP); ok {
			base4 = v
		}
	}
	if base4 == 0 {
		base4 = 0x0a080000 // 10.8.0.0
	}
	base6 := net.ParseIP("fdcc:ad94:bacf:61a4::cafe:0").To16()
	if _, n, err := net.ParseCIDR(data.IPv6CIDR); err == nil {
		if v := n.IP.To16(); v != nil {
			base6 = v
		}
	}
	for i := 2; i < 254; i++ {
		if used[i] {
			continue
		}
		v4 := ipv4String(base4 + uint32(i))
		v6 := make(net.IP, 16)
		copy(v6, base6)
		last := binary.BigEndian.Uint16(v6[14:])
		binary.BigEndian.PutUint16(v6[14:], last+uint16(i))
		return v4, v6.String()
	}
	return "", ""
}

func lastOctet(ip string) int {
	if i := strings.LastIndex(ip, "."); i > 0 {
		if n, err := strconv.Atoi(ip[i+1:]); err == nil {
			return n
		}
	}
	return 0
}

func handleClients(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		refreshStats()
		dataMu.Lock()
		clients := make([]map[string]interface{}, 0, len(data.Clients))
		for _, c := range data.Clients {
			clients = append(clients, clientJSON(c))
		}
		dataMu.Unlock()
		sort.Slice(clients, func(i, j int) bool {
			return clients[i]["id"].(int) < clients[j]["id"].(int)
		})
		writeJSON(w, 200, clients)
	case "POST":
		var req struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		name := strings.TrimSpace(req.Name)
		if name == "" {
			writeJSON(w, 400, map[string]string{"error": "名称不能为空"})
			return
		}
		priv, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		psk, _ := wgtypes.GenerateKey()
		v4, v6 := nextIPs()
		if v4 == "" {
			writeJSON(w, 409, map[string]string{"error": "地址池已满"})
			return
		}
		dataMu.Lock()
		c := Client{
			ID: data.NextID, Name: name, PrivateKey: priv.String(),
			PublicKey: priv.PublicKey().String(), PresharedKey: psk.String(),
			IPv4: v4, IPv6: v6, Enabled: true, PersistentKeepalive: 25,
			DNS: data.DNS, AllowedIPs: []string{"0.0.0.0/0", "::/0"}, CreatedAt: time.Now().Unix(),
		}
		data.NextID++
		data.Clients = append(data.Clients, c)
		dataMu.Unlock()
		if err := saveData(); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		markConfigDirty()
		_ = reconcile()
		writeJSON(w, 200, map[string]interface{}{"clientId": c.ID})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func handleClientByID(w http.ResponseWriter, r *http.Request, idStr string) {
	id, _ := strconv.Atoi(idStr)
	dataMu.Lock()
	c := findClient(id)
	dataMu.Unlock()
	if c == nil {
		writeJSON(w, 404, map[string]string{"error": "客户端不存在"})
		return
	}
	switch r.Method {
	case "DELETE":
		dataMu.Lock()
		out := data.Clients[:0]
		for _, x := range data.Clients {
			if x.ID != id {
				out = append(out, x)
			}
		}
		data.Clients = out
		dataMu.Unlock()
		_ = saveData()
		markConfigDirty()
		_ = reconcile()
		writeJSON(w, 200, map[string]interface{}{"ok": true})
	case "POST", "PATCH", "PUT":
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		peerDirty := false
		dataMu.Lock()
		if v, ok := req["name"].(string); ok && strings.TrimSpace(v) != "" {
			c.Name = v
		}
		if v, ok := req["enabled"].(bool); ok && v != c.Enabled {
			c.Enabled = v
			peerDirty = true
		}
		if v, ok := req["proxyEgress"].(bool); ok {
			c.ProxyEgress = v
		}
		if v, ok := req["proxySelfService"].(bool); ok {
			c.ProxySelfService = v
		}
		if v, ok := req["persistentKeepalive"].(float64); ok && int(v) != c.PersistentKeepalive {
			c.PersistentKeepalive = int(v)
			peerDirty = true
		}
		if arr, ok := req["dns"].([]interface{}); ok {
			var dns []string
			for _, x := range arr {
				if s, ok := x.(string); ok && s != "" {
					dns = append(dns, s)
				}
			}
			c.DNS = dns
		}
		if arr, ok := req["allowedIps"].([]interface{}); ok {
			var ips []string
			for _, x := range arr {
				if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
					ips = append(ips, strings.TrimSpace(s))
				}
			}
			if len(ips) > 0 {
				c.AllowedIPs = ips
			}
		}
		dataMu.Unlock()
		_ = saveData()
		// 只有会影响服务端 WireGuard peer 的字段才重建 peer；改代理开关/备注/DNS
		// 等不需要 ReplacePeers，避免切一下代理就把所有客户端会话重置。
		if peerDirty {
			markConfigDirty()
			_ = reconcile()
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// clientProfile 返回该客户端的原生 profile（endpoint 由管理台按当前打洞端点补）
func clientProfile(c *Client) map[string]interface{} {
	peer := map[string]interface{}{
		"publicKey":           data.PublicKey,
		"presharedKey":        c.PresharedKey,
		"allowedIps":          allowedIPsOf(*c),
		"persistentKeepalive": c.PersistentKeepalive,
		"endpoint":            "",
	}
	return map[string]interface{}{
		"format":     "homing-profile/1",
		"name":       c.Name,
		"privateKey": c.PrivateKey,
		"addresses":  []string{c.IPv4 + "/32", c.IPv6 + "/128"},
		"mtu":        data.MTU,
		"dns":        c.DNS,
		"peers":      []interface{}{peer},
	}
}

func handleInterface(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		dataMu.Lock()
		out := map[string]interface{}{
			"port": data.ListenPort, "listenPort": data.ListenPort, "mtu": data.MTU,
			"ipv4Cidr": data.IPv4CIDR, "ipv6Cidr": data.IPv6CIDR,
			"addresses": []string{data.AddressV4, data.AddressV6},
			"publicKey": data.PublicKey, "dns": data.DNS,
			"clients": len(data.Clients),
		}
		dataMu.Unlock()
		writeJSON(w, 200, out)
	case "POST":
		var req struct {
			Port     int      `json:"port"`
			DNS      []string `json:"dns"`
			IPv4CIDR string   `json:"ipv4Cidr"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求体不是合法 JSON"})
			return
		}
		// port 省略（0）表示不修改；显式传入时保持原有 1024-65535 校验。
		if req.Port != 0 && (req.Port < 1024 || req.Port > 65535) {
			writeJSON(w, 400, map[string]string{"error": "端口范围 1024-65535"})
			return
		}
		var newCIDR *net.IPNet
		if strings.TrimSpace(req.IPv4CIDR) != "" {
			n, err := parseTunnelIPv4CIDR(req.IPv4CIDR)
			if err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			newCIDR = n
		}
		var newDNS []string
		for _, s := range req.DNS {
			if v := strings.TrimSpace(s); v != "" {
				newDNS = append(newDNS, v)
			}
		}

		reconfigure := req.Port != 0
		dataMu.Lock()
		if req.Port != 0 {
			data.ListenPort = req.Port
		}
		if len(newDNS) > 0 {
			// 用于之后新建客户端的默认 DNS；已有客户端各自保存的 DNS 不变。
			data.DNS = newDNS
		}
		if newCIDR != nil {
			cidr := newCIDR.String()
			if cidr != data.IPv4CIDR {
				base, _ := ipv4Uint32(newCIDR.IP)
				ones, _ := newCIDR.Mask.Size()
				data.IPv4CIDR = cidr
				data.AddressV4 = fmt.Sprintf("%s/%d", ipv4String(base+1), ones)
				// 现有客户端按当前顺序重新编号为 网络地址+2、+3……；IPv6 保持不变。
				// 客户端配置因此需要重新下载，这是预期行为。
				for i := range data.Clients {
					data.Clients[i].IPv4 = ipv4String(base + 2 + uint32(i))
				}
				reconfigure = true
			}
		}
		out := map[string]interface{}{
			"ok":         true,
			"port":       data.ListenPort,
			"listenPort": data.ListenPort,
			"ipv4Cidr":   data.IPv4CIDR,
			"addresses":  []string{data.AddressV4, data.AddressV6},
			"dns":        data.DNS,
		}
		dataMu.Unlock()
		_ = saveData()
		if reconfigure {
			markConfigDirty()
			_ = reconcile()
		}
		writeJSON(w, 200, out)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// parseTunnelIPv4CIDR 校验管理台提交的隧道 IPv4 网段：仅 IPv4、前缀 16~30、拒绝 0.0.0.0/0。
// 返回规范化后的网络地址（主机位清零）。
func parseTunnelIPv4CIDR(s string) (*net.IPNet, error) {
	ip, n, err := net.ParseCIDR(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("IPv4 网段格式无效: %v", err)
	}
	if ip.To4() == nil || n.IP.To4() == nil {
		return nil, fmt.Errorf("仅支持 IPv4 网段")
	}
	ones, bits := n.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("仅支持 IPv4 网段")
	}
	if ones == 0 {
		return nil, fmt.Errorf("不能使用 0.0.0.0/0")
	}
	if ones < 16 || ones > 30 {
		return nil, fmt.Errorf("IPv4 网段前缀长度需在 16~30 之间")
	}
	return n, nil
}

func handleUserConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		dataMu.Lock()
		uc := data.UserConfig
		dataMu.Unlock()
		writeJSON(w, 200, map[string]interface{}{"host": uc.Host, "port": uc.Port})
	case "POST":
		var req userConfig
		_ = json.NewDecoder(r.Body).Decode(&req)
		dataMu.Lock()
		if req.Host != "" {
			data.UserConfig.Host = req.Host
		}
		if req.Port != 0 {
			data.UserConfig.Port = req.Port
		}
		uc := data.UserConfig
		dataMu.Unlock()
		_ = saveData()
		writeJSON(w, 200, map[string]interface{}{"host": uc.Host, "port": uc.Port})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func startAPI() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/api/auth/password", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "homing", Value: "ok", Path: "/"})
		writeJSON(w, 200, map[string]interface{}{"ok": true})
	})
	mux.HandleFunc("/api/client", handleClients)
	mux.HandleFunc("/api/client/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/client/")
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) == 2 && parts[1] == "configuration" {
			id, _ := strconv.Atoi(parts[0])
			dataMu.Lock()
			c := findClient(id)
			var prof map[string]interface{}
			if c != nil {
				prof = clientProfile(c)
			}
			dataMu.Unlock()
			if prof == nil {
				writeJSON(w, 404, map[string]string{"error": "客户端不存在"})
				return
			}
			writeJSON(w, 200, prof)
			return
		}
		if len(parts) >= 1 {
			handleClientByID(w, r, parts[0])
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/admin/interface", handleInterface)
	mux.HandleFunc("/api/admin/userconfig", handleUserConfig)
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		statMu.Lock()
		n := len(stats)
		statMu.Unlock()
		dataMu.Lock()
		cl := len(data.Clients)
		port := data.ListenPort
		dataMu.Unlock()
		writeJSON(w, 200, map[string]interface{}{
			"iface": *iface, "listenPort": port, "clients": cl,
			"peersWithStats": n, "uptimeSeconds": int64(time.Since(startedAt).Seconds()),
		})
	})
	log.Printf("homing-core API listening on %s（iface=%s port=%d）", *apiAddr, *iface, data.ListenPort)
	if err := http.ListenAndServe(*apiAddr, mux); err != nil {
		log.Fatalf("API 监听失败: %v", err)
	}
}

var startedAt = time.Now()

// ---------------- 隧道内健康探测（给客户端做快速故障检测） ----------------
//
// 客户端每 12s 往 10.8.0.1:51824 发一个小 UDP 包，这里原样回显；
// 连续 3 次（约 36s）收不到回包，客户端就判定隧道数据面已断并开始恢复，
// 不再干等 隧道 握手 180s 超时。
func startProbeEcho() {
	if *probePort == 0 {
		return
	}
	// host 网络下只监听隧道口地址，避免把探针回显暴露到宿主机所有网卡。
	bindIP := data.AddressV4
	if i := strings.IndexByte(bindIP, '/'); i > 0 {
		bindIP = bindIP[:i]
	}
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(bindIP, strconv.Itoa(*probePort)))
	if err != nil {
		log.Printf("隧道探针地址无效: %v", err)
		return
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Printf("隧道探针端口 %d 监听失败: %v", *probePort, err)
		return
	}
	log.Printf("隧道探针回显 listening on %s（客户端探测 %s:%d）", conn.LocalAddr(), bindIP, *probePort)
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			_, _ = conn.WriteToUDP(buf[:n], from)
		}
	}()
}

// ---------------- main ----------------

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if err := loadData(); err != nil {
		log.Fatalf("%v", err)
	}
	if err := saveData(); err != nil {
		log.Printf("写数据文件失败（可能是权限问题）: %v", err)
	}
	cleanupOwnedRules()
	if err := reconcile(); err != nil {
		log.Fatalf("初始化隧道失败: %v", err)
	}
	log.Printf("homing-core 就绪：接口 %s，端口 %d，IPv4 网段 %s（服务端 %s），DNS %s，客户端 %d 个，NAT 出口 %s",
		*iface, data.ListenPort, data.IPv4CIDR, data.AddressV4, strings.Join(data.DNS, ","), len(data.Clients), detectOutIface())

	startProbeEcho()

	// 正常退出（docker stop / sv restart）时清掉自己写的 iptables 规则，
	// 避免宿主网络模式下残留 NAT/FORWARD 规则。
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		log.Printf("收到退出信号，清理 homing-core iptables 规则")
		cleanupOwnedRules()
		os.Exit(0)
	}()

	if *reconcileEvery > 0 {
		go func() {
			t := time.NewTicker(*reconcileEvery)
			defer t.Stop()
			for range t.C {
				if err := reconcile(); err != nil {
					log.Printf("reconcile 失败: %v", err)
				}
			}
		}()
	}

	startAPI()
}
