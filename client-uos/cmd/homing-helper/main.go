// homing-helper —— 归途 Homing 客户端特权助手。
//
// GUI 不直接碰网络，所有需要 root 的操作都通过 `pkexec homing-helper ...` 完成：
//
//	up --daemon --profile <profile.json>   建隧道并常驻（自动跟随 DNS TXT 端点）
//	down                                   拆隧道
//	status                                 当前状态 JSON
//	set-endpoint <host:port>               热换对端端点（不重建隧道）
//	discover --profile <path>              查询 TXT 公告板（不需要 root）
//	parse-conf                             把 .conf 转成 profile JSON（不需要 root）
//	diagnose                               环境自检（内核是否支持 隧道 等）
//
// 内核没有 隧道（老内核/无模块）时会自动降级到用户态 内置隧道内核，可用 --userspace 强制。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"homing/client/internal/daemon"
	"homing/client/internal/discovery"
	"homing/client/internal/profile"
	"homing/client/internal/tunnel"
)

const version = "2.1.0"

func main() {
	tunnel.SetVersion(version)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	code := 0
	switch cmd {
	case "version", "--version", "-v":
		fmt.Printf("homing-helper %s\n", version)
	case "parse-conf":
		code = cmdParseConf()
	case "up":
		code = cmdUp(args)
	case "down":
		code = cmdDown()
	case "status":
		code = cmdStatus()
	case "discover":
		code = cmdDiscover(args)
	case "set-endpoint":
		code = cmdSetEndpoint(args)
	case "autostart":
		code = cmdAutostart(args)
	case "diagnose":
		code = cmdDiagnose()
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", cmd)
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `homing-helper —— 归途 Homing 客户端特权助手 (v`+version+`)

用法:
  homing-helper up --daemon --profile <profile.json> [--userspace]   建立隧道并常驻
  homing-helper up --profile <profile.json>                          前台运行（调试用）
  homing-helper down                                                 断开并清理
  homing-helper status                                               打印状态 JSON
  homing-helper set-endpoint <host:port>                             热换对端端点
  homing-helper discover --profile <path>                            查询端点公告板（JSON）
  homing-helper parse-conf [--name=名字]                             stdin 的 .conf -> profile JSON
  homing-helper autostart status                                     查看开机自启状态（JSON）
  homing-helper autostart on --profile <profile.json>                开启开机自动连接
  homing-helper autostart off                                        关闭开机自动连接
  homing-helper diagnose                                             环境自检
  homing-helper version                                              版本

说明:
  内核支持隧道模块时用内核实现；没有时自动降级到用户态实现。
  --userspace 可强制用户态实现。

需要 root 的命令请通过 pkexec 调用，例如:
  pkexec homing-helper up --daemon --profile ~/.config/homing/profiles/default.json
`)
}

func fail(format string, a ...interface{}) int {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", a...)
	return 1
}

func requireRoot() bool {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "错误: 该操作需要 root 权限，请通过 pkexec 调用本程序")
		return false
	}
	return true
}

// ---- 子命令实现 ----

func cmdParseConf() int {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail("读取 stdin 失败: %v", err)
	}
	p, err := profile.ParseJSON(data)
	if err != nil {
		return fail("%v", err)
	}
	for _, arg := range os.Args[2:] {
		if strings.HasPrefix(arg, "--name=") {
			if name := strings.TrimSpace(strings.TrimPrefix(arg, "--name=")); name != "" {
				p.Name = name
			}
		}
	}
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fail("序列化失败: %v", err)
	}
	fmt.Println(string(out))
	return 0
}

func loadProfile(path string) (*profile.Profile, string, error) {
	var (
		data []byte
		err  error
		name = "stdin"
	)
	if path == "" || path == "-" {
		data, err = io.ReadAll(bufio.NewReader(os.Stdin))
	} else {
		data, err = os.ReadFile(path)
		name = filepath.Base(path)
	}
	if err != nil {
		return nil, name, fmt.Errorf("读取配置失败: %w", err)
	}
	p, err := profile.ParseJSON(data)
	if err != nil {
		return nil, name, err
	}
	if p.Name == "" {
		p.Name = name
	}
	return p, name, nil
}

