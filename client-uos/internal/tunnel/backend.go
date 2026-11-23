package tunnel

import (
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"homing/client/internal/profile"
)

// PeerState 是后端（内核或用户态）里第一个 peer 的运行时状态。
type PeerState struct {
	Endpoint  string
	Handshake time.Time
	Rx        int64
	Tx        int64
}

// Backend 抽象两种 隧道 实现：
//   - kernel：内核 wireguard 模块 + 内核接口(netlink)；
//   - userspace：内置隧道内核（纯 Go，不依赖内核模块）。
//
// 内核不支持时（老内核/模块缺失）自动降级到 userspace，UOS 上常见。
type Backend interface {
	Kind() string
	Setup(p *profile.Profile, fwmark int) error
	PeerState() (PeerState, error)
	SetEndpoint(addr *net.UDPAddr, peerIndex int) error
	Close() error
}

// ---------------------------------------------------------------- 内核后端

type kernelBackend struct{}

func (k *kernelBackend) Kind() string { return "kernel" }

func (k *kernelBackend) Setup(p *profile.Profile, fwmark int) error {
	if _, err := run("ip", "link", "add", "dev", Iface, "type", "wireguard"); err != nil {
		return fmt.Errorf("创建内核隧道接口失败: %w", err)
	}
	client, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("连接内核隧道失败（模块未加载？）: %w", err)
	}
	defer client.Close()
	if err := configureKernel(client, p, fwmark); err != nil {
		return err
	}
	return nil
}

func (k *kernelBackend) PeerState() (PeerState, error) {
	var st PeerState
	client, err := wgctrl.New()
	if err != nil {
		return st, err
	}
	defer client.Close()
	dev, err := client.Device(Iface)
	if err != nil {
		return st, err
	}
	if len(dev.Peers) == 0 {
		return st, fmt.Errorf("设备没有 peer")
	}
	peer := dev.Peers[0]
	if peer.Endpoint != nil {
		st.Endpoint = peer.Endpoint.String()
	}
	st.Handshake = peer.LastHandshakeTime
	st.Rx = peer.ReceiveBytes
	st.Tx = peer.TransmitBytes
	return st, nil
}

func (k *kernelBackend) SetEndpoint(addr *net.UDPAddr, peerIndex int) error {
	client, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer client.Close()
	dev, err := client.Device(Iface)
	if err != nil {
		return err
	}
	if len(dev.Peers) == 0 {
		return fmt.Errorf("设备没有 peer")
	}
	if peerIndex < 0 || peerIndex >= len(dev.Peers) {
		peerIndex = 0
	}
	return client.ConfigureDevice(Iface, wgtypes.Config{
		Peers: []wgtypes.PeerConfig{{
			PublicKey: dev.Peers[peerIndex].PublicKey,
			Endpoint:  addr,
		}},
	})
}

func (k *kernelBackend) Close() error { return nil } // 接口由 Down 里的 ip link del 负责

func configureKernel(client *wgctrl.Client, p *profile.Profile, fwmark int) error {
	priv, err := wgtypes.ParseKey(p.PrivateKey)
	if err != nil {
		return fmt.Errorf("PrivateKey 格式不对: %w", err)
	}
	peers := make([]wgtypes.PeerConfig, 0, len(p.Peers))
	for _, peer := range p.Peers {
		pub, err := wgtypes.ParseKey(peer.PublicKey)
		if err != nil {
			return fmt.Errorf("PublicKey 格式不对: %w", err)
		}
		cfg := wgtypes.PeerConfig{PublicKey: pub, ReplaceAllowedIPs: true}
		if peer.PresharedKey != "" {
			psk, err := wgtypes.ParseKey(peer.PresharedKey)
			if err != nil {
				return fmt.Errorf("PresharedKey 格式不对: %w", err)
			}
			cfg.PresharedKey = &psk
		}
		if peer.Endpoint != "" {
			udpAddr, err := resolveEndpoint(peer.Endpoint)
			if err != nil {
				return fmt.Errorf("Endpoint %s 解析失败: %w", peer.Endpoint, err)
			}
			cfg.Endpoint = udpAddr
		}
		if peer.PersistentKeepalive > 0 {
			ka := time.Duration(peer.PersistentKeepalive) * time.Second
			cfg.PersistentKeepaliveInterval = &ka
		}
		for _, cidr := range peer.AllowedIPs {
			_, ipnet, err := net.ParseCIDR(cidr)
			if err != nil {
				return fmt.Errorf("AllowedIPs %s 格式不对: %w", cidr, err)
			}
			cfg.AllowedIPs = append(cfg.AllowedIPs, *ipnet)
		}
		peers = append(peers, cfg)
	}
	return client.ConfigureDevice(Iface, wgtypes.Config{
		PrivateKey:   &priv,
		FirewallMark: &fwmark,
		ReplacePeers: true,
		Peers:        peers,
	})
}

