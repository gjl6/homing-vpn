package io.homing.client

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.net.VpnService
import android.os.Build
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.core.content.ContextCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.app.ServiceCompat
import com.wireguard.android.backend.GoBackend
import com.wireguard.android.backend.Tunnel
import com.wireguard.config.Config
import org.json.JSONObject
import java.io.ByteArrayInputStream
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit

/**
 * 归途前台服务：常驻通知 + 持有隧道 + 公告板按需跟随 + 切网/重启自动重连。
 *
 * 设计要点：
 *  - 所有隧道变更（连接/断开/热换端点/重建）都投递到单线程 [exec] 串行执行，
 *    避免和官方 内置隧道内核 库的 setState 打架；
 *  - 2s 一次的 [tick] 读状态 / 握手 / 流量，跑「健康判定 + 故障恢复」；
 *  - **健康时不查公告板**：只有握手超时 / 长时间没有流量时才查 TXT，避免一直轮询；
 *  - 故障恢复有退避（5s→10s→…→5min）、端点回避（失败端点 10 分钟不再选）
 *    和回退（新端点验证失败就切回上一个端点），不会在坏端点上死循环；
 *  - 监听底层网络变化（VPN 之外的 wifi/蜂窝），换网后先重建隧道，不通再查公告板；
 *  - START_STICKY + 持久化 wantConnected，进程被系统杀掉后能自动恢复。
 */
class HomingService : Service(), Tunnel {
    private lateinit var backend: GoBackend
    private lateinit var cm: ConnectivityManager

    private val main = Handler(Looper.getMainLooper())
    private val exec: ExecutorService = Executors.newSingleThreadExecutor()
    private val scheduler: ScheduledExecutorService = Executors.newScheduledThreadPool(1)

    // 隧道内 UDP 探针：往网关 10.8.0.1:51824 发一个 8 字节包，网关原样回显。
    // 2.3.6 起：活跃档按 interval 探，空闲档按 idleInterval 探；一次没回包就进入「疑似故障」
    // 先查公告板确认端点是否真的变了（变了切、没变继续探），不再等连续多次失败。
    private val probeExec: ExecutorService = Executors.newSingleThreadExecutor()
    private val probeInFlight = java.util.concurrent.atomic.AtomicBoolean(false)
    @Volatile private var probeSocket: java.net.DatagramSocket? = null
    @Volatile private var lastProbeReplyAt = 0L
    @Volatile private var probeFailures = 0
    @Volatile private var probeEverSucceeded = false
    @Volatile private var lastTxBytes = 0L
    @Volatile private var lastTrafficAt = 0L
    @Volatile private var activeForceUntil = 0L

    // ---- 探针节流 / 疑似故障（只在单线程 exec 里读写） ----
    private var nextProbeAt = 0L
    private var probeMode = 0 // 0=未定 1=活跃 2=空闲 3=疑似故障
    private var suspectStage = 0 // 0=健康 1=只探不重建 2=重建后再探
    private var suspectAt = 0L
    private var suspectDeadline = 0L
    private var lastTxtQueryAt = 0L

    @Volatile private var destroyed = false
    @Volatile private var foreground = false
    @Volatile private var wantConnected = false
    @Volatile private var connectedAt = 0L
    @Volatile private var cfg: Config? = null
    @Volatile private var profileObject: JSONObject? = null
    @Volatile private var profileTextCache: String? = null
    @Volatile private var connectedProfileId: String? = null
    @Volatile private var retryBaseSec = 15
    private var probeCfg: HomingProfile.Probe = HomingProfile.Probe.DEFAULT
    @Volatile private var lastRecoveryAt = 0L
    @Volatile private var lastRebindAt = 0L
    @Volatile private var lastRotateAt = 0L

    // ---- 健康判定 / 故障恢复状态（全部只在单线程 exec 里读写） ----
    private var lastGoodEndpoint: String? = null
    private var lastRxBytes = 0L
    private var lastRxAt = 0L
    private var probe: Probe? = null
    private var recoveryAttempts = 0
    private var nextRecoveryAt = 0L
    private val avoidUntil = HashMap<String, Long>()
    private val random = java.util.Random()

    /** 正在验证的候选端点：切换后等一个握手窗口，成功才算恢复。 */
    private data class Probe(
        val endpoint: String,
        val previous: String?,
        val startedAt: Long,
        val startedHandshake: Long,
        val avoidOnFail: Boolean,
    )

    private var statsTask: ScheduledFuture<*>? = null
    private var netDebounceToken = 0L
    private val activeNetworks = java.util.concurrent.ConcurrentHashMap<String, Boolean>()

    private val store: ProfileStore by lazy { ProfileStore(this) }
    private val repo: ProfileRepository by lazy { ProfileRepository(this) }

