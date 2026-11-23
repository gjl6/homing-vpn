package io.homing.client

import android.content.Context
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import java.io.ByteArrayOutputStream
import java.net.DatagramPacket
import java.net.DatagramSocket
import java.net.Inet4Address
import java.net.Inet6Address
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.SocketTimeoutException
import java.util.Random

/**
 * 公告板（DNS TXT）解析与查询。
 *
 * 网关发布的 TXT 记录格式：
 * ```
 * v1;v4=203.0.113.10:9612;v6=[2001:db8::10]:51820;ts=...
 * ```
 *
 * 为什么自己发 DNS 包：隧道还没通（或端点已经变了）时，系统 DNS 会被塞进隧道里，
 * 查公告板必然失败 —— 典型的「先有鸡还是先有蛋」。这里直接 UDP 查公共 DNS，
 * 并把 socket 绑到**底层物理网络**上（Android 的 Network.bindSocket），绕开 VPN。
 */
object TxtResolver {
    private val V4_DNS = listOf("223.5.5.5", "119.29.29.29")
    private val V6_DNS = listOf("2400:3200::1", "2400:3200:baba::1", "2402:4e00::")

    private val random = Random()

    data class Result(
        val record: String,
        val v4: String = "",
        val v6: String = "",
        val ts: String = "",
        val raw: String = "",
        val server: String = "",
        val error: String = "",
    ) {
        val ok: Boolean get() = error.isEmpty() && (v4.isNotBlank() || v6.isNotBlank())
    }

    /** 查询一次公告板（阻塞，需要放在后台线程）。 */
    fun query(ctx: Context, d: HomingProfile.Discovery): Result {
        val cm = ctx.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
        val net = underlyingNetwork(cm) ?: defaultNonVpnNetwork(cm)
        val lp = net?.let { runCatching { cm.getLinkProperties(it) }.getOrNull() }
        val hasV4 = linkHasV4(lp)
        val hasV6 = linkHasGlobalV6(lp)
        val netDesc = netDesc(cm, net)
        val errors = mutableListOf<String>()
        var tried = 0
        for (server in servers(d, hasV4, hasV6)) {
            if (tried >= 4) break
            tried++
            // 先绑到「底层物理网络」查询（绕开 VPN，隧道没通时也能用）；
            // 失败再退回不绑定（走系统默认路由，隧道通的时候也能用）。
            var (txt, err) = lookupTxt(server, d.record, net)
            if (txt == null && net != null) {
                val (txt2, err2) = lookupTxt(server, d.record, null)
                if (txt2 != null) {
                    txt = txt2
                } else {
                    val firstLabel = if (err?.startsWith("bindSocket") == true) "绑定网络失败" else "绑定网络无响应"
                    err = "$firstLabel($err)；默认路由无响应($err2)"
                }
            }
            if (txt == null) {
                errors.add("${hostOf(server)}: ${err ?: "无响应"}")
                continue
            }
            val parsed = parseTxt(txt)
            if (parsed == null) {
                errors.add("${hostOf(server)}: TXT 不是 v1 公告")
                continue
            }
            HomingState.log("公告板查询走 $netDesc，DNS ${hostOf(server)}")
            return Result(
                record = d.record,
                v4 = parsed.first,
                v6 = parsed.second,
                ts = parsed.third,
                raw = txt,
                server = hostOf(server),
            )
        }
        return Result(
            record = d.record,
            error = "查询 TXT ${d.record} 失败（$netDesc）：" +
                (errors.joinToString("; ").ifBlank { "没有可用的 DNS" }),
        )
    }

