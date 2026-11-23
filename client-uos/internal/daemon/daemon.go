// Package daemon 是隧道建立后的常驻循环：
//
//	每 2 秒写一次状态文件（给 GUI 读）、跑隧道内 UDP 探针（活跃/空闲两档）；
//	任意一次探针没回包 → 先查 DNS TXT 公告板：端点变了立即热换，没变继续快探；
//	宽限期后才尝试重建/轮换端点（2.3.6 与 Android 客户端一致的软恢复策略）；
//	同时在 /run/homing/control.sock 上提供服务（用户态后端只能由本进程操作）。
//
// 日志原则：所有关键动作都同时 log.Printf 到 /var/log/homing/homing.log 和
// status.json 的 events（GUI 直接显示），这样“连上了但没网”这类问题一眼能看出卡在哪。
package daemon

import (
	"encoding/binary"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"homing/client/internal/discovery"
	"homing/client/internal/profile"
	"homing/client/internal/tunnel"
)

const maxEvents = 80

// 与 Android 2.3.6 对齐的常量
const (
	trafficActiveBytes = 1024              // 2s 内 ≥1KB 才算「有流量」（忽略 keepalive/探针自身）
	forceActiveDur     = 60 * time.Second  // 刚连接/切网/重建后强制活跃档
	txtQueryMinGap     = 10 * time.Second  // 疑似故障时公告板查询节流
	probeStartGrace    = 10 * time.Second  // 刚连接/刚重建不判故障
)

// Runner 守护进程状态。
type Runner struct {
	Tunnel      *tunnel.Tunnel
	Profile     *profile.Profile
	ProfileName string

	disc       tunnel.DiscoveryStatus
	lastDisc   discovery.Result
	lastRotate time.Time

	candidates   []string  // 端点候选（profile 主端点 + 备用 + 发现到的）
	candIdx      int       // 当前尝试到第几个
	connectStart time.Time // 建隧道时刻（判断“多久还没握手”）
	lastProbe    time.Time // 上次切换端点的时刻

	events             []string // 最近的关键事件（给 GUI 看）
	handshakeSeen      bool     // 这次连接是否成功握手过
	lastNoHandshakeLog time.Time
	lastLossLog        time.Time

	// ---- 2.3.6 探针节流 / 疑似故障（只在主循环里读写） ----
	probeCfg           profile.Probe
	probeCh            chan bool
	probeInFlight      bool
	probeNextAt        time.Time
	probeMode          string // active | idle | suspect
	probeFailures      int
	probeEverSucceeded bool
	lastProbeReply     time.Time
	lastTrafficAt      time.Time
	lastRxBytes        int64
	lastTxBytes        int64
	activeForceUntil   time.Time
	suspectStage       int // 0=健康 1=只探不重建 2=软重建后再探
	suspectAt          time.Time
	suspectDeadline    time.Time
	lastTxtQuery       time.Time
	recovering         bool
}

