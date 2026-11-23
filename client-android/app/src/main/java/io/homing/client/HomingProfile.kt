package io.homing.client

import org.json.JSONArray
import org.json.JSONObject

/**
 * 归途原生配置（`homing-profile/1`）的解析 / 校验 / 转换。
 *
 * 原生 JSON 里跟「公告板按需跟随」相关的是：
 * ```
 * "discovery": {
 *   "enabled": true,
 *   "record": "vpn.example.com",   // 公告板 TXT 记录名
 *   "autoFollow": true,          // 恢复时自动切换公告板里的新端点
 *   "preferred": "auto",         // auto / v4 / v6
 *   "interval": 15,              // 故障重查的基准间隔（秒），失败后指数退避
 *   "dns": ""                    // 可选：指定查询用的 DNS（host 或 host:port）
 * }
 * ```
 * 注意：客户端**健康时不会周期性查公告板**，只有隧道不通时才查。
 * 另外可选 `"probe": {"enabled":true,"interval":10,"failures":3,"timeoutMs":1500,"host":"10.8.0.1","port":51824}`
 * 控制隧道内健康探针（不写则用默认 10s / 3 次 / 1500ms）。
 */
object HomingProfile {
    const val FORMAT = "homing-profile/1"
    const val MIN_INTERVAL_SEC = 5
    const val MAX_INTERVAL_SEC = 3600

    /** 隧道内探针参数（可选配置，写在 JSON 的 `probe` 对象里；不写用默认值）。 */
    data class Probe(
        val enabled: Boolean = true,
        /** 活跃档间隔（最近有隧道流量时） */
        val intervalSec: Int = 10,
        /** 空闲档间隔（一段时间没有隧道流量时） */
        val idleIntervalSec: Int = 30,
        /** 多少秒没有 RX/TX 增长算空闲 */
        val idleAfterSec: Int = 5,
        /** 进入「疑似故障」的连续失败阈值；默认 1：一次没回包就查公告板确认 */
        val failures: Int = 1,
        val timeoutMs: Int = 1500,
        /** 疑似故障期间的快速探测间隔 */
        val recoveryIntervalSec: Int = 5,
        /** 公告板没变时「只探不重建」的宽限（秒） */
        val softGraceSec: Int = 30,
        val host: String = "10.8.0.1",
        val port: Int = 51824,
    ) {
        companion object {
            const val MIN_PROBE_INTERVAL_SEC = 3
            const val MAX_PROBE_INTERVAL_SEC = 120
            const val MIN_IDLE_INTERVAL_SEC = 3
            const val MAX_IDLE_INTERVAL_SEC = 300
            const val MIN_IDLE_AFTER_SEC = 2
            const val MAX_IDLE_AFTER_SEC = 60
            const val MIN_FAILURES = 1
            const val MAX_FAILURES = 10
            const val MIN_TIMEOUT_MS = 300
            const val MAX_TIMEOUT_MS = 5000
            const val MIN_RECOVERY_INTERVAL_SEC = 2
            const val MAX_RECOVERY_INTERVAL_SEC = 30
            const val MIN_SOFT_GRACE_SEC = 10
            const val MAX_SOFT_GRACE_SEC = 120
            val DEFAULT = Probe()
        }
    }

    data class Discovery(
        val record: String,
        val dns: String,
        val preferred: String,
        val intervalSec: Int,
        val autoFollow: Boolean,
    )

    fun isJson(text: String): Boolean = text.trimStart().startsWith("{")

    fun isConf(text: String): Boolean =
        text.contains("[Interface]", ignoreCase = true) && text.contains("[Peer]", ignoreCase = true)

    /**
     * 校验配置并返回「规范化文本」：JSON 会压成单行，.conf 原样返回。
     * 失败抛 [IllegalArgumentException]，message 是给用户看的中文原因。
     */
    fun normalize(text: String): String {
        val t = text.trim()
        if (t.isEmpty()) throw IllegalArgumentException("配置为空")
        if (isJson(t)) {
            val o = try {
                JSONObject(t)
            } catch (e: Exception) {
                throw IllegalArgumentException("JSON 解析失败：${e.message}")
            }
            validate(o)
            return o.toString()
        }
        if (isConf(t)) {
            if (!Regex("(?im)^\\s*PrivateKey\\s*=").containsMatchIn(t)) {
                throw IllegalArgumentException(".conf 缺少 PrivateKey")
            }
            if (!Regex("(?im)^\\s*PublicKey\\s*=").containsMatchIn(t)) {
                throw IllegalArgumentException(".conf 缺少 PublicKey")
            }
            return t
        }
        throw IllegalArgumentException("无法识别的配置格式（既不是归途 JSON，也不是 .conf）")
    }