    /**
     * 故障恢复时用的「新鲜」查询：并行问多家 DNS，取 ts 最大（最新）的一条。
     *
     * 递归 DNS 有 TTL 缓存（阿里云 TXT 默认 600s），只问一家可能拿到旧端点。
     * 并行多问几家取最新值，能在公告板换端点后尽快恢复。
     */
    fun queryFresh(ctx: Context, d: HomingProfile.Discovery, maxServers: Int = 3): Result {
        val cm = ctx.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
        val net = underlyingNetwork(cm) ?: defaultNonVpnNetwork(cm)
        val lp = net?.let { runCatching { cm.getLinkProperties(it) }.getOrNull() }
        val hasV4 = linkHasV4(lp)
        val hasV6 = linkHasGlobalV6(lp)
        val netDesc = netDesc(cm, net)
        val list = servers(d, hasV4, hasV6).take(maxServers.coerceAtLeast(1))
        if (list.isEmpty()) {
            return Result(record = d.record, error = "查询 TXT ${d.record} 失败（$netDesc）：没有可用的 DNS")
        }
        val results = java.util.Collections.synchronizedList(mutableListOf<Result>())
        val latch = java.util.concurrent.CountDownLatch(list.size)
        for (server in list) {
            Thread {
                try {
                    var (txt, _) = lookupTxt(server, d.record, net)
                    if (txt == null && net != null) {
                        // 绑定底层网络没响应时，再走默认路由试一次
                        val second = lookupTxt(server, d.record, null)
                        txt = second.first ?: txt
                    }
                    val raw = txt ?: return@Thread
                    val parsed = parseTxt(raw) ?: return@Thread
                    results.add(
                        Result(
                            record = d.record,
                            v4 = parsed.first,
                            v6 = parsed.second,
                            ts = parsed.third,
                            raw = raw,
                            server = hostOf(server),
                        ),
                    )
                } catch (_: Throwable) {
                    // 单家失败不影响其它 DNS
                } finally {
                    latch.countDown()
                }
            }.apply { isDaemon = true }.start()
        }
        latch.await(3, java.util.concurrent.TimeUnit.SECONDS)
        if (results.isEmpty()) {
            return Result(
                record = d.record,
                error = "查询 TXT ${d.record} 失败（$netDesc）：${list.size} 个 DNS 都没有响应",
            )
        }
        val best = results.maxByOrNull { freshness(it.ts) } ?: results.first()
        HomingState.log("公告板查询走 $netDesc，采用 DNS ${best.server}（ts=${best.ts.ifBlank { "-" }}，共 ${results.size} 家应答）")
        return best
    }

    /** ts 越大越新鲜；兼容 epoch 秒和 ISO 时间字符串。 */
    private fun freshness(ts: String): Long {
        val t = ts.trim()
        if (t.isEmpty()) return 0L
        t.toLongOrNull()?.let { return it }
        return runCatching {
            java.text.SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ss", java.util.Locale.US)
                .parse(t.removeSuffix("Z"))!!.time / 1000L
        }.getOrDefault(0L)
    }

