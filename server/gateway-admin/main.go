// gateway-admin: 自研服务端管理页
//
// 功能：
//   - 展示自研栈状态：gateway-stun 当前打洞端点 / STUN 保活健康 / 映射变化历史 / 中继统计
//   - 客户端管理（经由 隧道核心 API）：列表、新建、启停、删除、下载配置、显示二维码
//   - 端点配置：把当前打洞端点一键写入客户端默认 Endpoint（隧道核心 userconfig）
//   - STUN 运行配置：热写 /stun-data/config.json（STUN 服务器、ntfy、Cloudflare TXT）
//
// 仅监听内网/隧道口，自带走简易登录（ADMIN_PASSWORD）。
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed ui.html
var uiFS embed.FS

// ---------- 配置 ----------
var (
	listenAddr = getenv("ADMIN_LISTEN", ":51822")
	coreBase   = getenv("CORE_BASE", "http://127.0.0.1:51821")
	stunBase   = getenv("STUN_STATUS", "http://127.0.0.1:51831")
	dataDir    = getenv("DATA_DIR", "/data")
	adminPass  = os.Getenv("ADMIN_PASSWORD")
	coreUser   = getenv("CORE_ADMIN_USER", "")
	corePass   = os.Getenv("CORE_ADMIN_PASSWORD")
	// allInOne：单容器模式（docker-compose.single.yml）。此时管理台不再挂
	// docker.sock，所有“启停/重建容器”的动作改为写文件 + 内置 runner 生效。
	allInOne = os.Getenv("HOMING_MODE") == "all"
	// vpnDir 是容器内可见的部署目录（compose 把仓库目录挂到 /vpn）；
	// 宿主机上的真实路径由 detectVPNHostDir 通过 compose 标签自动探测。
	vpnDir     = getenv("VPN_DIR", "/vpn")
	vpnHostDir = detectVPNHostDir()
	// 统一的数据目录（容器内 /data）：core.json、admin.json、stun/、egress 等都在这里。
	settingsPath = dataDir + "/settings.json"
	stunCfgPath  = dataDir + "/stun/config.json"
	endpointPath = dataDir + "/stun/endpoint.json"
	// 客户端探针默认参数（管理台设置 → 写进新生成的客户端配置的 probe 字段）
	clientProbePath = dataDir + "/client-probe.json"
	// 连接方式 / 传输优先级（STUN 开关 + 公网直连地址 + v4/v6 优先级）
	connectCfgPath = dataDir + "/connect.json"
	// 可选 mihomo 出口：管理台写配置，homing-egress 读取并应用策略路由
	egressCfgPath    = dataDir + "/egress.json"
	egressEnvPath    = dataDir + "/egress.env"
	egressStatusPath = dataDir + "/egress-status.json"
	// 代理出口的客户端列表（每行一个隧道 IPv4）；plumber 每轮读取
	egressClientsPath = dataDir + "/egress-clients"
	// 远程代理模式：管理台生成本机 mihomo 配置，homing-proxy 以 host 网络启动
	proxyDir        = dataDir + "/proxy"
	proxyConfigPath = proxyDir + "/config.yaml"
	proxyStatusPath = proxyDir + "/status.json"
	// 单容器模式：隧道监听端口由管理台写入，stun runner 读取后决定 -target
	corePortPath = dataDir + "/core-port"
)

// systemSettings 是管理台维护的服务端默认值；环境变量仍然优先。
type systemSettings struct {
	DNS       string `json:"dns"`       // 客户端 DNS，逗号分隔；空=自动探测默认网关
	LAN       string `json:"lan"`       // 家庭/办公室局域网网段，逗号分隔；空=自动探测
	ClientNet string `json:"clientNet"` // 隧道客户端网段；空=跟随隧道核心
	StunPort  int    `json:"stunPort"`  // 打洞端口；0=随机
	UpdatedAt int64  `json:"updatedAt,omitempty"`
}

var (
	sysMu  sync.RWMutex
	sysCfg systemSettings
)

func detectVPNHostDir() string {
	// 单容器模式没有 compose 部署目录，也没有 docker.sock，直接跳过探测。
	if allInOne {
		return ""
	}
	// VPN_DIR 是容器内路径；宿主机路径优先用显式覆盖，其次从 compose 标签/挂载自动探测。
	if v := strings.TrimSpace(os.Getenv("VPN_HOST_DIR")); v != "" {
		return v
	}
	host, err := os.Hostname()
	if err == nil {
		out, err := exec.Command("docker", "inspect", host, "--format",
			`{{index .Config.Labels "com.docker.compose.project.working_dir"}}`).Output()
		if err == nil {
			if s := strings.TrimSpace(string(out)); s != "" {
				return s
			}
		}
		// 回退：从容器挂载里找 /vpn 的宿主机来源
		out, err = exec.Command("docker", "inspect", host, "--format",
			`{{range .Mounts}}{{.Destination}}={{.Source}} {{end}}`).Output()
		if err == nil {
			for _, f := range strings.Fields(string(out)) {
				if strings.HasPrefix(f, "/vpn=") {
					if s := strings.TrimPrefix(f, "/vpn="); s != "" {
						return s
					}
				}
			}
		}
	}
	return ""
}

// composeArgs 返回容器内可用的 docker compose 命令；compose 文件通过 /vpn 挂载，
// 数据目录则把宿主机绝对路径传给 daemon（容器内相对路径不能直接用）。
func composeArgs(args ...string) ([]string, string, []string) {
	dir := vpnDir
	hostDir := vpnHostDir
	if hostDir == "" {
		hostDir = dir
	}
	cmdArgs := append([]string{"compose", "--project-directory", dir, "-f", dir + "/docker-compose.yml"}, args...)
	env := append(os.Environ(), "HOMING_DATA_DIR="+hostDir+"/data")
	return cmdArgs, dir, env
}

func runCompose(args ...string) (string, error) {
	if allInOne {
		return "", fmt.Errorf("单容器模式不使用 docker compose，相关操作由内置 runner 完成")
	}
	if strings.TrimSpace(vpnHostDir) == "" {
		return "", fmt.Errorf("无法自动探测宿主机部署目录，请给 homing-admin 设置 VPN_HOST_DIR")
	}
	// 管理台只负责启停已有镜像，不让 compose 触发构建。
	if len(args) > 0 && args[0] == "up" {
		args = append([]string{"up", "--no-build"}, args[1:]...)
	}
	cmdArgs, dir, env := composeArgs(args...)
	cmd := exec.Command("docker", cmdArgs...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---------- 系统默认值（自动探测 + 管理台可改，环境变量优先） ----------

func loadSystemSettings() {
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		return
	}
	var s systemSettings
	if json.Unmarshal(b, &s) == nil {
		sysMu.Lock()
		sysCfg = s
		sysMu.Unlock()
	}
}

func saveSystemSettings(s systemSettings) error {
	s.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s, "", "  ")
	_ = os.MkdirAll(dataDir, 0o755)
	tmp := settingsPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, settingsPath); err != nil {
		return err
	}
	sysMu.Lock()
	sysCfg = s
	sysMu.Unlock()
	return nil
}

func currentSystemSettings() systemSettings {
	sysMu.RLock()
	defer sysMu.RUnlock()
	return sysCfg
}

// detectGateway 从 /proc/net/route 读默认路由网关（host 网络下可用）。
func detectGateway() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil {
			continue
		}
		return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)).String()
	}
	return ""
}

// detectLAN 返回默认路由网卡上的 IPv4 网段（如 192.168.9.0/24）。
func detectLAN() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	ifaceName := ""
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == "00000000" {
			ifaceName = f[0]
			break
		}
	}
	if ifaceName == "" {
		return ""
	}
	ifi, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return ""
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil {
			continue
		}
		return (&net.IPNet{IP: ipn.IP.Mask(ipn.Mask), Mask: ipn.Mask}).String()
	}
	return ""
}

func envDNS() string { return strings.TrimSpace(os.Getenv("CORE_DNS")) }
func envLAN() string { return strings.TrimSpace(os.Getenv("CORE_LAN")) }
func envNet() string { return strings.TrimSpace(os.Getenv("CLIENT_NET")) }

func effectiveDNS() []string {
	if v := envDNS(); v != "" {
		return splitCSV(v)
	}
	if v := strings.TrimSpace(currentSystemSettings().DNS); v != "" {
		return splitCSV(v)
	}
	if gw := detectGateway(); gw != "" {
		return []string{gw}
	}
	return []string{"223.5.5.5"}
}

func effectiveDNSString() string { return strings.Join(effectiveDNS(), ",") }

func effectiveLAN() string {
	if v := envLAN(); v != "" {
		return normalizeCIDRList(v)
	}
	if v := strings.TrimSpace(currentSystemSettings().LAN); v != "" {
		return normalizeCIDRList(v)
	}
	return normalizeCIDRList(detectLAN())
}

func coreInterfaceCIDR() string {
	if code, b, err := coreDo("GET", "/api/admin/interface", nil); err == nil && code == 200 {
		var iface map[string]interface{}
		if json.Unmarshal(b, &iface) == nil {
			if s, _ := iface["ipv4Cidr"].(string); s != "" {
				return s
			}
		}
	}
	return ""
}

func effectiveClientNet() string {
	if v := envNet(); v != "" {
		return v
	}
	if v := strings.TrimSpace(currentSystemSettings().ClientNet); v != "" {
		return v
	}
	if v := coreInterfaceCIDR(); v != "" {
		return v
	}
	return "10.8.0.0/24"
}

func settingsSources() map[string]string {
	src := map[string]string{}
	if envDNS() != "" {
		src["dns"] = "env"
	} else if strings.TrimSpace(currentSystemSettings().DNS) != "" {
		src["dns"] = "settings"
	} else if detectGateway() != "" {
		src["dns"] = "auto"
	} else {
		src["dns"] = "default"
	}
	if envLAN() != "" {
		src["lan"] = "env"
	} else if strings.TrimSpace(currentSystemSettings().LAN) != "" {
		src["lan"] = "settings"
	} else if detectLAN() != "" {
		src["lan"] = "auto"
	} else {
		src["lan"] = "default"
	}
	if envNet() != "" {
		src["clientNet"] = "env"
	} else if strings.TrimSpace(currentSystemSettings().ClientNet) != "" {
		src["clientNet"] = "settings"
	} else {
		src["clientNet"] = "core"
	}
	return src
}

func defaultPasswordActive() bool {
	accMu.Lock()
	a := acct
	accMu.Unlock()
	return a != nil && verifyPassword("admin123", a.PasswordHash)
}

func validDNSList(s string) error {
	for _, p := range splitCSV(s) {
		if net.ParseIP(p) == nil {
			return fmt.Errorf("DNS 地址不合法：%s", p)
		}
	}
	return nil
}

func validLANList(s string) error {
	for _, c := range splitCSV(s) {
		if _, _, err := net.ParseCIDR(c); err != nil {
			return fmt.Errorf("局域网网段不合法：%s", c)
		}
	}
	return nil
}

func validClientNet(s string) error {
	ip, n, err := net.ParseCIDR(s)
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("隧道网段不合法：%s", s)
	}
	ones, _ := n.Mask.Size()
	if ones < 16 || ones > 30 {
		return fmt.Errorf("隧道网段前缀只支持 /16~/30：%s", s)
	}
	return nil
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// ---------- 会话 ----------
var (
	sessMu sync.Mutex
	sess   = map[string]time.Time{}
)

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func authOK(r *http.Request) bool {
	c, err := r.Cookie("gwadmin")
	if err != nil {
		return false
	}
	sessMu.Lock()
	exp, ok := sess[c.Value]
	sessMu.Unlock()
	return ok && time.Now().Before(exp)
}

func requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			writeJSON(w, 401, map[string]interface{}{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}

// ---------- 账户 / 密码 / TOTP 2FA ----------
type account struct {
	Username       string    `json:"username"`
	PasswordHash   string    `json:"passwordHash"`
	TOTPSecret     string    `json:"totpSecret,omitempty"`
	TOTPEnabled    bool      `json:"totpEnabled"`
	RecoveryHashes []string  `json:"recoveryHashes,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

var (
	accMu       sync.Mutex
	acct        *account
	acctPath    = dataDir + "/admin.json"
	totpPending = map[string]string{} // 会话 token -> 待确认的 2FA secret
	tickets     = map[string]time.Time{}
)

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	prf.Write(salt)
	prf.Write([]byte{0, 0, 0, 1})
	u := prf.Sum(nil)
	out := make([]byte, 0, keyLen)
	cur := make([]byte, len(u))
	copy(cur, u)
	for len(out) < keyLen {
		need := keyLen - len(out)
		if need > len(cur) {
			need = len(cur)
		}
		out = append(out, cur[:need]...)
		prf.Reset()
		prf.Write(cur)
		u = prf.Sum(nil)
		copy(cur, u)
	}
	return out
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	dk := pbkdf2SHA256([]byte(pw), salt, 120000, 32)
	return fmt.Sprintf("pbkdf2$120000$%s$%s", hex.EncodeToString(salt), hex.EncodeToString(dk))
}

func verifyPassword(pw, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iters, err := strconv.Atoi(parts[1])
	if err != nil || iters <= 0 {
		return false
	}
	salt, e1 := hex.DecodeString(parts[2])
	want, e2 := hex.DecodeString(parts[3])
	if e1 != nil || e2 != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iters, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func saveAccountLocked() {
	if acct == nil {
		return
	}
	acct.UpdatedAt = time.Now()
	_ = os.MkdirAll(dataDir, 0o755)
	b, _ := json.MarshalIndent(acct, "", "  ")
	_ = os.WriteFile(acctPath, b, 0o600)
}

func loadAccount() {
	accMu.Lock()
	defer accMu.Unlock()
	if b, err := os.ReadFile(acctPath); err == nil {
		var a account
		if json.Unmarshal(b, &a) == nil && a.PasswordHash != "" {
			acct = &a
			return
		}
	}
	acct = &account{
		Username:     getenv("ADMIN_USERNAME", "admin"),
		PasswordHash: hashPassword(adminPass),
		UpdatedAt:    time.Now(),
	}
	saveAccountLocked()
	log.Printf("account bootstrapped from ADMIN_PASSWORD (user=%s)", acct.Username)
}

func newTicket() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	t := hex.EncodeToString(b)
	sessMu.Lock()
	tickets[t] = time.Now().Add(5 * time.Minute)
	sessMu.Unlock()
	return t
}

func takeTicket(t string) bool {
	sessMu.Lock()
	defer sessMu.Unlock()
	exp, ok := tickets[t]
	if !ok || time.Now().After(exp) {
		delete(tickets, t)
		return false
	}
	delete(tickets, t)
	return true
}

func issueSession(w http.ResponseWriter) string {
	tok := newToken()
	sessMu.Lock()
	sess[tok] = time.Now().Add(24 * time.Hour)
	sessMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "gwadmin", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 86400})
	return tok
}

func clearSessions() {
	sessMu.Lock()
	sess = map[string]time.Time{}
	sessMu.Unlock()
}

// ---- TOTP（RFC 6238，SHA1 / 30s / 6 位） ----
func generateTOTPSecret() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}

func totpAt(secret string, t time.Time) string {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return ""
	}
	counter := uint64(t.Unix() / 30)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	return fmt.Sprintf("%06d", code%1000000)
}

func verifyTOTP(secret, code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 || secret == "" {
		return false
	}
	now := time.Now()
	for _, d := range []time.Duration{0, -30 * time.Second, 30 * time.Second} {
		if subtle.ConstantTimeCompare([]byte(totpAt(secret, now.Add(d))), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

func otpauthURI(user, secret string) string {
	label := "归途 Homing:" + user
	return fmt.Sprintf("otpauth://totp/%s?secret=%s&issuer=WG%%20Gateway&digits=6&period=30",
		strings.ReplaceAll(label, ":", "%3A"), secret)
}

func genRecoveryCodes(n int) ([]string, []string) {
	plain := make([]string, 0, n)
	hashes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		b := make([]byte, 5)
		_, _ = rand.Read(b)
		c := strings.ToUpper(hex.EncodeToString(b))
		code := c[0:5] + "-" + c[5:10]
		plain = append(plain, code)
		h := sha256.Sum256([]byte(code))
		hashes = append(hashes, hex.EncodeToString(h[:]))
	}
	return plain, hashes
}

func accountView() map[string]interface{} {
	accMu.Lock()
	defer accMu.Unlock()
	if acct == nil {
		return map[string]interface{}{}
	}
	return map[string]interface{}{
		"username":          acct.Username,
		"totpEnabled":       acct.TOTPEnabled,
		"recoveryCodesLeft": len(acct.RecoveryHashes),
		"updatedAt":         acct.UpdatedAt,
		"file":              acctPath,
	}
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------- 隧道核心 API 客户端 ----------
var (
	wgMu       sync.Mutex
	coreCookie string
)

func wgLogin() error {
	wgMu.Lock()
	defer wgMu.Unlock()
	body := fmt.Sprintf(`{"username":%q,"password":%q,"remember":true}`, coreUser, corePass)
	req, _ := http.NewRequest("POST", coreBase+"/api/auth/password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("wg login status %d: %s", resp.StatusCode, string(b))
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "homing" || ck.Name == "core" {
			coreCookie = ck.Name + "=" + ck.Value
		}
	}
	if coreCookie == "" {
		return fmt.Errorf("wg login: no cookie")
	}
	return nil
}

func coreDo(method, path string, body []byte) (int, []byte, error) {
	wgMu.Lock()
	ck := coreCookie
	wgMu.Unlock()
	if ck == "" {
		if err := wgLogin(); err != nil {
			return 0, nil, err
		}
		wgMu.Lock()
		ck = coreCookie
		wgMu.Unlock()
	}
	req, _ := http.NewRequest(method, coreBase+path, strings.NewReader(string(body)))
	req.Header.Set("Cookie", ck)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 401 {
		wgMu.Lock()
		coreCookie = ""
		wgMu.Unlock()
		if err := wgLogin(); err != nil {
			return resp.StatusCode, b, err
		}
		return coreDo(method, path, body)
	}
	return resp.StatusCode, b, nil
}

// ---------- STUN 状态 ----------
func fetchStunStatus() (map[string]interface{}, error) {
	resp, err := httpClient.Get(stunBase + "/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

func readEndpointFile() (map[string]interface{}, error) {
	b, err := os.ReadFile(endpointPath)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func currentPunched() (string, string, int) {
	st, err := fetchStunStatus()
	if err != nil {
		return "", "", 0
	}
	cur, _ := st["current"].(map[string]interface{})
	if cur == nil {
		return "", "", 0
	}
	ip, _ := cur["ip"].(string)
	portF, _ := cur["port"].(float64)
	addr, _ := cur["address"].(string)
	return ip, addr, int(portF)
}

// ---------- 客户端管理 ----------
type clientView struct {
	ID                  int      `json:"id"`
	Name                string   `json:"name"`
	Ipv4Address         string   `json:"ipv4Address"`
	Ipv6Address         string   `json:"ipv6Address"`
	Enabled             bool     `json:"enabled"`
	LatestHandshakeAt   string   `json:"latestHandshakeAt"`
	Endpoint            string   `json:"endpoint"`
	PublicEndpoint      string   `json:"publicEndpoint,omitempty"`
	ViaStun             bool     `json:"viaStun,omitempty"`
	TransferRx          int64    `json:"transferRx"`
	TransferTx          int64    `json:"transferTx"`
	PersistentKeepalive int      `json:"persistentKeepalive"`
	Dns                 []string `json:"dns"`
	AllowedIps          []string `json:"allowedIps"`
	ProxyEgress         bool     `json:"proxyEgress"`
	ProxySelfService    bool     `json:"proxySelfService"`
}

func listClients() ([]map[string]interface{}, error) {
	code, b, err := coreDo("GET", "/api/client", nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("wg list status %d: %s", code, string(b))
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

func getClient(id int) (map[string]interface{}, error) {
	arr, err := listClients()
	if err != nil {
		return nil, err
	}
	for _, c := range arr {
		if int(c["id"].(float64)) == id {
			return c, nil
		}
	}
	return nil, fmt.Errorf("client %d not found", id)
}

// buildUpdateBody 按 隧道核心校验要求构造完整更新体
func buildUpdateBody(c map[string]interface{}) map[string]interface{} {
	get := func(k string) interface{} { return c[k] }
	body := map[string]interface{}{
		"name": get("name"), "enabled": get("enabled"), "expiresAt": get("expiresAt"),
		"ipv4Address": get("ipv4Address"), "ipv6Address": get("ipv6Address"),
		"preUp": get("preUp"), "postUp": get("postUp"), "preDown": get("preDown"), "postDown": get("postDown"),
		"allowedIps": get("allowedIps"), "serverAllowedIps": get("serverAllowedIps"),
		"firewallIps": get("firewallIps"), "mtu": get("mtu"),
		"jC": get("jC"), "jMin": get("jMin"), "jMax": get("jMax"),
		"i1": get("i1"), "i2": get("i2"), "i3": get("i3"), "i4": get("i4"), "i5": get("i5"),
		"contentPaddingAddition": get("contentPaddingAddition"),
		"rekeyAfterTime":         get("rekeyAfterTime"), "rekeyTimeout": get("rekeyTimeout"),
		"rejectAfterTime": get("rejectAfterTime"), "keepaliveTimeout": get("keepaliveTimeout"),
		"maxHandshakeAttempts": get("maxHandshakeAttempts"), "disableCookies": get("disableCookies"),
		"persistentKeepalive": get("persistentKeepalive"),
		"serverEndpoint":      get("serverEndpoint"), "dns": get("dns"),
		"proxyEgress": get("proxyEgress"), "proxySelfService": get("proxySelfService"),
	}
	return body
}

func updateClient(id int, mutate func(map[string]interface{})) (int, []byte, error) {
	c, err := getClient(id)
	if err != nil {
		return 0, nil, err
	}
	body := buildUpdateBody(c)
	if mutate != nil {
		mutate(body)
	}
	b, _ := json.Marshal(body)
	return coreDo("POST", fmt.Sprintf("/api/client/%d", id), b)
}

func createClient(name, mode string) (int, error) {
	body := fmt.Sprintf(`{"name":%q,"expiresAt":null}`, name)
	code, b, err := coreDo("POST", "/api/client", []byte(body))
	if err != nil {
		return 0, err
	}
	if code != 200 {
		return 0, fmt.Errorf("create status %d: %s", code, string(b))
	}
	var r struct {
		ClientID int `json:"clientId"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, err
	}
	allowed := []string{"0.0.0.0/0", "::/0"}
	if mode == "lan" {
		allowed = append([]string{effectiveClientNet()}, splitCSV(effectiveLAN())...)
	}
	code, ub, err := updateClient(r.ClientID, func(m map[string]interface{}) {
		m["allowedIps"] = allowed
		m["serverAllowedIps"] = []string{}
		m["dns"] = effectiveDNS()
		m["persistentKeepalive"] = 25
	})
	if err != nil {
		return r.ClientID, err
	}
	if code != 200 {
		return r.ClientID, fmt.Errorf("patch status %d: %s", code, string(ub))
	}
	return r.ClientID, nil
}

// ---------- HTTP 路由 ----------
func handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	accMu.Lock()
	a := acct
	accMu.Unlock()
	if a == nil {
		writeJSON(w, 500, map[string]string{"error": "account not initialized"})
		return
	}
	if req.Username != "" && req.Username != a.Username {
		writeJSON(w, 401, map[string]string{"error": "用户名或密码错误"})
		return
	}
	if !verifyPassword(req.Password, a.PasswordHash) {
		writeJSON(w, 401, map[string]string{"error": "用户名或密码错误"})
		return
	}
	if a.TOTPEnabled {
		writeJSON(w, 200, map[string]interface{}{"need2fa": true, "ticket": newTicket(), "username": a.Username})
		return
	}
	issueSession(w)
	writeJSON(w, 200, map[string]interface{}{"ok": true, "username": a.Username})
}

func handleLogin2FA(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ticket string `json:"ticket"`
		Code   string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	if !takeTicket(req.Ticket) {
		writeJSON(w, 401, map[string]string{"error": "会话已过期，请重新登录"})
		return
	}
	code := strings.TrimSpace(strings.ToUpper(req.Code))
	ok := false
	accMu.Lock()
	a := acct
	if a != nil {
		if verifyTOTP(a.TOTPSecret, code) {
			ok = true
		} else {
			h := sha256.Sum256([]byte(code))
			hh := hex.EncodeToString(h[:])
			for i, rh := range a.RecoveryHashes {
				if subtle.ConstantTimeCompare([]byte(rh), []byte(hh)) == 1 {
					a.RecoveryHashes = append(a.RecoveryHashes[:i], a.RecoveryHashes[i+1:]...)
					ok = true
					saveAccountLocked()
					break
				}
			}
		}
	}
	accMu.Unlock()
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "验证码错误"})
		return
	}
	issueSession(w)
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func handleAccount(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, accountView())
}

func handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if len(req.New) < 8 {
		writeJSON(w, 400, map[string]string{"error": "新密码至少 8 位"})
		return
	}
	accMu.Lock()
	a := acct
	if a == nil || !verifyPassword(req.Current, a.PasswordHash) {
		accMu.Unlock()
		writeJSON(w, 403, map[string]string{"error": "当前密码错误"})
		return
	}
	a.PasswordHash = hashPassword(req.New)
	saveAccountLocked()
	accMu.Unlock()
	clearSessions()
	issueSession(w)
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("gwadmin")
	if err != nil {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	accMu.Lock()
	a := acct
	accMu.Unlock()
	if a == nil {
		writeJSON(w, 500, map[string]string{"error": "not initialized"})
		return
	}
	secret := generateTOTPSecret()
	sessMu.Lock()
	totpPending[c.Value] = secret
	sessMu.Unlock()
	writeJSON(w, 200, map[string]interface{}{
		"secret": secret,
		"uri":    otpauthURI(a.Username, secret),
	})
}