func cmdUp(args []string) int {
	if !requireRoot() {
		return 1
	}
	var profilePath string
	daemonMode := false
	userspace := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--daemon":
			daemonMode = true
		case "--userspace":
			userspace = true
		case "--profile":
			if i+1 < len(args) {
				i++
				profilePath = args[i]
			}
		}
	}
	p, _, err := loadProfile(profilePath)
	if err != nil {
		return fail("%v", err)
	}
	opts := tunnel.Options{Userspace: userspace}
	if daemonMode {
		if profilePath == "" || profilePath == "-" {
			if err := tunnel.EnsureRunDir(); err != nil {
				return fail("%v", err)
			}
			raw, _ := json.MarshalIndent(p, "", "  ")
			profilePath = tunnel.RunDir + "/active_profile.json"
			if err := os.WriteFile(profilePath, raw, 0o600); err != nil {
				return fail("写入临时配置失败: %v", err)
			}
		}
		extra := ""
		if userspace {
			extra = " --userspace"
		}
		return daemonize(profilePath, extra)
	}

	fmt.Printf("正在建立隧道（profile=%s）...\n", p.Name)
	t, err := tunnel.Up(p, opts)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("隧道已建立：backend=%s iface=%s\n", t.Kind, tunnel.Iface)
	if err := daemon.Run(t, p, p.Name); err != nil {
		return fail("%v", err)
	}
	return 0
}

// daemonize 以 setsid 方式拉起自身的前台模式，然后等状态文件出现。
func daemonize(profilePath, extraArgs string) int {
	exe, err := os.Executable()
	if err != nil {
		return fail("定位自身可执行文件失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(tunnel.LogPath), 0o755); err != nil {
		return fail("创建日志目录失败: %v", err)
	}
	logFile, err := os.OpenFile(tunnel.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fail("打开日志文件失败: %v", err)
	}
	defer logFile.Close()

	argLine := []string{"up", "--profile", profilePath}
	if strings.TrimSpace(extraArgs) != "" {
		argLine = append(argLine, strings.Fields(extraArgs)...)
	}
	cmd := exec.Command(exe, argLine...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fail("启动守护进程失败: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case waitErr := <-done:
			return fail("守护进程启动失败（退出码 %v），日志尾部:\n%s", waitErr, logTail(20))
		case <-time.After(300 * time.Millisecond):
			if st, err := tunnel.ReadStatusFile(); err == nil && st.Running {
				fmt.Printf("隧道已建立: backend=%s endpoint=%s\n", st.Backend, st.Endpoint)
				fmt.Printf("日志: %s（tail -f 可实时查看）\n", tunnel.LogPath)
				return 0
			}
		}
	}
	return fail("启动超时（30s），日志尾部:\n%s", logTail(20))
}

func logTail(n int) string {
	data, err := os.ReadFile(tunnel.LogPath)
	if err != nil {
		return "(无日志)"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func cmdDown() int {
	if !requireRoot() {
		return 1
	}
	// 1) 让守护进程自己优雅退出（它会拆隧道、关闭用户态设备）
	if data, err := os.ReadFile(tunnel.PidPath); err == nil {
		pidText := strings.TrimSpace(string(data))
		if pid, err := strconv.Atoi(pidText); err == nil && pid > 1 && processIsHelper(pid) {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			for i := 0; i < 80; i++ {
				if !processIsHelper(pid) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if processIsHelper(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				time.Sleep(300 * time.Millisecond)
			}
		}
	}
	// 2) 兜底清理（幂等）
	tunnel.Cleanup()
	_ = os.Remove(tunnel.PidPath)
	_ = os.Remove(tunnel.StatusPath)
	_ = os.Remove(tunnel.ControlSock)
	fmt.Println("已断开，隧道与路由/DNS 已清理")
	return 0
}

func processIsHelper(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "homing-helper")
}

func cmdStatus() int {
	// 守护进程在跑：走控制 socket（用户态后端也能正确拿到状态）
	if resp, err := daemon.Call(daemon.Request{Cmd: "status"}); err == nil && resp.OK && resp.Status != nil {
		out, _ := json.MarshalIndent(resp.Status, "", "  ")
		fmt.Println(string(out))
		return 0
	}
	// 内核后端 + 守护进程不在：直接读内核设备
	if tunnel.KernelInterfaceExists() {
		st := tunnel.KernelSnapshot("", tunnel.DiscoveryStatus{})
		out, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(out))
		return 0
	}
	if st, err := tunnel.ReadStatusFile(); err == nil {
		out, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(out))
		return 0
	}
	return fail("未连接（并且无法读取 /run/homing/status.json）")
}

func cmdDiscover(args []string) int {
	var profilePath string
	for i := 0; i < len(args); i++ {
		if args[i] == "--profile" && i+1 < len(args) {
			i++
			profilePath = args[i]
		}
	}
	p, _, err := loadProfile(profilePath)
	if err != nil {
		return fail("%v", err)
	}
	discovery.Mark = tunnel.Table
	res := discovery.Fetch(p)
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	if res.Error != "" {
		return 1
	}
	return 0
}

func cmdSetEndpoint(args []string) int {
	if !requireRoot() {
		return 1
	}
	var target string
	peerIndex := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--peer-index":
			if i+1 < len(args) {
				i++
				if n, err := strconv.Atoi(args[i]); err == nil {
					peerIndex = n
				}
			}
		default:
			if target == "" {
				target = args[i]
			}
		}
	}
	if target == "" {
		return fail("缺少端点参数，用法: homing-helper set-endpoint host:port")
	}
	// 守护进程在跑：走控制 socket（用户态后端只能这样热换）
	if resp, err := daemon.Call(daemon.Request{Cmd: "set-endpoint", Endpoint: target, PeerIndex: peerIndex}); err == nil {
		if !resp.OK {
			return fail("%s", resp.Error)
		}
		fmt.Printf("端点已热换为 %s\n", resp.Endpoint)
		return 0
	}
	// 内核后端兜底
	if tunnel.KernelInterfaceExists() {
		applied, err := tunnel.SetEndpointKernel(target, peerIndex)
		if err != nil {
			return fail("%v", err)
		}
		fmt.Printf("端点已热换为 %s\n", applied)
		return 0
	}
	return fail("隧道未运行，无法热换端点")
}

