// gateway-stun: 自研 STUN 打洞 + 隧道 中继 + 端点发布 + 状态接口
//
// 1. 用同一个 UDP socket 向多个 STUN 服务器查询公网映射，每 15s 保活；
// 2. 同一 socket 接收 隧道 客户端包，中继给本机 隧道核心（默认 127.0.0.1:500），回包按会话送回；
// 3. 端点变化发布：状态文件 / ntfy / 自定义命令 / Cloudflare TXT；
// 4. HTTP 状态接口（默认 :51831）给管理页用：当前端点、STUN 健康、发布历史、中继统计；
// 5. 运行配置热加载：/stun-data/config.json（STUN 服务器、推送、TXT），10s 轮询，无需重启。
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- STUN 常量 ----------
const (
	stunMagicCookie = 0x2112A442
	stunBindingReq  = 0x0001
	stunBindingResp = 0x0101
	attrMappedAddr  = 0x0001
	attrXorMapped   = 0x0020
)

// ---------- 配置 ----------
var (
	listenAddr       = flag.String("listen", ":51830", "本地监听地址（打洞 socket / 中继入口）")
	targetAddr       = flag.String("target", "127.0.0.1:51820", "隧道服务地址（隧道核心 发布端口）")
	stunServers      = flag.String("stun", "stun.miwifi.com:3478,stun.chat.bilibili.com:3478,stun.l.google.com:19302", "STUN 服务器，逗号分隔")
	keepalive        = flag.Duration("keepalive", 15*time.Second, "STUN 保活间隔")
	sessionTTL       = flag.Duration("session-ttl", 3*time.Minute, "中继会话空闲回收时间")
	stateFile        = flag.String("state-file", "/stun-data/endpoint.json", "端点状态文件")
	configFile       = flag.String("config-file", "/stun-data/config.json", "热加载配置文件")
	httpAddr         = flag.String("http", ":51831", "状态 HTTP 接口地址（供管理页使用）")
	publicHTTP       = flag.String("public-http", ":18081", "打洞自检页 HTTP 地址（0.0.0.0，可从公网/IPv6 访问）")
	upnpEnabled      = flag.Bool("upnp", false, "启动时用 UPnP 自动建立 UDP 端口映射（免手配端口转发）")
	upnpExternalPort = flag.Int("upnp-external-port", 0, "UPnP 外部端口（0=优先与本地监听端口相同，占用则随机）")
	upnpInterval     = flag.Duration("upnp-interval", 5*time.Minute, "UPnP 映射检查/续租间隔")
	notifyURL        = flag.String("notify-url", "", "可选：ntfy 主题 URL")
	onChangeCmd      = flag.String("on-change-cmd", "", "可选：端点变化时执行的 shell 命令")
	cfToken          = flag.String("cf-token", "", "可选：Cloudflare API Token")
	cfZone           = flag.String("cf-zone", "", "可选：Cloudflare Zone ID")
	cfRecord         = flag.String("cf-record", "", "可选：TXT 记录名")
	once             = flag.Bool("once", false, "只查询一次 STUN 并打印后退出（调试用）")
	verbose          = flag.Bool("verbose", false, "打印每个中继包（调试用）")
)

// 热加载配置
//
// DNS 公告板做成"模板 + provider"结构：
//
//	provider: cloudflare | aliyun | dnspod | custom | ""
//	record   : 完整记录名，如 vpn.example.com
//	domain   : 主域名，如 example.com（阿里云/DNSPod 需要）
//	custom*  : 自定义 Webhook 模板（{{endpoint}} {{ip}} {{port}} {{record}} {{domain}} {{value}}）
type dnsConfig struct {
	Enabled    bool   `json:"enabled"`
	Provider   string `json:"provider"`
	Record     string `json:"record"`
	Domain     string `json:"domain"`
	ValueTpl   string `json:"valueTpl"`   // 记录内容模板，默认 v1;v4={{endpoint}};ts={{ts}}
	AutoCreate *bool  `json:"autoCreate"` // 记录不存在时自动创建（默认 true）

	CFToken  string `json:"cloudflareToken"`
	CFZoneID string `json:"cloudflareZoneId"`

	AliKeyID  string `json:"aliyunAccessKeyId"`
	AliSecret string `json:"aliyunAccessKeySecret"`

	DPTokenID string `json:"dnspodTokenId"`
	DPToken   string `json:"dnspodToken"`

	CustomURL     string `json:"customUrl"`
	CustomMethod  string `json:"customMethod"`
	CustomHeaders string `json:"customHeaders"`
	CustomBody    string `json:"customBody"`
}

type v6Config struct {
	Enabled bool   `json:"enabled"`
	Address string `json:"address"` // 留空自动探测本机全局 IPv6
	Port    int    `json:"port"`    // IPv6 直连端口（默认 500，与 隧道 监听一致）
}

type fileConfig struct {
	StunServers []string  `json:"stunServers"`
	NotifyURL   string    `json:"notifyURL"`
	DNS         dnsConfig `json:"dns"`
	Target      string    `json:"target"` // 隧道 服务地址，如 127.0.0.1:51820
	V6          v6Config  `json:"v6"`

	// 兼容旧字段
	CFToken  string `json:"cfToken"`
	CFZone   string `json:"cfZone"`
	CFRecord string `json:"cfRecord"`
}

var (
	cfgMu         sync.RWMutex
	activeServers []*net.UDPAddr
	activeNotify  string
	activeCF      fileConfig
	activeV6      v6Config
	activeTarget  string
	cfgMtime      time.Time
)

func loadFileConfig(force bool) {
	if *configFile == "" {
		return
	}
	st, err := os.Stat(*configFile)
	if err != nil {
		return
	}
	cfgMu.RLock()
	same := !st.ModTime().After(cfgMtime)
	cfgMu.RUnlock()
	if same && !force {
		return
	}
	b, err := os.ReadFile(*configFile)
	if err != nil {
		return
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		log.Printf("config parse error: %v", err)
		return
	}
	// 兼容旧 Cloudflare 字段
	if fc.DNS.Provider == "" && fc.CFToken != "" {
		fc.DNS = dnsConfig{Enabled: true, Provider: "cloudflare", Record: fc.CFRecord, CFToken: fc.CFToken, CFZoneID: fc.CFZone}
	}
	var addrs []*net.UDPAddr
	for _, s := range fc.StunServers {
		if a, err := net.ResolveUDPAddr("udp", strings.TrimSpace(s)); err == nil {
			addrs = append(addrs, a)
		}
	}
	cfgMu.Lock()
	if len(addrs) > 0 {
		activeServers = addrs
	}
	activeNotify = fc.NotifyURL
	activeCF = fc
	activeV6 = fc.V6
	if strings.TrimSpace(fc.Target) != "" {
		activeTarget = strings.TrimSpace(fc.Target)
	}
	cfgMtime = st.ModTime()
	cfgMu.Unlock()
	// 配置变更后清空 DNS 发布状态，等待下一次发布刷新
	lastDNSErr.Store("")
	lastDNSOK.Store("")
	log.Printf("config reloaded: stunServers=%d notify=%t dnsProvider=%s dnsRecord=%s", len(addrs), fc.NotifyURL != "", fc.DNS.Provider, fc.DNS.Record)
}

