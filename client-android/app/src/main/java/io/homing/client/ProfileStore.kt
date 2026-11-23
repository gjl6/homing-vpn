package io.homing.client

import android.content.Context
import android.content.SharedPreferences

/**
 * 全局设置（不属于某一条配置）：开机自启、导入后自动连接、默认公告板跟随、当前连接意图。
 */
class ProfileStore(context: Context) {
    private val sp: SharedPreferences =
        context.applicationContext.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    /** 开机后自动拉起隧道（用当前选中的配置） */
    var autoStart: Boolean
        get() = sp.getBoolean(KEY_AUTOSTART, false)
        set(value) = sp.edit().putBoolean(KEY_AUTOSTART, value).apply()

    /** 扫码/粘贴导入后是否立即连接 */
    var autoConnectOnImport: Boolean
        get() = sp.getBoolean(KEY_AUTOCONNECT, true)
        set(value) = sp.edit().putBoolean(KEY_AUTOCONNECT, value).apply()

    /** 公告板自动跟随的全局默认值（单条配置里没写 autoFollow 时用它） */
    var autoFollowDefault: Boolean
        get() = sp.getBoolean(KEY_AUTOFOLLOW, true)
        set(value) = sp.edit().putBoolean(KEY_AUTOFOLLOW, value).apply()

    /** 上一次用户是否希望保持连接（服务重启/开机自启恢复用） */
    var wantConnected: Boolean
        get() = sp.getBoolean(KEY_WANT, false)
        set(value) = sp.edit().putBoolean(KEY_WANT, value).apply()

    companion object {
        private const val PREFS = "homing_client"
        private const val KEY_AUTOSTART = "autostart"
        private const val KEY_AUTOCONNECT = "autoconnect_import"
        private const val KEY_AUTOFOLLOW = "autofollow_default"
        private const val KEY_WANT = "want_connected"
    }
}
