package main

// UPnP 自动端口映射：启动后在路由器（IGD）上自动建立
// 「外部端口 → 192.168.1.10:本地监听端口」的 UDP 静态映射，免去手动配端口转发。
//
// 说明：
//   - 只能作用于**本机所在的这一层路由器**（比如 TP-LINK 192.168.1.1）；
//   - 如果上面还有一层运营商 NAT/CGNAT，那层的放行由运营商决定，UPnP 够不着；
//   - 映射成功后我们会把路由器给的外部端口记录到状态里，配合 STUN 结果判断
//     “静态映射是否直达公网”（两者端口一致就说明没有再被上层改写）。

import (
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type upnpState struct {
	Enabled      bool      `json:"enabled"`
	IGD          string    `json:"igd,omitempty"`
	InternalIP   string    `json:"internalIp,omitempty"`
	InternalPort int       `json:"internalPort,omitempty"`
	ExternalPort int       `json:"externalPort,omitempty"`
	ExternalIP   string    `json:"externalIp,omitempty"`
	LastOK       time.Time `json:"lastOk,omitempty"`
	LastErr      string    `json:"lastErr,omitempty"`
}

var (
	upnpMu  sync.Mutex
	upnpCur upnpState
)

func upnpSnapshot() upnpState {
	upnpMu.Lock()
	defer upnpMu.Unlock()
	return upnpCur
}

func setUPnP(fn func(*upnpState)) {
	upnpMu.Lock()
	fn(&upnpCur)
	upnpMu.Unlock()
}

// upnpLoop 每隔一段时间确保映射存在（路由器重启/端口变化后自动重建）。
func upnpLoop() {
	setUPnP(func(s *upnpState) { s.Enabled = true })
	for {
		if err := upnpEnsureOnce(); err != nil {
			setUPnP(func(s *upnpState) { s.LastErr = err.Error() })
			log.Printf("UPnP: %v", err)
		}
		time.Sleep(*upnpInterval)
	}
}

// localListenPort 返回实际绑定的本地 UDP 端口（-listen 为 :0 时由内核分配，
// 必须用实际值，不能从 -listen 字符串里解析）。
func localListenPort() int {
	return actualListenPort
}

func upnpEnsureOnce() error {
	locs := ssdpDiscover()
	if len(locs) == 0 {
		return fmt.Errorf("没有发现 UPnP 路由器（SSDP 无响应，可能路由器关闭了 UPnP）")
	}
	var lastErr error
	for _, loc := range locs {
		root, err := fetchDeviceDesc(loc)
		if err != nil {
			lastErr = err
			continue
		}
		svc, ctrl, host, ok := findWANService(root, loc)
		if !ok {
			lastErr = fmt.Errorf("%s 上没有 WANIPConnection 服务", loc)
			continue
		}
		localIP := localIPTo(host)
		if localIP == "" {
			lastErr = fmt.Errorf("无法确定本机在 %s 网段的地址", host)
			continue
		}
		internalPort := localListenPort()
		extPort, err := chooseAndMap(ctrl, svc, localIP, internalPort)
		if err != nil {
			lastErr = err
			continue
		}
		extIP, _ := upnpGetExternalIP(ctrl, svc)
		setUPnP(func(s *upnpState) {
			s.IGD = loc
			s.InternalIP = localIP
			s.InternalPort = internalPort
			s.ExternalPort = extPort
			s.ExternalIP = extIP
			s.LastOK = time.Now()
			s.LastErr = ""
		})
		log.Printf("UPnP: 映射已建立 %d/UDP -> %s:%d（路由器 %s，WAN IP %s）",
			extPort, localIP, internalPort, host, extIP)
		// 用 STUN 结果对比：静态外部端口和 STUN 看到的一致，说明上层没再改写端口
		if ep := getEndpoint(); ep != nil {
			if p, err := strconv.Atoi(strings.TrimSpace(ep.Address[strings.LastIndex(ep.Address, ":")+1:])); err == nil {
				if p != extPort {
					log.Printf("UPnP: 注意，公网 STUN 端口 %d 与路由器外部端口 %d 不一致 —— 上层还有一层 NAT 改写了端口，静态映射不会直达公网", p, extPort)
				} else {
					log.Printf("UPnP: 公网 STUN 端口与路由器外部端口一致（%d），静态映射直达公网", extPort)
				}
			}
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的 UPnP 路由器")
	}
	return lastErr
}

// ---------- SSDP 发现 ----------

func ssdpDiscover() []string {
	target, err := net.ResolveUDPAddr("udp4", "239.255.255.250:1900")
	if err != nil {
		return nil
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil
	}
	defer conn.Close()
	msg := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n\r\n"
	for i := 0; i < 2; i++ {
		_, _ = conn.WriteToUDP([]byte(msg), target)
	}
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	buf := make([]byte, 4096)
	seen := map[string]bool{}
	var out []string
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		text := string(buf[:n])
		for _, line := range strings.Split(text, "\r\n") {
			if idx := strings.Index(strings.ToLower(line), "location:"); idx == 0 {
				loc := strings.TrimSpace(line[len("location:"):])
				if loc != "" && !seen[loc] {
					seen[loc] = true
					out = append(out, loc)
				}
			}
		}
	}
	return out
}

// ---------- 设备描述 ----------

type upnpService struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

type upnpDevice struct {
	FriendlyName string        `xml:"friendlyName"`
	Services     []upnpService `xml:"serviceList>service"`
	Devices      []upnpDevice  `xml:"deviceList>device"`
}

type upnpRoot struct {
	URLBase string     `xml:"URLBase"`
	Device  upnpDevice `xml:"device"`
}

func fetchDeviceDesc(loc string) (*upnpRoot, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(loc)
	if err != nil {
		return nil, fmt.Errorf("获取设备描述失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var root upnpRoot
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("解析设备描述失败: %w", err)
	}
	return &root, nil
}

func findWANService(root *upnpRoot, loc string) (serviceType, controlURL, host string, ok bool) {
	var walk func(dev upnpDevice, base string) bool
	walk = func(dev upnpDevice, base string) bool {
		for _, svc := range dev.Services {
			st := svc.ServiceType
			if strings.Contains(st, "WANIPConnection") || strings.Contains(st, "WANPPPConnection") {
				serviceType = st
				controlURL = resolveURL(base, svc.ControlURL)
				host = hostOf(controlURL)
				return true
			}
		}
		for _, child := range dev.Devices {
			if walk(child, base) {
				return true
			}
		}
		return false
	}
	base := root.URLBase
	if base == "" {
		base = loc
	}
	ok = walk(root.Device, base)
	return
}

func resolveURL(base, ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	// 去掉 base 的 path 后拼接
	if i := strings.Index(base, "://"); i >= 0 {
		rest := base[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			base = base[:i+3+j]
		}
	}
	if !strings.HasSuffix(base, "/") && !strings.HasPrefix(ref, "/") {
		base += "/"
	}
	return base + ref
}

func hostOf(u string) string {
	s := u
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if j := strings.Index(s, "/"); j >= 0 {
		s = s[:j]
	}
	return s
}

// localIPTo 找到本机到目标主机的源地址（用它作为 UPnP 的 InternalClient）。
func localIPTo(host string) string {
	h := host
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	conn, err := net.DialTimeout("udp4", net.JoinHostPort(h, "9"), 2*time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

// ---------- SOAP 调用 ----------

func soapCall(ctrlURL, serviceType, action string, args []string) (string, error) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?>`)
	body.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	body.WriteString(`<u:` + action + ` xmlns:u="` + serviceType + `">`)
	for _, a := range args {
		body.WriteString(a)
	}
	body.WriteString(`</u:` + action + `></s:Body></s:Envelope>`)

	req, err := http.NewRequest("POST", ctrlURL, strings.NewReader(body.String()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+serviceType+"#"+action+`"`)
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	text := string(data)
	if resp.StatusCode >= 300 {
		code := ""
		if i := strings.Index(text, "<errorCode>"); i >= 0 {
			if j := strings.Index(text[i:], "</errorCode>"); j > 0 {
				code = strings.TrimSpace(text[i+len("<errorCode>") : i+j])
			}
		}
		return text, fmt.Errorf("%s 失败: HTTP %d %s", action, resp.StatusCode, code)
	}
	return text, nil
}

func upnpAddMapping(ctrlURL, serviceType, internalIP string, externalPort, internalPort int) error {
	args := []string{
		"<NewRemoteHost></NewRemoteHost>",
		fmt.Sprintf("<NewExternalPort>%d</NewExternalPort>", externalPort),
		"<NewProtocol>UDP</NewProtocol>",
		fmt.Sprintf("<NewInternalPort>%d</NewInternalPort>", internalPort),
		"<NewInternalClient>" + internalIP + "</NewInternalClient>",
		"<NewEnabled>1</NewEnabled>",
		"<NewPortMappingDescription>homing-punch</NewPortMappingDescription>",
		"<NewLeaseDuration>0</NewLeaseDuration>",
	}
	_, err := soapCall(ctrlURL, serviceType, "AddPortMapping", args)
	return err
}

func upnpGetExternalIP(ctrlURL, serviceType string) (string, error) {
	text, err := soapCall(ctrlURL, serviceType, "GetExternalIPAddress", nil)
	if err != nil {
		return "", err
	}
	const tag = "<NewExternalIPAddress>"
	if i := strings.Index(text, tag); i >= 0 {
		if j := strings.Index(text[i:], "</NewExternalIPAddress>"); j > 0 {
			return strings.TrimSpace(text[i+len(tag) : i+j]), nil
		}
	}
	return "", fmt.Errorf("响应里没有 NewExternalIPAddress")
}

// chooseAndMap 先试与本地端口相同的外部端口，占用则随机换几个。
func chooseAndMap(ctrlURL, serviceType, internalIP string, internalPort int) (int, error) {
	var candidates []int
	if *upnpExternalPort > 0 {
		candidates = append(candidates, *upnpExternalPort)
	} else {
		candidates = append(candidates, internalPort)
		// 随机几个备用外部端口
		seed := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			candidates = append(candidates, 40000+int((seed+int64(i*7919))%20000))
		}
	}
	var lastErr error
	for _, p := range candidates {
		if err := upnpAddMapping(ctrlURL, serviceType, internalIP, p, internalPort); err != nil {
			lastErr = err
			continue
		}
		return p, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用外部端口")
	}
	return 0, lastErr
}