// Run 进入常驻循环（前台运行，由 up --daemon 的父进程 fork 出来）。
func Run(t *tunnel.Tunnel, p *profile.Profile, profileName string) error {
	now := time.Now()
	r := &Runner{
		Tunnel:      t,
		Profile:     p,
		ProfileName: profileName,
		disc: tunnel.DiscoveryStatus{
			Enabled:    p.Discovery.Enabled,
			Record:     p.Discovery.Record,
			AutoFollow: p.Discovery.AutoFollow,
		},
		probeCfg:         p.Probe,
		probeCh:          make(chan bool, 4),
		lastTrafficAt:    now,
		activeForceUntil: now.Add(forceActiveDur),
	}
	if !r.probeCfg.Enabled {
		r.event("隧道探针已关闭（退回握手 180s 判定）")
	} else {
		r.event("健康探针：活跃 %ds / 空闲 %ds（空闲判定 %ds，%d 次无回包即确认，超时 %dms）",
			r.probeCfg.Interval, r.probeCfg.IdleInterval, r.probeCfg.IdleAfter,
			r.probeCfg.Failures, r.probeCfg.TimeoutMs)
	}
	// TXT 查询用 SO_MARK 绕开隧道策略路由（否则隧道没通时 DNS 也查不到，形成死循环）
	discovery.Mark = tunnel.Table
	r.candidates = p.CandidateEndpoints()
	r.connectStart = now
	if err := tunnel.EnsureRunDir(); err != nil {
		return err
	}
	_ = os.WriteFile(tunnel.PidPath, []byte(strconv.Itoa(os.Getpid())), 0o644)
	r.event("守护进程启动 pid=%d profile=%q iface=%s 后端=%s", os.Getpid(), profileName, tunnel.Iface, t.Kind)
	r.event("端点候选(%d): %s", len(r.candidates), strings.Join(r.candidates, " → "))
	if p.Discovery.Enabled {
		r.event("端点自动发现: 记录=%s 自动跟随=%v 间隔=%ds（健康时不轮询）",
			p.Discovery.Record, p.Discovery.AutoFollow, p.Discovery.Interval)
	}

	// 先把“已连接”状态写出去，GUI 才能立刻看到
	r.writeStatus()

	// 控制 socket：热换端点 / 查状态（用户态后端必须走这里）
	listener, err := ServeControl(
		func() *tunnel.Status { return r.snapshot() },
		func(endpoint string, peerIndex int) (string, error) {
			applied, err := r.Tunnel.SetEndpoint(endpoint, peerIndex)
			if err == nil {
				r.event("手动热换端点 → %s", applied)
				r.afterEndpointSwitch(applied)
			}
			return applied, err
		},
		t.Kind,
	)
	if err != nil {
		r.event("控制 socket 启动失败（热换端点会不可用）: %v", err)
	} else {
		defer func() {
			_ = listener.Close()
			_ = os.Remove(tunnel.ControlSock)
		}()
	}

	// 首次端点发现（阻塞最多 8 秒，失败不影响隧道）
	if p.Discovery.Enabled {
		r.refreshDiscovery(true)
	}

	statusTicker := time.NewTicker(2 * time.Second)
	defer statusTicker.Stop()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case s := <-sigCh:
			r.event("收到信号 %v，拆隧道退出", s)
			return r.shutdown()
		case ok := <-r.probeCh:
			r.onProbeResult(ok, time.Now())
			r.writeStatus()
		case <-statusTicker.C:
			if !tunnel.KernelInterfaceExists() {
				r.event("接口 %s 已不存在（被外部拆除？），退出", tunnel.Iface)
				return r.shutdown()
			}
			now := time.Now()
			st := r.snapshot()
			r.updateTraffic(st, now)
			r.updateProbeMode(now)
			r.scheduleProbe(now)
			r.checkHandshake()
			r.checkHealth(st, now)
			r.writeStatus()
		}
	}
}

// event 记录一条关键事件：既写日志文件，也进 status.json 给 GUI 显示。
func (r *Runner) event(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("%s", msg)
	r.events = append(r.events, time.Now().Format("15:04:05")+" "+msg)
	if len(r.events) > maxEvents {
		r.events = r.events[len(r.events)-maxEvents:]
	}
}

func (r *Runner) shutdown() error {
	r.event("正在拆除隧道（接口 / 策略路由 / DNS）...")
	err := r.Tunnel.Down()
	_ = os.Remove(tunnel.PidPath)
	_ = os.Remove(tunnel.StatusPath)
	_ = os.Remove(tunnel.ControlSock)
	if err != nil {
		r.event("拆除完成，但有错误: %v", err)
	} else {
		r.event("已断开并清理完成")
	}
	return err
}

// snapshot 返回带探针信息的运行状态（控制 socket 与状态文件共用）。
func (r *Runner) snapshot() *tunnel.Status {
	st := r.Tunnel.Snapshot(r.ProfileName, r.disc)
	st.Probe = r.probeStatus()
	return st
}