    private fun validate(o: JSONObject) {
        val fmt = o.optString("format", "")
        if (fmt.isNotEmpty() && fmt != FORMAT) {
            throw IllegalArgumentException("不认识的配置格式 $fmt（期望 $FORMAT）")
        }
        if (o.optString("privateKey", "").isBlank()) {
            throw IllegalArgumentException("归途配置缺少 privateKey")
        }
        val peers = o.optJSONArray("peers") ?: throw IllegalArgumentException("归途配置缺少 peers")
        if (peers.length() == 0) throw IllegalArgumentException("归途配置 peers 为空")
        val p0 = peers.optJSONObject(0) ?: throw IllegalArgumentException("归途配置 peers[0] 无效")
        if (p0.optString("publicKey", "").isBlank()) {
            throw IllegalArgumentException("归途配置 peers[0] 缺少 publicKey")
        }
    }

    fun nameOf(o: JSONObject): String = o.optString("name", "")

    fun peerEndpoint(o: JSONObject): String? {
        val peers = o.optJSONArray("peers") ?: return null
        if (peers.length() == 0) return null
        val s = peers.optJSONObject(0)?.optString("endpoint", "") ?: ""
        return s.ifBlank { null }
    }

    /** 修改 peers[0].endpoint，同时把新端点放到候选列表第一位。 */
    fun setPeerEndpoint(o: JSONObject, endpoint: String) {
        val peers = o.optJSONArray("peers") ?: JSONArray().also { o.put("peers", it) }
        val p = if (peers.length() > 0) {
            peers.optJSONObject(0) ?: JSONObject().also { peers.put(it) }
        } else {
            JSONObject().also { peers.put(it) }
        }
        p.put("endpoint", endpoint)
        val old = o.optJSONArray("endpoints")
        val arr = JSONArray()
        arr.put(endpoint)
        if (old != null) {
            for (i in 0 until old.length()) {
                val v = old.optString(i, "")
                if (v.isNotBlank() && v != endpoint) arr.put(v)
            }
        }
        o.put("endpoints", arr)
    }

    /** 配置里除当前端点外的其它候选端点（用于长时间握手失败时轮换）。 */
    fun alternatives(o: JSONObject, current: String?): List<String> {
        val out = mutableListOf<String>()
        o.optJSONArray("endpoints")?.let { eps ->
            for (i in 0 until eps.length()) {
                val v = eps.optString(i, "")
                if (v.isNotBlank() && v != current && !out.contains(v)) out.add(v)
            }
        }
        val pe = peerEndpoint(o)
        if (!pe.isNullOrBlank() && pe != current && !out.contains(pe)) out.add(0, pe)
        return out
    }

    fun discovery(o: JSONObject): Discovery? {
        val d = o.optJSONObject("discovery") ?: return null
        if (!d.optBoolean("enabled", true)) return null
        val record = d.optString("record", "").trim()
        if (record.isBlank()) return null
        return Discovery(
            record = record,
            dns = d.optString("dns", "").trim(),
            preferred = d.optString("preferred", "auto").trim().lowercase().ifBlank { "auto" },
            intervalSec = d.optInt("interval", 15).coerceIn(MIN_INTERVAL_SEC, MAX_INTERVAL_SEC),
            autoFollow = d.optBoolean("autoFollow", true),
        )
    }

    /** 解析隧道探针参数；没写 `probe` 时用默认值（2.3.6 默认：活跃 10s / 空闲 30s / 一次失败确认）。 */
    fun probe(o: JSONObject): Probe {
        val p = o.optJSONObject("probe") ?: return Probe.DEFAULT
        val host = p.optString("host", Probe.DEFAULT.host).trim().ifBlank { Probe.DEFAULT.host }
        val interval = p.optInt("interval", Probe.DEFAULT.intervalSec)
            .coerceIn(Probe.MIN_PROBE_INTERVAL_SEC, Probe.MAX_PROBE_INTERVAL_SEC)
        // 旧配置没有 idleInterval：按「至少 30s，或活跃间隔的 2 倍」推断，保持兼容
        val idleDefault = maxOf(30, interval * 2)
            .coerceIn(Probe.MIN_IDLE_INTERVAL_SEC, Probe.MAX_IDLE_INTERVAL_SEC)
        return Probe(
            enabled = p.optBoolean("enabled", true),
            intervalSec = interval,
            idleIntervalSec = p.optInt("idleInterval", idleDefault)
                .coerceIn(Probe.MIN_IDLE_INTERVAL_SEC, Probe.MAX_IDLE_INTERVAL_SEC),
            idleAfterSec = p.optInt("idleAfter", Probe.DEFAULT.idleAfterSec)
                .coerceIn(Probe.MIN_IDLE_AFTER_SEC, Probe.MAX_IDLE_AFTER_SEC),
            failures = p.optInt("failures", Probe.DEFAULT.failures)
                .coerceIn(Probe.MIN_FAILURES, Probe.MAX_FAILURES),
            timeoutMs = p.optInt("timeoutMs", Probe.DEFAULT.timeoutMs)
                .coerceIn(Probe.MIN_TIMEOUT_MS, Probe.MAX_TIMEOUT_MS),
            recoveryIntervalSec = p.optInt("recoveryInterval", Probe.DEFAULT.recoveryIntervalSec)
                .coerceIn(Probe.MIN_RECOVERY_INTERVAL_SEC, Probe.MAX_RECOVERY_INTERVAL_SEC),
            softGraceSec = p.optInt("softGrace", Probe.DEFAULT.softGraceSec)
                .coerceIn(Probe.MIN_SOFT_GRACE_SEC, Probe.MAX_SOFT_GRACE_SEC),
            host = host,
            port = p.optInt("port", Probe.DEFAULT.port).coerceIn(1, 65535),
        )
    }