func currentServers() []*net.UDPAddr {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	out := make([]*net.UDPAddr, len(activeServers))
	copy(out, activeServers)
	return out
}

// ---------- 端点与状态 ----------
type Endpoint struct {
	IP        string    `json:"ip"`
	Port      int       `json:"port"`
	Address   string    `json:"address"`
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updated_at"`
}

var (
	epMu       sync.Mutex
	currentEP  *Endpoint
	firstSeen  time.Time
	stunOK     int64
	lastStunOK time.Time
	history    []Endpoint
	relayPkts  int64
	startedAt  = time.Now()
)

func setEndpoint(ep Endpoint, first bool) {
	epMu.Lock()
	if currentEP != nil && currentEP.IP == ep.IP && currentEP.Port == ep.Port {
		epMu.Unlock()
		return
	}
	old := ""
	firstPublish := currentEP == nil
	if currentEP != nil {
		old = currentEP.Address
	}
	currentEP = &ep
	if first || firstPublish {
		firstSeen = ep.UpdatedAt
	}
	history = append(history, ep)
	if len(history) > 50 {
		history = history[len(history)-50:]
	}
	epMu.Unlock()

	if firstPublish {
		log.Printf("ENDPOINT discovered: %s (source=%s)", ep.Address, ep.Source)
	} else {
		log.Printf("ENDPOINT changed: %s -> %s (source=%s)", old, ep.Address, ep.Source)
	}
	go publish(ep, old, firstPublish)
}

func getEndpoint() *Endpoint {
	epMu.Lock()
	defer epMu.Unlock()
	return currentEP
}

// ---------- STUN 报文 ----------
func newTransactionID() []byte {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return b
}

func buildBindingRequest(txID []byte) []byte {
	msg := make([]byte, 20)
	binary.BigEndian.PutUint16(msg[0:2], stunBindingReq)
	binary.BigEndian.PutUint16(msg[2:4], 0)
	binary.BigEndian.PutUint32(msg[4:8], stunMagicCookie)
	copy(msg[8:20], txID)
	return msg
}

func isSTUN(b []byte) bool {
	return len(b) >= 20 && binary.BigEndian.Uint32(b[4:8]) == stunMagicCookie
}

func stunTxID(b []byte) string {
	if len(b) < 20 {
		return ""
	}
	return hex.EncodeToString(b[8:20])
}

func parseMapped(b []byte) (net.IP, int, bool) {
	if len(b) < 20 {
		return nil, 0, false
	}
	if binary.BigEndian.Uint16(b[0:2]) != stunBindingResp {
		return nil, 0, false
	}
	length := int(binary.BigEndian.Uint16(b[2:4]))
	pos, end := 20, 20+length
	if end > len(b) {
		end = len(b)
	}
	for pos+4 <= end {
		atype := binary.BigEndian.Uint16(b[pos : pos+2])
		alen := int(binary.BigEndian.Uint16(b[pos+2 : pos+4]))
		if pos+4+alen > end {
			break
		}
		val := b[pos+4 : pos+4+alen]
		if (atype == attrXorMapped || atype == attrMappedAddr) && len(val) >= 8 {
			family := val[1]
			port := int(binary.BigEndian.Uint16(val[2:4]))
			if family == 0x01 {
				ip := net.IP(append([]byte{}, val[4:8]...))
				if atype == attrXorMapped {
					port ^= int(stunMagicCookie >> 16)
					xor := make([]byte, 4)
					binary.BigEndian.PutUint32(xor, stunMagicCookie)
					for i := 0; i < 4; i++ {
						ip[i] ^= xor[i]
					}
				}
				return ip, port, true
			}
		}
		pos += 4 + alen
		if alen%4 != 0 {
			pos += 4 - alen%4
		}
	}
	return nil, 0, false
}

// ---------- 中继 ----------
//
// 每个客户端只保留一条「入站 socket -> 上游 socket」会话，复用同一个上游源端口。
// 之前按包新建上游 socket 的写法会为每个 UDP 包泄漏一个 fd + goroutine（最长
// sessionTTL 才回收），大流量下会把进程 fd 打满、中继变慢甚至丢包——这就是
// 「隧道用一会儿就断」的根因之一。
type session struct {
	upstream *net.UDPConn
	target   string
	lastSeen time.Time
}

// relaySessionView 是状态页用的中继会话快照：
//   - Client: 客户端在公网侧的真实地址（NAT 后的公网 IP:端口）
//   - Local:  中继转发给隧道核心时使用的本机源地址（core 的 wg endpoint 看到的就是它）
type relaySessionView struct {
	Client      string `json:"client"`
	Local       string `json:"local"`
	Target      string `json:"target,omitempty"`
	LastSeen    string `json:"lastSeen"`
	IdleSeconds int64  `json:"idleSeconds"`
}

var (
	sessMu   sync.Mutex
	sessions = map[string]*session{}
)

