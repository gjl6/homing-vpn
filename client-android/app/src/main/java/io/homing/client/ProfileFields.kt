package io.homing.client

import org.json.JSONArray
import org.json.JSONObject

/**
 * 配置表单字段 <-> 原始配置文本 的转换。
 *
 * 归途 JSON 配置支持完整字段编辑；纯 隧道 `.conf` 只做基础行替换，
 * 其余高级项提示用户直接编辑原始文本。
 */
data class ProfileFields(
    val name: String = "",
    val endpoint: String = "",
    val mtu: String = "",
    val dns: String = "",
    val allowedIps: String = "",
    val keepalive: String = "",
    val record: String = "",
    val autoFollow: Boolean = true,
    val preferred: String = "auto",
    val interval: String = "",
    val probeEnabled: Boolean = true,
    val probeInterval: String = "",
    val probeIdleInterval: String = "",
    val probeIdleAfter: String = "",
    val probeFailures: String = "",
    val probeTimeout: String = "",
    val probeRecoveryInterval: String = "",
    val probeSoftGrace: String = "",
    val raw: String = "",
    val isJson: Boolean = true,
) {
    companion object {
        fun from(profile: Profile): ProfileFields {
            if (!profile.isJson) {
                val raw = profile.raw
                return ProfileFields(
                    name = profile.displayName(),
                    endpoint = HomingProfile.confEndpoint(raw).orEmpty(),
                    mtu = confValue(raw, "MTU"),
                    dns = confValue(raw, "DNS"),
                    keepalive = confValue(raw, "PersistentKeepalive"),
                    raw = raw,
                    isJson = false,
                )
            }
            val o = runCatching { JSONObject(profile.raw) }.getOrElse { return ProfileFields(raw = profile.raw) }
            val peer = o.optJSONArray("peers")?.optJSONObject(0)
            val discovery = o.optJSONObject("discovery")
            return ProfileFields(
                name = profile.displayName(),
                endpoint = peer?.optString("endpoint", "").orEmpty(),
                mtu = if (o.has("mtu") && !o.isNull("mtu")) o.optInt("mtu").toString() else "",
                dns = joinArray(o.optJSONArray("dns")),
                allowedIps = joinArray(peer?.optJSONArray("allowedIps")),
                keepalive = if (peer != null && peer.has("persistentKeepalive") && !peer.isNull("persistentKeepalive")) {
                    peer.optInt("persistentKeepalive").toString()
                } else "",
                record = discovery?.optString("record", "").orEmpty(),
                autoFollow = discovery?.optBoolean("autoFollow", true) ?: true,
                preferred = discovery?.optString("preferred", "auto")?.ifBlank { "auto" } ?: "auto",
                interval = if (discovery != null && discovery.has("interval")) {
                    discovery.optInt("interval", 30).toString()
                } else "",
                probeEnabled = runCatching { HomingProfile.probe(o).enabled }.getOrDefault(true),
                probeInterval = runCatching { HomingProfile.probe(o).intervalSec.toString() }.getOrDefault(""),
                probeIdleInterval = runCatching { HomingProfile.probe(o).idleIntervalSec.toString() }.getOrDefault(""),
                probeIdleAfter = runCatching { HomingProfile.probe(o).idleAfterSec.toString() }.getOrDefault(""),
                probeFailures = runCatching { HomingProfile.probe(o).failures.toString() }.getOrDefault(""),
                probeTimeout = runCatching { HomingProfile.probe(o).timeoutMs.toString() }.getOrDefault(""),
                probeRecoveryInterval = runCatching { HomingProfile.probe(o).recoveryIntervalSec.toString() }.getOrDefault(""),
                probeSoftGrace = runCatching { HomingProfile.probe(o).softGraceSec.toString() }.getOrDefault(""),
                raw = profile.raw,
                isJson = true,
            )
        }

        /** 把表单字段写回归途 JSON（[baseRaw] 是打开编辑器时的原文）。 */
        fun applyJson(fields: ProfileFields, baseRaw: String): String {
            val o = JSONObject(baseRaw)
            o.put("format", HomingProfile.FORMAT)
            if (fields.name.isNotBlank()) o.put("name", fields.name.trim())

            val peers = o.optJSONArray("peers") ?: JSONArray().also { o.put("peers", it) }
            val peer = if (peers.length() > 0) {
                peers.optJSONObject(0) ?: JSONObject().also { peers.put(it) }
            } else {
                JSONObject().also { peers.put(it) }
            }
            if (fields.endpoint.isNotBlank()) peer.put("endpoint", fields.endpoint.trim())

            fields.mtu.trim().toIntOrNull()?.let { o.put("mtu", it) }
            val dns = splitList(fields.dns)
            if (dns.isNotEmpty()) o.put("dns", JSONArray(dns)) else o.remove("dns")
            val allowed = splitList(fields.allowedIps)
            if (allowed.isNotEmpty()) peer.put("allowedIps", JSONArray(allowed))
            fields.keepalive.trim().toIntOrNull()?.let { peer.put("persistentKeepalive", it) }

            val record = fields.record.trim()
            if (record.isBlank()) {
                o.remove("discovery")
            } else {
                val discovery = o.optJSONObject("discovery") ?: JSONObject().also { o.put("discovery", it) }
                discovery.put("enabled", true)
                discovery.put("record", record)
                discovery.put("autoFollow", fields.autoFollow)
                discovery.put("preferred", fields.preferred.ifBlank { "auto" })
                fields.interval.trim().toIntOrNull()?.let { discovery.put("interval", it) }
            }

            val probe = o.optJSONObject("probe") ?: JSONObject().also { o.put("probe", it) }
            probe.put("enabled", fields.probeEnabled)
            fields.probeInterval.trim().toIntOrNull()?.let { probe.put("interval", it) }
            fields.probeIdleInterval.trim().toIntOrNull()?.let { probe.put("idleInterval", it) }
            fields.probeIdleAfter.trim().toIntOrNull()?.let { probe.put("idleAfter", it) }
            fields.probeFailures.trim().toIntOrNull()?.let { probe.put("failures", it) }
            fields.probeTimeout.trim().toIntOrNull()?.let { probe.put("timeoutMs", it) }
            fields.probeRecoveryInterval.trim().toIntOrNull()?.let { probe.put("recoveryInterval", it) }
            fields.probeSoftGrace.trim().toIntOrNull()?.let { probe.put("softGrace", it) }
            return o.toString()
        }

        /** 纯 .conf：只替换基础行。 */
        fun applyConf(fields: ProfileFields, baseRaw: String): String {
            var out = baseRaw
            fields.mtu.trim().takeIf { it.isNotEmpty() }?.let { v ->
                out = setConfValue(out, "MTU", v)
            }
            fields.dns.trim().takeIf { it.isNotEmpty() }?.let { v ->
                out = setConfValue(out, "DNS", v)
            }
            fields.keepalive.trim().takeIf { it.isNotEmpty() }?.let { v ->
                out = setConfValue(out, "PersistentKeepalive", v)
            }
            fields.allowedIps.trim().takeIf { it.isNotEmpty() }?.let { v ->
                out = setConfValue(out, "AllowedIPs", v)
            }
            fields.endpoint.trim().takeIf { it.isNotEmpty() }?.let { ep ->
                out = HomingProfile.replaceConfEndpoint(out, ep)
            }
            return out
        }

        private fun joinArray(arr: JSONArray?): String {
            if (arr == null || arr.length() == 0) return ""
            return (0 until arr.length()).joinToString(", ") { arr.optString(it, "") }
        }

        private fun splitList(s: String): List<String> =
            s.split(',', '\n', ' ', ';').map { it.trim() }.filter { it.isNotEmpty() }

        private fun confValue(raw: String, key: String): String =
            Regex("(?im)^\\s*${Regex.escape(key)}\\s*=\\s*(.+)$").find(raw)?.groupValues?.get(1)?.trim().orEmpty()

        private fun setConfValue(raw: String, key: String, value: String): String {
            val re = Regex("(?im)^\\s*${Regex.escape(key)}\\s*=.*$")
            if (re.containsMatchIn(raw)) return raw.replace(re, "$key = $value")
            val section = if (key == "AllowedIPs" || key == "PersistentKeepalive") "[Peer]" else "[Interface]"
            val idx = raw.indexOf(section, ignoreCase = true)
            if (idx < 0) return raw
            val insertAt = raw.indexOf('\n', idx).let { if (it < 0) raw.length else it + 1 }
            return raw.substring(0, insertAt) + "$key = $value\n" + raw.substring(insertAt)
        }
    }
}
