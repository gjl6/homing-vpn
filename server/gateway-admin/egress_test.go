package main

import (
	"strings"
	"testing"
)

func validRemoteEgress() egressConfig {
	c := defaultEgressConfig()
	c.Enabled = true
	c.Mode = "remote"
	c.Tun = "homing-proxy"
	c.ProxyType = "socks5"
	c.ProxyHost = "192.0.2.10"
	c.ProxyPort = 7890
	c.DNSListen = "0.0.0.0:1053"
	c.DNSServer = "223.5.5.5,119.29.29.29"
	return c
}

func TestValidateRemoteEgress(t *testing.T) {
	c := validRemoteEgress()
	if err := validateEgressConfig(c); err != nil {
		t.Fatalf("valid remote config rejected: %v", err)
	}
}

func TestValidateRemoteEgressRejectsBadDNS(t *testing.T) {
	c := validRemoteEgress()
	c.DNSListen = "localhost:1053"
	if err := validateEgressConfig(c); err == nil {
		t.Fatal("hostname DNS listen should be rejected")
	}
	c = validRemoteEgress()
	c.DNSServer = "223.5.5.5,not-an-ip"
	if err := validateEgressConfig(c); err == nil {
		t.Fatal("bad DNS upstream should be rejected")
	}
}

func TestRenderProxyConfig(t *testing.T) {
	c := validRemoteEgress()
	c.ProxyUsername = "user"
	c.ProxyPassword = "p@ss\"word"
	out := renderProxyConfig(c)
	for _, want := range []string{
		`listen: "0.0.0.0:1053"`,
		`device: "homing-proxy"`,
		`type: "socks5"`,
		`server: "192.0.2.10"`,
		`port: 7890`,
		`username: "user"`,
		`password: "p@ss\"word"`,
		`- MATCH,REMOTE`,
		`stack: gvisor`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered config missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\n\n\n") {
		t.Fatalf("rendered config has unexpected blank lines:\n%s", out)
	}
}

func TestRenderProxyConfigNoDNS(t *testing.T) {
	c := validRemoteEgress()
	c.DNSListen = ""
	out := renderProxyConfig(c)
	if !strings.Contains(out, "dns:\n  enable: false\n") {
		t.Fatalf("disabled DNS not rendered:\n%s", out)
	}
}
