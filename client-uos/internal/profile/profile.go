// Package profile 定义客户端连接配置（profile），并支持从标准 .conf 导入。
//
// 设计要点：
//   - profile 同时支持 IPv4/IPv6 地址与 AllowedIPs（双栈隧道）；
//   - 端点发现（DNS TXT）是可选能力，网关公网端口是动态的（STUN 打洞），
//     所以客户端需要能从 TXT 记录里拿到最新 v4/v6 端点并热切换。
package profile

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Peer 一个对端（网关服务端）。
type Peer struct {
	PublicKey           string   `json:"publicKey"`
	PresharedKey        string   `json:"presharedKey,omitempty"`
	AllowedIPs          []string `json:"allowedIps"`
	Endpoint            string   `json:"endpoint,omitempty"`
	PersistentKeepalive int      `json:"persistentKeepalive,omitempty"`
}

// Discovery 端点自动发现配置（DNS TXT 公告板）。
type Discovery struct {
	Enabled   bool   `json:"enabled"`
	Record    string `json:"record,omitempty"`    // 例如 vpn.example.com
	AutoFollow bool  `json:"autoFollow"`          // 发现到新端点后自动热换
	Preferred string `json:"preferred,omitempty"` // auto | v4 | v6
	DNS       string `json:"dns,omitempty"`       // 可选：指定解析 TXT 用的 DNS 服务器 host:port
	Interval  int    `json:"interval,omitempty"`  // 轮询间隔秒，默认 30
}

// Probe 隧道内 UDP 探针参数（默认值与 Android 客户端 2.3.6 一致）。
type Probe struct {
	Enabled          bool   `json:"enabled"`
	Interval         int    `json:"interval,omitempty"`         // 活跃档间隔（秒）
	IdleInterval     int    `json:"idleInterval,omitempty"`     // 空闲档间隔（秒）
	IdleAfter        int    `json:"idleAfter,omitempty"`        // 多久没流量算空闲（秒）
	Failures         int    `json:"failures,omitempty"`         // 进入疑似故障的失败阈值（默认 1）
	TimeoutMs        int    `json:"timeoutMs,omitempty"`        // 单次回包超时
	RecoveryInterval int    `json:"recoveryInterval,omitempty"` // 疑似故障快探间隔（秒）
	SoftGrace        int    `json:"softGrace,omitempty"`        // 公告板没变时「只探不重建」宽限（秒）
	Host             string `json:"host,omitempty"`             // 回显地址，默认 10.8.0.1
	Port             int    `json:"port,omitempty"`             // 回显端口，默认 51824
}

// DefaultProbe 返回默认探针参数（与 Android 2.3.6 相同）。
func DefaultProbe() Probe {
	return Probe{
		Enabled:          true,
		Interval:         10,
		IdleInterval:     30,
		IdleAfter:        5,
		Failures:         1,
		TimeoutMs:        1500,
		RecoveryInterval: 5,
		SoftGrace:        30,
		Host:             "10.8.0.1",
		Port:             51824,
	}
}