    /** 归途 JSON / WireGuard .conf 统一转成官方库可解析的 .conf 文本 */
    fun toConf(text: String): String {
        val t = text.trim()
        if (!isJson(t)) return t
        val o = JSONObject(t)
        val sb = StringBuilder()
        sb.append("[Interface]\n")
        sb.append("PrivateKey = ").append(o.getString("privateKey")).append('\n')
        o.optJSONArray("addresses")?.let { arr ->
            if (arr.length() > 0) {
                val addrs = (0 until arr.length()).joinToString(", ") { arr.getString(it) }
                sb.append("Address = ").append(addrs).append('\n')
            }
        }
        if (o.has("mtu") && !o.isNull("mtu")) {
            sb.append("MTU = ").append(o.optInt("mtu", 1420)).append('\n')
        }
        o.optJSONArray("dns")?.let { arr ->
            if (arr.length() > 0) {
                val dns = (0 until arr.length()).joinToString(", ") { arr.getString(it) }
                sb.append("DNS = ").append(dns).append('\n')
            }
        }
        val peers = o.optJSONArray("peers") ?: throw IllegalArgumentException("配置缺少 peers")
        if (peers.length() == 0) throw IllegalArgumentException("配置缺少 peers")
        for (i in 0 until peers.length()) {
            val p = peers.getJSONObject(i)
            sb.append("\n[Peer]\n")
            sb.append("PublicKey = ").append(p.getString("publicKey")).append('\n')
            p.optString("presharedKey", "").takeIf { it.isNotBlank() }?.let {
                sb.append("PresharedKey = ").append(it).append('\n')
            }
            p.optJSONArray("allowedIps")?.let { arr ->
                if (arr.length() > 0) {
                    val ips = (0 until arr.length()).joinToString(", ") { arr.getString(it) }
                    sb.append("AllowedIPs = ").append(ips).append('\n')
                }
            }
            p.optString("endpoint", "").takeIf { it.isNotBlank() }?.let {
                sb.append("Endpoint = ").append(it).append('\n')
            }
            if (p.has("persistentKeepalive") && !p.isNull("persistentKeepalive")) {
                sb.append("PersistentKeepalive = ").append(p.optInt("persistentKeepalive", 0)).append('\n')
            }
        }
        return sb.toString()
    }

    /** 把 .conf 里的 Endpoint 行替换成新端点（没有就插到第一个 [Peer] 后面）。 */
    fun replaceConfEndpoint(conf: String, endpoint: String): String {
        val re = Regex("(?im)^\\s*Endpoint\\s*=.*$")
        if (re.containsMatchIn(conf)) {
            return conf.replace(re, "Endpoint = $endpoint")
        }
        return conf.replaceFirst(Regex("(?im)^\\[Peer\\]\\s*$"), "[Peer]\nEndpoint = $endpoint")
    }

    fun confEndpoint(conf: String): String? {
        val m = Regex("(?im)^\\s*Endpoint\\s*=\\s*(.+)$").find(conf) ?: return null
        return m.groupValues[1].trim().ifBlank { null }
    }

    /** 一行摘要，导入/日志里用 */
    fun describe(text: String): String = try {
        if (isJson(text)) {
            val o = JSONObject(text)
            val name = nameOf(o).ifBlank { "未命名" }
            val addrs = o.optJSONArray("addresses")?.let { a ->
                (0 until a.length()).joinToString(",") { a.optString(it) }
            } ?: ""
            val ep = peerEndpoint(o) ?: "-"
            val d = discovery(o)
            buildString {
                append("配置「").append(name).append("」")
                if (addrs.isNotBlank()) append(" 地址=").append(addrs)
                append(" 端点=").append(ep)
                if (d != null) append(" 公告板=").append(d.record)
            }
        } else {
            ".conf 配置（无公告板信息）"
        }
    } catch (e: Exception) {
        "配置无效：${e.message}"
    }
}