// cmdDiagnose 环境自检：UOS 上排查“连不上/没界面”时先跑这个。
func cmdDiagnose() int {
	header := func(s string) { fmt.Printf("\n== %s ==\n", s) }
	release, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	kernel := strings.TrimSpace(string(release))

	header("版本/平台")
	fmt.Printf("homing-helper %s  %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
	fmt.Printf("内核: %s\n", kernel)
	fmt.Printf("当前用户: uid=%d（root 需要 0）\n", os.Geteuid())

	header("隧道内核支持")
	_, modprobeErr := runQuiet("modprobe", "wireguard")
	fmt.Printf("modprobe wireguard: %s\n", okText(modprobeErr))
	modPath := "/lib/modules/" + kernel + "/kernel/drivers/net/wireguard"
	if entries, err := os.ReadDir(modPath); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		fmt.Printf("模块目录: %s (%s)\n", modPath, strings.Join(names, ", "))
	} else {
		fmt.Printf("模块目录: %s 不存在\n", modPath)
	}
	if _, err := os.Stat("/sys/module/wireguard"); err == nil {
		fmt.Println("内核已加载模块: /sys/module/wireguard 存在")
	} else {
		fmt.Println("内核已加载模块: /sys/module/wireguard 不存在")
	}
	fmt.Printf("内核实现可用: %s\n", yesNo(tunnel.KernelSupported()))

	header("用户态实现（内置）")
	fmt.Printf("/dev/net/tun: %s\n", existsText("/dev/net/tun"))
	fmt.Printf("用户态可用: %s\n", yesNo(tunnel.UserspaceAvailable()))
	fmt.Printf("实际会使用的实现: %s\n", chosenBackend())

	header("运行状态")
	if resp, err := daemon.Call(daemon.Request{Cmd: "status"}); err == nil && resp.OK && resp.Status != nil {
		fmt.Printf("守护进程: 运行中 (backend=%s)\n", resp.Status.Backend)
		fmt.Printf("endpoint: %s\n", resp.Status.Endpoint)
		fmt.Printf("握手: %d 秒前\n", resp.Status.HandshakeAgeSeconds)
		fmt.Printf("隧道地址: %s\n", strings.Join(resp.Status.Addresses, ", "))
	} else if st, err := tunnel.ReadStatusFile(); err == nil && st.Running {
		fmt.Printf("状态文件: running=true backend=%s\n", st.Backend)
	} else {
		fmt.Println("守护进程: 未运行")
	}
	if _, err := os.Stat(tunnel.ControlSock); err == nil {
		fmt.Printf("控制 socket: %s 存在\n", tunnel.ControlSock)
	} else {
		fmt.Printf("控制 socket: %s 不存在\n", tunnel.ControlSock)
	}
	fmt.Printf("日志: %s\n", tunnel.LogPath)

	header("策略路由 / DNS")
	if out, err := runQuiet("ip", "route", "show", "table", strconv.Itoa(tunnel.Table)); err == nil && out != "" {
		fmt.Printf("table %d:\n%s\n", tunnel.Table, out)
	} else {
		fmt.Printf("table %d: 空（未连接或路由未装）\n", tunnel.Table)
	}
	if out, err := runQuiet("ip", "rule", "show"); err == nil {
		fmt.Printf("ip rule:\n%s\n", out)
	}
	if out, err := runQuiet("ip", "-o", "addr", "show", "dev", tunnel.Iface); err == nil && out != "" {
		fmt.Printf("接口地址: %s\n", out)
	}
	if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		fmt.Printf("/etc/resolv.conf:\n%s\n", strings.TrimSpace(string(data)))
	}

	header("活跃配置 / 候选端点")
	if data, err := os.ReadFile(tunnel.RunDir + "/active_profile.json"); err == nil {
		var prof profile.Profile
		if err := json.Unmarshal(data, &prof); err == nil {
			fmt.Printf("配置: %s（userspace=%v）\n", prof.Name, prof.Userspace)
			fmt.Printf("候选端点: %s\n", strings.Join(prof.CandidateEndpoints(), " → "))
			if len(prof.Peers) > 0 {
				fmt.Printf("AllowedIPs: %s\n", strings.Join(prof.Peers[0].AllowedIPs, ", "))
			}
		}
	} else {
		fmt.Println("(没有 /run/homing/active_profile.json，隧道未运行或非 daemon 模式启动)")
	}

	header("最近日志（20 行，完整日志 " + tunnel.LogPath + "）")
	if data, err := os.ReadFile(tunnel.LogPath); err == nil {
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) > 20 {
			lines = lines[len(lines)-20:]
		}
		for _, line := range lines {
			fmt.Println(line)
		}
	} else {
		fmt.Printf("读取日志失败: %v\n", err)
	}

	header("结论")
	if tunnel.KernelSupported() {
		fmt.Println("内核支持隧道模块，默认走内核实现（性能最好）。")
	} else if tunnel.UserspaceAvailable() {
		fmt.Println("内核不支持隧道模块，会自动降级到用户态实现（功能一致，性能略低）。")
	} else {
		fmt.Println("内核不支持隧道模块且 /dev/net/tun 不可用：需要安装内核隧道模块或加载 tun 模块。")
	}
	return 0
}

