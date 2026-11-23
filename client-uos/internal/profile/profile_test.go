package profile

import "testing"

const sampleConf = `[Interface]
PrivateKey = AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
Address = 10.8.0.2/32, fdcc:ad94:bacf:61a4::cafe:2/128
MTU = 1420
DNS = 192.168.1.1

[Peer]
PublicKey = h00fwIsyAbuT3oa/P/pSpRfjIx14XqKfxve4FoJ7nA4=
PresharedKey = AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
AllowedIPs = 0.0.0.0/0, ::/0
PersistentKeepalive = 25
Endpoint = 203.0.113.10:9612
`

func TestParseConf(t *testing.T) {
	p, err := ParseConf(sampleConf)
	if err != nil {
		t.Fatalf("ParseConf: %v", err)
	}
	if p.PrivateKey != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" {
		t.Errorf("PrivateKey = %q", p.PrivateKey)
	}
	if len(p.Addresses) != 2 || p.Addresses[0] != "10.8.0.2/32" || p.Addresses[1] != "fdcc:ad94:bacf:61a4::cafe:2/128" {
		t.Errorf("Addresses = %v，双栈地址没解析对", p.Addresses)
	}
	if p.MTU != 1420 {
		t.Errorf("MTU = %d", p.MTU)
	}
	if len(p.DNS) != 1 || p.DNS[0] != "192.168.1.1" {
		t.Errorf("DNS = %v", p.DNS)
	}
	if len(p.Peers) != 1 {
		t.Fatalf("Peers = %d", len(p.Peers))
	}
	peer := p.Peers[0]
	if peer.Endpoint != "203.0.113.10:9612" {
		t.Errorf("Endpoint = %q", peer.Endpoint)
	}
	if peer.PersistentKeepalive != 25 {
		t.Errorf("PersistentKeepalive = %d", peer.PersistentKeepalive)
	}
	if len(peer.AllowedIPs) != 2 {
		t.Errorf("AllowedIPs = %v", peer.AllowedIPs)
	}
}

func TestParseJSONKeepsDiscovery(t *testing.T) {
	raw := `{"name":"x","privateKey":"k","addresses":["10.8.0.2/32"],
	         "peers":[{"publicKey":"p","allowedIps":["0.0.0.0/0"]}],
	         "discovery":{"enabled":true,"record":"_wg.example.com","autoFollow":false,"preferred":"v6"}}`
	p, err := ParseJSON([]byte(raw))
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if !p.Discovery.Enabled || p.Discovery.Record != "_wg.example.com" {
		t.Errorf("Discovery = %+v", p.Discovery)
	}
	if p.Discovery.Preferred != "v6" {
		t.Errorf("Preferred = %q", p.Discovery.Preferred)
	}
	if p.MTU != 1420 {
		t.Errorf("MTU 默认值应为 1420，实际 %d", p.MTU)
	}
}

func TestParseConfMissingKey(t *testing.T) {
	if _, err := ParseConf("[Interface]\nAddress = 10.0.0.2/32\n"); err == nil {
		t.Fatal("缺少 PrivateKey 应该报错")
	}
}

func TestProbeDefaults(t *testing.T) {
	p, err := ParseConf(sampleConf)
	if err != nil {
		t.Fatalf("ParseConf: %v", err)
	}
	want := DefaultProbe()
	if p.Probe != want {
		t.Fatalf("默认探针参数 = %+v，期望 %+v", p.Probe, want)
	}
	if p.Probe.Failures != 1 || p.Probe.Interval != 10 || p.Probe.IdleInterval != 30 {
		t.Errorf("默认值不对: %+v", p.Probe)
	}
}

func TestProbeFromServerJSON(t *testing.T) {
	raw := `{"name":"x","privateKey":"k","addresses":["10.8.0.2/32"],
	         "peers":[{"publicKey":"p","allowedIps":["0.0.0.0/0"]}],
	         "probe":{"enabled":true,"interval":8,"idleInterval":25,"idleAfter":4,
	                  "failures":1,"timeoutMs":1200,"recoveryInterval":4,"softGrace":20}}`
	p, err := ParseJSON([]byte(raw))
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if p.Probe.Interval != 8 || p.Probe.IdleInterval != 25 || p.Probe.IdleAfter != 4 {
		t.Errorf("服务端 probe 未生效: %+v", p.Probe)
	}
	if p.Probe.RecoveryInterval != 4 || p.Probe.SoftGrace != 20 || p.Probe.TimeoutMs != 1200 {
		t.Errorf("服务端 probe 恢复参数未生效: %+v", p.Probe)
	}
	if p.Probe.Host != "10.8.0.1" || p.Probe.Port != 51824 {
		t.Errorf("探针地址默认值不对: %+v", p.Probe)
	}
}

func TestProbeClamp(t *testing.T) {
	raw := `{"name":"x","privateKey":"k","addresses":["10.8.0.2/32"],
	         "peers":[{"publicKey":"p","allowedIps":["0.0.0.0/0"]}],
	         "probe":{"interval":999,"idleInterval":1,"idleAfter":999,"failures":0,
	                  "timeoutMs":10,"recoveryInterval":0,"softGrace":1}}`
	p, err := ParseJSON([]byte(raw))
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if p.Probe.Interval != 10 || p.Probe.IdleAfter != 5 || p.Probe.Failures != 1 {
		t.Errorf("越界值未收敛: %+v", p.Probe)
	}
	if p.Probe.TimeoutMs != 1500 || p.Probe.RecoveryInterval != 5 || p.Probe.SoftGrace != 30 {
		t.Errorf("越界值未收敛: %+v", p.Probe)
	}
}