func (r *Runner) writeStatus() {
	st := r.snapshot()
	st.Events = r.events
	if err := tunnel.WriteStatus(st); err != nil {
		log.Printf("写状态文件失败: %v", err)
	}
}

// ---------------- 探针节流 ----------------

func (r *Runner) probeStatus() tunnel.ProbeStatus {
	ps := tunnel.ProbeStatus{Enabled: r.probeCfg.Enabled, Failures: r.probeFailures}
	if !r.probeCfg.Enabled {
		ps.State = "off"
		return ps
	}
	ps.State = r.probeMode
	if ps.State == "" {
		ps.State = "active"
	}
	ps.IntervalSeconds = r.currentProbeInterval(time.Now())
	if !r.lastProbeReply.IsZero() {
		ps.LastReplyUnix = r.lastProbeReply.Unix()
	}
	if r.suspectStage > 0 {
		ps.State = "suspect"
		ps.Note = fmt.Sprintf("疑似故障：查公告板/快探中（%d 次无回包）", r.probeFailures)
	}
	return ps
}

func (r *Runner) updateTraffic(st *tunnel.Status, now time.Time) {
	delta := (st.RxBytes - r.lastRxBytes) + (st.TxBytes - r.lastTxBytes)
	if delta >= trafficActiveBytes {
		r.lastTrafficAt = now
	}
	r.lastRxBytes, r.lastTxBytes = st.RxBytes, st.TxBytes
}

func (r *Runner) isIdle(now time.Time) bool {
	return now.After(r.activeForceUntil) &&
		now.Sub(r.lastTrafficAt) > time.Duration(r.probeCfg.IdleAfter)*time.Second
}

func (r *Runner) currentProbeInterval(now time.Time) int {
	if r.suspectStage > 0 {
		return r.probeCfg.RecoveryInterval
	}
	if r.isIdle(now) {
		return r.probeCfg.IdleInterval
	}
	return r.probeCfg.Interval
}

func (r *Runner) updateProbeMode(now time.Time) {
	if !r.probeCfg.Enabled {
		return
	}
	mode := "active"
	if r.suspectStage > 0 {
		mode = "suspect"
	} else if r.isIdle(now) {
		mode = "idle"
	}
	if mode == r.probeMode {
		return
	}
	old := r.probeMode
	r.probeMode = mode
	switch mode {
	case "active":
		r.event("探针切到活跃档（每 %ds）", r.probeCfg.Interval)
		if old == "idle" || old == "suspect" {
			r.probeNextAt = now // 用户开始用：立即补探
		}
	case "idle":
		r.event("探针切到空闲档（每 %ds）", r.probeCfg.IdleInterval)
	case "suspect":
		r.event("探针进入疑似故障快探（每 %ds）", r.probeCfg.RecoveryInterval)
	}
}

func (r *Runner) scheduleProbe(now time.Time) {
	if !r.probeCfg.Enabled || r.probeInFlight {
		return
	}
	if now.Before(r.probeNextAt) {
		return
	}
	interval := r.currentProbeInterval(now)
	r.probeNextAt = now.Add(time.Duration(interval) * time.Second)
	r.probeInFlight = true
	go func() {
		ok := r.probeOnce()
		// 阻塞发送（带缓冲）以保证 probeInFlight 一定会被主循环复位；
		// 之前用 default 丢弃结果会导致 probeInFlight 永远为 true、探针停摆。
		r.probeCh <- ok
	}()
}

func (r *Runner) probeOnce() bool {
	addr := net.JoinHostPort(r.probeCfg.Host, strconv.Itoa(r.probeCfg.Port))
	conn, err := net.DialTimeout("udp", addr, time.Duration(r.probeCfg.TimeoutMs)*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Duration(r.probeCfg.TimeoutMs) * time.Millisecond))
	var payload [8]byte
	binary.LittleEndian.PutUint64(payload[:], uint64(time.Now().UnixNano())^rand.Uint64())
	if _, err := conn.Write(payload[:]); err != nil {
		return false
	}
	var buf [64]byte
	if _, err := conn.Read(buf[:]); err != nil {
		return false
	}
	return true
}

