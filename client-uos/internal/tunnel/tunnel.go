// Package tunnel 负责真正把 隧道 隧道拉起来：
//
//	创建 隧道 接口（内核模块优先，不支持时自动降级到用户态 内置隧道内核）
//	-> 配置密钥/peer -> 加地址/MTU -> 全流量路由（wg-quick 同款策略路由）
//	-> 接管 DNS -> 对外提供状态 / 热换端点 / 拆除。
package tunnel

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"homing/client/internal/profile"
)

const (
	// Iface 是 Linux 侧创建的接口名（避免和系统自带的 wg0 冲突）。
	Iface = "homing0"
	// Table 是策略路由表号（与 wg-quick 默认一致）。
	Table = 51820

	RunDir     = "/run/homing"
	StatusPath = RunDir + "/status.json"
	StatePath  = RunDir + "/state_private.json"
	PidPath    = RunDir + "/homing-helper.pid"
	ControlSock = RunDir + "/control.sock"
	LogPath    = "/var/log/homing/homing.log"
)

// DiscoveryStatus 是出现在状态里的“端点发现”信息。
type DiscoveryStatus struct {
	Enabled        bool   `json:"enabled"`
	Record         string `json:"record,omitempty"`
	AutoFollow     bool   `json:"autoFollow"`
	LastOkUnix     int64  `json:"lastOkUnix,omitempty"`
	LastErr        string `json:"lastErr,omitempty"`
	V4             string `json:"v4,omitempty"`
	V6             string `json:"v6,omitempty"`
	LastChangeUnix int64  `json:"lastChangeUnix,omitempty"`
}

// ProbeStatus 是状态里展示的隧道内探针信息（2.3.6 起）。
type ProbeStatus struct {
	Enabled         bool   `json:"enabled"`
	State           string `json:"state,omitempty"` // active | idle | suspect | off
	IntervalSeconds int    `json:"intervalSeconds,omitempty"`
	Failures        int    `json:"failures,omitempty"`
	LastReplyUnix   int64  `json:"lastReplyUnix,omitempty"`
	Note            string `json:"note,omitempty"`
}

// Status 是 GUI 读取的运行状态（写到 /run/homing/status.json，0644）。
type Status struct {
	Version             string          `json:"version"`
	Running             bool            `json:"running"`
	Backend             string          `json:"backend,omitempty"` // kernel | userspace
	Iface               string          `json:"iface,omitempty"`
	Profile             string          `json:"profile,omitempty"`
	Endpoint            string          `json:"endpoint,omitempty"`
	LatestHandshakeUnix int64           `json:"latestHandshakeUnix,omitempty"`
	HandshakeAgeSeconds int             `json:"handshakeAgeSeconds"`
	RxBytes             int64           `json:"rxBytes"`
	TxBytes             int64           `json:"txBytes"`
	Addresses           []string        `json:"addresses,omitempty"`
	Discovery           DiscoveryStatus `json:"discovery"`
	Probe               ProbeStatus     `json:"probe"`
	Events              []string        `json:"events,omitempty"` // 最近关键事件（GUI 直接显示）
	UpdatedUnix         int64           `json:"updatedUnix"`
}

// Options 建隧道时的可选参数。
type Options struct {
	Userspace bool // 强制使用用户态 内置隧道内核
}

// Tunnel 一个已建立的隧道实例（持有后端句柄，用户态模式下必须由同一进程持有）。
type Tunnel struct {
	Kind    string
	Profile *profile.Profile
	backend Backend
}

// privateState 是需要 root 权限保存的内部状态（DNS 还原信息等）。
type privateState struct {
	DNSMode      string   `json:"dnsMode"`
	DNSServers   []string `json:"dnsServers,omitempty"`
	ResolvBackup string   `json:"resolvBackup,omitempty"`
	Endpoint     string   `json:"endpoint,omitempty"`
	Backend      string   `json:"backend,omitempty"`
}

// ErrNotRoot 表示需要 root 权限。
var ErrNotRoot = fmt.Errorf("需要 root 权限（请通过 pkexec 调用 homing-helper）")

// EnsureRunDir 创建运行目录。
func EnsureRunDir() error {
	if err := os.MkdirAll(RunDir, 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", RunDir, err)
	}
	return nil
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s %s 失败: %v: %s", name, strings.Join(args, " "), err, text)
		}
		return text, fmt.Errorf("%s %s 失败: %v", name, strings.Join(args, " "), err)
	}
	return text, nil
}

func hasCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// KernelInterfaceExists 判断内核里是否已有隧道接口。
func KernelInterfaceExists() bool {
	_, err := run("ip", "link", "show", "dev", Iface)
	return err == nil
}

// KernelSupported 探测内核是否支持 隧道（会尝试 modprobe，并用临时接口实测）。
func KernelSupported() bool {
	_, _ = run("modprobe", "wireguard")
	if _, err := run("ip", "link", "add", "dev", "homingchk0", "type", "wireguard"); err != nil {
		return false
	}
	_, _ = run("ip", "link", "del", "dev", "homingchk0")
	return true
}

// UserspaceAvailable 判断能否用用户态实现（需要 /dev/net/tun）。
func UserspaceAvailable() bool {
	_, err := os.Stat("/dev/net/tun")
	return err == nil
}

// Up 建隧道：内核支持就用内核，否则自动降级到用户态 内置隧道内核。
func Up(p *profile.Profile, opts Options) (*Tunnel, error) {
	if os.Geteuid() != 0 {
		return nil, ErrNotRoot
	}
	if err := p.Normalize(); err != nil {
		return nil, err
	}
	if err := EnsureRunDir(); err != nil {
		return nil, err
	}
	// 清理上一次可能残留的接口 / 规则 / DNS
	Cleanup()
	if KernelInterfaceExists() {
		_, _ = run("ip", "link", "del", "dev", Iface)
	}

	kind := "kernel"
	if opts.Userspace || p.Userspace {
		kind = "userspace"
		log.Printf("按配置/参数强制使用用户态隧道")
	} else if !KernelSupported() {
		kind = "userspace"
		log.Printf("内核不支持隧道模块（模块缺失或内核过老），自动降级到用户态隧道")
	} else {
		log.Printf("内核支持隧道模块，使用内核实现")
	}

	var backend Backend
	switch kind {
	case "kernel":
		backend = &kernelBackend{}
	case "userspace":
		if !UserspaceAvailable() {
			return nil, fmt.Errorf("内核不支持隧道模块，并且 %s 不存在：请安装内核隧道模块或确认 tun 模块已加载", "/dev/net/tun")
		}
		backend = &userspaceBackend{}
	}

	if err := backend.Setup(p, Table); err != nil {
		_ = backend.Close()
		Cleanup()
		return nil, err
	}
	log.Printf("接口 %s 已创建并就位（backend=%s）", Iface, kind)
	// 地址 + MTU + up
	for _, addr := range p.Addresses {
		if _, err := run("ip", familyFlag(addr), "addr", "add", addr, "dev", Iface); err != nil {
			_ = backend.Close()
			Cleanup()
			return nil, fmt.Errorf("添加地址 %s 失败: %w", addr, err)
		}
	}
	if _, err := run("ip", "link", "set", "dev", Iface, "mtu", strconv.Itoa(p.MTU), "up"); err != nil {
		_ = backend.Close()
		Cleanup()
		return nil, fmt.Errorf("设置 MTU/up 失败: %w", err)
	}
	log.Printf("隧道地址: %s   MTU=%d", strings.Join(p.Addresses, ", "), p.MTU)
	// 全流量策略路由
	if err := setupRoutes(p); err != nil {
		_ = backend.Close()
		Cleanup()
		return nil, err
	}
	// DNS
	log.Printf("策略路由: table %d + fwmark %d（IPv4/IPv6）", Table, Table)
	dnsMode, backup, err := applyDNS(p.DNS)
	if err != nil {
		_ = backend.Close()
		Cleanup()
		return nil, err
	}
	if len(p.DNS) > 0 {
		log.Printf("DNS 接管: %s（方式 %s）", strings.Join(p.DNS, ", "), dnsMode)
	}
	state := privateState{DNSMode: dnsMode, DNSServers: p.DNS, ResolvBackup: backup, Backend: kind}
	if len(p.Peers) > 0 {
		state.Endpoint = p.Peers[0].Endpoint
	}
	_ = writeJSON(StatePath, state, 0o600)

	return &Tunnel{Kind: kind, Profile: p, backend: backend}, nil
}