func handleTOTPQR(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("gwadmin")
	if err != nil {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	sessMu.Lock()
	secret := totpPending[c.Value]
	sessMu.Unlock()
	if secret == "" {
		writeJSON(w, 400, map[string]string{"error": "请先发起 2FA 设置"})
		return
	}
	accMu.Lock()
	user := "admin"
	if acct != nil {
		user = acct.Username
	}
	accMu.Unlock()
	png, err := qrcode.Encode(otpauthURI(user, secret), qrcode.Medium, 320)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("gwadmin")
	if err != nil {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	sessMu.Lock()
	secret := totpPending[c.Value]
	sessMu.Unlock()
	if secret == "" {
		writeJSON(w, 400, map[string]string{"error": "请先发起 2FA 设置"})
		return
	}
	if !verifyTOTP(secret, req.Code) {
		writeJSON(w, 400, map[string]string{"error": "验证码不对，请确认手机时间准确后重试"})
		return
	}
	plain, hashes := genRecoveryCodes(10)
	accMu.Lock()
	acct.TOTPSecret = secret
	acct.TOTPEnabled = true
	acct.RecoveryHashes = hashes
	saveAccountLocked()
	accMu.Unlock()
	sessMu.Lock()
	delete(totpPending, c.Value)
	sessMu.Unlock()
	writeJSON(w, 200, map[string]interface{}{"ok": true, "recoveryCodes": plain})
}

func handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	accMu.Lock()
	a := acct
	if a == nil || !verifyPassword(req.Password, a.PasswordHash) {
		accMu.Unlock()
		writeJSON(w, 403, map[string]string{"error": "密码错误"})
		return
	}
	a.TOTPEnabled = false
	a.TOTPSecret = ""
	a.RecoveryHashes = nil
	saveAccountLocked()
	accMu.Unlock()
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("gwadmin"); err == nil {
		sessMu.Lock()
		delete(sess, c.Value)
		sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "gwadmin", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

func modeName() string {
	if allInOne {
		return "all"
	}
	return "split"
}

func handleOverview(w http.ResponseWriter, r *http.Request) {
	out := map[string]interface{}{}
	out["mode"] = modeName()
	stun, stunErr := fetchStunStatus()
	if stunErr != nil {
		out["stunError"] = stunErr.Error()
	} else {
		out["stun"] = stun
	}
	if ep, err := readEndpointFile(); err == nil {
		out["endpointFile"] = ep
	}
	// 隧道 实际监听端口（用于和 v6 端口做一致性提示）
	if code, b, err := coreDo("GET", "/api/admin/interface", nil); err == nil && code == 200 {
		var iface map[string]interface{}
		if json.Unmarshal(b, &iface) == nil {
			out["wgPort"] = iface["port"]
			out["wgIpv6Cidr"] = iface["ipv6Cidr"]
		}
	}

	clients, err := listClients()
	if err != nil {
		out["wgError"] = err.Error()
	} else {
		// STUN 中继时 core 看到的 peer endpoint 是 127.0.0.1:<relayPort>，
		// 用网关中继会话表把它还原成客户端公网地址。
		relayByLocal := map[string]string{}
		if stun != nil {
			if arr, ok := stun["relaySessionList"].([]interface{}); ok {
				for _, item := range arr {
					m, ok := item.(map[string]interface{})
					if !ok {
						continue
					}
					local, client := str(m["local"]), str(m["client"])
					if local != "" && client != "" {
						relayByLocal[local] = client
					}
				}
			}
		}
		views := make([]clientView, 0, len(clients))
		for _, c := range clients {
			v := clientView{
				ID: int(c["id"].(float64)), Name: str(c["name"]),
				Ipv4Address: str(c["ipv4Address"]), Ipv6Address: str(c["ipv6Address"]),
				Enabled: boolv(c["enabled"]), LatestHandshakeAt: str(c["latestHandshakeAt"]),
				Endpoint: str(c["endpoint"]), TransferRx: int64v(c["transferRx"]), TransferTx: int64v(c["transferTx"]),
				PersistentKeepalive: int(int64v(c["persistentKeepalive"])),
				ProxyEgress:         boolv(c["proxyEgress"]),
				ProxySelfService:    boolv(c["proxySelfService"]),
			}
			if isLoopbackEndpoint(v.Endpoint) {
				v.ViaStun = true
				if pub := relayByLocal[v.Endpoint]; pub != "" {
					v.PublicEndpoint = pub
				}
			}
			if d, ok := c["dns"].([]interface{}); ok {
				for _, x := range d {
					v.Dns = append(v.Dns, str(x))
				}
			}
			if a, ok := c["allowedIps"].([]interface{}); ok {
				for _, x := range a {
					v.AllowedIps = append(v.AllowedIps, str(x))
				}
			}
			views = append(views, v)
		}
		out["clients"] = views
	}
	out["defaultPassword"] = defaultPasswordActive()
	writeJSON(w, 200, out)
}

func str(v interface{}) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// isLoopbackEndpoint 判断 core 报告的 peer endpoint 是否来自本机 STUN 中继
// （127.0.0.1:<port> / [::1]:<port>）。
func isLoopbackEndpoint(ep string) bool {
	host, _, err := net.SplitHostPort(ep)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
func boolv(v interface{}) bool { b, _ := v.(bool); return b }
func int64v(v interface{}) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, 400, map[string]string{"error": "名称不能为空"})
		return
	}
	id, err := createClient(req.Name, req.Mode)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	refreshEgressClients()
	writeJSON(w, 200, map[string]interface{}{"ok": true, "id": id})
}

func handleToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      int  `json:"id"`
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	code, b, err := updateClient(req.ID, func(m map[string]interface{}) { m["enabled"] = req.Enabled })
	if err != nil || code != 200 {
		writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("%v %s", err, string(b))})
		return
	}
	refreshEgressClients()
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func handleClientProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    int  `json:"id"`
		Proxy bool `json:"proxyEgress"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	code, b, err := updateClient(req.ID, func(m map[string]interface{}) { m["proxyEgress"] = req.Proxy })
	if err != nil || code != 200 {
		writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("%v %s", err, string(b))})
		return
	}
	refreshEgressClients()
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func handleClientProxySelf(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   int  `json:"id"`
		Self bool `json:"proxySelfService"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	code, b, err := updateClient(req.ID, func(m map[string]interface{}) { m["proxySelfService"] = req.Self })
	if err != nil || code != 200 {
		writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("%v %s", err, string(b))})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

// ---------- 隧道内客户端自助接口 ----------
//
// 客户端通过隧道访问 10.8.0.1:51822，源地址就是它在 WireGuard 里的固定地址
// （服务端按 peer 的 AllowedIPs 强制校验，不能被其它客户端伪造），
// 因此不需要额外的 token。
const (
	selfProxyMsgGlobalOff    = "管理员未开启全局出口，请联系管理员"
	selfProxyMsgNoPermission = "管理员未开启代理自助权限，请联系管理员"
	selfProxyMsgDisabled     = "该设备已被管理员停用"
)

func tunnelClientFromRequest(r *http.Request) (map[string]interface{}, string, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil, "", fmt.Errorf("无法识别客户端地址")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, "", fmt.Errorf("无法识别客户端地址")
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	_, cidr, err := net.ParseCIDR(effectiveClientNet())
	if err != nil || !cidr.Contains(ip) {
		return nil, "", fmt.Errorf("只允许隧道内客户端调用")
	}
	src := ip.String()
	clients, err := listClients()
	if err != nil {
		return nil, "", fmt.Errorf("读取客户端列表失败: %v", err)
	}
	for _, c := range clients {
		if str(c["ipv4Address"]) == src {
			return c, src, nil
		}
	}
	return nil, "", fmt.Errorf("找不到与该隧道地址匹配的客户端")
}

func selfProxyStatus(c map[string]interface{}) map[string]interface{} {
	egress := loadEgressConfig()
	self := boolv(c["proxySelfService"])
	active := boolv(c["enabled"])
	allowed := self && egress.Enabled && active
	msg := ""
	switch {
	case !egress.Enabled:
		msg = selfProxyMsgGlobalOff
	case !active:
		msg = selfProxyMsgDisabled
	case !self:
		msg = selfProxyMsgNoPermission
	}
	return map[string]interface{}{
		"clientId":      int64v(c["id"]),
		"name":          str(c["name"]),
		"ipv4":          str(c["ipv4Address"]),
		"enabled":       boolv(c["proxyEgress"]),
		"selfService":   self,
		"globalEnabled": egress.Enabled,
		"allowed":       allowed,
		"message":       msg,
	}
}

func handleSelfProxy(w http.ResponseWriter, r *http.Request) {
	c, _, err := tunnelClientFromRequest(r)
	if err != nil {
		writeJSON(w, 403, map[string]string{"error": err.Error()})
		return
	}
	switch r.Method {
	case "GET":
		writeJSON(w, 200, selfProxyStatus(c))
	case "POST":
		st := selfProxyStatus(c)
		if !boolv(st["allowed"]) {
			writeJSON(w, 403, map[string]string{"error": str(st["message"])})
			return
		}
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
			return
		}
		id := int(int64v(c["id"]))
		code, b, err := updateClient(id, func(m map[string]interface{}) { m["proxyEgress"] = req.Enabled })
		if err != nil || code != 200 {
			writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("保存失败: %v %s", err, string(b))})
			return
		}
		refreshEgressClients()
		nc, err := getClient(id)
		if err != nil {
			writeJSON(w, 200, map[string]interface{}{"ok": true, "enabled": req.Enabled})
			return
		}
		writeJSON(w, 200, selfProxyStatus(nc))
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// refreshEgressClients 把「启用且开启代理出口」的客户端 IPv4 写到
// /data/egress-clients，homing-egress 每轮读取并按源地址分流。
func refreshEgressClients() {
	clients, err := listClients()
	if err != nil {
		log.Printf("刷新出口客户端列表失败: %v", err)
		return
	}
	var b strings.Builder
	b.WriteString("# 由 homing-admin 生成，勿手改\n")
	n := 0
	for _, c := range clients {
		if !boolv(c["enabled"]) || !boolv(c["proxyEgress"]) {
			continue
		}
		ip := str(c["ipv4Address"])
		if net.ParseIP(ip) == nil {
			continue
		}
		b.WriteString(ip + "\n")
		n++
	}
	tmp := egressClientsPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		log.Printf("写出口客户端列表失败: %v", err)
		return
	}
	if err := os.Rename(tmp, egressClientsPath); err != nil {
		log.Printf("更新出口客户端列表失败: %v", err)
		return
	}
	log.Printf("出口客户端列表已更新: %d 台", n)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	code, b, err := coreDo("DELETE", fmt.Sprintf("/api/client/%d", req.ID), nil)
	if err != nil || (code != 200 && code != 204) {
		// 兜底：部分版本 DELETE 需要 body
		body, _ := json.Marshal(map[string]int{"clientId": req.ID})
		code, b, err = coreDo("DELETE", "/api/client", body)
	}
	if err != nil || (code != 200 && code != 204) {
		writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("delete failed %d %v %s", code, err, string(b))})
		return
	}
	refreshEgressClients()
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

func handleClientConfig(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	prof, err := clientProfileJSON(id)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=homing-%s.json", id))
	_, _ = w.Write(prof)
}

// homingMetaComment 生成给自研客户端的 "# homing:" 元数据注释：
// 公告板记录、候选端点（STUN 打洞 / 公网直连 / 局域网）。
func homingMetaComment() string {
	var b strings.Builder
	b.WriteString("\n# ---- homing client metadata (other clients ignore) ----\n")

	cs := loadConnectSettings()
	if cs.StunEnabled {
		if record, ok := discoveryRecord(); ok {
			b.WriteString("# homing:discovery.record=" + record + "\n")
			b.WriteString("# homing:discovery.enabled=true\n")
			b.WriteString("# homing:discovery.autoFollow=true\n")
			b.WriteString("# homing:discovery.preferred=" + discoveryPreferred(cs.Priority) + "\n")
			b.WriteString("# homing:discovery.interval=30\n")
		}
	}
	if eps := collectConnectEndpoints(cs); len(eps) > 0 {
		b.WriteString("# homing:endpoints=" + strings.Join(eps, ",") + "\n")
	}
	return b.String()
}

func listenPortOf(addr string) int {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		if p, err := strconv.Atoi(addr[i+1:]); err == nil {
			return p
		}
	}
	return 0
}

// selfAPIBase 返回客户端通过隧道访问管理台自助接口的地址。
// 管理台监听端口跟随 ADMIN_LISTEN；监听地址必须是 0.0.0.0/: 或宿主机
// 隧道口地址，否则客户端访问不到（默认 :51822 即可）。
func selfAPIBase() string {
	port := listenPortOf(listenAddr)
	if port == 0 {
		port = 51822
	}
	return fmt.Sprintf("http://10.8.0.1:%d", port)
}

