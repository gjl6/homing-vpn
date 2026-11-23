package io.homing.client

import android.util.Log
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * 进程内共享的「隧道状态 + 事件日志」。
 *
 * 前台服务（[HomingService]）负责写入，界面每秒轮询读取。
 * 服务和界面在同一进程，直接共享对象即可，不需要 Binder / 广播。
 */
object HomingState {
    private const val TAG = "HOMING"

    @Volatile
    var tunnelState: String = "DOWN"

    @Volatile
    var endpoint: String = ""

    @Volatile
    var handshakeAt: Long = 0L

    @Volatile
    var rxBytes: Long = 0L

    @Volatile
    var txBytes: Long = 0L

    @Volatile
    var connectedAt: Long = 0L

    @Volatile
    var profileName: String = ""

    /** 当前隧道正在使用的配置 id（对应配置列表里的卡片） */
    @Volatile
    var connectedProfileId: String? = null

    @Volatile
    var discoverySummary: String = ""

    @Volatile
    var discoveryError: String = ""

    @Volatile
    var discoveryAt: Long = 0L

    @Volatile
    var foreground: Boolean = false

    /** 是否正在故障恢复（隧道不健康 → 查公告板 / 切换候选端点）。 */
    @Volatile
    var recovering: Boolean = false

    /** 恢复进度提示（如「30s 后重试」），界面和通知里显示。 */
    @Volatile
    var recoveryNote: String = ""

    @Volatile
    var lastEvent: String = ""

    private const val MAX_LOG = 400
    private val logs = ArrayDeque<String>()

    fun log(msg: String) {
        val ts = SimpleDateFormat("HH:mm:ss", Locale.US).format(Date())
        val line = "[$ts] $msg"
        Log.i(TAG, msg)
        synchronized(logs) {
            logs.addLast(line)
            while (logs.size > MAX_LOG) logs.removeFirst()
        }
        lastEvent = line
    }

    fun logLines(): List<String> = synchronized(logs) { ArrayList(logs) }

    fun clearLog() {
        synchronized(logs) { logs.clear() }
        lastEvent = ""
    }

    fun handshakeAgoSec(): Long =
        if (handshakeAt <= 0L) -1L else (System.currentTimeMillis() - handshakeAt) / 1000

    /** 状态面板文本（界面直接显示） */
    fun statusText(): String {
        val sb = StringBuilder()
        val up = tunnelState == "UP"
        sb.append(if (up) "隧道：已连接（UP）" else "隧道：未连接（$tunnelState）")
        if (profileName.isNotBlank()) sb.append(" · 配置「").append(profileName).append("」")
        sb.append('\n').append("Endpoint：").append(endpoint.ifBlank { "(未设置)" })
        if (up) {
            val ago = handshakeAgoSec()
            sb.append('\n').append("最近握手：").append(if (ago >= 0) "${ago}s 前" else "等待中…")
            sb.append(" · RX/TX：").append(formatBytes(rxBytes)).append(" / ").append(formatBytes(txBytes))
        }
        if (discoverySummary.isNotBlank()) {
            sb.append('\n').append("公告板：").append(discoverySummary)
        }
        if (discoveryError.isNotBlank()) {
            sb.append('\n').append("公告板错误：").append(discoveryError)
        }
        if (foreground) sb.append('\n').append("前台服务：常驻中")
        return sb.toString()
    }

    private fun formatBytes(v: Long): String = when {
        v >= 1024L * 1024L -> String.format(Locale.US, "%.1fMB", v / (1024.0 * 1024.0))
        v >= 1024L -> String.format(Locale.US, "%.1fKB", v / 1024.0)
        else -> "${v}B"
    }
}