func (r *Runner) onProbeResult(ok bool, now time.Time) {
	r.probeInFlight = false
	if ok {
		r.lastProbeReply = now
		if !r.probeEverSucceeded {
			r.probeEverSucceeded = true
			r.event("隧道探针已连通（%s:%d）", r.probeCfg.Host, r.probeCfg.Port)
		}
		r.probeFailures = 0
		if r.suspectStage > 0 {
			r.clearSuspect()
			r.event("探针已恢复，清除疑似故障状态（未重建）")
		}
		return
	}
	r.probeFailures++
	if !r.probeEverSucceeded {
		// 网关没有探针回显（旧版）：不回退，靠握手 180s 判定
		return
	}
	if now.Sub(r.connectStart) < probeStartGrace {
		return
	}
	if r.probeFailures < r.probeCfg.Failures || r.suspectStage > 0 {
		return
	}
	r.enterSuspect(now)
}

func (r *Runner) enterSuspect(now time.Time) {
	r.suspectStage = 1
	r.suspectAt = now
	r.suspectDeadline = now.Add(time.Duration(r.probeCfg.SoftGrace) * time.Second)
	mode := "活跃"
	if r.isIdle(now) {
		mode = "空闲"
	}
	r.event("隧道探针无回包（%s档），查公告板确认端点是否变化", mode)
	r.queryDiscoveryForSuspect(now)
}

func (r *Runner) clearSuspect() {
	r.suspectStage = 0
	r.suspectAt = time.Time{}
	r.suspectDeadline = time.Time{}
	r.recovering = false
}

// checkHealth 2.3.6：探针优先；探针不可用时退回握手 180s + 候选轮换。
func (r *Runner) checkHealth(st *tunnel.Status, now time.Time) {
	if r.probeCfg.Enabled && r.probeEverSucceeded {
		if r.suspectStage > 0 {
			r.advanceSuspect(st, now)
		}
		return
	}
	// 旧网关/探针关闭：沿用握手 180s + 候选轮换逻辑
	if r.Profile.Discovery.Enabled && r.lastDisc.Error == "" {
		if st.Running && st.HandshakeAgeSeconds >= 180 && time.Since(r.lastRotate) >= 90*time.Second {
			alt := discovery.Alternate(r.Profile, r.lastDisc, st.Endpoint)
			if alt != "" && alt != st.Endpoint {
				applied, err := r.Tunnel.SetEndpoint(alt, 0)
				if err == nil {
					r.lastRotate = now
					r.disc.LastChangeUnix = now.Unix()
					r.event("已 %d 秒无握手，轮换端点到 %s", st.HandshakeAgeSeconds, applied)
					r.afterEndpointSwitch(applied)
					return
				}
				r.event("轮换端点失败: %v", err)
			}
		}
	}
	// 没有公告板/探针时，仍在「一直没握手」时依次尝试候选端点（家里外网两套端点都能用）
	r.checkRotationFallback(st)
}

// checkRotationFallback 一直握手不上时依次尝试候选端点（原 checkRotation 逻辑）。
func (r *Runner) checkRotationFallback(st *tunnel.Status) {
	if len(r.candidates) < 2 {
		return
	}
	if st.HandshakeAgeSeconds >= 0 && st.HandshakeAgeSeconds < 120 {
		return // 最近 2 分钟内握手过，不用折腾
	}
	if time.Since(r.connectStart) < 15*time.Second {
		return // 刚连上，给它一点时间
	}
	if time.Since(r.lastProbe) < 12*time.Second {
		return
	}
	r.lastProbe = time.Now()
	r.candIdx = (r.candIdx + 1) % len(r.candidates)
	candidate := r.candidates[r.candIdx]
	if applied, err := r.Tunnel.SetEndpoint(candidate, 0); err == nil {
		r.event("端点回退 %d/%d → %s（上一个端点没握上手）", r.candIdx+1, len(r.candidates), applied)
		r.afterEndpointSwitch(applied)
	} else {
		r.event("切换端点失败: %v", err)
	}
}