// localEgressIP 取本机走物理网络时的源 IP（用于推导局域网中继端点）。
func localEgressIP() string {
	conn, err := net.DialTimeout("udp4", "223.5.5.5:53", 2*time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

func handleClientQR(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	prof, err := clientProfileJSON(id)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// 把整份 JSON profile（含公告板 + 候选端点）编码进二维码
	png, err := qrcode.Encode(string(prof), qrcode.Low, 720)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "生成二维码失败: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// 把当前选定的端点（按连接方式/优先级）写入 隧道核心全局 host/port（生成的新客户端配置直接可用）
func handleApplyEndpoint(w http.ResponseWriter, r *http.Request) {
	cs := loadConnectSettings()
	eps := collectConnectEndpoints(cs)
	if len(eps) == 0 {
		writeJSON(w, 500, map[string]string{"error": "没有可用端点（STUN 未成功且未配置公网直连地址）"})
		return
	}
	addr := eps[0]
	ip, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "端点格式不对：" + addr})
		return
	}
	port, _ := strconv.Atoi(portStr)
	ip = strings.Trim(ip, "[]")
	code, b, err := coreDo("GET", "/api/admin/userconfig", nil)
	if err != nil || code != 200 {
		writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("read userconfig: %v %s", err, string(b))})
		return
	}
	var uc map[string]interface{}
	_ = json.Unmarshal(b, &uc)
	uc["host"] = ip
	uc["port"] = port
	uc["defaultAllowedIps"] = []string{"0.0.0.0/0", "::/0"}
	bc, _ := json.Marshal(uc)
	code, b, err = coreDo("POST", "/api/admin/userconfig", bc)
	if err != nil || code != 200 {
		writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("write userconfig %d: %v %s", code, err, string(b))})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "endpoint": addr, "host": ip, "port": port})
}

func handleDNSTest(w http.ResponseWriter, r *http.Request) {
	// 触发 gateway-stun 立即发布一次（用当前打洞端点）
	ip, addr, _ := currentPunched()
	if ip == "" {
		writeJSON(w, 500, map[string]string{"error": "还没有打洞端点"})
		return
	}
	body, _ := json.Marshal(map[string]string{"value": addr})
	req, _ := http.NewRequest("POST", stunBase+"/publish-test", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]interface{}
	_ = json.Unmarshal(b, &out)
	if resp.StatusCode >= 300 {
		msg, _ := out["error"].(string)
		if msg == "" {
			msg = string(b)
		}
		writeJSON(w, 500, map[string]string{"error": msg})
		return
	}
	writeJSON(w, 200, out)
}

func readWGInterface() (map[string]interface{}, error) {
	code, b, err := coreDo("GET", "/api/admin/interface", nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("wg interface status %d: %s", code, string(b))
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// ---------- 客户端探针默认参数（管理台可配置，写进新生成的客户端配置） ----------

type clientProbeDefaults struct {
	Enabled          bool `json:"enabled"`
	Interval         int  `json:"interval"`
	IdleInterval     int  `json:"idleInterval"`
	IdleAfter        int  `json:"idleAfter"`
	Failures         int  `json:"failures"`
	TimeoutMs        int  `json:"timeoutMs"`
	RecoveryInterval int  `json:"recoveryInterval"`
	SoftGrace        int  `json:"softGrace"`
}

func defaultClientProbe() clientProbeDefaults {
	return clientProbeDefaults{
		Enabled:          true,
		Interval:         10,
		IdleInterval:     30,
		IdleAfter:        5,
		Failures:         1,
		TimeoutMs:        1500,
		RecoveryInterval: 5,
		SoftGrace:        30,
	}
}

// loadClientProbeDefaults 读取管理台保存的探针默认参数；第二个返回值表示是否已配置过。
func loadClientProbeDefaults() (clientProbeDefaults, bool) {
	out := defaultClientProbe()
	b, err := os.ReadFile(clientProbePath)
	if err != nil {
		return out, false
	}
	var v clientProbeDefaults
	if err := json.Unmarshal(b, &v); err != nil {
		return out, false
	}
	return v, true
}

// clientProbeToMap 转成客户端 JSON 的 probe 对象。
func clientProbeToMap(v clientProbeDefaults) map[string]interface{} {
	return map[string]interface{}{
		"enabled":          v.Enabled,
		"interval":         v.Interval,
		"idleInterval":     v.IdleInterval,
		"idleAfter":        v.IdleAfter,
		"failures":         v.Failures,
		"timeoutMs":        v.TimeoutMs,
		"recoveryInterval": v.RecoveryInterval,
		"softGrace":        v.SoftGrace,
	}
}

func validateClientProbe(v clientProbeDefaults) error {
	switch {
	case v.Interval < 3 || v.Interval > 120:
		return fmt.Errorf("活跃间隔应为 3-120 秒")
	case v.IdleInterval < 3 || v.IdleInterval > 300:
		return fmt.Errorf("空闲间隔应为 3-300 秒")
	case v.IdleAfter < 2 || v.IdleAfter > 60:
		return fmt.Errorf("空闲判定应为 2-60 秒")
	case v.Failures < 1 || v.Failures > 10:
		return fmt.Errorf("失败阈值应为 1-10")
	case v.TimeoutMs < 300 || v.TimeoutMs > 5000:
		return fmt.Errorf("单次超时应为 300-5000 毫秒")
	case v.RecoveryInterval < 2 || v.RecoveryInterval > 30:
		return fmt.Errorf("快探间隔应为 2-30 秒")
	case v.SoftGrace < 10 || v.SoftGrace > 120:
		return fmt.Errorf("软恢复宽限应为 10-120 秒")
	}
	return nil
}

func handleClientProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var v clientProbeDefaults
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad request"})
			return
		}
		if err := validateClientProbe(v); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		_ = os.MkdirAll(dataDir, 0o755)
		b, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(clientProbePath, b, 0o600); err != nil {
			writeJSON(w, 500, map[string]string{"error": "保存失败：" + err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "probe": v})
		return
	}
	v, configured := loadClientProbeDefaults()
	writeJSON(w, 200, map[string]interface{}{
		"probe":      v,
		"configured": configured,
		"defaults":   defaultClientProbe(),
	})
}

// ---------- 连接方式 / 传输优先级（STUN 开关 + 公网直连 + v4/v6 优先级） ----------

type connectSettings struct {
	StunEnabled bool   `json:"stunEnabled"`        // 没有公网 IP：用 STUN 打洞；有公网 IP：可关闭
	PublicV4    string `json:"publicV4,omitempty"` // 公网直连 IPv4（host:port）
	PublicV6    string `json:"publicV6,omitempty"` // 公网直连 IPv6（[addr]:port）
	Priority    string `json:"priority"`           // auto | v6,v4 | v4,v6 | v4-only | v6-only
}

func defaultConnectSettings() connectSettings {
	return connectSettings{StunEnabled: true, Priority: "auto"}
}

func normalizePriority(p string) string {
	p = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), " ", ""))
	switch p {
	case "auto", "v4", "v6", "v4,v6", "v6,v4", "v4-only", "v6-only":
		return p
	}
	return "auto"
}

func validPriority(p string) bool {
	return p == "" || normalizePriority(p) == p
}

func saveConnectSettings(v connectSettings) error {
	v = normalizeConnectSettings(v)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return os.WriteFile(connectCfgPath, b, 0o600)
}

// corePort 从 STUN 转发目标里取隧道核心端口（如 127.0.0.1:51820）。
func corePort() int {
	if data, err := os.ReadFile(stunCfgPath); err == nil {
		var cfg struct {
			Target string `json:"target"`
		}
		if json.Unmarshal(data, &cfg) == nil {
			if _, p, err := net.SplitHostPort(cfg.Target); err == nil {
				if n, err := strconv.Atoi(p); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	return 51820
}

// stripHostPort 去掉地址里用户可能误填的端口，只保留 host（IPv6 不带括号）。
func stripHostPort(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "[") {
		if host, _, err := net.SplitHostPort(s); err == nil {
			return strings.Trim(host, "[]")
		}
		return strings.Trim(s, "[]")
	}
	if net.ParseIP(s) != nil {
		return s
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return s
}

// normalizePublicEndpoint 只接收 host（端口由隧道监听端口自动决定）。
func normalizePublicEndpoint(s string, defaultPort int) string {
	host := stripHostPort(s)
	if host == "" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() != nil {
			return net.JoinHostPort(host, strconv.Itoa(defaultPort))
		}
		return "[" + host + "]:" + strconv.Itoa(defaultPort)
	}
	return net.JoinHostPort(host, strconv.Itoa(defaultPort))
}

// normalizeConnectSettings 保存/读取时统一成 host（不含端口）。
func normalizeConnectSettings(v connectSettings) connectSettings {
	v.PublicV4 = stripHostPort(v.PublicV4)
	v.PublicV6 = stripHostPort(v.PublicV6)
	v.Priority = normalizePriority(v.Priority)
	return v
}

// ---------- 公网地址自动探测 ----------

var (
	detectMu      sync.Mutex
	detectCache   map[string]string
	detectCacheAt time.Time
)

// isPublicIPv4 判断是否是可直连的公网 IPv4（排除私有/保留/测试/运营商 NAT 网段）。
func isPublicIPv4(ip net.IP) bool {
	b := ip.To4()
	if b == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if b[0] == 0 || b[0] >= 240 {
		return false
	}
	// 100.64.0.0/10 CGNAT
	if b[0] == 100 && b[1] >= 64 && b[1] <= 127 {
		return false
	}
	// 198.18.0.0/15 基准测试保留段（mihomo / 部分 TUN 网卡会占用）
	if b[0] == 198 && (b[1] == 18 || b[1] == 19) {
		return false
	}
	// 文档/示例保留段
	if (b[0] == 192 && b[1] == 0 && b[2] == 2) ||
		(b[0] == 198 && b[1] == 51 && b[2] == 100) ||
		(b[0] == 203 && b[1] == 0 && b[2] == 113) {
		return false
	}
	return true
}

// interfaceIPv4 返回本机网卡上的公网 IPv4（直连模式下地址就在网卡上，和 IPv6 一样从网卡取）。
func interfaceIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if isPublicIPv4(ipnet.IP) {
				return ipnet.IP.To4().String()
			}
		}
	}
	return ""
}

// interfaceIPv6 返回本机网卡上的全局 IPv6。
func interfaceIPv6() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if ip.To4() != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				continue
			}
			if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
				continue
			}
			return ip.String()
		}
	}
	return ""
}

// detectPublicIPv4 直连模式：从本机网卡取公网 IPv4；网卡上没有（例如在 NAT 后面）
// 再用外部 IP 回显服务兜底。不再使用 gateway-stun 的打洞映射地址。
func detectPublicIPv4() string {
	if ip := interfaceIPv4(); ip != "" {
		return ip
	}
	client := &http.Client{Timeout: 2500 * time.Millisecond}
	for _, u := range []string{"https://api.ipify.org", "https://ipv4.icanhazip.com", "https://ifconfig.me/ip"} {
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "homing-admin/1.0")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			continue
		}
		host := stripHostPort(strings.TrimSpace(string(b)))
		if ip := net.ParseIP(host); ip != nil && ip.To4() != nil && !ip.IsPrivate() && !ip.IsLoopback() {
			return ip.String()
		}
	}
	return ""
}

// detectPublicIPv6 和 IPv4 一样从本机网卡取全局地址，不使用 STUN 映射。
func detectPublicIPv6() string {
	return interfaceIPv6()
}

func detectPublicAddressesQuick() map[string]string {
	return map[string]string{
		"v4": interfaceIPv4(),
		"v6": interfaceIPv6(),
	}
}

func detectPublicAddresses(force bool) map[string]string {
	detectMu.Lock()
	defer detectMu.Unlock()
	if !force && detectCache != nil && time.Since(detectCacheAt) < 5*time.Minute {
		return detectCache
	}
	out := map[string]string{
		"v4":       detectPublicIPv4(),
		"v6":       detectPublicIPv6(),
		"corePort": strconv.Itoa(corePort()),
	}
	detectCache = out
	detectCacheAt = time.Now()
	return out
}

func handleDetectAddresses(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	out := detectPublicAddresses(force)
	writeJSON(w, 200, map[string]interface{}{
		"ok":         true,
		"detectedV4": out["v4"],
		"detectedV6": out["v6"],
		"corePort":   corePort(),
	})
}

func loadConnectSettings() connectSettings {
	out := defaultConnectSettings()
	b, err := os.ReadFile(connectCfgPath)
	if err != nil {
		return out
	}
	var v connectSettings
	if err := json.Unmarshal(b, &v); err != nil {
		return out
	}
	return normalizeConnectSettings(v)
}

func endpointFamily(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = strings.Trim(addr, "[]")
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		// 域名/DDNS：按 v4 处理（放到 v4 候选里，至少不会被排序丢掉）
		return "v4"
	}
	if ip.To4() != nil {
		return "v4"
	}
	return "v6"
}

// discoveryRecord 返回公告板 TXT 记录名（STUN 运行配置里启用 DNS 才有）。
func discoveryRecord() (string, bool) {
	data, err := os.ReadFile(stunCfgPath)
	if err != nil {
		return "", false
	}
	var cfg struct {
		DNS struct {
			Enabled bool   `json:"enabled"`
			Record  string `json:"record"`
		} `json:"dns"`
	}
	if json.Unmarshal(data, &cfg) != nil || !cfg.DNS.Enabled || strings.TrimSpace(cfg.DNS.Record) == "" {
		return "", false
	}
	return strings.TrimSpace(cfg.DNS.Record), true
}

func discoveryPreferred(priority string) string {
	return normalizePriority(priority)
}