    private fun netDesc(cm: ConnectivityManager, net: Network?): String {
        if (net == null) return "无底层网络"
        val lp = runCatching { cm.getLinkProperties(net) }.getOrNull()
        val caps = runCatching { cm.getNetworkCapabilities(net) }.getOrNull()
        val transport = when {
            caps?.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) == true -> "Wi-Fi"
            caps?.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) == true -> "蜂窝"
            caps?.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) == true -> "以太网"
            else -> "未知"
        }
        return "$transport/${lp?.interfaceName ?: "?"}"
    }

    /** 解析一条 TXT 内容（v4/v6/ts）；不是有效的 v1 公告返回 null。 */
    fun parseTxt(raw: String): Triple<String, String, String>? {
        var v4 = ""
        var v6 = ""
        var ts = ""
        var ok = false
        for (part0 in raw.split(';')) {
            val part = part0.trim()
            if (part.isEmpty()) continue
            val eq = part.indexOf('=')
            if (eq < 0) {
                if (part.equals("v1", ignoreCase = true)) ok = true
                continue
            }
            val key = part.substring(0, eq).trim().lowercase()
            val value = part.substring(eq + 1).trim()
            when (key) {
                "v4" -> { v4 = value; ok = true }
                "v6" -> { v6 = value; ok = true }
                "ts" -> ts = value
            }
        }
        return if (ok && (v4.isNotEmpty() || v6.isNotEmpty())) Triple(v4, v6, ts) else null
    }

    /** 按用户偏好选一个可用端点：auto / v4 / v6 / v4,v6 / v6,v4 / v4-only / v6-only。 */
    fun choose(d: HomingProfile.Discovery, r: Result, hasV6: Boolean): String {
        val pref = d.preferred.trim().lowercase()
        if (pref == "v4-only") return r.v4
        if (pref == "v6-only") return r.v6
        val order = when (pref) {
            "v4", "v4,v6" -> listOf("v4", "v6")
            "v6", "v6,v4" -> listOf("v6", "v4")
            else -> if (hasV6) listOf("v6", "v4") else listOf("v4", "v6")
        }
        for (fam in order) {
            val v = if (fam == "v4") r.v4 else r.v6
            if (v.isNotBlank()) return v
        }
        return ""
    }

    /** 当前偏好是否允许某个协议（仅 IPv4/仅 IPv6 用于过滤公告板里的另一协议）。 */
    fun familyAllowed(d: HomingProfile.Discovery?, family: String): Boolean {
        val pref = d?.preferred?.trim()?.lowercase().orEmpty()
        return when (pref) {
            "v4-only" -> family == "v4"
            "v6-only" -> family == "v6"
            else -> true
        }
    }

    /** 本机（底层网络）是否有公网 IPv6 —— 有才优先用公告板里的 v6 端点。 */
    fun deviceHasGlobalV6(ctx: Context): Boolean {
        val cm = ctx.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
        for (n in cm.allNetworks) {
            val caps = cm.getNetworkCapabilities(n) ?: continue
            if (caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) continue
            val lp = runCatching { cm.getLinkProperties(n) }.getOrNull() ?: continue
            if (linkHasGlobalV6(lp)) return true
        }
        return false
    }

    private fun linkHasV4(lp: LinkProperties?): Boolean =
        lp?.linkAddresses?.any { it.address is Inet4Address } ?: false

    private fun linkHasGlobalV6(lp: LinkProperties?): Boolean =
        lp?.linkAddresses?.any { addr ->
            val a = addr.address
            a is Inet6Address && !a.isLinkLocalAddress && !a.isLoopbackAddress &&
                !a.isAnyLocalAddress && !isUla(a)
        } ?: false

    private fun isUla(a: InetAddress): Boolean {
        val b = a.address
        return b.size == 16 && (b[0].toInt() and 0xFE) == 0xFC
    }

    // ---------------- 网络选择 ----------------

    private fun underlyingNetwork(cm: ConnectivityManager): Network? {
        for (n in cm.allNetworks) {
            val caps = cm.getNetworkCapabilities(n) ?: continue
            if (caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) continue
            if (!caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)) continue
            if (caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) ||
                caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) ||
                caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)
            ) {
                return n
            }
        }
        return null
    }

    private fun defaultNonVpnNetwork(cm: ConnectivityManager): Network? {
        val active = cm.activeNetwork ?: return null
        val caps = cm.getNetworkCapabilities(active) ?: return null
        return if (caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) null else active
    }

    // ---------------- DNS ----------------

    private fun servers(
        d: HomingProfile.Discovery,
        hasV4: Boolean,
        hasV6: Boolean,
    ): List<InetSocketAddress> {
        val out = mutableListOf<InetSocketAddress>()
        fun add(host: String, port: Int = 53) {
            runCatching { out.add(InetSocketAddress(InetAddress.getByName(host), port)) }
        }
        if (d.dns.isNotBlank()) parseServer(d.dns)?.let { out.add(it) }
        if (hasV4) V4_DNS.forEach { add(it) }
        if (hasV6) V6_DNS.forEach { add(it) }
        if (out.isEmpty()) V4_DNS.forEach { add(it) }
        return out.distinctBy { hostOf(it) + ":" + it.port }
    }

    private fun parseServer(s: String): InetSocketAddress? {
        val t = s.trim()
        if (t.isEmpty()) return null
        if (t.startsWith("[")) {
            val end = t.indexOf(']')
            if (end < 0) return null
            val host = t.substring(1, end)
            val port = if (end + 1 < t.length && t[end + 1] == ':') {
                t.substring(end + 2).toIntOrNull() ?: 53
            } else 53
            return runCatching { InetSocketAddress(InetAddress.getByName(host), port) }.getOrNull()
        }
        if (t.count { it == ':' } == 1) {
            val host = t.substringBefore(':')
            val port = t.substringAfter(':').toIntOrNull() ?: 53
            return runCatching { InetSocketAddress(InetAddress.getByName(host), port) }.getOrNull()
        }
        return runCatching { InetSocketAddress(InetAddress.getByName(t), 53) }.getOrNull()
    }

    private fun hostOf(addr: InetSocketAddress): String = addr.address?.hostAddress ?: addr.hostString

    private fun lookupTxt(server: InetSocketAddress, name: String, net: Network?): Pair<String?, String?> {
        var sock: DatagramSocket? = null
        try {
            val opened = openSocket(server, net)
            sock = opened.first
            val bindError = opened.second
            if (net != null && bindError != null) {
                return null to "bindSocket 失败: $bindError"
            }
            val id = random.nextInt(0x10000)
            val query = buildQuery(id, name)
            sock.send(DatagramPacket(query, query.size, server))
            val deadline = System.currentTimeMillis() + TIMEOUT_MS
            val buf = ByteArray(2048)
            while (System.currentTimeMillis() < deadline) {
                val packet = DatagramPacket(buf, buf.size)
                try {
                    sock.receive(packet)
                } catch (e: SocketTimeoutException) {
                    continue
                }
                val data = packet.data.copyOf(packet.length)
                val txt = parseTxtResponse(data, id)
                if (txt != null) return txt to null
            }
            return null to "无响应(${TIMEOUT_MS}ms)"
        } catch (t: Throwable) {
            return null to "${t.javaClass.simpleName}: ${t.message}"
        } finally {
            runCatching { sock?.close() }
        }
    }

    /** @return socket 和一个「绑定网络失败」的错误描述（绑定成功时为 null）。 */
    private fun openSocket(server: InetSocketAddress, net: Network?): Pair<DatagramSocket, String?> {
        val v6 = server.address is Inet6Address
        val s: DatagramSocket = if (v6) {
            runCatching {
                DatagramSocket(null).apply { bind(InetSocketAddress(InetAddress.getByName("::"), 0)) }
            }.getOrElse { DatagramSocket() }
        } else {
            DatagramSocket()
        }
        runCatching { s.soTimeout = 600 }
        if (net != null) {
            val err = runCatching { net.bindSocket(s) }.exceptionOrNull()
            if (err != null) {
                return DatagramSocket().also { runCatching { it.soTimeout = 600 } } to
                    "${err.javaClass.simpleName}: ${err.message}"
            }
        }
        return s to null
    }

    private fun buildQuery(id: Int, name: String): ByteArray {
        val out = ByteArrayOutputStream()
        fun u16(v: Int) {
            out.write((v shr 8) and 0xFF)
            out.write(v and 0xFF)
        }
        u16(id)
        u16(0x0100) // RD=1
        u16(1)
        u16(0)
        u16(0)
        u16(0)
        for (label in name.trimEnd('.').split('.')) {
            if (label.isEmpty()) continue
            val b = label.toByteArray(Charsets.UTF_8)
            if (b.size > 63) throw IllegalArgumentException("记录名标签过长: $label")
            out.write(b.size)
            out.write(b)
        }
        out.write(0)
        u16(16) // TXT
        u16(1) // IN
        return out.toByteArray()
    }

    private fun parseTxtResponse(data: ByteArray, wantId: Int): String? {
        if (data.size < 12) return null
        fun u8(i: Int): Int = data[i].toInt() and 0xFF
        fun u16(i: Int): Int = (u8(i) shl 8) or u8(i + 1)

        if (u16(0) != wantId) return null
        val flags = u16(2)
        if ((flags and 0x8000) == 0) return null // 不是响应
        if ((flags and 0x000F) != 0) return null // RCODE != 0（NXDOMAIN 等）
        val qd = u16(4)
        val an = u16(6)

        var p = 12
        repeat(qd) {
            p = skipName(data, p)
            p += 4
        }
        for (i in 0 until an) {
            if (p >= data.size) break
            p = skipName(data, p)
            if (p + 10 > data.size) break
            val type = u16(p)
            val klass = u16(p + 2)
            val rdlen = u16(p + 8)
            p += 10
            if (p + rdlen > data.size) break
            if (type == 16 && klass == 1) {
                val sb = StringBuilder()
                var q = p
                val end = p + rdlen
                while (q < end) {
                    val len = data[q].toInt() and 0xFF
                    q += 1
                    if (len > 0 && q + len <= end) {
                        sb.append(String(data, q, len, Charsets.UTF_8))
                    }
                    q += len
                }
                val txt = sb.toString().trim()
                if (txt.isNotEmpty() && (txt.contains("v1", ignoreCase = true) || txt.contains("="))) {
                    return txt
                }
            }
            p += rdlen
        }
        return null
    }

    private fun skipName(data: ByteArray, start: Int): Int {
        var p = start
        while (p < data.size) {
            val len = data[p].toInt() and 0xFF
            if (len == 0) return p + 1
            if ((len and 0xC0) == 0xC0) return p + 2
            p += 1 + len
        }
        return data.size
    }

    private const val TIMEOUT_MS = 2500L
}
