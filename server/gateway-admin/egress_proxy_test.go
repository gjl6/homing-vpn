package main

import (
	"io"
	"net"
	"testing"
	"time"
)

// fakeSOCKS5Server 起一个只跑一次握手的最小 SOCKS5 服务端，用于验证客户端握手字节。
func fakeSOCKS5Server(t *testing.T, wantUser, wantPass string) (addr string, done chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done = make(chan struct{})
	go func() {
		defer close(done)
		defer ln.Close()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()

		head := make([]byte, 2)
		if _, err := io.ReadFull(c, head); err != nil {
			t.Errorf("read greeting: %v", err)
			return
		}
		if head[0] != 0x05 {
			t.Errorf("bad version %d", head[0])
			return
		}
		methods := make([]byte, int(head[1]))
		if _, err := io.ReadFull(c, methods); err != nil {
			t.Errorf("read methods: %v", err)
			return
		}
		useAuth := false
		for _, m := range methods {
			if m == 0x02 {
				useAuth = true
			}
		}
		if useAuth && wantUser != "" {
			if _, err := c.Write([]byte{0x05, 0x02}); err != nil {
				t.Errorf("write method: %v", err)
				return
			}
			ab := make([]byte, 2)
			if _, err := io.ReadFull(c, ab); err != nil {
				t.Errorf("read auth head: %v", err)
				return
			}
			u := make([]byte, int(ab[1]))
			if _, err := io.ReadFull(c, u); err != nil {
				t.Errorf("read auth user: %v", err)
				return
			}
			pl := make([]byte, 1)
			if _, err := io.ReadFull(c, pl); err != nil {
				t.Errorf("read auth pass len: %v", err)
				return
			}
			p := make([]byte, int(pl[0]))
			if _, err := io.ReadFull(c, p); err != nil {
				t.Errorf("read auth pass: %v", err)
				return
			}
			if string(u) != wantUser || string(p) != wantPass {
				t.Errorf("auth mismatch: %q/%q", u, p)
			}
			if _, err := c.Write([]byte{0x01, 0x00}); err != nil {
				t.Errorf("write auth reply: %v", err)
				return
			}
		} else {
			if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
				t.Errorf("write method: %v", err)
				return
			}
		}

		req := make([]byte, 4)
		if _, err := io.ReadFull(c, req); err != nil {
			t.Errorf("read connect: %v", err)
			return
		}
		if req[0] != 0x05 || req[1] != 0x01 {
			t.Errorf("bad connect head: %v", req)
			return
		}
		if req[3] != 0x03 {
			t.Errorf("expected domain address type, got %d", req[3])
			return
		}
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			t.Errorf("read host len: %v", err)
			return
		}
		host := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, host); err != nil {
			t.Errorf("read host: %v", err)
			return
		}
		if string(host) != "www.baidu.com" {
			t.Errorf("target host = %q", host)
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(c, port); err != nil {
			t.Errorf("read port: %v", err)
			return
		}
		if got := int(port[0])<<8 | int(port[1]); got != 443 {
			t.Errorf("target port = %d", got)
		}
		_, _ = c.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0x1f, 0x90})
	}()
	return ln.Addr().String(), done
}

func TestSOCKS5HandshakeNoAuth(t *testing.T) {
	addr, done := fakeSOCKS5Server(t, "", "")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := socks5Handshake(conn, "", "", "www.baidu.com:443", 3*time.Second); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	<-done
}

func TestSOCKS5HandshakeUserPass(t *testing.T) {
	addr, done := fakeSOCKS5Server(t, "user", "p@ss")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := socks5Handshake(conn, "user", "p@ss", "www.baidu.com:443", 3*time.Second); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	<-done
}