// NormalizeProbe 补齐/限制探针参数范围。
func NormalizeProbe(p *Probe) {
	if p.Host == "" {
		p.Host = "10.8.0.1"
	}
	if p.Port <= 0 || p.Port > 65535 {
		p.Port = 51824
	}
	if p.Interval < 3 || p.Interval > 120 {
		p.Interval = 10
	}
	if p.IdleInterval < 3 || p.IdleInterval > 300 {
		p.IdleInterval = max(30, p.Interval*2)
	}
	if p.IdleAfter < 2 || p.IdleAfter > 60 {
		p.IdleAfter = 5
	}
	if p.Failures < 1 || p.Failures > 10 {
		p.Failures = 1
	}
	if p.TimeoutMs < 300 || p.TimeoutMs > 5000 {
		p.TimeoutMs = 1500
	}
	if p.RecoveryInterval < 2 || p.RecoveryInterval > 30 {
		p.RecoveryInterval = 5
	}
	if p.SoftGrace < 10 || p.SoftGrace > 120 {
		p.SoftGrace = 30
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Profile 一份客户端连接配置。
type Profile struct {
	Name       string    `json:"name"`
	PrivateKey string    `json:"privateKey"`
	Addresses  []string  `json:"addresses"` // 10.8.0.2/32, fdcc:...::2/128
	MTU        int       `json:"mtu"`
	DNS        []string  `json:"dns"`
	Peers      []Peer    `json:"peers"`
	Discovery  Discovery `json:"discovery"`
	Probe      Probe     `json:"probe"`
	// Endpoints 是备用端点列表：守护进程在“一直握手不上”时会依次尝试，
	// 这样同一份配置在「家里内网 / 外网」都能连上（内网走中继、外网走打洞）。
	Endpoints []string `json:"endpoints,omitempty"`
	// Userspace=true 时强制用用户态 内置隧道内核（内核没有 wireguard 模块的机器用）
	Userspace bool `json:"userspace,omitempty"`
}

// CandidateEndpoints 返回按优先级尝试的端点列表（去重，主 Endpoint 在最前）。
func (p *Profile) CandidateEndpoints() []string {
	seen := make(map[string]bool, 4)
	out := make([]string, 0, 4)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, peer := range p.Peers {
		add(peer.Endpoint)
	}
	for _, e := range p.Endpoints {
		add(e)
	}
	return out
}

// Normalize 补齐默认值并做基本校验。
func (p *Profile) Normalize() error {
	if p.MTU <= 0 {
		p.MTU = 1420
	}
	if strings.TrimSpace(p.PrivateKey) == "" {
		return fmt.Errorf("配置缺少 PrivateKey")
	}
	if len(p.Peers) == 0 {
		return fmt.Errorf("配置缺少 [Peer]")
	}
	for i := range p.Peers {
		if strings.TrimSpace(p.Peers[i].PublicKey) == "" {
			return fmt.Errorf("配置缺少 Peer 的 PublicKey")
		}
		if len(p.Peers[i].AllowedIPs) == 0 {
			p.Peers[i].AllowedIPs = []string{"0.0.0.0/0", "::/0"}
		}
	}
	if p.Discovery.Interval <= 0 {
		p.Discovery.Interval = 30
	}
	if p.Discovery.Preferred == "" {
		p.Discovery.Preferred = "auto"
	}
	if p.Probe.Interval == 0 && p.Probe.IdleInterval == 0 && p.Probe.Port == 0 {
		p.Probe = DefaultProbe()
	}
	NormalizeProbe(&p.Probe)
	if strings.TrimSpace(p.Name) == "" {
		p.Name = "默认配置"
	}
	return nil
}

// ParseJSON 解析 profile JSON（也兼容直接把 .conf 内容传进来）。
func ParseJSON(data []byte) (*Profile, error) {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "{") {
		p := Profile{Probe: DefaultProbe()}
		if err := json.Unmarshal([]byte(trimmed), &p); err != nil {
			return nil, fmt.Errorf("配置 JSON 解析失败: %w", err)
		}
		if err := p.Normalize(); err != nil {
			return nil, err
		}
		return &p, nil
	}
	return ParseConf(trimmed)
}

// ParseConf 把标准 .conf 解析成 profile。
// 支持 Interface 段的 PrivateKey / Address / DNS / MTU / ListenPort，
// Peer 段的 PublicKey / PresharedKey / AllowedIPs / Endpoint / PersistentKeepalive。
func ParseConf(text string) (*Profile, error) {
	p := &Profile{Name: "导入的配置", Probe: DefaultProbe()}
	section := ""
	peer := -1
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			// 服务端会附带 # homing:key=value 元数据（公告板/候选端点），标准 隧道 客户端会忽略
			parseMetaLine(p, line)
			continue
		}
		if strings.HasPrefix(line, "[") {
			switch strings.ToLower(strings.Trim(line, "[] ")) {
			case "interface":
				section = "interface"
			case "peer":
				section = "peer"
				p.Peers = append(p.Peers, Peer{})
				peer = len(p.Peers) - 1
			default:
				section = ""
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.TrimSpace(v)
		switch section {
		case "interface":
			switch key {
			case "privatekey":
				p.PrivateKey = val
			case "address":
				p.Addresses = append(p.Addresses, splitList(val)...)
			case "dns":
				p.DNS = append(p.DNS, splitList(val)...)
			case "mtu":
				if n, err := strconv.Atoi(val); err == nil {
					p.MTU = n
				}
			}
		case "peer":
			if peer < 0 {
				continue
			}
			switch key {
			case "publickey":
				p.Peers[peer].PublicKey = val
			case "presharedkey":
				p.Peers[peer].PresharedKey = val
			case "allowedips":
				p.Peers[peer].AllowedIPs = append(p.Peers[peer].AllowedIPs, splitList(val)...)
			case "endpoint":
				p.Peers[peer].Endpoint = val
			case "persistentkeepalive":
				if n, err := strconv.Atoi(val); err == nil {
					p.Peers[peer].PersistentKeepalive = n
				}
			}
		}
	}
	if err := p.Normalize(); err != nil {
		return nil, err
	}
	return p, nil
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseMetaLine 解析服务端下发的 "# homing:key=value" 元数据行。
// 例如：
//
//	# homing:discovery.record=vpn.example.com
//	# homing:discovery.enabled=true
//	# homing:discovery.autoFollow=true
//	# homing:endpoints=203.0.113.10:9612,192.168.1.10:51830,[2001:db8::10]:51820
func parseMetaLine(p *Profile, line string) {
	meta := strings.TrimSpace(strings.TrimPrefix(line, "#"))
	if !strings.HasPrefix(meta, "homing:") {
		return
	}
	key, value, ok := strings.Cut(strings.TrimPrefix(meta, "homing:"), "=")
	if !ok {
		return
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	switch key {
	case "discovery.record":
		if value != "" {
			p.Discovery.Record = value
			p.Discovery.Enabled = true
			p.Discovery.AutoFollow = true
		}
	case "discovery.enabled":
		p.Discovery.Enabled = value == "true" || value == "1"
	case "discovery.autofollow", "discovery.autoFollow":
		p.Discovery.AutoFollow = value == "true" || value == "1"
	case "discovery.preferred":
		p.Discovery.Preferred = value
	case "discovery.interval":
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			p.Discovery.Interval = n
		}
	case "discovery.dns":
		p.Discovery.DNS = value
	case "endpoints":
		for _, e := range strings.Split(value, ",") {
			if s := strings.TrimSpace(e); s != "" {
				p.Endpoints = append(p.Endpoints, s)
			}
		}
	case "userspace":
		p.Userspace = value == "true" || value == "1"
	case "probe.enabled":
		p.Probe.Enabled = value == "true" || value == "1"
	case "probe.interval":
		if n, err := strconv.Atoi(value); err == nil {
			p.Probe.Interval = n
		}
	case "probe.idleinterval":
		if n, err := strconv.Atoi(value); err == nil {
			p.Probe.IdleInterval = n
		}
	case "probe.idleafter":
		if n, err := strconv.Atoi(value); err == nil {
			p.Probe.IdleAfter = n
		}
	case "probe.failures":
		if n, err := strconv.Atoi(value); err == nil {
			p.Probe.Failures = n
		}
	case "probe.timeoutms":
		if n, err := strconv.Atoi(value); err == nil {
			p.Probe.TimeoutMs = n
		}
	case "probe.recoveryinterval":
		if n, err := strconv.Atoi(value); err == nil {
			p.Probe.RecoveryInterval = n
		}
	case "probe.softgrace":
		if n, err := strconv.Atoi(value); err == nil {
			p.Probe.SoftGrace = n
		}
	case "probe.host":
		if value != "" {
			p.Probe.Host = value
		}
	case "probe.port":
		if n, err := strconv.Atoi(value); err == nil {
			p.Probe.Port = n
		}
	}
}