// advanceSuspect 推进疑似故障：宽限到期后软重建/切候选，再到期进入完整恢复。
func (r *Runner) advanceSuspect(st *tunnel.Status, now time.Time) {
	if r.suspectStage == 1 && now.After(r.suspectDeadline) {
		r.suspectStage = 2
		r.suspectDeadline = now.Add(time.Duration(r.probeCfg.SoftGrace) * time.Second)
		r.recovering = true
		r.event("持续 %ds 无回包，尝试重建/切换端点", r.probeCfg.SoftGrace)
		// 软重建：优先换一个不同的候选端点（家里 LAN / v6 / 备用端点）
		if r.tryAlternate(st.Endpoint, now) {
			return
		}
		r.event("没有其他候选端点，继续快探（%ds 后再进入完整恢复）", r.probeCfg.SoftGrace)
		return
	}
	if r.suspectStage == 2 && now.After(r.suspectDeadline) {
		r.event("重建后仍无回包，进入完整恢复")
		r.clearSuspect()
		r.lastRotate = now
		if r.Profile.Discovery.Enabled {
			r.refreshDiscovery(false)
		}
		r.tryAlternate(st.Endpoint, now)
		return
	}
	// 恢复期间端点可能刚好变化：每 10s 重查公告板
	if now.Sub(r.lastTxtQuery) >= txtQueryMinGap {
		r.queryDiscoveryForSuspect(now)
	}
}

func (r *Runner) tryAlternate(current string, now time.Time) bool {
	// 先试公告板里的另一个协议端点，再试候选列表里的其它端点
	if r.Profile.Discovery.Enabled {
		alt := discovery.Alternate(r.Profile, r.lastDisc, current)
		if alt != "" && alt != current {
			applied, err := r.Tunnel.SetEndpoint(alt, 0)
			if err == nil {
				r.lastRotate = now
				r.disc.LastChangeUnix = now.Unix()
				r.event("切换到候选端点 %s（当前 %s 无回包）", applied, current)
				r.afterEndpointSwitch(applied)
				return true
			}
			r.event("切换候选端点失败: %v", err)
		}
	}
	for _, c := range r.candidates {
		if c == "" || c == current {
			continue
		}
		if applied, err := r.Tunnel.SetEndpoint(c, 0); err == nil {
			r.lastRotate = now
			r.disc.LastChangeUnix = now.Unix()
			r.event("切换到备用端点 %s（当前 %s 无回包）", applied, current)
			r.afterEndpointSwitch(applied)
			return true
		}
	}
	return false
}

func (r *Runner) afterEndpointSwitch(endpoint string) {
	_ = endpoint // 端点已写入状态文件，这里只负责重置健康状态
	r.lastTrafficAt = time.Now()
	r.activeForceUntil = time.Now().Add(forceActiveDur)
	r.probeNextAt = time.Now().Add(probeStartGrace)
	r.probeInFlight = false
	r.suspectStage = 0
	r.suspectAt = time.Time{}
	r.suspectDeadline = time.Time{}
	r.probeFailures = 0
	r.probeEverSucceeded = false
	r.connectStart = time.Now()
	r.handshakeSeen = false
}

// queryDiscoveryForSuspect 疑似故障时查公告板：变了就热换，没变继续快探。
func (r *Runner) queryDiscoveryForSuspect(now time.Time) {
	if !r.Profile.Discovery.Enabled {
		r.event("配置没有公告板，继续快探")
		return
	}
	if now.Sub(r.lastTxtQuery) < txtQueryMinGap {
		return
	}
	r.lastTxtQuery = now
	changed, errMsg := r.refreshDiscovery(false)
	if errMsg != "" {
		r.event("疑似故障查公告板失败: %s，继续快探", errMsg)
		return
	}
	if changed {
		r.event("公告板端点已变并热切换，等待握手验证")
		r.clearSuspect()
		return
	}
	r.event("公告板端点未变，继续快探 %ds（%ds 内恢复则不重建）",
		r.probeCfg.RecoveryInterval, r.probeCfg.SoftGrace)
}