func setupRoutes(p *profile.Profile) error {
	for _, peer := range p.Peers {
		for _, cidr := range peer.AllowedIPs {
			fam := familyFlag(cidr)
			var args []string
			if cidr == "0.0.0.0/0" || cidr == "::/0" {
				args = []string{fam, "route", "add", "default", "dev", Iface, "table", strconv.Itoa(Table)}
			} else {
				args = []string{fam, "route", "add", cidr, "dev", Iface, "table", strconv.Itoa(Table)}
			}
			if _, err := run("ip", args...); err != nil {
				if !strings.Contains(err.Error(), "File exists") {
					return fmt.Errorf("添加路由 %s 失败: %w", cidr, err)
				}
			}
		}
	}
	for _, fam := range []string{"-4", "-6"} {
		_, _ = run("ip", fam, "rule", "add", "not", "fwmark", strconv.Itoa(Table), "table", strconv.Itoa(Table))
		_, _ = run("ip", fam, "rule", "add", "table", "main", "suppress_prefixlength", "0")
	}
	_ = os.WriteFile("/proc/sys/net/ipv4/conf/all/src_valid_mark", []byte("1"), 0o644)
	return nil
}

func removeRules() {
	for _, fam := range []string{"-4", "-6"} {
		for i := 0; i < 4; i++ {
			if _, err := run("ip", fam, "rule", "del", "not", "fwmark", strconv.Itoa(Table), "table", strconv.Itoa(Table)); err != nil {
				break
			}
		}
		for i := 0; i < 4; i++ {
			if _, err := run("ip", fam, "rule", "del", "table", "main", "suppress_prefixlength", "0"); err != nil {
				break
			}
		}
	}
}

// applyDNS 接管系统 DNS；返回方式与（文件方式下的）原 resolv.conf 备份。
func applyDNS(servers []string) (string, string, error) {
	if len(servers) == 0 {
		return "", "", nil
	}
	if hasCmd("resolvectl") {
		if _, err := run("resolvectl", "dns", Iface, strings.Join(servers, " ")); err == nil {
			_, _ = run("resolvectl", "domain", Iface, "~.")
			_, _ = run("resolvectl", "default-route", Iface, "yes")
			return "resolvectl", "", nil
		}
	}
	if hasCmd("resolvconf") {
		var sb strings.Builder
		for _, s := range servers {
			sb.WriteString("nameserver " + s + "\n")
		}
		cmd := exec.Command("resolvconf", "-a", Iface)
		cmd.Stdin = strings.NewReader(sb.String())
		if _, err := cmd.CombinedOutput(); err == nil {
			return "resolvconf", "", nil
		}
	}
	backup, _ := os.ReadFile("/etc/resolv.conf")
	var sb strings.Builder
	for _, s := range servers {
		sb.WriteString("nameserver " + s + "\n")
	}
	if err := os.WriteFile("/etc/resolv.conf", []byte(sb.String()), 0o644); err != nil {
		return "", "", fmt.Errorf("写入 /etc/resolv.conf 失败: %w", err)
	}
	return "file", string(backup), nil
}

func revertDNS() {
	state := readPrivateState()
	switch state.DNSMode {
	case "resolvectl":
		_, _ = run("resolvectl", "revert", Iface)
	case "resolvconf":
		_, _ = run("resolvconf", "-d", Iface)
	case "file":
		if state.ResolvBackup != "" {
			_ = os.WriteFile("/etc/resolv.conf", []byte(state.ResolvBackup), 0o644)
		}
	}
}

// Cleanup 兜底清理：DNS、ip rule、内核接口、状态文件（幂等，可在任何时机调用）。
func Cleanup() {
	revertDNS()
	removeRules()
	if KernelInterfaceExists() {
		_, _ = run("ip", "link", "del", "dev", Iface)
	}
	_ = os.Remove(StatePath)
}

// Down 拆除隧道（幂等）。
func (t *Tunnel) Down() error {
	log.Printf("拆除隧道：还原 DNS / 清理 ip rule / 删除接口")
	revertDNS()
	removeRules()
	if t.backend != nil {
		_ = t.backend.Close() // 用户态：关闭后 TUN 接口消失
	}
	if KernelInterfaceExists() {
		if _, err := run("ip", "link", "del", "dev", Iface); err != nil {
			return fmt.Errorf("删除接口失败: %w", err)
		}
	}
	_ = os.Remove(StatePath)
	_ = os.Remove(StatusPath)
	return nil
}

