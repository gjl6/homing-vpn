package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"homing/client/internal/tunnel"
)

// Request 是控制 socket 的请求（本地 root 与守护进程之间的简单 JSON 协议）。
type Request struct {
	Cmd       string `json:"cmd"`                 // status | set-endpoint | ping
	Endpoint  string `json:"endpoint,omitempty"`  // set-endpoint 用
	PeerIndex int    `json:"peerIndex,omitempty"` // 默认 0
}

// Response 是控制 socket 的响应。
type Response struct {
	OK       bool           `json:"ok"`
	Error    string         `json:"error,omitempty"`
	Status   *tunnel.Status `json:"status,omitempty"`
	Endpoint string         `json:"endpoint,omitempty"`
	Backend  string         `json:"backend,omitempty"`
}

// ServeControl 在 /run/homing/control.sock 上提供服务。
// 用户态 内置隧道内核 的设备只能被持有它的进程操作，所以热换端点/查状态必须走这里。
func ServeControl(statusFn func() *tunnel.Status, setEndpoint func(string, int) (string, error), backend string) (net.Listener, error) {
	_ = os.Remove(tunnel.ControlSock)
	listener, err := net.Listen("unix", tunnel.ControlSock)
	if err != nil {
		return nil, fmt.Errorf("监听 %s 失败: %w", tunnel.ControlSock, err)
	}
	_ = os.Chmod(tunnel.ControlSock, 0o600)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleControlConn(conn, statusFn, setEndpoint, backend)
		}
	}()
	return listener, nil
}

func handleControlConn(conn net.Conn, statusFn func() *tunnel.Status, setEndpoint func(string, int) (string, error), backend string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req Request
	resp := Response{OK: true, Backend: backend}
	if err := json.Unmarshal(line, &req); err != nil {
		resp = Response{OK: false, Error: "请求解析失败: " + err.Error()}
	} else {
		switch req.Cmd {
		case "status":
			resp.Status = statusFn()
		case "set-endpoint":
			applied, err := setEndpoint(req.Endpoint, req.PeerIndex)
			if err != nil {
				resp = Response{OK: false, Error: err.Error(), Backend: backend}
			} else {
				resp.Endpoint = applied
			}
		case "ping":
			resp.OK = true
		default:
			resp = Response{OK: false, Error: "未知命令: " + req.Cmd}
		}
	}
	data, _ := json.Marshal(resp)
	_, _ = conn.Write(append(data, '\n'))
}

// Call 给 CLI 用：连接控制 socket 发一条请求。
func Call(req Request) (*Response, error) {
	conn, err := net.DialTimeout("unix", tunnel.ControlSock, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// IsAlive 判断守护进程是否在跑（控制 socket 可连）。
func IsAlive() bool {
	resp, err := Call(Request{Cmd: "ping"})
	return err == nil && resp != nil && resp.OK
}