// collectConnectEndpoints 汇总所有候选端点并按优先级排序：
// STUN 打洞端点（v4/v6）→ 用户填的公网直连地址 → 局域网直连。
func collectConnectEndpoints(cs connectSettings) []string {
	var raw []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		raw = append(raw, s)
	}
	if cs.StunEnabled {
		if st, err := fetchStunStatus(); err == nil {
			if cur, ok := st["current"].(map[string]interface{}); ok {
				if a, _ := cur["address"].(string); a != "" {
					add(a)
				}
			}
			if v6, _ := st["v6Endpoint"].(string); v6 != "" {
				add(v6)
			}
			if listen, _ := st["listen"].(string); listen != "" {
				if port := listenPortOf(listen); port > 0 {
					if ip := localEgressIP(); ip != "" {
						add(net.JoinHostPort(ip, strconv.Itoa(port)))
					}
				}
			}
		}
	}
	port := corePort()
	add(normalizePublicEndpoint(cs.PublicV4, port))
	add(normalizePublicEndpoint(cs.PublicV6, port))
	// 局域网内直连隧道核心（STUN/公网模式下也可用）
	if ip := localEgressIP(); ip != "" {
		add(net.JoinHostPort(ip, strconv.Itoa(port)))
	}
	return orderEndpoints(raw, cs.Priority)
}

// orderEndpoints 按 v4/v6 优先级排序；v4-only / v6-only 会过滤掉另一协议。
func orderEndpoints(eps []string, priority string) []string {
	priority = normalizePriority(priority)
	order := []string{"v4", "v6"} // auto：首次连接优先用通吃性更好的 v4；客户端会按自身 IPv6 情况再选
	switch priority {
	case "v6", "v6,v4", "v6-only":
		order = []string{"v6", "v4"}
	}
	only := ""
	if priority == "v4-only" {
		only = "v4"
	} else if priority == "v6-only" {
		only = "v6"
	}
	out := make([]string, 0, len(eps))
	for _, fam := range order {
		for _, e := range eps {
			if endpointFamily(e) != fam {
				continue
			}
			if only != "" && fam != only {
				continue
			}
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		// 只选了某一协议但当前没有该协议端点：退回全部候选，避免生成空配置
		return eps
	}
	return out
}

// applyConnectToProfile 把连接方式写进归途 profile：peer endpoint、候选列表、公告板偏好。
func applyConnectToProfile(prof map[string]interface{}) {
	cs := loadConnectSettings()
	eps := collectConnectEndpoints(cs)
	if len(eps) > 0 {
		if peers, ok := prof["peers"].([]interface{}); ok && len(peers) > 0 {
			if p, ok := peers[0].(map[string]interface{}); ok {
				p["endpoint"] = eps[0]
			}
		}
		prof["endpoints"] = eps
	}
	if cs.StunEnabled {
		if record, ok := discoveryRecord(); ok {
			prof["discovery"] = map[string]interface{}{
				"enabled":    true,
				"record":     record,
				"autoFollow": true,
				"preferred":  discoveryPreferred(cs.Priority),
				"interval":   30,
			}
		}
	} else {
		// 公网直连模式没有 STUN 发布者，不写公告板（客户端用固定端点/候选列表）
		delete(prof, "discovery")
	}
}

func validateConnectSettings(v connectSettings) error {
	if !v.StunEnabled && strings.TrimSpace(v.PublicV4) == "" && strings.TrimSpace(v.PublicV6) == "" {
		return fmt.Errorf("关闭 STUN 后至少要填一个公网地址（IPv4 或 IPv6，端口自动使用隧道监听端口）")
	}
	for _, s := range []string{v.PublicV4, v.PublicV6} {
		if strings.TrimSpace(s) == "" {
			continue
		}
		norm := normalizePublicEndpoint(s, corePort())
		host, port, err := net.SplitHostPort(norm)
		if err != nil || strings.TrimSpace(host) == "" {
			return fmt.Errorf("公网地址格式不对：%s（只填 IP/域名即可，端口自动使用隧道监听端口）", s)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("隧道监听端口不合法：%s", s)
		}
	}
	return nil
}

func handleConnectSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var req struct {
			StunEnabled *bool  `json:"stunEnabled"`
			PublicV4    string `json:"publicV4"`
			PublicV6    string `json:"publicV6"`
			Priority    string `json:"priority"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad request"})
			return
		}
		v := defaultConnectSettings()
		if req.StunEnabled != nil {
			v.StunEnabled = *req.StunEnabled
		}
		if !validPriority(req.Priority) {
			writeJSON(w, 400, map[string]string{"error": "优先级只能是 auto / v6,v4 / v4,v6 / v4-only / v6-only"})
			return
		}
		v.PublicV4 = normalizePublicEndpoint(req.PublicV4, corePort())
		v.PublicV6 = normalizePublicEndpoint(req.PublicV6, corePort())
		v.Priority = normalizePriority(req.Priority)
		v = normalizeConnectSettings(v)
		if err := validateConnectSettings(v); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if err := saveConnectSettings(v); err != nil {
			writeJSON(w, 500, map[string]string{"error": "保存失败：" + err.Error()})
			return
		}
		quick := detectPublicAddressesQuick()
		writeJSON(w, 200, map[string]interface{}{
			"ok":         true,
			"settings":   v,
			"endpoints":  collectConnectEndpoints(v),
			"corePort":   corePort(),
			"detectedV4": quick["v4"],
			"detectedV6": quick["v6"],
		})
		return
	}
	v := loadConnectSettings()
	_, discoOK := discoveryRecord()
	quick := detectPublicAddressesQuick()
	writeJSON(w, 200, map[string]interface{}{
		"settings":         v,
		"endpoints":        collectConnectEndpoints(v),
		"corePort":         corePort(),
		"detectedV4":       quick["v4"],
		"detectedV6":       quick["v6"],
		"discoveryEnabled": discoOK,
	})
}

func handleGetNetwork(w http.ResponseWriter, r *http.Request) {
	out := map[string]interface{}{"mode": modeName()}
	if iface, err := readWGInterface(); err == nil {
		out["wgPort"] = iface["port"]
		out["mtu"] = iface["mtu"]
		out["ipv4Cidr"] = iface["ipv4Cidr"]
		out["ipv6Cidr"] = iface["ipv6Cidr"]
	} else {
		out["wgError"] = err.Error()
	}
	if b, err := os.ReadFile(stunCfgPath); err == nil {
		var cfg map[string]interface{}
		if json.Unmarshal(b, &cfg) == nil {
			out["stunTarget"] = cfg["target"]
			if v6, ok := cfg["v6"].(map[string]interface{}); ok {
				out["v6Enabled"] = v6["enabled"]
				out["v6Address"] = v6["address"]
			}
		}
	}
	if st, err := fetchStunStatus(); err == nil {
		out["stunTarget"] = st["target"]
		out["v6Endpoint"] = st["v6Endpoint"]
	}
	writeJSON(w, 200, out)
}

// 修改接口：端口同步到 隧道核心、.env、STUN 转发目标，并重建隧道核心容器
func handleSetPort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Port < 1024 || req.Port > 65535 {
		writeJSON(w, 400, map[string]string{"error": "端口范围 1024-65535"})
		return
	}
	iface, err := readWGInterface()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "读取 隧道核心接口失败：" + err.Error()})
		return
	}
	iface["port"] = req.Port
	b, _ := json.Marshal(iface)
	if code, rb, err := coreDo("POST", "/api/admin/interface", b); err != nil || code != 200 {
		writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("更新 隧道核心接口失败: %v %s", err, string(rb))})
		return
	}
	// .env 只在分容器模式维护；单容器模式没有部署目录。
	if !allInOne {
		envPath := vpnDir + "/.env"
		if data, err := os.ReadFile(envPath); err == nil {
			lines := strings.Split(string(data), "\n")
			found := false
			for i, ln := range lines {
				if strings.HasPrefix(strings.TrimSpace(ln), "CORE_PORT=") {
					lines[i] = fmt.Sprintf("CORE_PORT=%d", req.Port)
					found = true
				}
			}
			if !found {
				lines = append(lines, fmt.Sprintf("CORE_PORT=%d", req.Port))
			}
			_ = os.WriteFile(envPath, []byte(strings.Join(lines, "\n")), 0o600)
		}
	}
	// STUN 配置 target（同时决定 IPv6 直连端口）
	if data, err := os.ReadFile(stunCfgPath); err == nil {
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) == nil {
			cfg["target"] = fmt.Sprintf("127.0.0.1:%d", req.Port)
			nb, _ := json.MarshalIndent(cfg, "", "  ")
			_ = os.WriteFile(stunCfgPath, nb, 0o600)
		}
	}
	if allInOne {
		// 单容器：core 端口由 core API 热更新；把端口写给 stun runner，它会重启 stun。
		if err := os.WriteFile(corePortPath, []byte(fmt.Sprintf("%d\n", req.Port)), 0o644); err != nil {
			writeJSON(w, 500, map[string]string{"error": "端口已更新，但写入 /data/core-port 失败：" + err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{
			"ok": true, "port": req.Port,
			"v6Endpoint": fmt.Sprintf("[<v6>]:%d", req.Port),
			"message":    "端口已更新，内置打洞进程会自动重启",
		})
		return
	}
	// 分容器模式：重建 隧道核心（端口是容器启动参数）
	out, err := runCompose("up", "-d", "--force-recreate", "--no-deps", "homing-core")
	msg := out
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "端口已写入配置，但重建容器失败：" + err.Error() + "\n" + truncateMsg(msg)})
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"ok": true, "port": req.Port,
		"v6Endpoint": fmt.Sprintf("[<v6>]:%d", req.Port),
		"message":    truncateMsg(msg),
	})
}

// handleSystemSettings 管理台「网络与默认值」：DNS / 局域网网段 / 隧道网段 / 打洞端口。
func handleSystemSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cur := currentSystemSettings()
		out := map[string]interface{}{
			"settings":        cur,
			"dns":             effectiveDNSString(),
			"lan":             effectiveLAN(),
			"clientNet":       effectiveClientNet(),
			"sources":         settingsSources(),
			"detectedGateway": detectGateway(),
			"detectedLAN":     detectLAN(),
			"defaultPassword": defaultPasswordActive(),
			"vpnHostDir":      vpnHostDir,
			"dataDir":         dataDir,
			"mode":            modeName(),
		}
		if iface, err := readWGInterface(); err == nil {
			out["core"] = iface
		} else {
			out["coreError"] = err.Error()
		}
		if st, err := fetchStunStatus(); err == nil {
			out["stun"] = map[string]interface{}{
				"listen":        st["listen"],
				"localPort":     st["localPort"],
				"current":       st["current"],
				"target":        st["target"],
				"v6Endpoint":    st["v6Endpoint"],
				"lastStunOk":    st["lastStunOk"],
				"relaySessions": st["relaySessions"],
				"relayPackets":  st["relayPackets"],
				"uptimeSeconds": st["uptimeSeconds"],
			}
		}
		writeJSON(w, 200, out)
		return

	case http.MethodPost:
		var req struct {
			DNS       string `json:"dns"`
			LAN       string `json:"lan"`
			ClientNet string `json:"clientNet"`
			StunPort  int    `json:"stunPort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
			return
		}
		dns := strings.TrimSpace(req.DNS)
		lan := strings.TrimSpace(req.LAN)
		netStr := strings.TrimSpace(req.ClientNet)
		if err := validDNSList(dns); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if err := validLANList(lan); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if netStr != "" {
			if err := validClientNet(netStr); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
		}
		if req.StunPort != 0 && (req.StunPort < 1024 || req.StunPort > 65535) {
			writeJSON(w, 400, map[string]string{"error": "打洞端口范围 1024-65535（0=随机）"})
			return
		}

		old := currentSystemSettings()
		msgs := []string{}
		warnings := []string{}

		// 1) 隧道网段：交给隧道核心改（会重排现有客户端 IPv4）
		if netStr != "" && netStr != coreInterfaceCIDR() {
			body, _ := json.Marshal(map[string]interface{}{"ipv4Cidr": netStr})
			if code, rb, err := coreDo("POST", "/api/admin/interface", body); err != nil || code != 200 {
				writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("更新隧道核心网段失败: %v %s", err, string(rb))})
				return
			}
			msgs = append(msgs, "隧道网段已更新")
			warnings = append(warnings, "现有客户端的 IPv4 已重新编号，请让用户重新下载/扫码导入配置")
		}

		// 2) DNS：同时写入核心全局默认（新客户端默认 DNS）
		dnsToApply := splitCSV(dns)
		if len(dnsToApply) == 0 {
			dnsToApply = effectiveDNS()
		}
		body, _ := json.Marshal(map[string]interface{}{"dns": dnsToApply})
		if code, rb, err := coreDo("POST", "/api/admin/interface", body); err != nil || code != 200 {
			// DNS 写入失败不阻塞其它设置，但提示
			warnings = append(warnings, fmt.Sprintf("DNS 写入隧道核心失败: %v %s", err, truncateMsg(string(rb))))
		}

		// 3) 先保存设置（打洞端口重启后 stun 会读它）
		next := systemSettings{DNS: dns, LAN: lan, ClientNet: netStr, StunPort: req.StunPort}
		if err := saveSystemSettings(next); err != nil {
			writeJSON(w, 500, map[string]string{"error": "保存设置失败：" + err.Error()})
			return
		}

		// 4) 打洞端口变化：分容器模式重建 homing-stun；单容器模式由 stun runner 自动重启。
		if req.StunPort != old.StunPort {
			if allInOne {
				msgs = append(msgs, "打洞端口已保存，内置打洞进程会自动重启")
			} else {
				out, err := runCompose("up", "-d", "--force-recreate", "--no-deps", "homing-stun")
				if err != nil {
					writeJSON(w, 500, map[string]interface{}{
						"error": "设置已保存，但重建打洞容器失败：" + err.Error(),
						"log":   truncateMsg(out),
					})
					return
				}
			}
			if req.StunPort == 0 {
				msgs = append(msgs, "打洞端口已改为随机，正在重新发现公网端点")
			} else {
				msgs = append(msgs, fmt.Sprintf("打洞端口已改为 %d", req.StunPort))
			}
		}

		writeJSON(w, 200, map[string]interface{}{
			"ok":        true,
			"dns":       effectiveDNSString(),
			"lan":       effectiveLAN(),
			"clientNet": effectiveClientNet(),
			"messages":  msgs,
			"warnings":  warnings,
		})
		return
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func truncateMsg(s string) string {
	if len(s) > 500 {
		return s[len(s)-500:]
	}
	return s
}