func snapshotRelaySessions() []relaySessionView {
	sessMu.Lock()
	defer sessMu.Unlock()
	out := make([]relaySessionView, 0, len(sessions))
	for key, s := range sessions {
		local := ""
		if s.upstream != nil {
			local = s.upstream.LocalAddr().String()
		}
		out = append(out, relaySessionView{
			Client:      key,
			Local:       local,
			Target:      s.target,
			LastSeen:    s.lastSeen.Format(time.RFC3339),
			IdleSeconds: int64(time.Since(s.lastSeen).Seconds()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IdleSeconds < out[j].IdleSeconds })
	return out
}

// relaySessionCount 返回当前中继会话数（状态页诊断用）。
func relaySessionCount() int {
	sessMu.Lock()
	defer sessMu.Unlock()
	return len(sessions)
}

func relayLoop(conn *net.UDPConn, target *net.UDPAddr) {
	_ = target
	buf := make([]byte, 65535)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("read error: %v", err)
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		if isSTUN(pkt) {
			handleSTUN(pkt)
			continue
		}
		atomic.AddInt64(&relayPkts, 1)
		rec := classifyProbe(from, pkt)
		tgt := currentTarget()
		if tgt == nil {
			recordProbe(rec)
			continue
		}
		s, created, err := sessionFor(conn, from, tgt)
		if err != nil || s == nil {
			if *verbose && err != nil {
				log.Printf("dial target: %v", err)
			}
			rec.Forwarded = false
			rec.Target = tgt.String()
			recordProbe(rec)
			continue
		}
		if !created {
			sessMu.Lock()
			s.lastSeen = time.Now()
			sessMu.Unlock()
		}
		_, werr := s.upstream.Write(pkt)
		rec.Forwarded = werr == nil
		rec.Target = tgt.String()
		recordProbe(rec)
		if *verbose && created {
			log.Printf("relay session created: %s -> %s", from, tgt)
		}
	}
}

// sessionFor 取客户端已有会话，没有（或目标变了）就新建一条；返回是否新建。
func sessionFor(conn *net.UDPConn, client *net.UDPAddr, tgt *net.UDPAddr) (*session, bool, error) {
	key := client.String()
	target := tgt.String()

	sessMu.Lock()
	if s := sessions[key]; s != nil {
		if s.target == target {
			s.lastSeen = time.Now()
			sessMu.Unlock()
			return s, false, nil
		}
		delete(sessions, key)
		sessMu.Unlock()
		_ = s.upstream.Close()
	} else {
		sessMu.Unlock()
	}

	up, err := net.DialUDP("udp", nil, tgt)
	if err != nil {
		return nil, false, err
	}
	s := &session{upstream: up, target: target, lastSeen: time.Now()}
	sessMu.Lock()
	sessions[key] = s
	sessMu.Unlock()
	go upstreamReader(conn, client, s)
	return s, true, nil
}

func upstreamReader(conn *net.UDPConn, client *net.UDPAddr, s *session) {
	buf := make([]byte, 65535)
	for {
		_ = s.upstream.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := s.upstream.Read(buf)
		if err != nil {
			// 读超时不一定代表会话死亡（可能只是暂时没有回包），
			// 只有客户端也长时间没有发包才回收，避免频繁重建源端口。
			sessMu.Lock()
			idle := time.Since(s.lastSeen)
			current := sessions[client.String()] == s
			if !current {
				// 会话已被替换/删除：直接退出，避免对已关闭 socket 空转
				sessMu.Unlock()
				_ = s.upstream.Close()
				return
			}
			if idle > *sessionTTL {
				delete(sessions, client.String())
			}
			sessMu.Unlock()
			if idle > *sessionTTL {
				_ = s.upstream.Close()
				if *verbose {
					log.Printf("session closed (%s), active=%d", client, relaySessionCount())
				}
				return
			}
			continue
		}
		sessMu.Lock()
		s.lastSeen = time.Now()
		sessMu.Unlock()
		_, _ = conn.WriteToUDP(buf[:n], client)
	}
}

// ---------- STUN 保活 ----------
var (
	stunMu    sync.Mutex
	pendingTx = map[string]time.Time{}
)

func handleSTUN(pkt []byte) {
	tx := stunTxID(pkt)
	if tx == "" {
		return
	}
	stunMu.Lock()
	_, ok := pendingTx[tx]
	if ok {
		delete(pendingTx, tx)
	}
	stunMu.Unlock()
	if !ok {
		return
	}
	ip, port, ok := parseMapped(pkt)
	if !ok || ip == nil || ip.IsUnspecified() {
		return
	}
	atomic.AddInt64(&stunOK, 1)
	lastStunOK = time.Now()
	setEndpoint(Endpoint{
		IP: ip.String(), Port: port,
		Address:   net.JoinHostPort(ip.String(), fmt.Sprintf("%d", port)),
		Source:    "stun",
		UpdatedAt: time.Now(),
	}, getEndpoint() == nil)
}

func stunLoop(conn *net.UDPConn) {
	sendAll := func() {
		tx := newTransactionID()
		req := buildBindingRequest(tx)
		stunMu.Lock()
		pendingTx[hex.EncodeToString(tx)] = time.Now()
		stunMu.Unlock()
		for _, srv := range currentServers() {
			_, _ = conn.WriteToUDP(req, srv)
		}
		stunMu.Lock()
		for k, t := range pendingTx {
			if time.Since(t) > 5*time.Second {
				delete(pendingTx, k)
			}
		}
		stunMu.Unlock()
	}
	sendAll()
	ticker := time.NewTicker(*keepalive)
	defer ticker.Stop()
	for range ticker.C {
		loadFileConfig(false)
		sendAll()
	}
}

// ---------- 发布 ----------
type publishPayload struct {
	Type      string    `json:"type"`
	V4        string    `json:"v4,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Source    string    `json:"source"`
}

func publish(ep Endpoint, old string, first bool) {
	payload := publishPayload{Type: "endpoint", V4: ep.Address, UpdatedAt: ep.UpdatedAt, Source: ep.Source}
	if *stateFile != "" {
		_ = os.MkdirAll(dirOf(*stateFile), 0o755)
		b, _ := json.MarshalIndent(payload, "", "  ")
		tmp := *stateFile + ".tmp"
		if err := os.WriteFile(tmp, b, 0o644); err == nil {
			_ = os.Rename(tmp, *stateFile)
		}
	}
	cfgMu.RLock()
	notify := *notifyURL
	if activeNotify != "" {
		notify = activeNotify
	}
	cf := activeCF
	cfgMu.RUnlock()
	if notify != "" {
		body := fmt.Sprintf("隧道端点已更新\nv4=%s\n时间=%s", ep.Address, ep.UpdatedAt.Format(time.RFC3339))
		req, err := http.NewRequest("POST", notify, strings.NewReader(body))
		if err == nil {
			req.Header.Set("Title", "Homing endpoint changed")
			req.Header.Set("Tags", "homing")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}
	}
	if *onChangeCmd != "" {
		cmd := exec.Command("/bin/sh", "-c", *onChangeCmd)
		cmd.Env = append(os.Environ(), "STUN_IP="+ep.IP, fmt.Sprintf("STUN_PORT=%d", ep.Port), "STUN_ENDPOINT="+ep.Address)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("on-change cmd failed: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	if cf.DNS.Enabled && cf.DNS.Provider != "" && cf.DNS.Record != "" {
		if err := publishDNS(cf.DNS, ep.Address); err != nil {
			log.Printf("dns publish failed (%s %s): %v", cf.DNS.Provider, cf.DNS.Record, err)
			lastDNSErr.Store(err.Error())
		} else {
			lastDNSErr.Store("")
			lastDNSOK.Store(time.Now().Format(time.RFC3339))
			log.Printf("dns publish ok (%s %s = %s)", cf.DNS.Provider, cf.DNS.Record, recordValue(cf.DNS, ep.Address))
		}
	}
}

var (
	lastDNSOK  atomic.Value
	lastDNSErr atomic.Value
)

func init() { lastDNSOK.Store(""); lastDNSErr.Store("") }

type v6Cand struct {
	Iface  string `json:"iface"`
	Addr   string `json:"addr"`
	Prefix int    `json:"prefix"`
}

// v6Candidates 枚举宿主机全局 IPv6（容器是 host 网络，看到的就是宿主机网卡）
func v6Candidates() []v6Cand {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []v6Cand
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP
			if ip.To4() != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				continue
			}
			plen, _ := ipn.Mask.Size()
			out = append(out, v6Cand{Iface: ifc.Name, Addr: ip.String(), Prefix: plen})
		}
	}
	return out
}

// detectGlobalV6 选一个最稳定的全局 IPv6：优先非 /64（DHCPv6/静态），否则取 /64
func detectGlobalV6() string {
	cands := v6Candidates()
	if len(cands) == 0 {
		return ""
	}
	for _, c := range cands {
		if c.Prefix != 64 {
			return c.Addr
		}
	}
	return cands[len(cands)-1].Addr
}

var (
	targetMu       sync.Mutex
	targetResolved *net.UDPAddr
	targetStr      string
)

// currentTarget 返回当前 隧道 服务地址（配置热加载可变）
func currentTarget() *net.UDPAddr {
	cfgMu.RLock()
	s := activeTarget
	cfgMu.RUnlock()
	if s == "" {
		s = *targetAddr
	}
	targetMu.Lock()
	defer targetMu.Unlock()
	if s == targetStr && targetResolved != nil {
		return targetResolved
	}
	if a, err := net.ResolveUDPAddr("udp", s); err == nil {
		targetResolved, targetStr = a, s
	}
	return targetResolved
}

func targetPort() int {
	cfgMu.RLock()
	s := activeTarget
	cfgMu.RUnlock()
	if s == "" {
		s = *targetAddr
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		if n, err := strconv.Atoi(s[i+1:]); err == nil {
			return n
		}
	}
	return 0
}

func currentV6() (addr string, port int, endpoint string) {
	cfgMu.RLock()
	v := activeV6
	cfgMu.RUnlock()
	if !v.Enabled {
		return "", 0, ""
	}
	addr = strings.TrimSpace(v.Address)
	if addr == "" {
		addr = detectGlobalV6()
	}
	if addr == "" {
		return "", 0, ""
	}
	addr = strings.Trim(addr, "[]")
	port = targetPort() // IPv6 直连端口 = 隧道 监听端口（自动跟随）
	if port == 0 {
		port = v.Port
	}
	if port == 0 {
		port = 51820
	}
	return addr, port, "[" + addr + "]:" + strconv.Itoa(port)
}

func recordValue(d dnsConfig, endpoint string) string {
	tpl := d.ValueTpl
	if tpl == "" {
		tpl = "v1;v4={{endpoint}};ts={{ts}}"
	}
	host, port := endpoint, ""
	if i := strings.LastIndex(endpoint, ":"); i > 0 {
		host, port = endpoint[:i], endpoint[i+1:]
	}
	tpl = strings.ReplaceAll(tpl, "{{endpoint}}", endpoint)
	tpl = strings.ReplaceAll(tpl, "{{ip}}", host)
	tpl = strings.ReplaceAll(tpl, "{{port}}", port)
	tpl = strings.ReplaceAll(tpl, "{{record}}", d.Record)
	tpl = strings.ReplaceAll(tpl, "{{domain}}", d.Domain)
	v6addr, v6port, v6ep := currentV6()
	tpl = strings.ReplaceAll(tpl, "{{v6endpoint}}", v6ep)
	tpl = strings.ReplaceAll(tpl, "{{v6addr}}", v6addr)
	tpl = strings.ReplaceAll(tpl, "{{v6port}}", strconv.Itoa(v6port))
	tpl = strings.ReplaceAll(tpl, "{{ts}}", strconv.FormatInt(time.Now().Unix(), 10))
	return tpl
}

func publishDNS(d dnsConfig, endpoint string) error {
	value := recordValue(d, endpoint)
	switch d.Provider {
	case "cloudflare":
		return publishCloudflare(d, value)
	case "aliyun":
		return publishAliyun(d, value)
	case "dnspod":
		return publishDNSPod(d, value)
	case "custom":
		return publishCustom(d, endpoint, value)
	default:
		return fmt.Errorf("unknown provider %q", d.Provider)
	}
}

func subDomain(record, domain string) string {
	if domain == "" {
		return record
	}
	r := strings.TrimSuffix(record, "."+strings.TrimSuffix(domain, "."))
	return strings.TrimSuffix(r, ".")
}

func httpJSON(req *http.Request, out interface{}) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---------- Cloudflare ----------
type cfDNSRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

func autoCreate(d dnsConfig) bool {
	if d.AutoCreate == nil {
		return true
	}
	return *d.AutoCreate
}

func cfDo(method, url, token string, body interface{}, out interface{}) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return httpJSON(req, out)
}

func publishCloudflare(d dnsConfig, value string) error {
	zone := d.CFZoneID
	if zone == "" {
		if d.Domain == "" {
			return fmt.Errorf("Cloudflare 需要 Zone ID 或 Domain")
		}
		var zr struct {
			Success bool `json:"success"`
			Result  []struct {
				ID string `json:"id"`
			} `json:"result"`
		}
		u := "https://api.cloudflare.com/client/v4/zones?name=" + url.QueryEscape(d.Domain)
		if err := cfDo("GET", u, d.CFToken, nil, &zr); err != nil {
			return fmt.Errorf("查询 zone 失败: %w", err)
		}
		if !zr.Success || len(zr.Result) == 0 {
			return fmt.Errorf("未找到 domain %s 的 zone", d.Domain)
		}
		zone = zr.Result[0].ID
	}
	base := "https://api.cloudflare.com/client/v4/zones/" + zone + "/dns_records"

	// 先查同名 TXT 记录，存在则更新，避免每次发布产生重复记录
	var list struct {
		Success bool          `json:"success"`
		Result  []cfDNSRecord `json:"result"`
	}
	u := base + "?type=TXT&name=" + url.QueryEscape(d.Record) + "&per_page=50"
	if err := cfDo("GET", u, d.CFToken, nil, &list); err != nil {
		return fmt.Errorf("查询 TXT 记录失败: %w", err)
	}
	if !list.Success {
		return fmt.Errorf("cloudflare: 查询 TXT 记录失败")
	}
	body := map[string]interface{}{"type": "TXT", "name": d.Record, "content": value, "ttl": 60}
	var out struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if len(list.Result) > 0 {
		if err := cfDo("PUT", base+"/"+list.Result[0].ID, d.CFToken, body, &out); err != nil {
			return fmt.Errorf("更新 TXT 记录失败: %w", err)
		}
	} else {
		if !autoCreate(d) {
			return fmt.Errorf("TXT 记录 %s 不存在，且未开启自动创建", d.Record)
		}
		if err := cfDo("POST", base, d.CFToken, body, &out); err != nil {
			return fmt.Errorf("创建 TXT 记录失败: %w", err)
		}
	}
	if !out.Success {
		if len(out.Errors) > 0 {
			return fmt.Errorf("cloudflare: %s", out.Errors[0].Message)
		}
		return fmt.Errorf("cloudflare: unknown error")
	}
	return nil
}

// ---------- 阿里云 AliDNS（RPC 签名 v1） ----------
func aliEncode(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "*", "%2A")
	e = strings.ReplaceAll(e, "%7E", "~")
	return e
}

func aliSignedURL(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, aliEncode(k)+"="+aliEncode(params[k]))
	}
	canonical := strings.Join(parts, "&")
	stringToSign := "GET&%2F&" + aliEncode(canonical)
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return "https://alidns.aliyuncs.com/?" + canonical + "&Signature=" + aliEncode(sig)
}

func aliyunCall(d dnsConfig, action string, extra map[string]string) ([]byte, error) {
	params := map[string]string{
		"AccessKeyId":      d.AliKeyID,
		"Action":           action,
		"Format":           "JSON",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   strconv.FormatInt(time.Now().UnixNano(), 10),
		"SignatureVersion": "1.0",
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"Version":          "2015-01-09",
	}
	for k, v := range extra {
		params[k] = v
	}
	req, _ := http.NewRequest("GET", aliSignedURL(params, d.AliSecret), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("aliyun %s status %d: %s", action, resp.StatusCode, truncate(string(b), 200))
	}
	return b, nil
}

func publishAliyun(d dnsConfig, value string) error {
	if d.Domain == "" {
		return fmt.Errorf("阿里云需要 Domain（如 example.com）")
	}
	rr := subDomain(d.Record, d.Domain)
	b, err := aliyunCall(d, "DescribeDomainRecords", map[string]string{
		"DomainName": d.Domain, "RRKeyWord": rr, "Type": "TXT", "PageSize": "50",
	})
	if err != nil {
		return err
	}
	var list struct {
		DomainRecords struct {
			Record []struct {
				RecordID string `json:"RecordId"`
				RR       string `json:"RR"`
				Type     string `json:"Type"`
			} `json:"Record"`
		} `json:"DomainRecords"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("解析 DescribeDomainRecords 失败: %w", err)
	}
	recordID := ""
	for _, r := range list.DomainRecords.Record {
		if r.RR == rr && r.Type == "TXT" {
			recordID = r.RecordID
			break
		}
	}
	if recordID == "" {
		if !autoCreate(d) {
			return fmt.Errorf("阿里云未找到 TXT 记录 %s，且未开启自动创建", d.Record)
		}
		_, err := aliyunCall(d, "AddDomainRecord", map[string]string{
			"DomainName": d.Domain, "RR": rr, "Type": "TXT", "Value": value, "TTL": "600",
		})
		return err
	}
	_, err = aliyunCall(d, "UpdateDomainRecord", map[string]string{
		"RecordId": recordID, "RR": rr, "Type": "TXT", "Value": value, "TTL": "600",
	})
	return err
}

