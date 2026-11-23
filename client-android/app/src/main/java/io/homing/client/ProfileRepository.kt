package io.homing.client

import android.content.Context
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.util.UUID

/**
 * 一条客户端配置。
 *
 * [raw] 是原始配置文本：归途 JSON（homing-profile/1）或 .conf。
 * 名称、最后使用的端点等元信息单独存，避免每次都要解析/改写原文。
 */
data class Profile(
    val id: String,
    val name: String,
    val raw: String,
    val createdAt: Long,
    val updatedAt: Long,
    val lastEndpoint: String? = null,
) {
    val isJson: Boolean get() = HomingProfile.isJson(raw)

    val endpoint: String?
        get() = if (isJson) {
            runCatching { HomingProfile.peerEndpoint(JSONObject(raw)) }.getOrNull()
        } else {
            HomingProfile.confEndpoint(raw)
        }

    val discoveryRecord: String?
        get() = if (isJson) {
            runCatching { HomingProfile.discovery(JSONObject(raw))?.record }.getOrNull()
        } else null

    /** 列表里显示的一行摘要 */
    fun summary(): String {
        val ep = endpoint ?: lastEndpoint ?: "无端点"
        val record = discoveryRecord
        return if (record != null) "$ep · 公告板 $record" else "$ep"
    }

    fun displayName(): String = name.ifBlank { "未命名配置" }
}

/**
 * 多配置仓库：profiles.json 持久化（配置列表 + 当前选中项）。
 *
 * 兼容旧版（2.1.0 之前）只存了一条配置的情况：首次加载时会把旧配置迁移成第一条记录。
 */