    private val networkCallback = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) {
            if (isVpnNetwork(network)) return
            if (activeNetworks.put(network.toString(), true) != null) return
            scheduleNetworkChange("新网络 ${describeNetwork(network)}")
        }

        override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) {
            // 只关心网络出现/消失，能力变化不重复触发
        }

        override fun onLost(network: Network) {
            if (isVpnNetwork(network)) return
            activeNetworks.remove(network.toString())
            HomingState.log("底层网络断开（${describeNetwork(network)}），等待恢复")
        }
    }

    // ---------------- 生命周期 ----------------

    override fun onCreate() {
        super.onCreate()
        backend = GoBackend(this)
        cm = getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
        createNotificationChannel()
        registerNetworkCallbacks()
        HomingState.log("前台服务已创建")
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val action = intent?.action
        if (action == null) {
            // 系统把服务重启了（START_STICKY，intent 为空）
            if (store.wantConnected) {
                startForegroundCompat("归途 · 恢复连接", "系统重启了客户端进程，正在自动重连")
                submit { recoverFromRestart() }
            } else {
                stopSelf()
            }
            return START_STICKY
        }
        when (action) {
            ACTION_CONNECT -> {
                startForegroundCompat("归途 · 正在连接", "正在建立隧道…")
                val profileId = intent.getStringExtra(EXTRA_PROFILE_ID)
                submit { doConnect(profileId) }
            }
            ACTION_DISCONNECT -> submit { doDisconnect(stopService = true) }
            ACTION_REFRESH -> {
                startForegroundCompat(
                    if (wantConnected) "归途 · 已连接" else "归途 · 查询公告板",
                    "正在查询公告板…",
                )
                submit { refreshDiscoveryManually() }
            }
            ACTION_SWITCH -> {
                val ep = intent.getStringExtra(EXTRA_ENDPOINT).orEmpty().trim()
                submit { manualSwitch(ep) }
            }
            else -> HomingState.log("未知服务指令：$action")
        }
        return START_STICKY
    }

    override fun onDestroy() {
        destroyed = true
        runCatching { cm.unregisterNetworkCallback(networkCallback) }
        stopLoops()
        probeExec.shutdownNow()
        closeProbeSocket()
        exec.shutdownNow()
        scheduler.shutdownNow()
        foreground = false
        HomingState.foreground = false
        HomingState.log("前台服务已停止")
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    // ---------------- Tunnel 回调 ----------------

    override fun getName(): String = TUNNEL_NAME

    override fun onStateChange(newState: Tunnel.State) {
        HomingState.tunnelState = when (newState) {
            Tunnel.State.UP -> "UP"
            Tunnel.State.TOGGLE -> "TOGGLE"
            else -> "DOWN"
        }
        HomingState.log("隧道状态 -> $newState")
    }

    // ---------------- 连接 / 断开 ----------------

    private fun recoverFromRestart() {
        HomingState.log("服务被系统重启，按上次状态自动重连")
        doConnect(null)
    }

    private fun doConnect(profileId: String?) {
        if (destroyed) return
        try {
            val profile = repo.find(profileId) ?: repo.active()
            if (profile == null) {
                return failConnect("没有可用的配置：请打开应用扫码导入，或先连接一次")
            }
            val normalized = try {
                HomingProfile.normalize(profile.raw)
            } catch (e: Exception) {
                return failConnect("配置无效：${e.message}")
            }
            val isJson = HomingProfile.isJson(normalized)
            val prof = if (isJson) JSONObject(normalized) else null

            profileObject = prof
            profileTextCache = normalized
            connectedProfileId = profile.id
            repo.setActive(profile.id)
            if (prof != null) {
                HomingState.profileName = HomingProfile.nameOf(prof).ifBlank { profile.displayName() }
                HomingState.endpoint = HomingProfile.peerEndpoint(prof).orEmpty()
                val d = HomingProfile.discovery(prof)
                retryBaseSec = d?.intervalSec ?: DEFAULT_RETRY_BASE_SEC
                probeCfg = HomingProfile.probe(prof)
            } else {
                HomingState.profileName = profile.displayName()
                HomingState.endpoint = HomingProfile.confEndpoint(normalized).orEmpty()
                retryBaseSec = DEFAULT_RETRY_BASE_SEC
                probeCfg = HomingProfile.Probe.DEFAULT
            }
            HomingState.log(
                if (probeCfg.enabled) {
                    "健康探针：活跃 ${probeCfg.intervalSec}s / 空闲 ${probeCfg.idleIntervalSec}s" +
                        "（空闲判定 ${probeCfg.idleAfterSec}s，${probeCfg.failures} 次无回包即确认，" +
                        "超时 ${probeCfg.timeoutMs}ms，${probeCfg.host}:${probeCfg.port}）"
                } else {
                    "健康探针已关闭（退回握手超时判定）"
                },
            )

            if (VpnService.prepare(this) != null) {
                return failConnect("缺少 VPN 授权：请打开应用点一次「连接」并在系统弹窗里允许")
            }
            val conf = Config.parse(ByteArrayInputStream(HomingProfile.toConf(normalized).toByteArray()))
            if (conf.peers.isEmpty()) return failConnect("配置里没有 peer")

            // 已在连接状态时重新连接（例如编辑配置保存后）：先明确 DOWN 再 UP，
            // 避免在 UP 状态下直接 setState(UP,newConfig) 偶发卡在切换中间态。
            val wasUp = runCatching { backend.getState(this) }
                .getOrNull() == Tunnel.State.UP
            if (wasUp) {
                runCatching { backend.setState(this, Tunnel.State.DOWN, null) }
                Thread.sleep(350)
            }
            backend.setState(this, Tunnel.State.UP, conf)
            cfg = conf
            wantConnected = true
            store.wantConnected = true
            connectedAt = System.currentTimeMillis()
            HomingState.connectedAt = connectedAt
            HomingState.connectedProfileId = profile.id
            HomingState.handshakeAt = 0L
            HomingState.rxBytes = 0L
            HomingState.txBytes = 0L
            HomingState.tunnelState = "UP"
            // 新连接/重连：清掉上一轮的恢复状态；健康检查从干净状态开始
            probe = null
            recoveryAttempts = 0
            nextRecoveryAt = 0L
            avoidUntil.clear()
            HomingState.recovering = false
            HomingState.recoveryNote = ""
            lastGoodEndpoint = HomingState.endpoint.ifBlank { null }
            lastRxBytes = 0L
            lastRxAt = connectedAt
            lastTxBytes = 0L
            lastTrafficAt = connectedAt
            activeForceUntil = connectedAt + FORCE_ACTIVE_MS
            nextProbeAt = 0L
            probeMode = 0
            clearSuspect()
            resetProbe(grace = false)
            HomingState.log("已连接「${profile.displayName()}」（Endpoint ${HomingState.endpoint.ifBlank { "未知" }}）")
            startLoops()
            updateNotification("归途 · 已连接", notificationText())
        } catch (t: Throwable) {
            Log.e(TAG, "connect failed", t)
            failConnect("连接失败：${t.message ?: t.javaClass.simpleName}")
        }
    }

    private fun doDisconnect(stopService: Boolean) {
        wantConnected = false
        store.wantConnected = false
        stopLoops()
        runCatching { backend.setState(this, Tunnel.State.DOWN, null) }
        cfg = null
        connectedAt = 0L
        connectedProfileId = null
        HomingState.connectedProfileId = null
        HomingState.tunnelState = "DOWN"
        HomingState.handshakeAt = 0L
        HomingState.rxBytes = 0L
        HomingState.txBytes = 0L
        HomingState.endpoint = ""
        HomingState.recovering = false
        HomingState.recoveryNote = ""
        probe = null
        avoidUntil.clear()
        probeMode = 0
        nextProbeAt = 0L
        clearSuspect()
        resetProbe(grace = false)
        HomingState.log("已断开")
        if (stopService) {
            stopForegroundCompat()
            stopSelf()
        }
    }

    private fun failConnect(msg: String) {
        wantConnected = false
        store.wantConnected = false
        cfg = null
        connectedProfileId = null
        HomingState.connectedProfileId = null
        stopLoops()
        HomingState.tunnelState = "DOWN"
        HomingState.recovering = false
        HomingState.recoveryNote = ""
        probe = null
        HomingState.log(msg)
        updateNotification("归途 · 连接失败", msg, ongoing = false)
        main.postDelayed({
            if (!wantConnected && !destroyed) {
                stopForegroundCompat()
                stopSelf()
            }
        }, 6000)
    }

    // ---------------- 状态 / 通知 / 健康判定 ----------------

    private fun tick() {
        if (destroyed) return
        var state = "DOWN"
        try {
            state = when (backend.getState(this)) {
                Tunnel.State.UP -> "UP"
                Tunnel.State.TOGGLE -> "TOGGLE"
                else -> "DOWN"
            }
        } catch (t: Throwable) {
            state = "UNKNOWN"
        }
        HomingState.tunnelState = state
        try {
            val stats = backend.getStatistics(this)
            if (stats != null) {
                val keys = stats.peers()
                if (keys.isNotEmpty()) {
                    val p = stats.peer(keys.first())
                    if (p != null) {
                        HomingState.handshakeAt = p.latestHandshakeEpochMillis()
                        HomingState.rxBytes = p.rxBytes()
                        HomingState.txBytes = p.txBytes()
                    }
                }
            }
        } catch (t: Throwable) {
            // 状态读取失败不影响隧道
        }
        if (!wantConnected) return
        val now = System.currentTimeMillis()
        updateTrafficProgress(now)
        updateProbeMode(now)
        maybeScheduleProbe(now)
        val healthy = tunnelHealthy(now, state)
        if (healthy) onHealthy(now) else onUnhealthy(now)
        val title = when {
            probe != null || (state != "UP" && state != "TOGGLE") -> "归途 · 恢复中"
            else -> "归途 · 已连接"
        }
        updateNotification(title, notificationText())
    }

    private fun notificationText(): String {
        val sb = StringBuilder()
        sb.append("Endpoint ").append(HomingState.endpoint.ifBlank { "未知" })
        val ago = HomingState.handshakeAgoSec()
        sb.append(" · 握手 ").append(if (ago >= 0) "${ago}s 前" else "等待中")
        sb.append(" · RX/TX ").append(HomingState.rxBytes).append("/").append(HomingState.txBytes)
        HomingState.recoveryNote.takeIf { it.isNotBlank() }?.let { sb.append(" · ").append(it) }
        return sb.toString()
    }

    private fun updateTrafficProgress(now: Long) {
        val rx = HomingState.rxBytes
        val tx = HomingState.txBytes
        val rxChanged = rx != lastRxBytes
        val delta = (rx - lastRxBytes) + (tx - lastTxBytes)
        if (rxChanged) lastRxAt = now
        // 只把「有意义的流量」算活跃：忽略 隧道 keepalive / 零星控制包的几十字节，
        // 否则空闲时每 25s 的 keepalive 会让探针一直在活跃/空闲档之间跳。
        if (delta >= TRAFFIC_ACTIVE_BYTES) lastTrafficAt = now
        lastRxBytes = rx
        lastTxBytes = tx
    }

    /**
     * 健康判定：握手在 [HANDSHAKE_STALE_MS] 内刷新 → 健康。
     * 空闲时 wireguard 的 keepalive 会每 2 分钟左右触发一次新握手；
     * 只有在握手迟迟不刷新、并且连下行流量也没有时才判为不健康，
     * 避免「空闲一分钟就被当成掉线反复重建隧道」。
     */
    private fun tunnelHealthy(now: Long, state: String): Boolean {
        // 2.3.6：探针已进入「疑似故障」时，不急着下结论：
        // 只有「疑似故障开始之后」又出现了新握手 / 新下行流量，才认为隧道其实活着
        // （探针回显链路丢包）；旧的握手记录不算数，否则单次丢包会被立刻误判为健康。
        if (probeCfg.enabled && probeEverSucceeded && suspectStage > 0) {
            val hs = HomingState.handshakeAt
            val hsAfterSuspect = hs > 0L && hs >= suspectAt - 1_000L
            val rxAfterSuspect = lastRxAt >= suspectAt - 1_000L && now - lastRxAt <= RX_PROGRESS_MS
            return hsAfterSuspect || rxAfterSuspect
        }
        // 快速判定：隧道内探针无回包 → 不健康（进入疑似故障流程）
        if (probeUnhealthy(now)) return false
        val hs = HomingState.handshakeAt
        val inGrace = now - connectedAt < NEVER_HANDSHAKE_GRACE_MS
        if (hs <= 0L) {
            // 恢复过程中不享受「首次握手宽限」，否则回退/重建后会被误判为已恢复
            if (HomingState.recovering || probe != null || recoveryAttempts > 0) return false
            return inGrace
        }
        if (state != "UP" && state != "TOGGLE") {
            // setState(UP, newConfig) 切换瞬间后端可能短暂报 DOWN；只容忍重连宽限期内的短暂
            // DOWN，超过宽限期仍是 DOWN 就按不健康处理（否则隧道真挂了会一直不恢复）。
            return inGrace
        }
        if (now - hs <= HANDSHAKE_STALE_MS) return true
        return now - lastRxAt <= RX_PROGRESS_MS
    }

    // ---------------- 隧道内探针（快速故障检测） ----------------

    private fun probeUnhealthy(now: Long): Boolean {
        if (!probeCfg.enabled) return false
        // 从未成功过（例如连的是没部署探针回显的旧网关）→ 不靠探针判定，退回握手超时逻辑
        if (!probeEverSucceeded) return false
        if (now - connectedAt < PROBE_START_GRACE_MS) return false
        if (probeFailures < probeCfg.failures) return false
        return now - lastProbeReplyAt > probeCfg.timeoutMs * 2L
    }
    private fun closeProbeSocket() {
        runCatching { probeSocket?.close() }
        probeSocket = null
    }

    private fun resetProbe(grace: Boolean) {
        closeProbeSocket()
        probeFailures = 0
        if (grace) {
            lastProbeReplyAt = System.currentTimeMillis()
        } else {
            lastProbeReplyAt = 0L
            probeEverSucceeded = false
        }
    }

    /** 每个 tick 都更新探针档位：活跃/空闲/疑似故障；空闲↔活跃切换时立即重排下一次探针。 */
    private fun updateProbeMode(now: Long) {
        if (!probeCfg.enabled) return
        val mode = when {
            suspectStage > 0 -> 3
            isIdleNow(now) -> 2
            else -> 1
        }
        if (mode == probeMode) return
        val old = probeMode
        probeMode = mode
        when (mode) {
            1 -> {
                HomingState.log("探针切到活跃档（每 ${probeCfg.intervalSec}s）")
                if (old == 2 || old == 3) nextProbeAt = 0L // 用户开始用：立即补探
            }
            2 -> HomingState.log("探针切到空闲档（每 ${probeCfg.idleIntervalSec}s）")
            3 -> if (old != 3) {
                HomingState.log("探针进入疑似故障快探（每 ${probeCfg.recoveryIntervalSec}s）")
            }
        }
    }

    /** 2s tick 里决定下一次探针时间：活跃/空闲/疑似故障三档。 */
    private fun maybeScheduleProbe(now: Long) {
        if (!wantConnected || cfg == null || !probeCfg.enabled) return
        if (probeInFlight.get()) return
        if (now < nextProbeAt) return
        val intervalSec = effectiveProbeIntervalSec(now)
        nextProbeAt = now + intervalSec * 1000L
        scheduleProbe()
    }

    private fun isIdleNow(now: Long): Boolean =
        now > activeForceUntil && now - lastTrafficAt > probeCfg.idleAfterSec * 1000L

    private fun effectiveProbeIntervalSec(now: Long): Int = when {
        suspectStage > 0 -> probeCfg.recoveryIntervalSec
        now < activeForceUntil -> probeCfg.intervalSec
        isIdleNow(now) -> probeCfg.idleIntervalSec
        else -> probeCfg.intervalSec
    }

    private fun scheduleProbe() {
        if (destroyed || !wantConnected || cfg == null || !probeCfg.enabled) return
        if (!probeInFlight.compareAndSet(false, true)) return
        probeExec.execute {
            try {
                doProbe()
            } catch (t: Throwable) {
                Log.e(TAG, "probe failed", t)
            } finally {
                probeInFlight.set(false)
            }
        }
    }

    private fun doProbe() {
        if (destroyed || !wantConnected || cfg == null || !probeCfg.enabled) return
        val now = System.currentTimeMillis()
        if (now - connectedAt < PROBE_START_GRACE_MS) return
        if (BuildConfig.DEBUG) {
            HomingState.log(
                "调试：探针发送（${if (suspectStage > 0) "疑似故障" else if (isIdleNow(now)) "空闲" else "活跃"}档）",
            )
        }
        try {
            val socket = probeSocket ?: java.net.DatagramSocket().also {
                it.soTimeout = probeCfg.timeoutMs
                probeSocket = it
            }
            val target = java.net.InetSocketAddress(probeCfg.host, probeCfg.port)
            val nonce = ByteArray(8)
            val seed = System.nanoTime()
            for (i in nonce.indices) nonce[i] = (seed shr (i * 8)).toByte()
            socket.send(java.net.DatagramPacket(nonce, nonce.size, target))
            val buf = ByteArray(64)
            socket.receive(java.net.DatagramPacket(buf, buf.size))
            val repliedAt = System.currentTimeMillis()
            lastProbeReplyAt = repliedAt
            probeFailures = 0
            if (!probeEverSucceeded) {
                HomingState.log("隧道探针已连通（${probeCfg.host}:${probeCfg.port}）")
            }
            probeEverSucceeded = true
            submit { onProbeReply(repliedAt) }
        } catch (e: java.net.SocketTimeoutException) {
            probeFailures++
            submit { onProbeMiss(System.currentTimeMillis()) }
        } catch (t: Throwable) {
            closeProbeSocket()
            probeFailures++
            submit { onProbeMiss(System.currentTimeMillis()) }
        }
    }

    /** 探针成功：清疑似故障、恢复正常节流。 */
    private fun onProbeReply(now: Long) {
        if (!wantConnected || destroyed) return
        if (suspectStage > 0) {
            HomingState.log("探针已恢复，清除疑似故障状态（未重建）")
            clearSuspect()
        }
        nextProbeAt = now + effectiveProbeIntervalSec(now) * 1000L
    }

    /**
     * 探针一次没回包：进入「疑似故障」——先查公告板确认端点是否真的变了。
     * 单次丢包不会重建/切端点/回避：下一次回包就回健康。
     */
    private fun onProbeMiss(now: Long) {
        if (destroyed || !wantConnected) return
        if (!probeEverSucceeded) return
        if (probe != null) return // 正在验证候选端点，由验证流程负责
        if (HomingState.recovering && suspectStage == 0) return // 完整恢复流程中，不再重复进入疑似故障
        if (now - connectedAt < PROBE_START_GRACE_MS) return
        if (suspectStage > 0) {
            nextProbeAt = now + probeCfg.recoveryIntervalSec * 1000L
            return
        }
        suspectStage = 1
        suspectAt = now
        suspectDeadline = now + probeCfg.softGraceSec * 1000L
        HomingState.log(
            "探针 ${probeFailures} 次无回包（${if (isIdleNow(now)) "空闲" else "活跃"}档），" +
                "查公告板确认端点是否变化",
        )
        nextProbeAt = now + probeCfg.recoveryIntervalSec * 1000L
        queryTxtForSuspect(now, first = true)
    }

    /** 疑似故障时按需查公告板：变了就切端点，没变继续快探（有 10s 查询节流）。 */
    private fun queryTxtForSuspect(now: Long, first: Boolean) {
        if (!wantConnected || cfg == null) return
        if (now - lastTxtQueryAt < TXT_QUERY_MIN_GAP_MS) return
        lastTxtQueryAt = now
        val d = currentDiscovery() ?: run {
            HomingState.log("配置没有公告板信息，继续快探")
            return
        }
        val result = runCatching { TxtResolver.queryFresh(this, d, maxServers = 3) }.getOrNull()
        if (result == null || !result.ok) {
            HomingState.log("疑似故障查公告板失败：${result?.error ?: "查询异常"}，继续快探")
            return
        }
        HomingState.discoveryAt = now
        HomingState.discoveryError = ""
        HomingState.discoverySummary = buildString {
            append("v4=").append(result.v4.ifBlank { "-" })
            append(" v6=").append(result.v6.ifBlank { "-" })
            append(" ts=").append(result.ts.ifBlank { "-" })
            append("（").append(result.server).append("）")
        }
        val current = currentEndpoint()
        val chosen = TxtResolver.choose(d, result, TxtResolver.deviceHasGlobalV6(this))
        if (chosen.isNotBlank() && chosen != current) {
            HomingState.log("公告板端点已变：${current ?: "-"} → $chosen，切换并验证")
            clearSuspect()
            trialEndpoint(chosen, current, now)
        } else {
            HomingState.log(
                "公告板端点未变（${current ?: "-"}），继续快探 ${probeCfg.recoveryIntervalSec}s，" +
                    "${probeCfg.softGraceSec}s 内恢复则不重建",
            )
        }
    }

    /** 疑似故障推进：softGrace 到期 → 重建一次；再到期 → 完整恢复。 */
    private fun advanceSuspect(now: Long) {
        if (suspectStage == 1 && now >= suspectDeadline) {
            suspectStage = 2
            suspectDeadline = now + probeCfg.softGraceSec * 1000L
            HomingState.recovering = true
            HomingState.recoveryNote = "正在重建隧道"
            HomingState.log("持续 ${probeCfg.softGraceSec}s 无回包，重建一次隧道")
            if (now - lastRebindAt >= REBIND_MIN_GAP_MS) {
                rebind("疑似故障：重建一次")
            } else {
                suspectDeadline = lastRebindAt + REBIND_MIN_GAP_MS + probeCfg.softGraceSec * 1000L
                HomingState.log("距上次重建太近，等待后再试")
            }
            nextProbeAt = 0L
            return
        }
        if (suspectStage == 2 && now >= suspectDeadline) {
            HomingState.log("重建后仍无回包，进入完整恢复")
            clearSuspect()
            startRecovery(now)
            return
        }
        // 恢复期间端点可能刚好变化：每 10s 重查一次公告板
        if (now - lastTxtQueryAt >= TXT_QUERY_MIN_GAP_MS) queryTxtForSuspect(now, first = false)
    }

    private fun clearSuspect() {
        suspectStage = 0
        suspectAt = 0L
        suspectDeadline = 0L
    }

    private fun onHealthy(now: Long) {
        // 有候选端点正在验证：只有真正出现新握手才算恢复（不能因为刚 setState(UP)
        // 的 45s「首次握手宽限」就误判成功——否则切到坏端点也会立刻显示恢复）。
        if (probe != null) {
            verifyProbe(now)
            return
        }
        if (suspectStage > 0) {
            HomingState.log("握手/流量恢复正常，清除疑似故障状态")
            clearSuspect()
        }
        val ep = currentEndpoint()
        if (!ep.isNullOrBlank() && HomingState.handshakeAt > 0) lastGoodEndpoint = ep
        recoveryAttempts = 0
        nextRecoveryAt = 0L
        if (HomingState.recovering) {
            HomingState.recovering = false
            HomingState.recoveryNote = ""
        }
        if (avoidUntil.isNotEmpty()) {
            val expired = avoidUntil.filterValues { it <= now }.keys
            avoidUntil.keys.removeAll(expired)
        }
    }

    private fun onUnhealthy(now: Long) {
        if (probe != null) {
            verifyProbe(now)
            return
        }
        // 2.3.6：先走「疑似故障」软流程（查公告板 → 变了切 / 没变继续探 → 才重建）
        if (suspectStage > 0) {
            advanceSuspect(now)
            return
        }
        if (probeCfg.enabled && probeEverSucceeded && !HomingState.recovering) {
            // 兜底：探针线程还没回调，tick 先发现无回包
            onProbeMiss(now)
            advanceSuspect(now)
            return
        }
        if (now < nextRecoveryAt) return
        if (now - lastRecoveryAt < 5_000) return
        startRecovery(now)
    }

    // ---------------- 故障恢复（按需查公告板 + 退避 + 回避 + 回退） ----------------

    private fun currentEndpoint(): String? =
        profileObject?.let { HomingProfile.peerEndpoint(it) } ?: HomingState.endpoint.ifBlank { null }

    private fun startRecovery(now: Long) {
        if (!wantConnected || destroyed || cfg == null) return
        lastRecoveryAt = now
        HomingState.recovering = true
        val reason = when {
            probeUnhealthy(now) -> "隧道探针 ${probeFailures} 次无回包"
            HomingState.handshakeAt <= 0L -> "连接后 ${(now - connectedAt) / 1000}s 仍没有握手"
            else -> "握手已 ${(now - HomingState.handshakeAt) / 1000}s 没刷新"
        }
        HomingState.recoveryNote = "正在恢复"
        HomingState.log("隧道不健康（$reason），检查公告板并恢复")

        // 只有隧道不健康时才查公告板；查询失败也不影响后面重建隧道
        val d = currentDiscovery()
        var result: TxtResolver.Result? = null
        if (d != null) {
            result = runCatching { TxtResolver.queryFresh(this, d, maxServers = 3) }.getOrNull()
            HomingState.discoveryAt = now
            if (result != null && result.ok) {
                HomingState.discoveryError = ""
                HomingState.discoverySummary = buildString {
                    append("v4=").append(result.v4.ifBlank { "-" })
                    append(" v6=").append(result.v6.ifBlank { "-" })
                    append(" ts=").append(result.ts.ifBlank { "-" })
                    append("（").append(result.server).append("）")
                }
                HomingState.log("公告板：${result.raw}（来自 ${result.server}）")
            } else {
                HomingState.discoveryError = result?.error ?: "公告板查询异常"
                HomingState.log("公告板查询失败：${HomingState.discoveryError}")
            }
        } else {
            HomingState.log("当前配置没有公告板信息，直接尝试已有端点/重建隧道")
        }

        val current = currentEndpoint()
        val candidate = pickCandidate(d, result, current, now)
        if (candidate != null && candidate != current) {
            trialEndpoint(candidate, current, now)
            return
        }
        // 公告板端点没变化（或不可用）：先给当前端点一次重建 socket 的机会
        if (now - lastRebindAt >= REBIND_MIN_GAP_MS) {
            probe = Probe(
                endpoint = current.orEmpty(),
                previous = current,
                startedAt = now,
                startedHandshake = HomingState.handshakeAt,
                avoidOnFail = false,
            )
            rebind("恢复：重建隧道")
        } else {
            scheduleNextRecovery("重建过于频繁")
        }
    }

    /** 候选端点顺序：公告板首选 → 另一协议 → 配置里的候选 → 上次成功端点 → 当前端点。 */
    private fun pickCandidate(
        d: HomingProfile.Discovery?,
        r: TxtResolver.Result?,
        current: String?,
        now: Long,
    ): String? {
        val ordered = mutableListOf<String>()
        if (d != null && r != null && r.ok) {
            val hasV6 = TxtResolver.deviceHasGlobalV6(this)
            val chosen = TxtResolver.choose(d, r, hasV6)
            if (chosen.isNotBlank()) ordered.add(chosen)
            if (r.v4.isNotBlank() && TxtResolver.familyAllowed(d, "v4")) ordered.add(r.v4)
            if (r.v6.isNotBlank() && TxtResolver.familyAllowed(d, "v6")) ordered.add(r.v6)
        }
        profileObject?.let { prof ->
            ordered.addAll(HomingProfile.alternatives(prof, current))
        }
        lastGoodEndpoint?.let { ordered.add(it) }
        current?.let { ordered.add(it) }

        val seen = HashSet<String>()
        val uniq = ordered.map { it.trim() }.filter { it.isNotBlank() && seen.add(it) }
        val usable = uniq.filter { (avoidUntil[it] ?: 0L) <= now }
        if (usable.isNotEmpty()) return usable.first()
        // 所有端点都在回避期：挑最早解除的再试，否则永远恢复不了
        return uniq.minByOrNull { avoidUntil[it] ?: 0L }
    }

    private fun trialEndpoint(candidate: String, previous: String?, now: Long) {
        val prof = profileObject
        if (prof == null) {
            rebind("无法切换端点，重建隧道")
            return
        }
        lastRotateAt = now
        HomingState.log("尝试切换到端点 $candidate（当前 ${previous ?: "-"}）")
        probe = Probe(
            endpoint = candidate,
            previous = previous,
            startedAt = now,
            startedHandshake = HomingState.handshakeAt,
            avoidOnFail = true,
        )
        switchEndpoint(prof, candidate, previous)
        if (probe != null) nextRecoveryAt = now + PROBE_TIMEOUT_MS + 3_000
    }

    private fun verifyProbe(now: Long) {
        val p = probe ?: return
        val hs = HomingState.handshakeAt
        if (hs > 0 && hs >= p.startedAt - 1_000) {
            finishProbeSuccess(now)
            return
        }
        if (now - p.startedAt < PROBE_TIMEOUT_MS) return

        probe = null
        if (p.avoidOnFail && p.endpoint.isNotBlank()) {
            avoidUntil[p.endpoint] = now + ENDPOINT_AVOID_MS
            HomingState.log(
                "端点 ${p.endpoint} 在 ${PROBE_TIMEOUT_MS / 1000}s 内没有握手，" +
                    "判定不可用（回避 ${ENDPOINT_AVOID_MS / 60_000} 分钟）",
            )
        } else {
            HomingState.log("端点 ${p.endpoint.ifBlank { "当前端点" }} 重建后仍没有握手")
        }
        // 回退到上一个端点（新端点验证失败时不停留在坏端点上）
        val back = p.previous
        if (p.avoidOnFail && !back.isNullOrBlank() && back != p.endpoint) {
            val prof = profileObject
            if (prof != null) {
                HomingState.log("回退到上一个端点 $back")
                switchEndpoint(prof, back, p.endpoint)
            } else {
                rebind("回退重建")
            }
        } else if (now - lastRebindAt >= REBIND_MIN_GAP_MS) {
            rebind("恢复重试")
        }
        scheduleNextRecovery("端点验证失败")
    }

    private fun finishProbeSuccess(now: Long) {
        val p = probe ?: return
        probe = null
        lastGoodEndpoint = p.endpoint.ifBlank { currentEndpoint() }
        avoidUntil.remove(p.endpoint)
        recoveryAttempts = 0
        nextRecoveryAt = 0L
        lastRxAt = now
        lastTrafficAt = now
        activeForceUntil = now + FORCE_ACTIVE_MS
        nextProbeAt = now
        clearSuspect()
        probeFailures = 0
        lastProbeReplyAt = now
        HomingState.recovering = false
        HomingState.recoveryNote = ""
        HomingState.log("端点 ${p.endpoint.ifBlank { "当前端点" }} 验证成功，隧道已恢复")
    }

    private fun scheduleNextRecovery(reason: String) {
        if (!wantConnected) return
        recoveryAttempts = (recoveryAttempts + 1).coerceAtMost(20)
        val base = retryBaseSec.coerceAtLeast(5).toLong() * 1000L
        var delay = base
        repeat((recoveryAttempts - 1).coerceAtMost(8)) {
            delay = (delay * 2).coerceAtMost(RECOVERY_MAX_DELAY_MS)
        }
        delay = (delay * (0.85 + random.nextDouble() * 0.3)).toLong().coerceAtLeast(3_000L)
        nextRecoveryAt = System.currentTimeMillis() + delay
        HomingState.recoveryNote = "${delay / 1000}s 后重试"
        HomingState.log("$reason，${delay / 1000}s 后重试（第 $recoveryAttempts 次）")
    }

    private fun rebind(reason: String) {
        val conf = cfg ?: return
        if (destroyed) return
        lastRebindAt = System.currentTimeMillis()
        HomingState.log("重建隧道（$reason）")
        runCatching { backend.setState(this, Tunnel.State.DOWN, null) }
        Thread.sleep(350)
        try {
            backend.setState(this, Tunnel.State.UP, conf)
            connectedAt = System.currentTimeMillis()
            HomingState.connectedAt = connectedAt
            HomingState.handshakeAt = 0L
            lastTrafficAt = connectedAt
            activeForceUntil = connectedAt + FORCE_ACTIVE_MS
            nextProbeAt = connectedAt + PROBE_START_GRACE_MS
            resetProbe(grace = true)
            HomingState.log("隧道已重建，等待握手验证")
        } catch (t: Throwable) {
            HomingState.log("重建失败：${t.message}")
            scheduleNextRecovery("重建失败")
        }
    }

    // ---------------- 公告板自动跟随 ----------------

    private fun currentDiscovery(): HomingProfile.Discovery? =
        profileObject?.let { HomingProfile.discovery(it) }

    private fun autoFollowEnabled(d: HomingProfile.Discovery): Boolean {
        // 配置里显式写了 discovery.autoFollow 就以配置为准，否则用全局默认开关
        val explicit = profileObject?.optJSONObject("discovery")?.has("autoFollow") == true
        return if (explicit) d.autoFollow else store.autoFollowDefault
    }

    /**
     * 手动查询公告板并热切换端点（「立即跟随公告板端点」/控件点状态卡的公告板行）。
     * 自动侧只在隧道不健康时由 [startRecovery] 查询，不再周期轮询。
     * @return true 表示本次确实切换到了新端点。
     */
    private fun doDiscovery(apply: Boolean, source: String, force: Boolean): Boolean {
        if (destroyed) return false
        val prof = profileObject ?: run {
            if (source == "manual") HomingState.log("当前配置是 .conf 格式，没有公告板信息")
            return false
        }
        val d = currentDiscovery() ?: run {
            if (source == "manual") HomingState.log("配置里没有公告板（discovery）信息")
            return false
        }
        val autoFollow = autoFollowEnabled(d)
        val res = TxtResolver.queryFresh(this, d, maxServers = 3)
        HomingState.discoveryAt = System.currentTimeMillis()
        if (!res.ok) {
            HomingState.discoveryError = res.error
            HomingState.log("公告板查询失败：${res.error}")
            return false
        }
        HomingState.discoveryError = ""
        HomingState.discoverySummary = buildString {
            append("v4=").append(res.v4.ifBlank { "-" })
            append(" v6=").append(res.v6.ifBlank { "-" })
            append(" ts=").append(res.ts.ifBlank { "-" })
            append("（").append(res.server).append("）")
        }
        HomingState.log("公告板：${res.raw}（来自 ${res.server}）")
        if (source == "manual") {
            HomingState.log("公告板记录：${res.record} v4=${res.v4.ifBlank { "-" }} v6=${res.v6.ifBlank { "-" }}")
        }
        val hasV6 = TxtResolver.deviceHasGlobalV6(this)
        val chosen = TxtResolver.choose(d, res, hasV6)
        if (source == "manual") {
            HomingState.log(
                "本机 IPv6 " + (if (hasV6) "可用，auto 偏好 v6 端点" else "不可用，使用 v4 端点") +
                    "；选择 $chosen",
            )
        }
        val current = HomingProfile.peerEndpoint(prof) ?: HomingState.endpoint
        if (chosen.isBlank()) {
            if (source == "manual") HomingState.log("公告板里没有可用端点")
            return false
        }
        if (chosen == current) {
            if (source == "manual") HomingState.log("公告板端点没有变化：$current")
            return false
        }
        if (!apply) return false
        if (!force && !autoFollow) {
            HomingState.log("公告板出现新端点 $chosen（当前 $current），但自动跟随已关闭")
            return false
        }
        HomingState.log("公告板端点变化：${current ?: "-"} -> $chosen")
        switchEndpoint(prof, chosen, current)
        return true
    }

    private fun refreshDiscoveryManually() {
        val applied = doDiscovery(apply = true, source = "manual", force = true)
        if (applied) {
            HomingState.log("已应用公告板最新端点")
        } else if (!wantConnected) {
            HomingState.log("当前未连接，公告板信息已更新，下次连接使用最新端点")
        }
        if (!wantConnected) {
            stopForegroundCompat()
            stopSelf()
        }
    }

    private fun manualSwitch(endpoint: String) {
        val prof = profileObject
        if (prof == null) {
            HomingState.log("当前配置是 .conf 格式，无法按 JSON 修改端点")
            return
        }
        if (endpoint.isBlank()) {
            HomingState.log("没有指定要切换的端点")
            return
        }
        val current = HomingProfile.peerEndpoint(prof)
        if (endpoint == current) {
            HomingState.log("当前已经是该端点：$endpoint")
            return
        }
        switchEndpoint(prof, endpoint, current)
    }

    /** 修改配置里的端点并热切换（不重建 VpnService，不断网）。 */
    private fun switchEndpoint(prof: JSONObject, newEndpoint: String, oldEndpoint: String?) {
        try {
            HomingProfile.setPeerEndpoint(prof, newEndpoint)
            profileObject = prof
            val text = prof.toString()
            profileTextCache = text
            // 把新端点写回配置列表里的当前配置
            val pid = connectedProfileId ?: repo.activeId
            if (pid != null) repo.updateRaw(pid, text, newEndpoint)
            HomingState.endpoint = newEndpoint
            if (wantConnected) {
                val conf = Config.parse(ByteArrayInputStream(HomingProfile.toConf(text).toByteArray()))
                cfg = conf
                try {
                    backend.setState(this, Tunnel.State.UP, conf)
                } catch (t: Throwable) {
                    HomingState.log("热切换失败，改为重建：${t.message}")
                    runCatching { backend.setState(this, Tunnel.State.DOWN, null) }
                    Thread.sleep(300)
                    backend.setState(this, Tunnel.State.UP, conf)
                }
                connectedAt = System.currentTimeMillis()
                HomingState.connectedAt = connectedAt
                HomingState.handshakeAt = 0L
                resetProbe(grace = true)
                HomingState.log("端点已热切换为 $newEndpoint，隧道已更新")
                updateNotification("归途 · 已连接", notificationText())
            } else {
                HomingState.log("配置里的端点已更新为 $newEndpoint（未连接）")
            }
        } catch (t: Throwable) {
            HomingState.log("切换端点失败：${t.message}")
        }
    }

    // ---------------- 周期任务 ----------------

    private fun startLoops() {
        stopLoops()
        statsTask = scheduler.scheduleWithFixedDelay(
            { submit { tick() } }, 0, 2, TimeUnit.SECONDS,
        )
        nextProbeAt = 0L
        HomingState.log(
            "后台健康检查已启动（探针活跃 ${probeCfg.intervalSec}s / 空闲 ${probeCfg.idleIntervalSec}s；健康时不查公告板）",
        )
    }

    private fun stopLoops() {
        runCatching { statsTask?.cancel(false) }
        statsTask = null
        nextProbeAt = 0L
    }

    private fun submit(block: () -> Unit) {
        if (destroyed) return
        runCatching {
            exec.execute {
                if (destroyed) return@execute
                runCatching { block() }.onFailure { Log.e(TAG, "service task failed", it) }
            }
        }
    }

    // ---------------- 网络变化 ----------------

    private fun registerNetworkCallbacks() {
        val request = NetworkRequest.Builder()
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .build()
        runCatching { cm.registerNetworkCallback(request, networkCallback) }
    }

    private fun isVpnNetwork(network: Network): Boolean {
        val caps = runCatching { cm.getNetworkCapabilities(network) }.getOrNull() ?: return false
        return caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)
    }

    private fun describeNetwork(network: Network): String {
        val caps = runCatching { cm.getNetworkCapabilities(network) }.getOrNull()
        val transport = when {
            caps?.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) == true -> "Wi-Fi"
            caps?.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) == true -> "蜂窝"
            caps?.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) == true -> "以太网"
            else -> "未知"
        }
        return transport
    }

    private fun scheduleNetworkChange(reason: String) {
        val token = System.currentTimeMillis()
        netDebounceToken = token
        main.postDelayed({
            if (netDebounceToken != token) return@postDelayed
            if (destroyed || !wantConnected) return@postDelayed
            if (System.currentTimeMillis() - connectedAt < 4000) return@postDelayed
            HomingState.log("检测到$reason，准备自动重连")
            submit { networkRecover(reason) }
        }, 1500)
    }

    private fun networkRecover(reason: String) {
        if (!wantConnected || destroyed || cfg == null) return
        if (System.currentTimeMillis() - lastRebindAt < 3000) return
        // 换网后路径变了：清空端点回避表，先重建隧道；仍不通再由健康检查查公告板
        avoidUntil.clear()
        recoveryAttempts = 0
        nextRecoveryAt = 0L
        clearSuspect()
        probeMode = 0
        nextProbeAt = 0L
        lastTrafficAt = System.currentTimeMillis()
        activeForceUntil = lastTrafficAt + FORCE_ACTIVE_MS
        closeProbeSocket()
        val ep = currentEndpoint()
        probe = Probe(
            endpoint = ep.orEmpty(),
            previous = ep,
            startedAt = System.currentTimeMillis(),
            startedHandshake = HomingState.handshakeAt,
            avoidOnFail = false,
        )
        HomingState.log("网络变化（$reason），重建隧道")
        rebind("网络切换")
    }

    // ---------------- 通知 ----------------

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        val nm = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        if (nm.getNotificationChannel(CHANNEL_ID) != null) return
        val channel = NotificationChannel(CHANNEL_ID, "归途隧道", NotificationManager.IMPORTANCE_LOW).apply {
            description = "归途隧道在前台运行时的常驻通知"
            setShowBadge(false)
        }
        nm.createNotificationChannel(channel)
    }

    private fun startForegroundCompat(title: String, text: String) {
        val notification = buildNotification(title, text, ongoing = true)
        val type = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE
        } else {
            0
        }
        runCatching { ServiceCompat.startForeground(this, NOTIF_ID, notification, type) }
            .onFailure { Log.e(TAG, "startForeground failed", it) }
        foreground = true
        HomingState.foreground = true
    }

    private fun stopForegroundCompat() {
        runCatching { ServiceCompat.stopForeground(this, ServiceCompat.STOP_FOREGROUND_REMOVE) }
        foreground = false
        HomingState.foreground = false
    }

    private fun updateNotification(title: String, text: String, ongoing: Boolean = true) {
        if (!foreground || destroyed) return
        runCatching {
            NotificationManagerCompat.from(this).notify(NOTIF_ID, buildNotification(title, text, ongoing))
        }
    }

    private fun buildNotification(title: String, text: String, ongoing: Boolean): Notification {
        val openIntent = PendingIntent.getActivity(
            this, 0,
            Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val disconnectIntent = PendingIntent.getService(
            this, 1,
            Intent(this, HomingService::class.java).setAction(ACTION_DISCONNECT),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.ic_lock_lock)
            .setContentTitle(title)
            .setContentText(text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setOngoing(ongoing)
            .setOnlyAlertOnce(true)
            .setShowWhen(false)
            .setContentIntent(openIntent)
            .addAction(0, "断开", disconnectIntent)
            .setForegroundServiceBehavior(NotificationCompat.FOREGROUND_SERVICE_IMMEDIATE)
            .build()
    }

    companion object {
        private const val TAG = "HOMING"
        private const val TUNNEL_NAME = "homing0"
        private const val CHANNEL_ID = "homing_tunnel"
        private const val NOTIF_ID = 51820

        /** 握手在 180s 内刷新算健康：keepalive=25s 时约 2 分钟一次新握手。 */
        private const val HANDSHAKE_STALE_MS = 180_000L

        /** 刚连接时给首次握手留的宽限。 */
        private const val NEVER_HANDSHAKE_GRACE_MS = 45_000L

        /** 握手统计没刷新但下行在动，也认为隧道还活着。 */
        private const val RX_PROGRESS_MS = 60_000L

        /** 切换候选端点后等握手的验证窗口。 */
        private const val PROBE_TIMEOUT_MS = 12_000L

        /** 验证失败的端点回避多久（期间不再优先选它）。 */
        private const val ENDPOINT_AVOID_MS = 10 * 60_000L

        /** 故障恢复退避上限。 */
        private const val RECOVERY_MAX_DELAY_MS = 5 * 60_000L

        /** 两次重建隧道的最小间隔。 */
        private const val REBIND_MIN_GAP_MS = 20_000L

        /** 配置里没写 discovery.interval 时，故障重查的基准间隔（秒）。 */
        private const val DEFAULT_RETRY_BASE_SEC = 15

        /** 刚连接 / 切网 / 重建后强制按活跃档运行的时长。 */
        private const val FORCE_ACTIVE_MS = 60_000L

        /** 疑似故障期间两次公告板查询的最小间隔（防止单次丢包频繁刷 DNS）。 */
        private const val TXT_QUERY_MIN_GAP_MS = 10_000L

        /** 2s tick 内超过这么多字节才算「有流量」：忽略 keepalive（几十字节）和探针本身（约 150B），
         *  否则空闲时探针会把自己判成「活跃」，空闲档永远进不去。 */
        private const val TRAFFIC_ACTIVE_BYTES = 1024L

        /** 探针启动宽限：刚连上/刚切换端点时给 VPN 一点时间。 */
        private const val PROBE_START_GRACE_MS = 10_000L

        const val ACTION_CONNECT = "io.homing.client.action.CONNECT"
        const val ACTION_DISCONNECT = "io.homing.client.action.DISCONNECT"
        const val ACTION_REFRESH = "io.homing.client.action.REFRESH"
        const val ACTION_SWITCH = "io.homing.client.action.SWITCH"
        const val EXTRA_PROFILE_ID = "profile_id"
        const val EXTRA_ENDPOINT = "endpoint"

        /** 从任意前台场景下发服务指令（Activity / 编辑页用）。 */
        fun startAction(ctx: Context, action: String) {
            runCatching {
                ContextCompat.startForegroundService(
                    ctx,
                    Intent(ctx, HomingService::class.java).setAction(action),
                )
            }
        }

        fun simpleIntent(ctx: Context, action: String): Intent =
            Intent(ctx, HomingService::class.java).setAction(action)
    }
}
