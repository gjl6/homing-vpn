package io.homing.client

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log
import androidx.core.content.ContextCompat

/**
 * 开机 / 应用更新后自动拉起前台服务（前提：用户勾选了「开机自动连接」且已保存配置）。
 *
 * 首次连接时用户已经授权过 VPN，权限会一直保留，所以这里不需要界面即可重连。
 * 如果授权被系统清掉，前台服务会记一条「缺少 VPN 授权」并在通知栏提示。
 */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent?) {
        val action = intent?.action ?: return
        if (action != Intent.ACTION_BOOT_COMPLETED && action != Intent.ACTION_MY_PACKAGE_REPLACED) return
        val store = ProfileStore(context)
        val repo = ProfileRepository(context)
        HomingState.log("收到系统广播：$action")
        if (!store.autoStart) {
            HomingState.log("未开启「开机自动连接」，跳过自动重连")
            return
        }
        val active = repo.active()
        if (active == null) {
            HomingState.log("没有已保存的配置，跳过自动重连")
            return
        }
        runCatching {
            ContextCompat.startForegroundService(
                context,
                HomingService.simpleIntent(context, HomingService.ACTION_CONNECT),
            )
            HomingState.log("开机自启：已请求前台服务连接「${active.displayName()}」")
        }.onFailure { Log.e("HOMING", "boot autostart failed", it) }
    }
}