// ---------- 腾讯云 DNSPod ----------
func publishDNSPod(d dnsConfig, value string) error {
	if d.Domain == "" {
		return fmt.Errorf("DNSPod 需要 Domain（如 example.com）")
	}
	loginToken := d.DPTokenID + "," + d.DPToken
	rr := subDomain(d.Record, d.Domain)
	form := url.Values{}
	form.Set("login_token", loginToken)
	form.Set("format", "json")
	form.Set("domain", d.Domain)
	form.Set("sub_domain", rr)
	form.Set("record_type", "TXT")
	req, _ := http.NewRequest("POST", "https://dnsapi.cn/Record.List", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var list struct {
		Status struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"status"`
		Records []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"records"`
	}
	if err := httpJSON(req, &list); err != nil {
		return err
	}
	if list.Status.Code != "1" {
		return fmt.Errorf("dnspod Record.List: %s", list.Status.Message)
	}
	recordID := ""
	for _, r := range list.Records {
		if r.Name == rr && strings.EqualFold(r.Type, "TXT") {
			recordID = r.ID
			break
		}
	}
	if recordID == "" {
		if !autoCreate(d) {
			return fmt.Errorf("DNSPod 未找到 TXT 记录 %s，且未开启自动创建", d.Record)
		}
		cf := url.Values{}
		cf.Set("login_token", loginToken)
		cf.Set("format", "json")
		cf.Set("domain", d.Domain)
		cf.Set("sub_domain", rr)
		cf.Set("record_type", "TXT")
		cf.Set("record_line", "默认")
		cf.Set("value", value)
		cf.Set("ttl", "60")
		creq, _ := http.NewRequest("POST", "https://dnsapi.cn/Record.Create", strings.NewReader(cf.Encode()))
		creq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		var cout struct {
			Status struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"status"`
		}
		if err := httpJSON(creq, &cout); err != nil {
			return err
		}
		if cout.Status.Code != "1" {
			return fmt.Errorf("dnspod Record.Create: %s", cout.Status.Message)
		}
		return nil
	}
	form2 := url.Values{}
	form2.Set("login_token", loginToken)
	form2.Set("format", "json")
	form2.Set("domain", d.Domain)
	form2.Set("record_id", recordID)
	form2.Set("sub_domain", rr)
	form2.Set("record_type", "TXT")
	form2.Set("record_line", "默认")
	form2.Set("value", value)
	form2.Set("ttl", "600")
	req2, _ := http.NewRequest("POST", "https://dnsapi.cn/Record.Modify", strings.NewReader(form2.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		Status struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"status"`
	}
	if err := httpJSON(req2, &out); err != nil {
		return err
	}
	if out.Status.Code != "1" {
		return fmt.Errorf("dnspod Record.Modify: %s", out.Status.Message)
	}
	return nil
}

// ---------- 自定义 Webhook 模板 ----------
func renderTpl(tpl string, d dnsConfig, endpoint, value string) string {
	host, port := endpoint, ""
	if i := strings.LastIndex(endpoint, ":"); i > 0 {
		host, port = endpoint[:i], endpoint[i+1:]
	}
	r := strings.NewReplacer(
		"{{endpoint}}", endpoint, "{{ip}}", host, "{{port}}", port,
		"{{record}}", d.Record, "{{domain}}", d.Domain, "{{value}}", value,
		"{{ts}}", strconv.FormatInt(time.Now().Unix(), 10),
	)
	return r.Replace(tpl)
}

func publishCustom(d dnsConfig, endpoint, value string) error {
	if d.CustomURL == "" {
		return fmt.Errorf("自定义 Webhook 需要 URL")
	}
	method := d.CustomMethod
	if method == "" {
		method = "POST"
	}
	body := renderTpl(d.CustomBody, d, endpoint, value)
	req, err := http.NewRequest(method, renderTpl(d.CustomURL, d, endpoint, value), strings.NewReader(body))
	if err != nil {
		return err
	}
	if d.CustomHeaders != "" {
		var headers map[string]string
		if json.Unmarshal([]byte(renderTpl(d.CustomHeaders, d, endpoint, value)), &headers) == nil {
			for k, v := range headers {
				req.Header.Set(k, v)
			}
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook status %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	return nil
}

func dirOf(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "."
	}
	return p[:i]
}

// ---------- 状态 HTTP 接口 ----------
type statusResp struct {
	Listen           string             `json:"listen"`
	Target           string             `json:"target"`
	Servers          []string           `json:"servers"`
	Current          *Endpoint          `json:"current"`
	FirstSeen        *time.Time         `json:"firstSeen"`
	LastStunOK       string             `json:"lastStunOk"`
	StunOkCount      int64              `json:"stunOkCount"`
	MappingChanges   []Endpoint         `json:"mappingChanges"`
	RelaySessions    int                `json:"relaySessions"`
	RelayPackets     int64              `json:"relayPackets"`
	RelaySessionList []relaySessionView `json:"relaySessionList"`
	Goroutines       int                `json:"goroutines"`
	UptimeSeconds    int64              `json:"uptimeSeconds"`
	UPnP             upnpState          `json:"upnp"`

	V6Enabled    bool     `json:"v6Enabled"`
	V6Address    string   `json:"v6Address"`
	V6Endpoint   string   `json:"v6Endpoint"`
	V6Candidates []v6Cand `json:"v6Candidates"`

	DNSEnabled  bool   `json:"dnsEnabled"`
	DNSProvider string `json:"dnsProvider"`
	DNSRecord   string `json:"dnsRecord"`
	DNSValue    string `json:"dnsValue"`
	LastDNSOK   string `json:"lastDnsOk"`
	LastDNSErr  string `json:"lastDnsErr"`
}

func targetDisplay() string {
	if a := currentTarget(); a != nil {
		return a.String()
	}
	cfgMu.RLock()
	s := activeTarget
	cfgMu.RUnlock()
	if s != "" {
		return s
	}
	return *targetAddr
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	epMu.Lock()
	ep := currentEP
	var fs *time.Time
	if !firstSeen.IsZero() {
		t := firstSeen
		fs = &t
	}
	hist := make([]Endpoint, len(history))
	copy(hist, history)
	epMu.Unlock()
	nSess := relaySessionCount()
	var servers []string
	for _, s := range currentServers() {
		servers = append(servers, s.String())
	}
	lsk := ""
	if !lastStunOK.IsZero() {
		lsk = lastStunOK.Format(time.RFC3339)
	}
	cfgMu.RLock()
	dcfg := activeCF.DNS
	cfgMu.RUnlock()
	dnsVal := ""
	if ep != nil && dcfg.Enabled {
		dnsVal = recordValue(dcfg, ep.Address)
	}
	v6addr, _, v6ep := currentV6()
	dnsOK, _ := lastDNSOK.Load().(string)
	dnsErr, _ := lastDNSErr.Load().(string)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(statusResp{
		Listen: *listenAddr, Target: targetDisplay(), Servers: servers,
		Current: ep, FirstSeen: fs, LastStunOK: lsk,
		StunOkCount: atomic.LoadInt64(&stunOK), MappingChanges: hist,
		RelaySessions: nSess, RelayPackets: atomic.LoadInt64(&relayPkts),
		RelaySessionList: snapshotRelaySessions(),
		Goroutines:       runtime.NumGoroutine(),
		UptimeSeconds:    int64(time.Since(startedAt).Seconds()),
		UPnP:             upnpSnapshot(),
		V6Enabled:        activeV6.Enabled,
		V6Address:        v6addr,
		V6Endpoint:       v6ep,
		V6Candidates:     v6Candidates(),
		DNSEnabled:       dcfg.Enabled,
		DNSProvider:      dcfg.Provider,
		DNSRecord:        dcfg.Record,
		DNSValue:         dnsVal,
		LastDNSOK:        dnsOK,
		LastDNSErr:       dnsErr,
	})
}

func publishTestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Value string `json:"value"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	loadFileConfig(true)
	cfgMu.RLock()
	d := activeCF.DNS
	cfgMu.RUnlock()
	if !d.Enabled || d.Provider == "" || d.Record == "" {
		writeJSONHTTP(w, 400, map[string]interface{}{"ok": false, "error": "DNS 公告板未启用或未配置 provider/record"})
		return
	}
	value := req.Value
	if value == "" {
		if ep := getEndpoint(); ep != nil {
			value = ep.Address
		}
	}
	if value == "" {
		writeJSONHTTP(w, 400, map[string]interface{}{"ok": false, "error": "还没有可用端点"})
		return
	}
	if err := publishDNS(d, value); err != nil {
		lastDNSErr.Store(err.Error())
		writeJSONHTTP(w, 500, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	lastDNSErr.Store("")
	lastDNSOK.Store(time.Now().Format(time.RFC3339))
	writeJSONHTTP(w, 200, map[string]interface{}{"ok": true, "provider": d.Provider, "record": d.Record, "value": recordValue(d, value)})
}

func writeJSONHTTP(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func startHTTP() {
	if *httpAddr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", statusHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/publish-test", publishTestHandler)
	go func() {
		log.Printf("status http listening on %s", *httpAddr)
		if err := http.ListenAndServe(*httpAddr, mux); err != nil {
			log.Printf("status http error: %v", err)
		}
	}()
}

// ---------- main ----------
func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	target, err := net.ResolveUDPAddr("udp", *targetAddr)
	if err != nil {
		log.Fatalf("resolve target: %v", err)
	}
	for _, s := range strings.Split(*stunServers, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if a, err := net.ResolveUDPAddr("udp", s); err == nil {
			activeServers = append(activeServers, a)
		}
	}
	if len(activeServers) == 0 {
		log.Fatalf("no stun servers")
	}
	loadFileConfig(true)

	laddr, err := net.ResolveUDPAddr("udp", *listenAddr)
	if err != nil {
		log.Fatalf("resolve listen: %v", err)
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("gateway-stun listening on %s, relay -> %s, keepalive=%s, http=%s",
		conn.LocalAddr(), target, *keepalive, *httpAddr)

	if *once {
		tx := newTransactionID()
		req := buildBindingRequest(tx)
		for _, srv := range currentServers() {
			_, _ = conn.WriteToUDP(req, srv)
		}
		buf := make([]byte, 4096)
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			_ = conn.SetReadDeadline(deadline)
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if isSTUN(buf[:n]) && stunTxID(buf[:n]) == hex.EncodeToString(tx) {
				if ip, port, ok := parseMapped(buf[:n]); ok {
					fmt.Printf("mapped=%s\n", net.JoinHostPort(ip.String(), fmt.Sprintf("%d", port)))
					return
				}
			}
		}
		log.Fatalf("no STUN response")
	}

	go relayLoop(conn, target)
	go stunLoop(conn)
	go func() {
		t := time.NewTicker(3 * time.Second)
		lastV6 := ""
		for range t.C {
			loadFileConfig(false)
			// IPv6 前缀变化（v4 端口没变）也要重新发布 TXT
			if _, _, ep6 := currentV6(); ep6 != "" && ep6 != lastV6 {
				lastV6 = ep6
				if cur := getEndpoint(); cur != nil {
					go publish(*cur, "", false)
				}
			}
		}
	}()
	startHTTP()
	startPublicHTTP()
	if *upnpEnabled {
		go upnpLoop()
	}

	go func() {
		t := time.NewTicker(60 * time.Second)
		for range t.C {
			loadFileConfig(false)
			if ep := getEndpoint(); ep != nil {
				log.Printf("heartbeat: endpoint=%s stunOk=%d relayPkts=%d",
					ep.Address, atomic.LoadInt64(&stunOK), atomic.LoadInt64(&relayPkts))
			} else {
				log.Printf("heartbeat: endpoint not discovered yet")
			}
		}
	}()
	select {}
}

// ---------- 打洞端口自检（/punch 页面 + /api/punch） ----------
//
// 用途：从公网/外部网络验证「STUN 打洞得到的公网端口」是否真的映射到本机中继，
// 并由中继转发到 隧道端口。页面显示：
//   - 当前 STUN 映射端点、映射变化次数、STUN 健康
//   - 本机监听端口、转发目标、目标 WG 端口是否在监听
//   - 最近收到的外部 UDP 包（来源 / 大小 / 类型：WG 握手 / WG 数据 / 探测 token / 其它）
//   - 一键复制的“外部探测命令”：收到带 token 的包即证明端口映射正确

type probeRecord struct {
	Time      time.Time `json:"time"`
	Src       string    `json:"src"`
	Size      int       `json:"size"`
	Kind      string    `json:"kind"`
	Token     string    `json:"token,omitempty"`
	Forwarded bool      `json:"forwarded"`
	Target    string    `json:"target,omitempty"`
}

var (
	probeMu     sync.Mutex
	probeRing   []probeRecord
	probeTokens int64
)

func classifyProbe(from *net.UDPAddr, pkt []byte) probeRecord {
	rec := probeRecord{Time: time.Now(), Src: from.String(), Size: len(pkt)}
	if idx := bytes.Index(pkt, []byte("homing-probe:")); idx >= 0 {
		tok := strings.TrimSpace(string(pkt[idx+len("homing-probe:"):]))
		if len(tok) > 40 {
			tok = tok[:40]
		}
		rec.Kind = "token"
		rec.Token = tok
		return rec
	}
	// 隧道 报文类型：1=握手发起(148B) 2=握手响应(92B) 3=cookie(64B) 4=数据(>=32B)
	if len(pkt) >= 16 {
		switch pkt[0] {
		case 1:
			if len(pkt) == 148 {
				rec.Kind = "wg-handshake"
				return rec
			}
		case 2:
			if len(pkt) == 92 {
				rec.Kind = "wg-handshake-reply"
				return rec
			}
		case 3:
			if len(pkt) == 64 {
				rec.Kind = "wg-cookie"
				return rec
			}
		case 4:
			if len(pkt) >= 32 {
				rec.Kind = "wg-data"
				return rec
			}
		}
	}
	rec.Kind = "other"
	return rec
}

func recordProbe(rec probeRecord) {
	probeMu.Lock()
	defer probeMu.Unlock()
	if rec.Kind == "token" && rec.Token != "" {
		atomic.AddInt64(&probeTokens, 1)
	}
	probeRing = append(probeRing, rec)
	if len(probeRing) > 50 {
		probeRing = probeRing[len(probeRing)-50:]
	}
}

func recentProbes() []probeRecord {
	probeMu.Lock()
	defer probeMu.Unlock()
	out := make([]probeRecord, len(probeRing))
	copy(out, probeRing)
	return out
}

// udpPortListening 判断本机某 UDP 端口是否已有 socket 绑定（确认 WG 端口真的在监听）。
func udpPortListening(port int) bool {
	want := fmt.Sprintf(":%04X", port)
	for _, f := range []string{"/proc/net/udp", "/proc/net/udp6"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			local := fields[1]
			if idx := strings.LastIndex(local, ":"); idx >= 0 && strings.EqualFold(local[idx:], want) {
				return true
			}
		}
	}
	return false
}

type punchStatus struct {
	Endpoint        string        `json:"endpoint"`
	EndpointUpdated string        `json:"endpointUpdated"`
	StunOK          int64         `json:"stunOk"`
	MappingChanges  int           `json:"mappingChanges"`
	Listen          string        `json:"listen"`
	Target          string        `json:"target"`
	TargetPort      int           `json:"targetPort"`
	TargetListening bool          `json:"targetListening"`
	RelayPackets    int64         `json:"relayPackets"`
	RelaySessions   int           `json:"relaySessions"`
	TokenProbes     int64         `json:"tokenProbes"`
	Probes          []probeRecord `json:"probes"`
	UPnP            upnpState     `json:"upnp"`
	Now             string        `json:"now"`
}

func punchAPIHandler(w http.ResponseWriter, r *http.Request) {
	epMu.Lock()
	ep := currentEP
	changes := len(history)
	epMu.Unlock()
	sessMu.Lock()
	nSess := len(sessions)
	sessMu.Unlock()
	port := targetPort()
	st := punchStatus{
		Listen:          *listenAddr,
		Target:          targetDisplay(),
		TargetPort:      port,
		TargetListening: udpPortListening(port),
		RelayPackets:    atomic.LoadInt64(&relayPkts),
		RelaySessions:   nSess,
		StunOK:          atomic.LoadInt64(&stunOK),
		MappingChanges:  changes,
		TokenProbes:     atomic.LoadInt64(&probeTokens),
		Probes:          recentProbes(),
		UPnP:            upnpSnapshot(),
		Now:             time.Now().Format("2006-01-02 15:04:05"),
	}
	if ep != nil {
		st.Endpoint = ep.Address
		st.EndpointUpdated = ep.UpdatedAt.Format("2006-01-02 15:04:05")
	}
	writeJSONHTTP(w, 200, st)
}

func startPublicHTTP() {
	if *publicHTTP == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/punch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(punchPageHTML))
	})
	mux.HandleFunc("/api/punch", punchAPIHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/punch", http.StatusFound)
	})
	go func() {
		log.Printf("punch self-test page listening on %s", *publicHTTP)
		if err := http.ListenAndServe(*publicHTTP, mux); err != nil {
			log.Printf("punch http error: %v", err)
		}
	}()
}

const punchPageHTML = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>打洞端口自检 · 归途 Homing</title>
<style>
:root{color-scheme:dark}
body{margin:0;font:14px/1.6 -apple-system,"Segoe UI",Roboto,"Noto Sans CJK SC",sans-serif;background:#0f1720;color:#e6edf3}
.wrap{max-width:1000px;margin:0 auto;padding:20px 16px 48px}
h1{font-size:20px;margin:0 0 4px}
h3{font-size:15px;margin:22px 0 8px}
.sub{color:#8b98a5;font-size:12px;margin-bottom:16px}
.verdict{border-radius:10px;padding:14px 16px;margin:0 0 16px;border:1px solid #2a3a4a;background:#161f2b}
.v-ok{background:#0f2d1c;border-color:#2ea04355}
.v-warn{background:#33260d;border-color:#d2992255}
.v-bad{background:#33161a;border-color:#f8514955}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:12px}
.card{background:#161f2b;border:1px solid #232f3d;border-radius:10px;padding:12px 14px}
.card .k{color:#8b98a5;font-size:12px}
.card .v{font-size:16px;font-weight:600;word-break:break-all}
.ok{color:#3fb950}.bad{color:#f85149}.warn{color:#d29922}
table{width:100%;border-collapse:collapse;font-size:12.5px}
th,td{text-align:left;padding:6px 8px;border-bottom:1px solid #232f3d}
th{color:#8b98a5;font-weight:500}
code,pre{font-family:ui-monospace,Consolas,monospace}
pre{background:#0b1219;border:1px solid #232f3d;border-radius:8px;padding:12px;overflow:auto;font-size:12.5px;margin:6px 0}
.tag{display:inline-block;padding:1px 7px;border-radius:99px;font-size:11px;border:1px solid #2a3a4a}
.t-wg{background:#0f2d1c;border-color:#2ea04355;color:#3fb950}
.t-tok{background:#1b2a3a;border-color:#388bfd55;color:#58a6ff}
.t-other{background:#222a33;color:#8b98a5}
.muted{color:#8b98a5}
</style></head>
<body><div class="wrap">
<h1>打洞端口自检</h1>
<div class="sub">验证「STUN 打洞得到的公网端口 → 本机中继 → 隧道端口」这条链路是否真的通</div>
<div id="verdict" class="verdict">正在读取网关状态…</div>
<div class="grid">
  <div class="card"><div class="k">打洞端点（STUN 映射）</div><div class="v" id="ep">-</div><div class="k" id="epu"></div></div>
  <div class="card"><div class="k">本机中继监听</div><div class="v" id="listen">-</div><div class="k">转发目标 <span id="target">-</span></div></div>
  <div class="card"><div class="k">隧道端口</div><div class="v" id="wg">-</div><div class="k" id="wgk"></div></div>
  <div class="card"><div class="k">STUN 保活 / 映射变化</div><div class="v" id="stun">-</div><div class="k" id="stunk"></div></div>
  <div class="card"><div class="k">UPnP 自动映射</div><div class="v" id="upnp">-</div><div class="k" id="upnpk"></div></div>
  <div class="card"><div class="k">中继统计</div><div class="v" id="relay">-</div><div class="k" id="relayk"></div></div>
  <div class="card"><div class="k">服务器时间</div><div class="v" id="now">-</div><div class="k">每 2 秒自动刷新</div></div>
</div>

<h3>① 从「另一个网络」发一个探测包</h3>
<div class="muted">必须在别的网络执行（手机热点 / 另一条宽带 / 云主机）。同一条宽带访问自己的公网 IP 需要 NAT hairpin，测不出来。</div>
<div class="card" style="margin-top:8px">
  <div class="k">本次探测 token：<b id="tok" style="color:#58a6ff"></b></div>
  <div class="k" style="margin-top:8px">Linux / macOS / UOS：</div>
  <pre id="cmdlinux"></pre>
  <div class="k">Windows PowerShell：</div>
  <pre id="cmdwin"></pre>
  <div class="muted">发完等 2~3 秒：出现绿色结论就说明「公网端口 → 中继 → 隧道端口」这条路是通的。</div>
</div>

<h3>② 最近收到的外部 UDP 包</h3>
<table><thead><tr><th>时间</th><th>来源</th><th>类型</th><th>大小</th><th>转发到隧道</th></tr></thead>
<tbody id="rows"><tr><td colspan="5" class="muted">还没有收到任何外部 UDP 包</td></tr></tbody></table>

<div class="muted" style="margin-top:20px">
说明：<br>
· <b>隧道握手</b> = 148 字节、首字节 0x01，是隧道客户端握手包；收到它说明链路上已经有真实客户端在连。<br>
· <b>探测 token</b> = 你从外部网络用上面的命令发的包；收到即证明端口映射正确。<br>
· 一直收不到：公网端口到本机的入站 UDP 被 NAT/防火墙挡了（双重 NAT、CGNAT、运营商过滤都可能导致），需要改用 IPv6 直连或公网中转。
</div>
</div>
<script>
var TOKEN='p'+Math.random().toString(36).slice(2,10);
document.getElementById('tok').textContent=TOKEN;
function fmtAge(s){if(s<60)return s+' 秒前';if(s<3600)return Math.floor(s/60)+' 分钟前';if(s<86400)return Math.floor(s/3600)+' 小时前';return Math.floor(s/86400)+' 天前';}
function render(d){
  var ep=d.endpoint||'-';
  document.getElementById('ep').textContent=ep;
  document.getElementById('epu').textContent=d.endpointUpdated?('更新于 '+d.endpointUpdated):'尚未发现';
  document.getElementById('listen').textContent=d.listen||'-';
  document.getElementById('target').textContent=d.target||'-';
  var wgEl=document.getElementById('wg');
  wgEl.textContent=d.targetPort?('UDP '+d.targetPort):'-';
  wgEl.className='v '+(d.targetListening?'ok':'bad');
  document.getElementById('wgk').textContent=d.targetListening?'端口在监听 ✓':'端口未监听 ✗（隧道核心没起来？）';
  document.getElementById('stun').textContent=d.stunOk+' 次';
  document.getElementById('stunk').textContent='映射变化 '+d.mappingChanges+' 次';
  var u=d.upnp||{};
  var upEl=document.getElementById('upnp');
  if(u.enabled&&u.externalPort){
    upEl.textContent=u.externalPort+'/UDP → '+u.internalIp+':'+u.internalPort;
    upEl.className='v ok';
    document.getElementById('upnpk').textContent='路由器 '+(u.igd||'-')+' | WAN IP '+(u.externalIp||'-');
  }else{
    upEl.textContent=u.enabled?'未建立':'未启用';
    upEl.className='v '+(u.enabled?'warn':'');
    document.getElementById('upnpk').textContent=u.lastErr||'启动参数加 -upnp 可自动映射';
  }
  document.getElementById('relay').textContent=d.relayPackets+' 包';
  document.getElementById('relayk').textContent='活跃会话 '+d.relaySessions;
  document.getElementById('now').textContent=d.now;
  var host=ep,port='-';
  if(ep.lastIndexOf(':')>0){port=ep.slice(ep.lastIndexOf(':')+1);host=ep.slice(0,ep.lastIndexOf(':'));}
  document.getElementById('cmdlinux').textContent="python3 -c 'import socket;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.sendto(b\"homing-probe:"+TOKEN+"\",(\""+host+"\","+port+"));print(\"sent\")'";
  document.getElementById('cmdwin').textContent='$u=New-Object System.Net.Sockets.UdpClient; $b=[Text.Encoding]::ASCII.GetBytes("homing-probe:'+TOKEN+'"); $u.Send($b,$b.Length,"'+host+'",'+port+') | Out-Null; "sent"';
  var probes=d.probes||[];
  var rows='',hasTok=false,hasWg=false,recent=0,nowMs=Date.now();
  for(var i=probes.length-1;i>=0;i--){
    var p=probes[i];
    var age=Math.max(0,Math.floor((nowMs-new Date(p.time).getTime())/1000));
    if(age<300)recent++;
    if(p.kind==='token'&&p.token===TOKEN)hasTok=true;
    if(p.kind.indexOf('wg-')===0&&age<300)hasWg=true;
    var cls=p.kind.indexOf('wg-')===0?'t-wg':(p.kind==='token'?'t-tok':'t-other');
    var mark=(p.kind==='token'&&p.token===TOKEN)?' ★本页 token':'';
    rows+='<tr><td class="muted">'+fmtAge(age)+'</td><td><code>'+p.src+'</code></td>'+
          '<td><span class="tag '+cls+'">'+p.kind+mark+'</span></td><td>'+p.size+' B</td>'+
          '<td>'+(p.forwarded?'<span class="ok">✓</span>':'<span class="bad">✗</span>')+'</td></tr>';
  }
  document.getElementById('rows').innerHTML=rows||'<tr><td colspan="5" class="muted">还没有收到任何外部 UDP 包</td></tr>';
  var v=document.getElementById('verdict');
  if(hasTok){v.className='verdict v-ok';v.innerHTML='<b>✅ 端口映射正确</b>：已收到你从外部网络发来的探测包（token 匹配），并转发到了隧道端口 '+d.targetPort+'。打洞链路是通的。';}
  else if(hasWg){v.className='verdict v-ok';v.innerHTML='<b>✅ 链路在工作</b>：最近 5 分钟收到过真实的隧道握手包（来自外部网络），端口映射正常。';}
  else if(recent>0){v.className='verdict v-warn';v.innerHTML='<b>⚠ 收到了 UDP 包，但不是隧道握手、也不含本页 token</b>：可能是别的客户端或扫描。用上面的命令再发一次。';}
  else{v.className='verdict v-bad';v.innerHTML='<b>❌ 最近 5 分钟没有收到任何外部 UDP 包</b>：公网端口到本机的入站 UDP 大概率被 NAT/防火墙挡了（双重 NAT、CGNAT、运营商过滤都可能）。建议改用 IPv6 直连或公网中转。';}
}
function tick(){
  fetch('/api/punch',{cache:'no-store'}).then(function(r){return r.json();}).then(render).catch(function(e){
    var v=document.getElementById('verdict');v.className='verdict v-bad';v.textContent='读取网关状态失败：'+e;
  });
}
tick();setInterval(tick,2000);
</script>
</body></html>`