class ProfileRepository(context: Context) {
    private val appContext = context.applicationContext
    private val file = File(appContext.filesDir, FILE_NAME)
    private val prefs = appContext.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)

    private var profiles: MutableList<Profile> = mutableListOf()
    private var lastStamp: Long = 0L

    var activeId: String? = null
        private set

    init {
        load()
        lastStamp = stampOf()
        migrateLegacyIfNeeded()
    }

    fun all(): List<Profile> {
        ensureFresh()
        return profiles.toList()
    }

    fun active(): Profile? {
        ensureFresh()
        return profiles.firstOrNull { it.id == activeId }
    }

    fun find(id: String?): Profile? {
        ensureFresh()
        return profiles.firstOrNull { it.id == id }
    }

    /**
     * 界面的编辑页和服务各自持有 Repository 实例，通过文件时间戳判断是否有别的实例改过配置，
     * 避免「编辑页保存了，但正在连接的服务仍读旧配置」。
     */
    private fun stampOf(): Long = if (file.isFile) file.lastModified() * 1000 + file.length() else 0L

    private fun ensureFresh() {
        val stamp = stampOf()
        if (stamp == lastStamp) return
        lastStamp = stamp
        if (file.isFile) load() else {
            profiles.clear()
            activeId = null
        }
        if (activeId == null || profiles.none { it.id == activeId }) {
            activeId = profiles.firstOrNull()?.id
        }
    }

    /**
     * 导入一条配置。同名同内容 → 更新；同名不同内容 → 自动改名「名字 (2)」。
     * @param preferredName 二维码/界面里指定的名字（默认取配置里的 name）
     */
    fun add(raw: String, preferredName: String? = null): Profile {
        ensureFresh()
        val normalized = HomingProfile.normalize(raw)
        val baseName = preferredName?.trim()?.takeIf { it.isNotBlank() }
            ?: HomingProfile.nameOfSafe(normalized)
        val existingSameContent = profiles.firstOrNull { it.raw == normalized }
        if (existingSameContent != null) {
            return existingSameContent
        }
        var name = baseName
        var i = 2
        while (profiles.any { it.name.equals(name, ignoreCase = true) }) {
            name = "$baseName ($i)"
            i++
        }
        val now = System.currentTimeMillis()
        val profile = Profile(
            id = UUID.randomUUID().toString(),
            name = name,
            raw = normalized,
            createdAt = now,
            updatedAt = now,
            lastEndpoint = if (HomingProfile.isJson(normalized)) {
                runCatching { HomingProfile.peerEndpoint(JSONObject(normalized)) }.getOrNull()
            } else {
                HomingProfile.confEndpoint(normalized)
            },
        )
        profiles.add(0, profile)
        if (activeId == null) activeId = profile.id
        save()
        return profile
    }

    fun update(profile: Profile) {
        ensureFresh()
        val idx = profiles.indexOfFirst { it.id == profile.id }
        if (idx < 0) return
        profiles[idx] = profile.copy(updatedAt = System.currentTimeMillis())
        save()
    }

    fun updateName(id: String, name: String) {
        ensureFresh()
        val idx = profiles.indexOfFirst { it.id == id }
        if (idx < 0) return
        profiles[idx] = profiles[idx].copy(name = name, updatedAt = System.currentTimeMillis())
        save()
    }

    /** 公告板/手动切换端点后，把最新端点写回配置原文。 */
    fun updateRaw(id: String, raw: String, lastEndpoint: String?) {
        ensureFresh()
        val idx = profiles.indexOfFirst { it.id == id }
        if (idx < 0) return
        profiles[idx] = profiles[idx].copy(
            raw = raw,
            lastEndpoint = lastEndpoint ?: profiles[idx].lastEndpoint,
            updatedAt = System.currentTimeMillis(),
        )
        save()
    }

    fun delete(id: String) {
        ensureFresh()
        profiles.removeAll { it.id == id }
        if (activeId == id) activeId = profiles.firstOrNull()?.id
        save()
    }

    fun duplicate(id: String): Profile? {
        ensureFresh()
        val src = find(id) ?: return null
        return add(src.raw, "${src.displayName()} 副本")
    }

    fun setActive(id: String) {
        ensureFresh()
        if (profiles.any { it.id == id }) {
            activeId = id
            save()
        }
    }

    // ---------------- 持久化 ----------------

    private fun load() {
        profiles = mutableListOf()
        if (!file.isFile) return
        try {
            val o = JSONObject(file.readText())
            activeId = o.optString("activeId", "").takeIf { it.isNotBlank() }
            val arr = o.optJSONArray("profiles") ?: JSONArray()
            for (i in 0 until arr.length()) {
                val p = arr.optJSONObject(i) ?: continue
                val raw = p.optString("raw", "")
                if (raw.isBlank()) continue
                profiles.add(
                    Profile(
                        id = p.optString("id", UUID.randomUUID().toString()),
                        name = p.optString("name", ""),
                        raw = raw,
                        createdAt = p.optLong("createdAt", 0L),
                        updatedAt = p.optLong("updatedAt", 0L),
                        lastEndpoint = p.optString("lastEndpoint", "").takeIf { it.isNotBlank() },
                    ),
                )
            }
            if (activeId == null || profiles.none { it.id == activeId }) {
                activeId = profiles.firstOrNull()?.id
            }
        } catch (t: Throwable) {
            HomingState.log("读取配置列表失败：${t.message}")
        }
    }

    private fun save() {
        try {
            val o = JSONObject()
            o.put("version", 1)
            o.put("activeId", activeId ?: "")
            val arr = JSONArray()
            for (p in profiles) {
                arr.put(
                    JSONObject().apply {
                        put("id", p.id)
                        put("name", p.name)
                        put("raw", p.raw)
                        put("createdAt", p.createdAt)
                        put("updatedAt", p.updatedAt)
                        if (!p.lastEndpoint.isNullOrBlank()) put("lastEndpoint", p.lastEndpoint)
                    },
                )
            }
            o.put("profiles", arr)
            val tmp = File(file.parentFile, "$FILE_NAME.tmp")
            tmp.writeText(o.toString())
            if (file.exists()) file.delete()
            tmp.renameTo(file)
            lastStamp = stampOf()
        } catch (t: Throwable) {
            HomingState.log("保存配置列表失败：${t.message}")
        }
    }

    /** 2.1.0 及之前只有一条 profile 存在 SharedPreferences 里，迁移成第一条记录。 */
    private fun migrateLegacyIfNeeded() {
        if (profiles.isNotEmpty()) return
        val legacy = prefs.getString(KEY_LEGACY_PROFILE, null)
        if (legacy.isNullOrBlank()) return
        runCatching {
            val p = add(legacy)
            setActive(p.id)
            HomingState.log("已迁移旧版配置「${p.displayName()}」到配置列表")
        }.onFailure {
            HomingState.log("旧版配置迁移失败：${it.message}")
        }
        prefs.edit().remove(KEY_LEGACY_PROFILE).apply()
    }

    companion object {
        private const val FILE_NAME = "profiles.json"
        private const val PREFS_NAME = "homing_client"
        private const val KEY_LEGACY_PROFILE = "profile_text"
    }
}

/** 取配置里的名字（解析失败时返回空串）。 */
fun HomingProfile.nameOfSafe(raw: String): String = try {
    if (isJson(raw)) nameOf(JSONObject(raw)) else "配置文件"
} catch (t: Throwable) {
    "配置文件"
}
