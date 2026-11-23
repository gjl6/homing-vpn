package io.homing.client

import org.json.JSONObject
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL

/**
 * 隧道内代理自助接口（由服务端管理台提供）。
 *
 * 客户端通过隧道访问 `10.8.0.1:<adminPort>/api/self/proxy`，服务端按
 * WireGuard 源地址识别客户端；服务端没开权限时返回 403 + 中文提示。
 */
object SelfProxy {

    data class Status(
        val allowed: Boolean = false,
        val enabled: Boolean = false,
        val globalEnabled: Boolean = false,
        val selfService: Boolean = false,
        val name: String = "",
        val message: String = "",
    )

    fun fetch(apiBase: String): Status {
        val (code, text) = request("GET", apiBase, "/api/self/proxy", null)
        if (code !in 200..299) throw IllegalStateException(errorMessage(text, code))
        return parse(JSONObject(text))
    }

    fun setEnabled(apiBase: String, enabled: Boolean): Status {
        val body = JSONObject().put("enabled", enabled).toString()
        val (code, text) = request("POST", apiBase, "/api/self/proxy", body)
        if (code !in 200..299) throw IllegalStateException(errorMessage(text, code))
        return parse(JSONObject(text))
    }

    private fun parse(o: JSONObject): Status = Status(
        allowed = o.optBoolean("allowed", false),
        enabled = o.optBoolean("enabled", false),
        globalEnabled = o.optBoolean("globalEnabled", false),
        selfService = o.optBoolean("selfService", false),
        name = o.optString("name", ""),
        message = o.optString("message", ""),
    )

    private fun errorMessage(text: String, code: Int): String {
        val msg = runCatching { JSONObject(text).optString("error", "") }.getOrDefault("")
        return msg.ifBlank { "服务端返回 $code" }
    }

    private fun request(method: String, base: String, path: String, body: String?): Pair<Int, String> {
        val url = URL(base.trimEnd('/') + path)
        val conn = url.openConnection() as HttpURLConnection
        try {
            conn.requestMethod = method
            conn.connectTimeout = 2500
            conn.readTimeout = 3500
            conn.useCaches = false
            if (body != null) {
                conn.doOutput = true
                conn.setRequestProperty("Content-Type", "application/json; charset=utf-8")
                conn.outputStream.use { it.write(body.toByteArray(Charsets.UTF_8)) }
            }
            val code = conn.responseCode
            val stream = if (code in 200..299) conn.inputStream else conn.errorStream
            val text = stream?.bufferedReader(Charsets.UTF_8)?.use { it.readText() }.orEmpty()
            return code to text
        } finally {
            conn.disconnect()
        }
    }
}