func chosenBackend() string {
	if tunnel.KernelSupported() {
		return "kernel"
	}
	if tunnel.UserspaceAvailable() {
		return "userspace"
	}
	return "不可用"
}

func runQuiet(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func okText(err error) string {
	if err == nil {
		return "OK"
	}
	return "失败: " + err.Error()
}

func yesNo(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

func existsText(path string) string {
	if _, err := os.Stat(path); err == nil {
		return "存在"
	}
	return "不存在"
}

// ---------- 开机自动连接（systemd） ----------

const (
	autostartUnit    = "/etc/systemd/system/homing-autostart.service"
	autostartConfDir = "/etc/homing"
	autostartProfile = "/etc/homing/autostart-profile.json"
)

type autostartState struct {
	Enabled     bool   `json:"enabled"`
	UnitPath    string `json:"unitPath"`
	Profile     string `json:"profile,omitempty"`
	ProfilePath string `json:"profilePath,omitempty"`
	Systemd     bool   `json:"systemdAvailable"`
}

func cmdAutostart(args []string) int {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "status":
		out, _ := json.MarshalIndent(autostartStatus(), "", "  ")
		fmt.Println(string(out))
		return 0
	case "on":
		if !requireRoot() {
			return 1
		}
		profilePath := ""
		for i := 0; i < len(args); i++ {
			if args[i] == "--profile" && i+1 < len(args) {
				i++
				profilePath = args[i]
			}
		}
		if profilePath == "" {
			return fail("用法: homing-helper autostart on --profile <profile.json>")
		}
		if err := autostartEnable(profilePath); err != nil {
			return fail("%v", err)
		}
		fmt.Printf("已开启开机自动连接（配置：%s）\n", profilePath)
		return 0
	case "off":
		if !requireRoot() {
			return 1
		}
		if err := autostartDisable(); err != nil {
			return fail("%v", err)
		}
		fmt.Println("已关闭开机自动连接")
		return 0
	default:
		return fail("未知子命令 %q（可用 on / off / status）", sub)
	}
}

func autostartStatus() autostartState {
	st := autostartState{UnitPath: autostartUnit, ProfilePath: autostartProfile}
	if _, err := os.Stat(autostartUnit); err == nil {
		st.Enabled = true
	}
	if data, err := os.ReadFile(autostartProfile); err == nil {
		var prof profile.Profile
		if json.Unmarshal(data, &prof) == nil {
			st.Profile = prof.Name
		}
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		if fi, err := os.Stat("/run/systemd/system"); err == nil && fi.IsDir() {
			st.Systemd = true
		}
	}
	return st
}

func autostartEnable(profilePath string) error {
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	var prof profile.Profile
	if err := json.Unmarshal(raw, &prof); err != nil {
		return fmt.Errorf("配置不是合法 JSON: %w", err)
	}
	if err := os.MkdirAll(autostartConfDir, 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", autostartConfDir, err)
	}
	if err := os.WriteFile(autostartProfile, raw, 0o600); err != nil {
		return fmt.Errorf("写入自动连接配置失败: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "/usr/libexec/homing/homing-helper"
	}
	unit := "[Unit]\n" +
		"Description=归途 Homing automatic VPN connection\n" +
		"After=network-online.target\n" +
		"Wants=network-online.target\n\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"ExecStart=" + exe + " up --profile " + autostartProfile + "\n" +
		"Restart=on-failure\n" +
		"RestartSec=15\n" +
		"KillMode=mixed\n\n" +
		"[Install]\n" +
		"WantedBy=multi-user.target\n"
	if err := os.MkdirAll("/etc/systemd/system", 0o755); err != nil {
		return fmt.Errorf("创建 /etc/systemd/system 失败: %w", err)
	}
	if err := os.WriteFile(autostartUnit, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("写入 systemd 单元失败: %w", err)
	}
	if err := systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("已写入 %s，但 systemctl daemon-reload 失败（系统没有 systemd？）: %w", autostartUnit, err)
	}
	if err := systemctl("enable", "homing-autostart.service"); err != nil {
		return fmt.Errorf("systemctl enable 失败: %w", err)
	}
	return nil
}

func autostartDisable() error {
	errDisable := systemctl("disable", "homing-autostart.service")
	_ = os.Remove(autostartUnit)
	_ = os.Remove(autostartProfile)
	_ = os.Remove(autostartConfDir)
	reloadErr := systemctl("daemon-reload")
	if reloadErr != nil {
		return fmt.Errorf("已删除单元文件，但 systemctl daemon-reload 失败（系统没有 systemd？）: %w", reloadErr)
	}
	if errDisable != nil {
		// 单元可能本来就没 enable，忽略
		return nil
	}
	return nil
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if text != "" {
			return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, text)
		}
		return fmt.Errorf("systemctl %s: %v", strings.Join(args, " "), err)
	}
	return nil
}