func handleGetConfig(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(stunCfgPath)
	if err != nil {
		writeJSON(w, 200, map[string]interface{}{"stunServers": []string{}, "notifyURL": "", "cfToken": "", "cfZone": "", "cfRecord": ""})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}

func handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var fc map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&fc); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	b, _ := json.MarshalIndent(fc, "", "  ")
	if err := os.WriteFile(stunCfgPath, b, 0o600); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

// ---------- 可选 mihomo 出口（配置 + 启停 homing-egress） ----------

type egressConfig struct {
	Enabled     bool   `json:"enabled"`
	Mode        string `json:"mode"` // tun: 宿主机已有 mihomo TUN；remote: 本机透明代理 + 远端代理端口
	Tun         string `json:"tun"`
	Table       int    `json:"table"`
	CoreNet     string `json:"coreNet"`   // 旧版 core bridge 网段，仅保留配置兼容
	ClientNet   string `json:"clientNet"` // 隧道客户端网段（host 网络模式下按客户端源地址分流）
	DirectCIDRs string `json:"directCidrs"`

	// remote 模式：本机 mihomo 把 TCP/UDP 转给远端 HTTP/SOCKS5 代理
	ProxyType     string `json:"proxyType"`
	ProxyHost     string `json:"proxyHost"`
	ProxyPort     int    `json:"proxyPort"`
	ProxyUsername string `json:"proxyUsername,omitempty"`
	ProxyPassword string `json:"proxyPassword,omitempty"`

	// 本机代理 DNS（留空关闭）；客户端 53 会被 plumber 劫持到该端口
	DNSListen string `json:"dnsListen"`
	DNSServer string `json:"dnsServer"`
	FakeIP    bool   `json:"fakeIp"`

	UpdatedAt string `json:"updatedAt,omitempty"`
}

type egressRuntimeStatus struct {
	Mode           string `json:"mode"`
	Container      string `json:"container"`
	ProxyContainer string `json:"proxyContainer,omitempty"`
	Tun            string `json:"tun"`
	TunReady       bool   `json:"tunReady"`
	RulesApplied   bool   `json:"rulesApplied"`
	DNSPort        int    `json:"dnsPort,omitempty"`
	Clients        int    `json:"clients"`
	UpdatedAt      string `json:"updatedAt,omitempty"`
	Message        string `json:"message,omitempty"`
}

func defaultEgressConfig() egressConfig {
	return egressConfig{
		Enabled:   false,
		Mode:      "tun",
		Tun:       "mihomo-gw",
		Table:     100,
		CoreNet:   "10.43.43.0/24",
		ClientNet: effectiveClientNet(),
		ProxyType: "socks5",
		ProxyPort: 7890,
		DNSListen: "0.0.0.0:1053",
		DNSServer: "223.5.5.5,119.29.29.29",
		FakeIP:    true,
	}
}

func loadEgressConfig() egressConfig {
	cfg := defaultEgressConfig()
	if b, err := os.ReadFile(egressCfgPath); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	if strings.TrimSpace(cfg.Mode) == "" {
		cfg.Mode = "tun"
	}
	if strings.TrimSpace(cfg.Tun) == "" {
		if cfg.Mode == "remote" {
			cfg.Tun = "homing-proxy"
		} else {
			cfg.Tun = "mihomo-gw"
		}
	}
	if cfg.Table == 0 {
		cfg.Table = 100
	}
	if strings.TrimSpace(cfg.CoreNet) == "" {
		cfg.CoreNet = "10.43.43.0/24"
	}
	if strings.TrimSpace(cfg.ClientNet) == "" {
		cfg.ClientNet = effectiveClientNet()
	}
	if strings.TrimSpace(cfg.ProxyType) == "" {
		cfg.ProxyType = "socks5"
	}
	if cfg.ProxyPort == 0 {
		cfg.ProxyPort = 7890
	}
	cfg.Mode = strings.TrimSpace(cfg.Mode)
	cfg.Tun = strings.TrimSpace(cfg.Tun)
	cfg.CoreNet = strings.TrimSpace(cfg.CoreNet)
	cfg.ClientNet = strings.TrimSpace(cfg.ClientNet)
	cfg.DirectCIDRs = strings.TrimSpace(cfg.DirectCIDRs)
	cfg.ProxyType = strings.TrimSpace(cfg.ProxyType)
	cfg.ProxyHost = strings.TrimSpace(cfg.ProxyHost)
	cfg.DNSListen = strings.TrimSpace(cfg.DNSListen)
	cfg.DNSServer = normalizeCSV(cfg.DNSServer)
	return cfg
}

func validateEgressConfig(c egressConfig) error {
	if c.Mode != "tun" && c.Mode != "remote" {
		return fmt.Errorf("出口模式必须是 tun 或 remote")
	}
	if len(c.Tun) == 0 || len(c.Tun) > 15 {
		return fmt.Errorf("TUN 设备名长度必须在 1~15 个字符")
	}
	for _, r := range c.Tun {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
			return fmt.Errorf("TUN 设备名只能包含字母、数字、_ . -")
		}
	}
	if c.Table < 1 || c.Table > 252 {
		return fmt.Errorf("策略路由表号必须在 1~252")
	}
	if err := validateIPv4CIDR(c.ClientNet, false); err != nil {
		return fmt.Errorf("隧道客户端网段: %w", err)
	}
	if c.DirectCIDRs != "" {
		for _, part := range strings.Split(c.DirectCIDRs, ",") {
			if err := validateIPv4CIDR(strings.TrimSpace(part), true); err != nil {
				return fmt.Errorf("直连网段 %q: %w", part, err)
			}
		}
	}
	if c.Mode == "remote" {
		if c.ProxyType != "http" && c.ProxyType != "socks5" {
			return fmt.Errorf("远端代理类型必须是 http 或 socks5")
		}
		if err := validateProxyHost(c.ProxyHost); err != nil {
			return fmt.Errorf("远端代理地址: %w", err)
		}
		if c.ProxyPort < 1 || c.ProxyPort > 65535 {
			return fmt.Errorf("远端代理端口必须在 1~65535")
		}
		if err := validateCredential("用户名", c.ProxyUsername); err != nil {
			return err
		}
		if err := validateCredential("密码", c.ProxyPassword); err != nil {
			return err
		}
		if c.DNSListen != "" {
			if _, err := egressDNSPort(c.DNSListen); err != nil {
				return fmt.Errorf("本机代理 DNS: %w", err)
			}
			if strings.TrimSpace(c.DNSServer) == "" {
				return fmt.Errorf("本机代理 DNS 开启时，DNS 上游不能为空")
			}
		}
		if c.DNSServer != "" {
			for _, part := range splitCSV(c.DNSServer) {
				if err := validateDNSServer(part); err != nil {
					return fmt.Errorf("DNS 上游 %q: %w", part, err)
				}
			}
		}
	}
	return nil
}

func validateProxyHost(s string) error {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 253 {
		return fmt.Errorf("长度必须在 1~253 个字符")
	}
	host := strings.Trim(s, "[]")
	if net.ParseIP(host) != nil {
		return nil
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return fmt.Errorf("只支持 IP 或域名")
		}
	}
	return nil
}

func validateCredential(name, s string) error {
	if len(s) > 256 {
		return fmt.Errorf("%s最多 256 个字符", name)
	}
	for _, r := range s {
		if r == '\n' || r == '\r' || r == 0 {
			return fmt.Errorf("%s不能包含换行或控制字符", name)
		}
	}
	return nil
}

func validateDNSServer(s string) error {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 253 {
		return fmt.Errorf("格式不正确")
	}
	host := s
	if h, p, err := net.SplitHostPort(s); err == nil {
		host = h
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("端口必须在 1~65535")
		}
	}
	host = strings.Trim(host, "[]")
	if net.ParseIP(host) == nil {
		return fmt.Errorf("只支持 IP（可带端口）")
	}
	return nil
}

func egressDNSPort(listen string) (int, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return 0, fmt.Errorf("格式应为 IP:端口，如 0.0.0.0:1053")
	}
	if host != "" && net.ParseIP(strings.Trim(host, "[]")) == nil {
		return 0, fmt.Errorf("监听地址必须是 IP")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("端口必须在 1~65535")
	}
	return n, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func normalizeCSV(s string) string {
	return strings.Join(splitCSV(s), ",")
}

func normalizeCIDRList(s string) string {
	return normalizeCSV(s)
}

func validateIPv4CIDR(s string, allowEmpty bool) error {
	if strings.TrimSpace(s) == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("不能为空")
	}
	ip, n, err := net.ParseCIDR(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("不是合法 CIDR")
	}
	if ip.To4() == nil {
		return fmt.Errorf("只支持 IPv4")
	}
	if ones, _ := n.Mask.Size(); ones == 0 {
		return fmt.Errorf("不能使用 0.0.0.0/0")
	}
	return nil
}