// ---------------------------------------------------------------- 用户态后端

type userspaceBackend struct {
	tunDev tun.Device
	dev    *device.Device
}

func (u *userspaceBackend) Kind() string { return "userspace" }

func (u *userspaceBackend) Setup(p *profile.Profile, fwmark int) error {
	if _, err := run("ip", "link", "show", "dev", "homingupchk0"); err == nil {
		_, _ = run("ip", "link", "del", "dev", "homingupchk0")
	}
	tunDev, err := tun.CreateTUN(Iface, p.MTU)
	if err != nil {
		return fmt.Errorf("创建 TUN 设备失败（/dev/net/tun 不可用？UOS 上可执行 modprobe tun）: %w", err)
	}
	u.tunDev = tunDev
	logger := device.NewLogger(device.LogLevelError, "homing/userspace: ")
	u.dev = device.NewDevice(tunDev, conn.NewDefaultBind(), logger)
	if err := u.dev.IpcSet(uapiConfig(p, fwmark)); err != nil {
		u.dev.Close()
		return fmt.Errorf("用户态隧道配置失败: %w", err)
	}
	if err := u.dev.Up(); err != nil {
		u.dev.Close()
		return fmt.Errorf("启动用户态隧道失败: %w", err)
	}
	return nil
}

// uapiConfig 生成 内置隧道内核 的 UAPI 配置文本。
func uapiConfig(p *profile.Profile, fwmark int) string {
	var sb strings.Builder
	priv, err := wgtypes.ParseKey(p.PrivateKey)
	if err == nil {
		sb.WriteString("private_key=" + hex.EncodeToString(priv[:]) + "\n")
	}
	sb.WriteString("listen_port=0\n")
	sb.WriteString(fmt.Sprintf("fwmark=%d\n", fwmark))
	sb.WriteString("replace_peers=true\n")
	for _, peer := range p.Peers {
		pub, err := wgtypes.ParseKey(peer.PublicKey)
		if err != nil {
			continue
		}
		sb.WriteString("public_key=" + hex.EncodeToString(pub[:]) + "\n")
		if peer.PresharedKey != "" {
			if psk, err := wgtypes.ParseKey(peer.PresharedKey); err == nil {
				sb.WriteString("preshared_key=" + hex.EncodeToString(psk[:]) + "\n")
			}
		}
		if peer.Endpoint != "" {
			if addr, err := resolveEndpoint(peer.Endpoint); err == nil {
				sb.WriteString("endpoint=" + addr.String() + "\n")
			}
		}
		if peer.PersistentKeepalive > 0 {
			sb.WriteString(fmt.Sprintf("persistent_keepalive_interval=%d\n", peer.PersistentKeepalive))
		}
		for _, cidr := range peer.AllowedIPs {
			sb.WriteString("allowed_ip=" + cidr + "\n")
		}
	}
	return sb.String()
}

func (u *userspaceBackend) PeerState() (PeerState, error) {
	var st PeerState
	if u.dev == nil {
		return st, fmt.Errorf("用户态设备未初始化")
	}
	text, err := u.dev.IpcGet()
	if err != nil {
		return st, err
	}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "endpoint":
			st.Endpoint = value
		case "last_handshake_time_sec":
			var sec int64
			if _, err := fmt.Sscanf(value, "%d", &sec); err == nil && sec > 0 {
				st.Handshake = time.Unix(sec, 0)
			}
		case "rx_bytes":
			var n int64
			if _, err := fmt.Sscanf(value, "%d", &n); err == nil {
				st.Rx = n
			}
		case "tx_bytes":
			var n int64
			if _, err := fmt.Sscanf(value, "%d", &n); err == nil {
				st.Tx = n
			}
		}
	}
	return st, nil
}

func (u *userspaceBackend) SetEndpoint(addr *net.UDPAddr, peerIndex int) error {
	if u.dev == nil {
		return fmt.Errorf("用户态设备未初始化")
	}
	text, err := u.dev.IpcGet()
	if err != nil {
		return err
	}
	// 从 IpcGet 里找出目标 peer 的 public_key（顺序与 profile 一致）
	var pubHex string
	seen := -1
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		if key == "public_key" {
			seen++
			if seen == peerIndex || (peerIndex < 0 && seen == 0) {
				pubHex = value
				break
			}
		}
	}
	if pubHex == "" {
		return fmt.Errorf("找不到要热换的 peer")
	}
	conf := "public_key=" + pubHex + "\nendpoint=" + addr.String() + "\n"
	if err := u.dev.IpcSet(conf); err != nil {
		return fmt.Errorf("用户态热换端点失败: %w", err)
	}
	return nil
}

func (u *userspaceBackend) Close() error {
	if u.dev != nil {
		u.dev.Close() // 关闭后 TUN 接口自动消失
	}
	return nil
}