// refreshDiscovery 查 TXT 并按需热换端点；返回是否发生了端点切换。
// 2.3.6 起守护进程不再周期轮询，只在启动/疑似故障/手动时查。
func (r *Runner) refreshDiscovery(first bool) (bool, string) {
	res := discovery.Fetch(r.Profile)
	r.lastDisc = res
	r.disc.V4 = res.V4
	r.disc.V6 = res.V6
	r.addCandidate(res.V4)
	r.addCandidate(res.V6)
	changed := false
	if res.Error != "" {
		r.disc.LastErr = res.Error
		if first {
			r.event("端点发现失败: %s", res.Error)
		}
	} else {
		r.disc.LastErr = ""
		r.disc.LastOkUnix = time.Now().Unix()
		r.event("端点发现 OK: v4=%q v6=%q ts=%s", res.V4, res.V6, res.TS)
	}
	if r.Profile.Discovery.Enabled && r.Profile.Discovery.AutoFollow && res.Error == "" {
		candidate := discovery.Choose(r.Profile, res)
		current := r.currentEndpoint()
		if candidate != "" && candidate != current {
			if applied, err := r.Tunnel.SetEndpoint(candidate, 0); err == nil {
				r.event("端点已热换（发现）: %s → %s", current, applied)
				r.disc.LastChangeUnix = time.Now().Unix()
				r.afterEndpointSwitch(applied)
				changed = true
			} else {
				r.event("热换端点失败: %v", err)
				r.disc.LastErr = err.Error()
			}
		}
	}
	r.writeStatus()
	return changed, res.Error
}

// ---------------- 握手观察 / 候选轮换（探针不可用时的兜底） ----------------

// checkHandshake 观察握手状态变化：首次握手成功 / 握手丢失，都记一条。
func (r *Runner) checkHandshake() {
	st := r.Tunnel.Snapshot(r.ProfileName, r.disc)
	if st.HandshakeAgeSeconds >= 0 {
		if !r.handshakeSeen {
			r.handshakeSeen = true
			r.event("✅ 握手成功（endpoint=%s，从建立到握手 %ds）", st.Endpoint, int(time.Since(r.connectStart).Seconds()))
		}
		return
	}
	if r.handshakeSeen {
		// 曾经握手过，现在丢了
		if time.Since(r.lastLossLog) > 60*time.Second {
			r.lastLossLog = time.Now()
			r.event("⚠ 握手已丢失（当前 endpoint=%s），隧道可能已不可用", st.Endpoint)
		}
		r.handshakeSeen = false
		return
	}
	// 从来没握手成功：每 15 秒提示一次，方便用户看日志判断卡在哪
	if time.Since(r.connectStart) > 15*time.Second && time.Since(r.lastNoHandshakeLog) > 15*time.Second {
		r.lastNoHandshakeLog = time.Now()
		r.event("… 仍未握手（已等 %ds，当前端点 %s，候选 %d/%d）",
			int(time.Since(r.connectStart).Seconds()), st.Endpoint, r.candIdx+1, len(r.candidates))
	}
}

// ---------------- 工具 ----------------

// addCandidate 把发现到的端点并入候选列表（去重）。
func (r *Runner) addCandidate(endpoint string) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return
	}
	for _, c := range r.candidates {
		if c == endpoint {
			return
		}
	}
	r.candidates = append(r.candidates, endpoint)
}

func (r *Runner) currentEndpoint() string {
	if st, err := tunnel.ReadStatusFile(); err == nil && st.Endpoint != "" {
		return st.Endpoint
	}
	if st := r.Tunnel.Snapshot(r.ProfileName, r.disc); st.Endpoint != "" {
		return st.Endpoint
	}
	return ""
}