func saveEgressConfig(c egressConfig) error {
	c.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	dnsPort := 0
	if c.Mode == "remote" {
		dnsPort, _ = egressDNSPort(c.DNSListen)
	}
	env := fmt.Sprintf(`# 由 homing-admin 生成，勿手改
EGRESS_ENABLED=%d
EGRESS_MODE=%s
MIHOMO_TUN=%s
MIHOMO_TABLE=%d
CLIENT_NET=%s
LAN_CIDRS=%s
EGRESS_DNS_LISTEN=%s
EGRESS_DNS_PORT=%d
EGRESS_CLIENTS_FILE=/data/egress-clients
`, boolToInt(c.Enabled), c.Mode, c.Tun, c.Table, c.ClientNet, c.DirectCIDRs,
		c.DNSListen, dnsPort)
	if err := os.WriteFile(egressEnvPath, []byte(env), 0o600); err != nil {
		return err
	}
	if c.Mode == "remote" {
		if err := os.MkdirAll(proxyDir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(proxyConfigPath, []byte(renderProxyConfig(c)), 0o600); err != nil {
			return err
		}
	}
	// egress.json 最后写，作为“配置已生效”的标记
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(egressCfgPath, b, 0o600)
}

// renderProxyConfig 生成本机 mihomo 的配置：TUN 只负责接住 plumber 送来的流量，
// 所有出站统一走远端 HTTP/SOCKS5 代理。
func renderProxyConfig(c egressConfig) string {
	var b strings.Builder
	b.WriteString("# 由 homing-admin 生成，勿手改\n")
	b.WriteString("mixed-port: 0\nallow-lan: false\nmode: rule\nlog-level: info\nipv6: false\n")
	b.WriteString("profile:\n  store-selected: false\n  store-fake-ip: false\n")

	servers := splitCSV(c.DNSServer)
	if c.DNSListen != "" {
		b.WriteString("dns:\n  enable: true\n")
		fmt.Fprintf(&b, "  listen: %q\n", c.DNSListen)
		b.WriteString("  ipv6: false\n")
		if c.FakeIP {
			b.WriteString("  enhanced-mode: fake-ip\n  fake-ip-range: 198.18.0.1/16\n")
			b.WriteString("  fake-ip-filter:\n")
			for _, f := range []string{"*.lan", "*.local", "+.local", "+.home.arpa",
				"localhost.ptlogin2.qq.com", "+.msftconnecttest.com", "+.msftncsi.com"} {
				fmt.Fprintf(&b, "    - %q\n", f)
			}
		} else {
			b.WriteString("  enhanced-mode: redir-host\n")
		}
		b.WriteString("  default-nameserver:\n")
		writeYAMLList(&b, "    ", servers)
		b.WriteString("  nameserver:\n")
		writeYAMLList(&b, "    ", servers)
		b.WriteString("  proxy-server-nameserver:\n")
		writeYAMLList(&b, "    ", servers)
		// 局域网域名交给现有 DNS，避免被公共 DNS 解析成外网地址。
		dnsList := effectiveDNS()
		if len(dnsList) > 0 {
			if ip := net.ParseIP(strings.Trim(dnsList[0], "[]")); ip != nil && len(servers) > 0 {
				b.WriteString("  nameserver-policy:\n")
				for _, domain := range []string{"+.lan", "+.local", "+.home.arpa"} {
					fmt.Fprintf(&b, "    %q: [%q]\n", domain, dnsList[0])
				}
			}
		}
	} else {
		b.WriteString("dns:\n  enable: false\n")
	}

	b.WriteString("tun:\n  enable: true\n")
	fmt.Fprintf(&b, "  device: %q\n", c.Tun)
	b.WriteString("  stack: gvisor\n  auto-route: false\n  auto-redirect: false\n")
	b.WriteString("  auto-detect-interface: false\n  mtu: 1500\n")
	b.WriteString("sniffer:\n  enable: true\n  parse-pure-ip: true\n  override-destination: false\n")

	b.WriteString("proxies:\n  - name: \"remote\"\n")
	fmt.Fprintf(&b, "    type: %q\n    server: %q\n    port: %d\n", c.ProxyType, strings.Trim(c.ProxyHost, "[]"), c.ProxyPort)
	if c.ProxyType == "socks5" {
		b.WriteString("    udp: true\n")
	}
	if c.ProxyUsername != "" {
		fmt.Fprintf(&b, "    username: %q\n", c.ProxyUsername)
	}
	if c.ProxyPassword != "" {
		fmt.Fprintf(&b, "    password: %q\n", c.ProxyPassword)
	}
	b.WriteString("proxy-groups:\n  - name: \"REMOTE\"\n    type: select\n    proxies:\n      - \"remote\"\n")
	b.WriteString("rules:\n  - MATCH,REMOTE\n")
	return b.String()
}

func writeYAMLList(b *strings.Builder, indent string, vals []string) {
	for _, v := range vals {
		fmt.Fprintf(b, "%s- %q\n", indent, v)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func containerStatus(name string) string {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", name).Output()
	if err != nil {
		return "missing"
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "missing"
	}
	return s
}

func egressContainerStatus() string {
	return containerStatus("homing-egress")
}

func containerLogTail(name string, max int) string {
	out, err := exec.Command("docker", "logs", "--tail", "8", name).CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(out))
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

func egressTunReady(tun string) bool {
	if strings.TrimSpace(tun) == "" {
		return false
	}
	_, err := os.Stat("/sys/class/net/" + tun)
	return err == nil
}

type egressFileStatus struct {
	Enabled   int    `json:"enabled"`
	Applied   bool   `json:"applied"`
	Mode      string `json:"mode"`
	DNSPort   int    `json:"dnsPort"`
	Clients   int    `json:"clients"`
	UpdatedAt string `json:"updatedAt"`
	Message   string `json:"message"`
}

func readEgressStatusFile() egressFileStatus {
	var st egressFileStatus
	b, err := os.ReadFile(egressStatusPath)
	if err != nil {
		return st
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return egressFileStatus{}
	}
	return st
}

type proxyRunStatus struct {
	Running   bool   `json:"running"`
	PID       int    `json:"pid"`
	Tun       string `json:"tun"`
	UpdatedAt string `json:"updatedAt"`
	Message   string `json:"message"`
}

func readProxyStatus() proxyRunStatus {
	var st proxyRunStatus
	b, err := os.ReadFile(proxyStatusPath)
	if err != nil {
		return st
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return proxyRunStatus{}
	}
	return st
}

func egressStatus(cfg egressConfig) egressRuntimeStatus {
	st := readEgressStatusFile()
	container := egressContainerStatus()
	proxyContainer := ""
	if allInOne {
		// 单容器没有 docker 容器可查：出口进程是否在跑由配置决定，
		// remote 代理看 proxy runner 写的状态文件。
		container = "exited"
		if cfg.Enabled {
			container = "running"
		}
		if cfg.Mode == "remote" {
			proxyContainer = "exited"
			if readProxyStatus().Running {
				proxyContainer = "running"
			}
		}
		if !cfg.Enabled || (cfg.Mode == "remote" && proxyContainer != "running") {
			st.Applied = false
			st.Clients = 0
		}
	} else {
		if cfg.Mode == "remote" {
			proxyContainer = containerStatus("homing-proxy")
		}
		// 容器不在运行时，不要相信上次残留的 applied 状态
		if container != "running" || (cfg.Mode == "remote" && proxyContainer != "running") {
			st.Applied = false
			st.Clients = 0
		}
	}
	if st.UpdatedAt == "" && cfg.UpdatedAt != "" {
		st.UpdatedAt = cfg.UpdatedAt
	}
	dnsPort := st.DNSPort
	if cfg.Mode != "remote" || !cfg.Enabled {
		dnsPort = 0
	}
	return egressRuntimeStatus{
		Mode:           cfg.Mode,
		Container:      container,
		ProxyContainer: proxyContainer,
		Tun:            cfg.Tun,
		TunReady:       egressTunReady(cfg.Tun),
		RulesApplied:   st.Applied,
		DNSPort:        dnsPort,
		Clients:        st.Clients,
		UpdatedAt:      st.UpdatedAt,
		Message:        st.Message,
	}
}

// waitAllInOneProxy 等 proxy runner 把 mihomo 拉起来、TUN 就绪。
func waitAllInOneProxy(tun string, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ps := readProxyStatus()
		if ps.Running && egressTunReady(tun) {
			return true, ""
		}
		time.Sleep(250 * time.Millisecond)
	}
	ps := readProxyStatus()
	msg := ps.Message
	if msg == "" {
		msg = "本机代理未就绪"
	}
	return false, msg
}

// waitAllInOneEgress 等内置 plumber 把 egress-status.json 更新到目标开关状态。
// plumber 每 10 秒一轮，配置变化后 plumb runner 还会重启它，所以最多等十几秒。
func waitAllInOneEgress(enabled bool, timeout time.Duration) {
	want := 0
	if enabled {
		want = 1
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if readEgressStatusFile().Enabled == want {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func runEgressCompose(args ...string) (string, error) {
	return runCompose(append([]string{"--profile", "egress"}, args...)...)
}

func waitContainerState(name string, running bool, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	var st string
	for time.Now().Before(deadline) {
		st = containerStatus(name)
		if running && st == "running" {
			return st
		}
		if !running && st != "running" {
			return st
		}
		time.Sleep(150 * time.Millisecond)
	}
	return st
}

func waitEgressTun(tun string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if egressTunReady(tun) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return egressTunReady(tun)
}

func handleEgress(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		cfg := loadEgressConfig()
		writeJSON(w, 200, map[string]interface{}{"config": cfg, "status": egressStatus(cfg)})
	case "POST":
		var req egressConfig
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
			return
		}
		req.Mode = strings.TrimSpace(req.Mode)
		if req.Mode == "" {
			req.Mode = "tun"
		}
		req.Tun = strings.TrimSpace(req.Tun)
		req.CoreNet = strings.TrimSpace(req.CoreNet)
		req.ClientNet = strings.TrimSpace(req.ClientNet)
		if req.ClientNet == "" {
			req.ClientNet = effectiveClientNet()
		}
		req.DirectCIDRs = normalizeCIDRList(req.DirectCIDRs)
		req.ProxyType = strings.TrimSpace(req.ProxyType)
		req.ProxyHost = strings.TrimSpace(req.ProxyHost)
		req.ProxyUsername = strings.TrimSpace(req.ProxyUsername)
		req.DNSListen = strings.TrimSpace(req.DNSListen)
		req.DNSServer = normalizeCSV(req.DNSServer)
		if req.Mode == "remote" && req.Tun == "" {
			req.Tun = "homing-proxy"
		}
		if err := validateEgressConfig(req); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if err := saveEgressConfig(req); err != nil {
			writeJSON(w, 500, map[string]string{"error": "保存配置失败: " + err.Error()})
			return
		}
		refreshEgressClients()
		if allInOne {
			// 单容器：没有 compose 可调，配置已落盘，由 plumber / proxy runner 生效。
			if req.Enabled && req.Mode == "remote" {
				if ok, msg := waitAllInOneProxy(req.Tun, 12*time.Second); !ok {
					req.Enabled = false
					_ = saveEgressConfig(req)
					writeJSON(w, 502, map[string]string{"error": "启动内置代理失败：" + msg})
					return
				}
			} else if req.Enabled {
				// 本机 TUN 模式：TUN 由宿主机代理提供，plumber 会持续等待，不阻塞保存。
				waitEgressTun(req.Tun, 5*time.Second)
			}
			waitAllInOneEgress(req.Enabled, 15*time.Second)
			time.Sleep(300 * time.Millisecond)
			cfg := loadEgressConfig()
			writeJSON(w, 200, map[string]interface{}{"ok": true, "config": cfg, "status": egressStatus(cfg)})
			return
		}
		if req.Enabled {
			if req.Mode == "remote" {
				// 先起本机代理，TUN 出现后 plumber 才会应用策略路由
				out, err := runEgressCompose("up", "-d", "--no-deps", "--force-recreate", "homing-proxy")
				if err != nil {
					_, _ = runEgressCompose("stop", "homing-egress", "homing-proxy")
					req.Enabled = false
					_ = saveEgressConfig(req)
					writeJSON(w, 502, map[string]string{"error": "启动本机代理失败: " + err.Error() + " " + out})
					return
				}
				if st := waitContainerState("homing-proxy", true, 8*time.Second); st != "running" {
					logs := containerLogTail("homing-proxy", 700)
					_, _ = runEgressCompose("stop", "homing-egress", "homing-proxy")
					req.Enabled = false
					_ = saveEgressConfig(req)
					msg := "本机代理未运行（" + st + "）"
					if logs != "" {
						msg += "：" + logs
					}
					writeJSON(w, 502, map[string]string{"error": msg})
					return
				}
				if !waitEgressTun(req.Tun, 8*time.Second) {
					logs := containerLogTail("homing-proxy", 900)
					_, _ = runEgressCompose("stop", "homing-egress", "homing-proxy")
					req.Enabled = false
					_ = saveEgressConfig(req)
					msg := "本机代理 TUN " + req.Tun + " 未出现"
					if logs != "" {
						msg += "：" + logs
					}
					writeJSON(w, 502, map[string]string{"error": msg})
					return
				}
			} else {
				// 本机 TUN 模式不需要 homing-proxy
				_, _ = runEgressCompose("stop", "homing-proxy")
			}
			out, err := runEgressCompose("up", "-d", "--no-deps", "--force-recreate", "homing-egress")
			if err != nil {
				_, _ = runEgressCompose("stop", "homing-egress", "homing-proxy")
				req.Enabled = false
				_ = saveEgressConfig(req)
				writeJSON(w, 502, map[string]string{"error": "启动 homing-egress 失败: " + err.Error() + " " + out})
				return
			}
			waitContainerState("homing-egress", true, 5*time.Second)
		} else {
			// 停止失败不阻塞保存（容器可能本来就没启动）
			_, _ = runEgressCompose("stop", "homing-egress", "homing-proxy")
			waitContainerState("homing-egress", false, 5*time.Second)
		}
		time.Sleep(200 * time.Millisecond)
		cfg := loadEgressConfig()
		writeJSON(w, 200, map[string]interface{}{"ok": true, "config": cfg, "status": egressStatus(cfg)})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// egressTestURL 用于验证远端代理是否真的能出网。
// 选百度而不是 gstatic：百度从国内和海外都可达，远端代理无论在哪都能验证。
const (
	egressTestURL     = "https://www.baidu.com/"
	egressTestTimeout = 10 * time.Second
)

type egressTestResult struct {
	OK           bool   `json:"ok"`
	Mode         string `json:"mode"`
	Proxy        string `json:"proxy,omitempty"`
	Target       string `json:"target,omitempty"`
	LatencyMs    int64  `json:"latencyMs,omitempty"`
	StatusCode   int    `json:"statusCode,omitempty"`
	TunReady     bool   `json:"tunReady"`
	ProxyRunning bool   `json:"proxyRunning"`
	Message      string `json:"message,omitempty"`
}

func proxyAddr(cfg egressConfig) string {
	return net.JoinHostPort(cfg.ProxyHost, strconv.Itoa(cfg.ProxyPort))
}

func proxyURLText(cfg egressConfig) string {
	return cfg.ProxyType + "://" + proxyAddr(cfg)
}

// socks5DialContext 返回一个经 SOCKS5 代理建连的 DialContext，供 http.Transport 复用。
func socks5DialContext(proxyAddr, username, password string, timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := &net.Dialer{Timeout: timeout}
		conn, err := d.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("连接 SOCKS5 代理 %s 失败: %w", proxyAddr, err)
		}
		if err := socks5Handshake(conn, username, password, addr, timeout); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

func socks5Handshake(conn net.Conn, username, password, target string, timeout time.Duration) error {
	_ = conn.SetDeadline(time.Now().Add(timeout))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	methods := []byte{0x00}
	if username != "" {
		methods = append(methods, 0x02)
	}
	greet := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greet); err != nil {
		return fmt.Errorf("SOCKS5 握手失败: %w", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(conn, method); err != nil {
		return fmt.Errorf("SOCKS5 握手读取失败: %w", err)
	}
	if method[0] != 0x05 {
		return fmt.Errorf("对端不是 SOCKS5 代理（版本 %d）", method[0])
	}
	switch method[1] {
	case 0x00:
	case 0x02:
		if username == "" {
			return fmt.Errorf("SOCKS5 代理要求用户名密码")
		}
		u, p := []byte(username), []byte(password)
		if len(u) > 255 || len(p) > 255 {
			return fmt.Errorf("SOCKS5 用户名或密码过长")
		}
		auth := []byte{0x01, byte(len(u))}
		auth = append(auth, u...)
		auth = append(auth, byte(len(p)))
		auth = append(auth, p...)
		if _, err := conn.Write(auth); err != nil {
			return fmt.Errorf("SOCKS5 认证发送失败: %w", err)
		}
		ar := make([]byte, 2)
		if _, err := io.ReadFull(conn, ar); err != nil {
			return fmt.Errorf("SOCKS5 认证读取失败: %w", err)
		}
		if ar[1] != 0x00 {
			return fmt.Errorf("SOCKS5 用户名或密码错误")
		}
	default:
		return fmt.Errorf("SOCKS5 代理不支持所选认证方式（%d）", method[1])
	}

	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("测试地址无效: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("测试端口无效: %q", portText)
	}
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("测试域名过长")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("SOCKS5 CONNECT 发送失败: %w", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("SOCKS5 CONNECT 读取失败: %w", err)
	}
	if head[0] != 0x05 {
		return fmt.Errorf("SOCKS5 CONNECT 响应版本异常（%d）", head[0])
	}
	if head[1] != 0x00 {
		return fmt.Errorf("SOCKS5 代理拒绝连接（错误码 %d）", head[1])
	}
	skip := 0
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return fmt.Errorf("SOCKS5 CONNECT 读取失败: %w", err)
		}
		skip = int(l[0])
	case 0x04:
		skip = 16
	default:
		return fmt.Errorf("SOCKS5 CONNECT 响应地址类型异常（%d）", head[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, skip+2)); err != nil {
		return fmt.Errorf("SOCKS5 CONNECT 读取失败: %w", err)
	}
	return nil
}

// testRemoteEgress 经远端代理请求测试地址，返回耗时、HTTP 状态码和错误。
func testRemoteEgress(cfg egressConfig) (int64, int, error) {
	timeout := egressTestTimeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
	}
	if cfg.ProxyType == "socks5" {
		tr.DialContext = socks5DialContext(proxyAddr(cfg), cfg.ProxyUsername, cfg.ProxyPassword, timeout)
	} else {
		u := &url.URL{Scheme: "http", Host: proxyAddr(cfg)}
		if cfg.ProxyUsername != "" {
			u.User = url.UserPassword(cfg.ProxyUsername, cfg.ProxyPassword)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, egressTestURL, nil)
	if err != nil {
		return 0, 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return latency, 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return latency, resp.StatusCode, nil
}

// handleEgressTest 测试「出口到没到代理」：remote 模式直接经远端代理请求测试地址；
// tun 模式由宿主机提供 TUN，无法从管理台发起客户端流量，只回报 TUN 状态。
func handleEgressTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	cfg := loadEgressConfig()
	res := egressTestResult{Mode: cfg.Mode, TunReady: egressTunReady(cfg.Tun)}
	if cfg.Mode == "remote" {
		res.Proxy = proxyURLText(cfg)
		res.ProxyRunning = readProxyStatus().Running
	}
	if !cfg.Enabled {
		res.Message = "出口未启用：先打开「启用出口」并保存，再测试"
		writeJSON(w, 200, res)
		return
	}
	if cfg.Mode != "remote" {
		if res.TunReady {
			res.Message = "本机 TUN「" + cfg.Tun + "」已存在；该模式只对客户端源地址生效，管理台无法自测，想实测请改用「远程代理」模式"
		} else {
			res.Message = "本机 TUN「" + cfg.Tun + "」未发现：先在宿主机启动带 TUN 的 mihomo，或改用「远程代理」模式"
		}
		writeJSON(w, 200, res)
		return
	}
	if cfg.ProxyHost == "" || cfg.ProxyPort <= 0 {
		res.Message = "远端代理地址或端口未填写完整"
		writeJSON(w, 200, res)
		return
	}
	res.Target = egressTestURL
	latency, status, err := testRemoteEgress(cfg)
	res.LatencyMs = latency
	res.StatusCode = status
	if err != nil {
		res.Message = "代理测试失败：" + err.Error()
		log.Printf("egress test failed: proxy=%s target=%s err=%v", res.Proxy, egressTestURL, err)
		writeJSON(w, 200, res)
		return
	}
	if status < 200 || status >= 400 {
		res.Message = fmt.Sprintf("代理可达，但测试地址返回 HTTP %d（远端出口网络异常）", status)
		writeJSON(w, 200, res)
		return
	}
	res.OK = true
	res.Message = "代理连通"
	if !res.ProxyRunning {
		res.Message = "代理连通，但本机 mihomo 未运行，客户端流量进不了出口"
	}
	if !res.TunReady {
		res.Message = "代理连通，但 TUN「" + cfg.Tun + "」未就绪，客户端流量暂时进不了出口"
	}
	log.Printf("egress test ok: proxy=%s latency=%dms status=%d", res.Proxy, latency, status)
	writeJSON(w, 200, res)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	// 自研 homing-core 不需要管理员密码（只监听 127.0.0.1，由管理台独占访问）
	_ = os.MkdirAll(dataDir, 0o755)
	loadSystemSettings()
	if adminPass == "" {
		adminPass = "admin123"
		log.Printf("未设置 ADMIN_PASSWORD，首次启动使用默认密码 admin123，请登录后尽快修改")
	}
	loadAccount()
	// 让 plumber 的客户端列表始终跟 core 保持一致（core 重启/手动改数据也能自愈）
	refreshEgressClients()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			refreshEgressClients()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/login/2fa", handleLogin2FA)
	mux.HandleFunc("/api/logout", handleLogout)
	mux.HandleFunc("/api/account", requireAuth(handleAccount))
	mux.HandleFunc("/api/account/password", requireAuth(handleChangePassword))
	mux.HandleFunc("/api/account/totp/setup", requireAuth(handleTOTPSetup))
	mux.HandleFunc("/api/account/totp/qr", requireAuth(handleTOTPQR))
	mux.HandleFunc("/api/account/totp/enable", requireAuth(handleTOTPEnable))
	mux.HandleFunc("/api/account/totp/disable", requireAuth(handleTOTPDisable))
	mux.HandleFunc("/api/overview", requireAuth(handleOverview))
	mux.HandleFunc("/api/clients/create", requireAuth(handleCreate))
	mux.HandleFunc("/api/clients/toggle", requireAuth(handleToggle))
	mux.HandleFunc("/api/clients/proxy", requireAuth(handleClientProxy))
	mux.HandleFunc("/api/clients/proxy-self", requireAuth(handleClientProxySelf))
	mux.HandleFunc("/api/self/proxy", handleSelfProxy)
	mux.HandleFunc("/api/clients/delete", requireAuth(handleDelete))
	mux.HandleFunc("/api/clients/config", requireAuth(handleClientConfig))
	mux.HandleFunc("/api/clients/qr", requireAuth(handleClientQR))
	mux.HandleFunc("/api/apply-endpoint", requireAuth(handleApplyEndpoint))
	mux.HandleFunc("/api/dns/test", requireAuth(handleDNSTest))
	mux.HandleFunc("/api/network", requireAuth(handleGetNetwork))
	mux.HandleFunc("/api/system-settings", requireAuth(handleSystemSettings))
	mux.HandleFunc("/api/client-probe", requireAuth(handleClientProbe))
	mux.HandleFunc("/api/connect-settings", requireAuth(handleConnectSettings))
	mux.HandleFunc("/api/connect-settings/detect", requireAuth(handleDetectAddresses))
	mux.HandleFunc("/api/egress", requireAuth(handleEgress))
	mux.HandleFunc("/api/egress/test", requireAuth(handleEgressTest))
	mux.HandleFunc("/api/network/port", requireAuth(handleSetPort))
	mux.HandleFunc("/api/stun-config", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		if r.Method == "POST" {
			handleSetConfig(w, r)
		} else {
			handleGetConfig(w, r)
		}
	})
	// 管理台页面禁用浏览器缓存：升级后若继续用缓存的旧 HTML/JS，
	// 会和新的接口（如二维码改为 PNG）不匹配，出现弹窗乱码/旧按钮失效。
	serveUI := func(w http.ResponseWriter, r *http.Request) {
		b, _ := uiFS.ReadFile("ui.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		_, _ = w.Write(b)
	}
	mux.HandleFunc("/login", serveUI)
	mux.HandleFunc("/", serveUI)
	log.Printf("gateway-admin listening on %s (wg=%s stun=%s data=%s)", listenAddr, coreBase, stunBase, dataDir)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}

// ---------- 归途原生配置格式（不再用 .conf） ----------
//
//	{
//	  "format": "homing-profile/1",
//	  "name": "uos-pc",
//	  "privateKey": "...",
//	  "addresses": ["10.8.0.5/32", "fdcc:...::cafe:5/128"],
//	  "mtu": 1420,
//	  "dns": ["192.168.1.1"],
//	  "peers": [{"publicKey":"...","presharedKey":"...","allowedIps":["0.0.0.0/0","::/0"],
//	             "endpoint":"203.0.113.10:9612","persistentKeepalive":25}],
//	  "discovery": {"enabled":true,"record":"vpn.example.com","autoFollow":true,"preferred":"auto","interval":30},
//	  "endpoints": ["203.0.113.10:9612", "192.168.1.10:51830"]
//	}
func clientDisplayName(id string) string {
	clients, err := listClients()
	if err != nil {
		return ""
	}
	for _, c := range clients {
		if fmt.Sprint(c["id"]) == id {
			if n, ok := c["name"].(string); ok {
				return n
			}
		}
	}
	return ""
}

// homingProfileFromConf 把 隧道核心的配置 解析成归途 profile。
func homingProfileFromConf(name, conf string) (map[string]interface{}, error) {
	prof := map[string]interface{}{
		"format": "homing-profile/1",
		"name":   name,
	}
	var privateKey string
	var addresses, dns []string
	mtu := 0
	var peers []map[string]interface{}
	section := ""
	var cur map[string]interface{}

	addList := func(dst *[]string, v string) {
		for _, part := range strings.Split(v, ",") {
			if s := strings.TrimSpace(part); s != "" {
				*dst = append(*dst, s)
			}
		}
	}
	for _, raw := range strings.Split(conf, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			switch strings.ToLower(strings.Trim(line, "[] ")) {
			case "interface":
				section = "iface"
			case "peer":
				section = "peer"
				cur = map[string]interface{}{}
				peers = append(peers, cur)
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.TrimSpace(v)
		if section == "iface" {
			switch key {
			case "privatekey":
				privateKey = val
			case "address":
				addList(&addresses, val)
			case "dns":
				addList(&dns, val)
			case "mtu":
				if n, err := strconv.Atoi(val); err == nil {
					mtu = n
				}
			}
		} else if section == "peer" && cur != nil {
			switch key {
			case "publickey":
				cur["publicKey"] = val
			case "presharedkey":
				cur["presharedKey"] = val
			case "allowedips":
				var ips []string
				addList(&ips, val)
				cur["allowedIps"] = ips
			case "endpoint":
				cur["endpoint"] = val
			case "persistentkeepalive":
				if n, err := strconv.Atoi(val); err == nil {
					cur["persistentKeepalive"] = n
				}
			}
		}
	}
	if privateKey == "" || len(peers) == 0 {
		return nil, fmt.Errorf("解析客户端配置失败（缺 PrivateKey 或 Peer）")
	}
	if mtu == 0 {
		mtu = 1420
	}
	prof["privateKey"] = privateKey
	prof["addresses"] = addresses
	prof["mtu"] = mtu
	if len(dns) > 0 {
		prof["dns"] = dns
	}
	prof["peers"] = peers
	return prof, nil
}

// clientProfileJSON 生成客户端的原生 JSON 配置（紧凑，便于编码进二维码）。
func clientProfileJSON(id string) ([]byte, error) {
	code, b, err := coreDo("GET", "/api/client/"+id+"/configuration", nil)
	if err != nil || code != 200 {
		return nil, fmt.Errorf("获取客户端配置失败: %v %s", err, string(b))
	}
	var prof map[string]interface{}
	trimmed := strings.TrimSpace(string(b))
	if strings.HasPrefix(trimmed, "{") {
		// homing-core 直接返回归途 profile JSON
		if err := json.Unmarshal(b, &prof); err != nil {
			return nil, fmt.Errorf("解析客户端配置失败: %w", err)
		}
	} else {
		// 兼容旧 隧道核心 .conf
		name := clientDisplayName(id)
		if name == "" {
			name = "homing-" + id
		}
		prof, err = homingProfileFromConf(name, string(b))
		if err != nil {
			return nil, err
		}
	}
	enrichProfile(prof)
	return json.Marshal(prof)
}

// enrichProfile 补上「当前端点 / 公告板 / 候选端点」（core 返回的 profile 不含这些环境信息）。
func enrichProfile(prof map[string]interface{}) {
	// 隧道内自助接口地址（客户端通过隧道访问 10.8.0.1 上的管理台）
	prof["selfApi"] = selfAPIBase()
	// 1) 连接方式：peer endpoint / 候选端点 / 公告板偏好（STUN 开关 + v4/v6 优先级）
	applyConnectToProfile(prof)
	// 2) 客户端探针默认参数（管理台「客户端探针默认参数」保存过才注入；
	//    客户端配置里已有 probe 时以客户端为准）
	if v, configured := loadClientProbeDefaults(); configured {
		if _, has := prof["probe"]; !has {
			prof["probe"] = clientProbeToMap(v)
		}
	}
}