// Snapshot 采集当前状态。
func (t *Tunnel) Snapshot(profileName string, disc DiscoveryStatus) *Status {
	st := &Status{
		Version:             Version(),
		Iface:               Iface,
		Profile:             profileName,
		Discovery:           disc,
		UpdatedUnix:         time.Now().Unix(),
		HandshakeAgeSeconds: -1,
	}
	if t != nil {
		st.Backend = t.Kind
	}
	if !KernelInterfaceExists() {
		st.Running = false
		return st
	}
	st.Running = true
	st.Addresses = interfaceAddrs()
	if t != nil && t.backend != nil {
		if peer, err := t.backend.PeerState(); err == nil {
			st.Endpoint = peer.Endpoint
			if !peer.Handshake.IsZero() {
				st.LatestHandshakeUnix = peer.Handshake.Unix()
				st.HandshakeAgeSeconds = int(time.Since(peer.Handshake).Seconds())
				if st.HandshakeAgeSeconds < 0 {
					st.HandshakeAgeSeconds = 0
				}
			}
			st.RxBytes = peer.Rx
			st.TxBytes = peer.Tx
		}
	}
	return st
}

// SetEndpoint 热换对端端点（只改 peer.endpoint，不重建接口、不断流）。
func (t *Tunnel) SetEndpoint(hostport string, peerIndex int) (string, error) {
	if t == nil || t.backend == nil {
		return "", fmt.Errorf("隧道未运行，无法热换端点")
	}
	addr, err := resolveEndpoint(hostport)
	if err != nil {
		return "", fmt.Errorf("解析端点 %s 失败: %w", hostport, err)
	}
	if err := t.backend.SetEndpoint(addr, peerIndex); err != nil {
		return "", err
	}
	log.Printf("peer 端点已更新: %s", addr.String())
	state := readPrivateState()
	state.Endpoint = addr.String()
	_ = writeJSON(StatePath, state, 0o600)
	return addr.String(), nil
}

// WriteStatus 把状态写到 GUI 可读的 status.json（0644）。
func WriteStatus(st *Status) error {
	if err := EnsureRunDir(); err != nil {
		return err
	}
	return writeJSON(StatusPath, st, 0o644)
}

func interfaceAddrs() []string {
	out, err := run("ip", "-o", "addr", "show", "dev", Iface)
	if err != nil {
		return nil
	}
	var addrs []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if (f == "inet" || f == "inet6") && i+1 < len(fields) {
				addrs = append(addrs, fields[i+1])
			}
		}
	}
	return addrs
}

func familyFlag(cidr string) string {
	if strings.Contains(cidr, ":") {
		return "-6"
	}
	return "-4"
}

func resolveEndpoint(hostport string) (*net.UDPAddr, error) {
	s := strings.TrimSpace(hostport)
	if s == "" {
		return nil, fmt.Errorf("端点为空白")
	}
	if addr, err := net.ResolveUDPAddr("udp", s); err == nil {
		return addr, nil
	}
	if !strings.HasPrefix(s, "[") && strings.Count(s, ":") >= 2 {
		if addr, err := net.ResolveUDPAddr("udp", "["+s+"]"); err == nil {
			return addr, nil
		}
	}
	return nil, fmt.Errorf("无法解析 %q（应为 host:port 或 [ipv6]:port）", hostport)
}

func writeJSON(path string, v interface{}, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	_ = os.Chmod(tmp, perm)
	return os.Rename(tmp, path)
}

func readPrivateState() privateState {
	var st privateState
	data, err := os.ReadFile(StatePath)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st)
	return st
}

// ReadStatusFile 读取 status.json（给 CLI/调试用）。
func ReadStatusFile() (*Status, error) {
	var st Status
	data, err := os.ReadFile(StatusPath)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Version 返回版本号。
func Version() string { return version }

// KernelSnapshot 在内核后端下直接采集状态（守护进程不在时 CLI 用）。
func KernelSnapshot(profileName string, disc DiscoveryStatus) *Status {
	t := &Tunnel{Kind: "kernel", backend: &kernelBackend{}}
	return t.Snapshot(profileName, disc)
}

// SetEndpointKernel 在内核后端下直接热换端点（守护进程不在时 CLI 用）。
func SetEndpointKernel(hostport string, peerIndex int) (string, error) {
	t := &Tunnel{Kind: "kernel", backend: &kernelBackend{}}
	return t.SetEndpoint(hostport, peerIndex)
}
