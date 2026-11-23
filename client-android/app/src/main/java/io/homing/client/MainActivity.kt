package io.homing.client

import android.Manifest
import android.app.Activity
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.provider.Settings
import android.util.Base64
import android.view.View
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import androidx.recyclerview.widget.LinearLayoutManager
import androidx.recyclerview.widget.RecyclerView
import com.google.android.material.appbar.MaterialToolbar
import com.google.android.material.button.MaterialButton
import com.google.android.material.dialog.MaterialAlertDialogBuilder
import com.google.android.material.materialswitch.MaterialSwitch
import com.google.android.material.textfield.TextInputEditText
import com.google.android.material.textfield.TextInputLayout
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions
import com.wireguard.android.backend.GoBackend
import org.json.JSONObject
import java.util.concurrent.Executors

/**
 * 归途客户端主界面（2.2.0 全新布局）：
 *
 *  - 顶部状态卡：连接状态、端点、握手/流量、公告板，以及连接/断开主按钮；
 *  - 配置列表：可保存多条配置，点卡片选中、卡片内「连接/编辑/更多」；
 *  - 扫码导入 / 粘贴导入；右上角「设置」。
 */
class MainActivity : AppCompatActivity() {

    private lateinit var repo: ProfileRepository
    private lateinit var store: ProfileStore
    private lateinit var adapter: ProfileAdapter

    private lateinit var toolbar: MaterialToolbar
    private lateinit var listProfiles: RecyclerView
    private lateinit var emptyState: LinearLayout
    private lateinit var textProfileHeader: TextView
    private lateinit var textStatusDot: TextView
    private lateinit var textStatusTitle: TextView
    private lateinit var textActiveProfile: TextView
    private lateinit var textEndpoint: TextView
    private lateinit var textHandshake: TextView
    private lateinit var textTraffic: TextView
    private lateinit var textDiscovery: TextView
    private lateinit var btnTunnel: MaterialButton
    private lateinit var rowProxy: LinearLayout
    private lateinit var textProxyState: TextView
    private lateinit var switchProxy: MaterialSwitch

    private val handler = Handler(Looper.getMainLooper())
    private val worker = Executors.newSingleThreadExecutor()

    private var proxyStatus: SelfProxy.Status? = null
    private var proxyLoading = false
    private var proxyUpdating = false
    private var proxyLastFetch = 0L

    private var manualDiscoveryText: String? = null
    private var lastConnectedId: String? = null
    private var lastRepoVersion: Long = -1L
    private var ticking = false
    private var vpnPendingProfile: Profile? = null
    private var notifPermissionAsked = false

    private val vpnPermission = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { result ->
        val profile = vpnPendingProfile
        vpnPendingProfile = null
        if (result.resultCode == Activity.RESULT_OK) {
            HomingState.log("VPN 授权通过")
            profile?.let { startTunnel(it) }
        } else {
            HomingState.log("VPN 授权被拒绝，无法建立隧道")
            toast("VPN 授权被拒绝")
        }
    }

    private val cameraPermission = registerForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted ->
        if (granted) {
            launchScanner()
        } else {
            toast("没有相机权限，正在打开相册…")
            HomingState.log("相机权限被拒绝，改为相册导入")
            startGalleryImport()
        }
    }

    /** 相册选图导入：系统文件选择器返回图片后，用 ZXing core 在本地识别二维码。 */
    private val galleryLauncher = registerForActivityResult(
        ActivityResultContracts.GetContent(),
    ) { uri ->
        if (uri == null) {
            HomingState.log("相册导入已取消")
        } else {
            importQrFromImage(uri)
        }
    }

    private val notifPermission = registerForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted ->
        HomingState.log(if (granted) "通知权限已允许" else "通知权限被拒绝：前台服务通知可能不显示")
    }

    private val scanLauncher = registerForActivityResult(ScanContract()) { result ->
        val contents = result?.contents
        if (contents.isNullOrBlank()) {
            HomingState.log("扫码已取消")
        } else {
            importPayload(contents, autoConnect = true)
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        repo = ProfileRepository(this)
        store = ProfileStore(this)

        toolbar = findViewById(R.id.toolbar)
        toolbar.setOnMenuItemClickListener { item ->
            if (item.itemId == R.id.action_settings) {
                showSettingsDialog()
                true
            } else {
                false
            }
        }

        listProfiles = findViewById(R.id.listProfiles)
        emptyState = findViewById(R.id.emptyState)
        textProfileHeader = findViewById(R.id.textProfileHeader)
        textStatusDot = findViewById(R.id.textStatusDot)
        textStatusTitle = findViewById(R.id.textStatusTitle)
        textActiveProfile = findViewById(R.id.textActiveProfile)
        textEndpoint = findViewById(R.id.textEndpoint)
        textHandshake = findViewById(R.id.textHandshake)
        textTraffic = findViewById(R.id.textTraffic)
        textDiscovery = findViewById(R.id.textDiscovery)
        btnTunnel = findViewById(R.id.btnTunnel)
        rowProxy = findViewById(R.id.rowProxy)
        textProxyState = findViewById(R.id.textProxyState)
        switchProxy = findViewById(R.id.switchProxy)
        switchProxy.setOnCheckedChangeListener { _, checked -> onProxySwitch(checked) }

        adapter = ProfileAdapter(
            onClickProfile = { profile -> selectProfile(profile, toastHint = true) },
            onConnect = { profile -> onItemConnect(profile) },
            onEdit = { profile -> openEditor(profile.id) },
            onMore = { profile, anchor -> showProfileMenu(profile, anchor) },
        )
        listProfiles.layoutManager = LinearLayoutManager(this)
        listProfiles.adapter = adapter
        listProfiles.isNestedScrollingEnabled = false

        findViewById<MaterialButton>(R.id.btnScan).setOnClickListener { showImportChooser() }
        findViewById<MaterialButton>(R.id.btnPaste).setOnClickListener { showPasteDialog() }
        btnTunnel.setOnClickListener { onTunnelButton() }
        textDiscovery.setOnClickListener { queryDiscoveryForActive() }

        requestNotificationPermissionOnce()
        handleIntent(intent)
        refreshProfiles()
    }

    override fun onNewIntent(intent: Intent?) {
        super.onNewIntent(intent)
        setIntent(intent)
        handleIntent(intent)
        refreshProfiles()
    }

    override fun onResume() {
        super.onResume()
        refreshProfiles()
        if (!ticking) {
            ticking = true
            handler.post(uiTicker)
        }
    }

    override fun onPause() {
        super.onPause()
        ticking = false
        handler.removeCallbacks(uiTicker)
    }

    override fun onDestroy() {
        ticking = false
        handler.removeCallbacks(uiTicker)
        worker.shutdownNow()
        super.onDestroy()
    }

    // ---------------- 配置列表 ----------------

    private fun activeProfileLabel(): String {
        val connected = HomingState.connectedProfileId?.let { repo.find(it) }
        if (connected != null) return "使用中：${connected.displayName()}"
        return repo.active()?.let { "当前：${it.displayName()}" }.orEmpty()
    }

    private fun refreshProfiles() {
        val profiles = repo.all()
        val connectedId = HomingState.connectedProfileId
        adapter.submit(profiles, repo.activeId, connectedId)
        lastConnectedId = connectedId
        lastRepoVersion = profiles.sumOf { it.updatedAt }
        textProfileHeader.text = "配置（${profiles.size}）"
        emptyState.visibility = if (profiles.isEmpty()) View.VISIBLE else View.GONE
        listProfiles.visibility = if (profiles.isEmpty()) View.GONE else View.VISIBLE
        textActiveProfile.text = activeProfileLabel()
    }

    private fun selectProfile(profile: Profile, toastHint: Boolean) {
        repo.setActive(profile.id)
        HomingState.log("已选中配置「${profile.displayName()}」")
        if (toastHint) toast("已选中「${profile.displayName()}」")
        refreshProfiles()
        updateStatusCard()
    }

    private fun onItemConnect(profile: Profile) {
        if (profile.id == HomingState.connectedProfileId &&
            HomingState.tunnelState == "UP"
        ) {
            disconnect()
        } else {
            repo.setActive(profile.id)
            refreshProfiles()
            connectProfile(profile)
        }
    }

    private fun openEditor(profileId: String) {
        startActivity(Intent(this, ProfileEditActivity::class.java).putExtra(ProfileEditActivity.EXTRA_PROFILE_ID, profileId))
    }

    private fun showProfileMenu(profile: Profile, anchor: View) {
        val items = mutableListOf<String>()
        val actions = mutableListOf<() -> Unit>()
        if (profile.id != repo.activeId) {
            items.add("设为当前配置")
            actions.add {
                repo.setActive(profile.id)
                refreshProfiles()
                toast("已设为当前：${profile.displayName()}")
            }
        }
        items.add("编辑配置")
        actions.add { openEditor(profile.id) }
        if (profile.discoveryRecord != null) {
            items.add("立即跟随公告板端点")
            actions.add {
                repo.setActive(profile.id)
                HomingService.startAction(this, HomingService.ACTION_REFRESH)
                toast("正在查询公告板…")
            }
        }
        items.add("重命名")
        actions.add { showRenameDialog(profile) }
        items.add("复制一份")
        actions.add {
            repo.duplicate(profile.id)?.let {
                refreshProfiles()
                toast("已复制为「${it.displayName()}」")
            }
        }
        items.add("删除")
        actions.add { confirmDelete(profile) }
        MaterialAlertDialogBuilder(this)
            .setTitle(profile.displayName())
            .setItems(items.toTypedArray()) { _, which -> actions.getOrNull(which)?.invoke() }
            .show()
    }

    private fun showRenameDialog(profile: Profile) {
        val input = TextInputEditText(this).apply { setText(profile.displayName()) }
        val layout = TextInputLayout(this).apply {
            hint = "配置名称"
            addView(input)
        }
        MaterialAlertDialogBuilder(this)
            .setTitle("重命名")
            .setView(layout)
            .setNegativeButton("取消", null)
            .setPositiveButton("保存") { _, _ ->
                val name = input.text?.toString()?.trim().orEmpty()
                if (name.isNotBlank()) {
                    repo.updateName(profile.id, name)
                    refreshProfiles()
                }
            }
            .show()
    }

    private fun confirmDelete(profile: Profile) {
        MaterialAlertDialogBuilder(this)
            .setTitle("删除配置")
            .setMessage("确定删除「${profile.displayName()}」？")
            .setNegativeButton("取消", null)
            .setPositiveButton("删除") { _, _ ->
                if (profile.id == HomingState.connectedProfileId) disconnect()
                repo.delete(profile.id)
                refreshProfiles()
                toast("已删除")
            }
            .show()
    }

    // ---------------- 连接 / 断开 ----------------

    private fun onTunnelButton() {
        if (HomingState.tunnelState == "UP" || HomingState.tunnelState == "TOGGLE") {
            disconnect()
        } else {
            val profile = repo.active() ?: repo.all().firstOrNull()
            if (profile == null) {
                toast("还没有配置，先扫码或粘贴导入")
                return
            }
            connectProfile(profile)
        }
    }

    private fun connectProfile(profile: Profile) {
        repo.setActive(profile.id)
        vpnPendingProfile = profile
        val prepare = GoBackend.VpnService.prepare(this)
        if (prepare != null) {
            HomingState.log("请求 VPN 授权")
            vpnPermission.launch(prepare)
        } else {
            vpnPendingProfile = null
            startTunnel(profile)
        }
    }

    private fun startTunnel(profile: Profile) {
        store.wantConnected = true
        runCatching {
            val intent = Intent(this, HomingService::class.java)
                .setAction(HomingService.ACTION_CONNECT)
                .putExtra(HomingService.EXTRA_PROFILE_ID, profile.id)
            ContextCompat.startForegroundService(this, intent)
        }.onFailure {
            HomingState.log("启动前台服务失败：${it.message}")
            toast("启动失败：${it.message}")
        }
    }

    private fun disconnect() {
        store.wantConnected = false
        HomingService.startAction(this, HomingService.ACTION_DISCONNECT)
    }

    // ---------------- 导入 ----------------

    /**
     * 「扫码导入」统一入口（2.3.5）：先让用户选用相机扫，还是从相册选图片。
     * 扫码和相册是同一个导入动作的两种取图方式，不再单独占一个按钮。
     */
    private fun showImportChooser() {
        HomingState.log("打开导入方式选择：相机扫描 / 相册图片")
        MaterialAlertDialogBuilder(this)
            .setTitle("导入配置")
            .setItems(arrayOf("用相机扫描二维码", "从相册选择图片")) { _, which ->
                when (which) {
                    0 -> startScanFlow()
                    1 -> startGalleryImport()
                }
            }
            .setNegativeButton("取消", null)
            .show()
    }

    private fun startScanFlow() {
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.CAMERA) ==
            PackageManager.PERMISSION_GRANTED
        ) {
            launchScanner()
        } else {
            cameraPermission.launch(Manifest.permission.CAMERA)
        }
    }

    private fun launchScanner() {
        val options = ScanOptions().apply {
            setDesiredBarcodeFormats(ScanOptions.QR_CODE)
            setPrompt("请扫描管理页的「归途配置」二维码")
            setBeepEnabled(false)
            setOrientationLocked(true)
            setBarcodeImageEnabled(false)
        }
        scanLauncher.launch(options)
    }

    private fun startGalleryImport() {
        HomingState.log("打开相册选择二维码图片…")
        galleryLauncher.launch("image/*")
    }

    private fun importQrFromImage(uri: Uri) {
        HomingState.log("相册图片识别开始：$uri（scheme=${uri.scheme}）")
        toast("正在识别图片中的二维码…")
        worker.execute {
            val result = QrImageDecoder.decode(this, uri)
            runOnUiThread {
                when (result) {
                    is QrImageDecoder.Result.Success -> {
                        HomingState.log("相册图片识别成功（${result.text.length} 字符），开始导入配置")
                        importPayload(result.text, autoConnect = true)
                    }
                    is QrImageDecoder.Result.Failure -> {
                        HomingState.log("相册图片识别失败：${result.reason}")
                        toast("识别失败：${result.reason}")
                    }
                }
            }
        }
    }

    private fun showPasteDialog() {
        val view = layoutInflater.inflate(R.layout.dialog_import_paste, null)
        val input = view.findViewById<TextInputEditText>(R.id.etImportRaw)
        MaterialAlertDialogBuilder(this)
            .setTitle("粘贴导入")
            .setView(view)
            .setNegativeButton("取消", null)
            .setPositiveButton("导入") { _, _ ->
                val raw = input.text?.toString().orEmpty()
                if (raw.isBlank()) {
                    toast("内容为空")
                } else {
                    importPayload(raw, autoConnect = true)
                }
            }
            .show()
    }

    /** 扫码 / 粘贴 / intent 共用的导入入口。 */
    private fun importPayload(raw: String, autoConnect: Boolean) {
        val text = unwrapPayload(raw)
        val normalized = try {
            HomingProfile.normalize(text)
        } catch (e: Exception) {
            HomingState.log("配置导入失败：${e.message}")
            toast("导入失败：${e.message}")
            return
        }
        val profile = repo.add(normalized)
        repo.setActive(profile.id)
        manualDiscoveryText = null
        HomingState.log("已导入配置「${profile.displayName()}」：${profile.summary()}")
        refreshProfiles()
        toast("已导入「${profile.displayName()}」")
        if (autoConnect && store.autoConnectOnImport) {
            connectProfile(profile)
        }
    }

    private fun unwrapPayload(raw: String): String {
        var text = raw.trim()
        if (text.startsWith("homing://", ignoreCase = true)) {
            val query = text.substringAfter('?', "")
            val param = query.split('&').firstOrNull {
                it.startsWith("d=") || it.startsWith("data=") || it.startsWith("profile=")
            }
            if (param != null) {
                val value = param.substringAfter('=').replace('-', '+').replace('_', '/')
                runCatching { Base64.decode(value, Base64.DEFAULT) }.getOrNull()?.let {
                    return String(it, Charsets.UTF_8)
                }
            }
        }
        if (text.startsWith("%7B", ignoreCase = true) || text.contains("%22")) {
            text = runCatching { java.net.URLDecoder.decode(text, "UTF-8") }.getOrDefault(text)
        }
        return text
    }

    // ---------------- 公告板 ----------------

    private fun queryDiscoveryForActive() {
        val profile = repo.active() ?: return
        if (!profile.isJson || profile.discoveryRecord == null) {
            toast("当前配置没有公告板信息")
            return
        }
        val discovery = runCatching {
            HomingProfile.discovery(org.json.JSONObject(profile.raw))
        }.getOrNull() ?: return
        manualDiscoveryText = "公告板：正在查询 ${discovery.record} …"
        updateStatusCard()
        worker.execute {
            val result = TxtResolver.queryFresh(this, discovery, maxServers = 3)
            runOnUiThread {
                if (!result.ok) {
                    manualDiscoveryText = "公告板查询失败：${result.error}"
                    HomingState.log("公告板查询失败：${result.error}")
                    updateStatusCard()
                    return@runOnUiThread
                }
                val current = profile.endpoint
                val chosen = TxtResolver.choose(discovery, result, TxtResolver.deviceHasGlobalV6(this))
                manualDiscoveryText = buildString {
                    append("公告板 ").append(discovery.record)
                    append("：v4=").append(result.v4.ifBlank { "-" })
                    append(" v6=").append(result.v6.ifBlank { "-" })
                    append("（").append(result.server).append("）")
                    if (chosen.isNotBlank() && chosen != current) {
                        append("\n当前端点 ").append(current ?: "-")
                        append(" ≠ 公告板 ").append(chosen).append("，可在配置「更多」里立即跟随")
                    }
                }
                HomingState.log("公告板查询成功：${result.raw}（来自 ${result.server}）")
                updateStatusCard()
            }
        }
    }

    // ---------------- 设置 ----------------

    private fun showSettingsDialog() {
        val view = layoutInflater.inflate(R.layout.dialog_settings, null)
        val swAutoStart = view.findViewById<com.google.android.material.switchmaterial.SwitchMaterial>(R.id.swAutoStart)
        val swAutoConnect = view.findViewById<com.google.android.material.switchmaterial.SwitchMaterial>(R.id.swAutoConnectImport)
        val swAutoFollow = view.findViewById<com.google.android.material.switchmaterial.SwitchMaterial>(R.id.swAutoFollowDefault)
        val textVersion = view.findViewById<TextView>(R.id.textVersion)

        swAutoStart.isChecked = store.autoStart
        swAutoConnect.isChecked = store.autoConnectOnImport
        swAutoFollow.isChecked = store.autoFollowDefault
        textVersion.text = "归途客户端 ${BuildConfig.VERSION_NAME} · 归途隧道内核 · 配置列表/扫码/公告板按需跟随"

        swAutoStart.setOnCheckedChangeListener { _, checked -> store.autoStart = checked }
        swAutoConnect.setOnCheckedChangeListener { _, checked -> store.autoConnectOnImport = checked }
        swAutoFollow.setOnCheckedChangeListener { _, checked -> store.autoFollowDefault = checked }

        view.findViewById<MaterialButton>(R.id.btnBattery).setOnClickListener {
            openBatterySettings()
        }
        view.findViewById<MaterialButton>(R.id.btnNotification).setOnClickListener {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                notifPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
            } else {
                toast("当前系统不需要单独申请通知权限")
            }
        }
        view.findViewById<MaterialButton>(R.id.btnLogs).setOnClickListener { showLogDialog() }

        MaterialAlertDialogBuilder(this)
            .setTitle("设置")
            .setView(view)
            .setPositiveButton("完成", null)
            .show()
    }

    private fun showLogDialog() {
        val logView = TextView(this).apply {
            textSize = 11f
            typeface = android.graphics.Typeface.MONOSPACE
            setTextIsSelectable(true)
            text = HomingState.logLines().joinToString("\n")
        }
        val scroll = android.widget.ScrollView(this).apply { addView(logView) }
        MaterialAlertDialogBuilder(this)
            .setTitle("事件日志")
            .setView(scroll)
            .setNeutralButton("清空") { _, _ ->
                HomingState.clearLog()
                toast("日志已清空")
            }
            .setPositiveButton("关闭", null)
            .show()
    }

    private fun openBatterySettings() {
        val pm = getSystemService(PowerManager::class.java)
        if (pm != null && pm.isIgnoringBatteryOptimizations(packageName)) {
            toast("已在「忽略电池优化」白名单中")
            return
        }
        val intent = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS)
            .setData(Uri.parse("package:$packageName"))
        runCatching { startActivity(intent) }.onFailure {
            HomingState.log("打开电池优化设置失败：${it.message}")
        }
    }

    private fun requestNotificationPermissionOnce() {
        if (notifPermissionAsked) return
        notifPermissionAsked = true
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) !=
            PackageManager.PERMISSION_GRANTED
        ) {
            notifPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    // ---------------- 状态卡 ----------------

    private val uiTicker = object : Runnable {
        override fun run() {
            updateStatusCard()
            handler.postDelayed(this, 1200)
        }
    }

    private fun updateStatusCard() {
        val state = HomingState.tunnelState
        val up = state == "UP" || state == "TOGGLE"
        val recovering = HomingState.recovering
        val active = repo.active()
        when {
            recovering -> {
                textStatusTitle.text = "恢复中…"
                textStatusDot.setTextColor(getColor(R.color.status_warn))
            }
            state == "UP" -> {
                textStatusTitle.text = "已连接"
                textStatusDot.setTextColor(getColor(R.color.status_ok))
            }
            state == "TOGGLE" -> {
                textStatusTitle.text = "正在切换…"
                textStatusDot.setTextColor(getColor(R.color.status_warn))
            }
            else -> {
                textStatusTitle.text = "未连接"
                textStatusDot.setTextColor(getColor(R.color.status_off))
            }
        }
        textActiveProfile.text = activeProfileLabel()
        textEndpoint.text = "端点：" + HomingState.endpoint.ifBlank { active?.endpoint ?: "—" }
        val ago = HomingState.handshakeAgoSec()
        textHandshake.text = if (up) {
            if (ago >= 0) "${ago}s 前" else "等待中"
        } else {
            "—"
        }
        textTraffic.text = if (up) {
            "${formatBytes(HomingState.rxBytes)} / ${formatBytes(HomingState.txBytes)}"
        } else {
            "—"
        }
        textDiscovery.text = manualDiscoveryText ?: serviceDiscoveryText(recovering)
        btnTunnel.text = if (up) "断开" else "连接"
        updateProxyRow(up, active)

        // 连接中的配置变化时刷新列表按钮状态
        if (HomingState.connectedProfileId != lastConnectedId) {
            refreshProfiles()
        }
        // 公告板热切换端点 / 编辑配置后，配置列表的摘要也要跟着更新
        val version = repo.all().sumOf { it.updatedAt }
        if (version != lastRepoVersion) {
            refreshProfiles()
        }
    }

    // ---------------- 代理出口（服务端授权 + 客户端自助） ----------------

    private fun updateProxyRow(up: Boolean, active: Profile?) {
        if (!up) {
            rowProxy.visibility = View.GONE
            proxyStatus = null
            proxyLoading = false
            proxyLastFetch = 0L
            return
        }
        rowProxy.visibility = View.VISIBLE
        val now = System.currentTimeMillis()
        if (proxyStatus == null && proxyLastFetch == 0L) {
            textProxyState.text = "读取代理权限…"
            switchProxy.isEnabled = false
        }
        if (proxyLoading || now - proxyLastFetch < 8000) return
        proxyLastFetch = now
        proxyLoading = true
        val api = selfApiOf(active)
        if (api == null) {
            proxyLoading = false
            textProxyState.text = "未找到服务端自助接口"
            return
        }
        worker.execute {
            val result = runCatching { SelfProxy.fetch(api) }
            runOnUiThread {
                proxyLoading = false
                if (isFinishing || isDestroyed) return@runOnUiThread
                result.onSuccess { applyProxyStatus(it) }.onFailure { onProxyFetchFailed(it) }
            }
        }
    }

    private fun selfApiOf(profile: Profile?): String? {
        val fallback = "http://10.8.0.1:51822"
        val raw = profile?.raw ?: return fallback
        if (!HomingProfile.isJson(raw)) return fallback
        return runCatching {
            JSONObject(raw).optString("selfApi", "").trim().ifBlank { null }
        }.getOrNull() ?: fallback
    }

    private fun applyProxyStatus(st: SelfProxy.Status) {
        proxyStatus = st
        proxyUpdating = true
        switchProxy.isChecked = st.enabled
        switchProxy.isEnabled = st.allowed
        proxyUpdating = false
        textProxyState.text = when {
            !st.globalEnabled -> st.message.ifBlank { "管理员未开启全局出口，请联系管理员" }
            !st.selfService -> st.message.ifBlank { "管理员未开启代理自助权限，请联系管理员" }
            !st.allowed -> st.message.ifBlank { "当前不可用，请联系管理员" }
            st.enabled -> "已开启 · 可自行关闭"
            else -> "已关闭 · 可自行开启"
        }
    }

    private fun onProxyFetchFailed(e: Throwable) {
        switchProxy.isEnabled = false
        textProxyState.text = "代理控制暂不可用：${e.message ?: "隧道未就绪"}"
    }

    private fun onProxySwitch(checked: Boolean) {
        if (proxyUpdating) return
        val st = proxyStatus
        if (st == null || !st.allowed) {
            proxyUpdating = true
            switchProxy.isChecked = st?.enabled ?: false
            proxyUpdating = false
            toast(st?.message?.ifBlank { "正在读取代理权限，请稍后再试" } ?: "正在读取代理权限，请稍后再试")
            return
        }
        val active = HomingState.connectedProfileId?.let { repo.find(it) } ?: repo.active()
        val api = selfApiOf(active) ?: return
        proxyUpdating = true
        switchProxy.isEnabled = false
        textProxyState.text = "正在保存…"
        worker.execute {
            val result = runCatching { SelfProxy.setEnabled(api, checked) }
            runOnUiThread {
                proxyUpdating = false
                if (isFinishing || isDestroyed) return@runOnUiThread
                result.onSuccess {
                    applyProxyStatus(it)
                    toast(if (checked) "代理出口已开启（约 10 秒生效）" else "代理出口已关闭（约 10 秒生效）")
                }.onFailure { e ->
                    switchProxy.isChecked = !checked
                    switchProxy.isEnabled = true
                    textProxyState.text = e.message ?: "切换失败"
                    toast("切换失败：${e.message ?: "未知错误"}")
                }
            }
        }
    }

    private fun serviceDiscoveryText(recovering: Boolean): String {        val summary = HomingState.discoverySummary
        val error = HomingState.discoveryError
        val note = HomingState.recoveryNote
        return when {
            recovering && note.isNotBlank() -> "公告板：$note（隧道不通时才查询）"
            recovering -> "公告板：正在按需检查…"
            error.isNotBlank() -> "公告板：查询失败（${summarizeDiscoveryError(error)}），点此重试"
            summary.isNotBlank() -> "公告板：$summary"
            else -> "公告板：—（仅在隧道不通时自动检查，点此手动查询）"
        }
    }

    /** 状态卡片里只显示简短原因，完整报错去日志里看。 */
    private fun summarizeDiscoveryError(error: String): String {
        val reason = error.substringAfter("：", error)
            .substringBefore("；")
            .substringBefore(";")
            .trim()
        return if (reason.length > 28) reason.take(28) + "…" else reason
    }

    private fun formatBytes(v: Long): String = when {
        v >= 1024L * 1024L -> String.format(java.util.Locale.US, "%.1fMB", v / (1024.0 * 1024.0))
        v >= 1024L -> String.format(java.util.Locale.US, "%.1fKB", v / 1024.0)
        else -> "${v}B"
    }

    // ---------------- intent 注入（自动化测试 / 第三方调起） ----------------

    private fun handleIntent(intent: Intent?) {
        if (intent == null) return
        // 自动化测试钩子（仅 debug 包）：直接把 PNG 文件 Uri 喂给相册识别链路，
        // 绕过系统文件选择器（模拟器 DocumentsUI 无法用 adb 注入点击）。
        if (BuildConfig.DEBUG) {
            val imageUri = intent.getStringExtra("image_uri")
            if (!imageUri.isNullOrBlank()) {
                HomingState.log("通过 intent 注入相册图片：$imageUri")
                importQrFromImage(Uri.parse(imageUri))
                return
            }
        }
        val qr64 = intent.getStringExtra("qr64")
        if (!qr64.isNullOrBlank()) {
            try {
                val text = String(Base64.decode(qr64, Base64.DEFAULT), Charsets.UTF_8)
                HomingState.log("通过 intent 注入二维码内容（${text.length} 字符）")
                importPayload(text, autoConnect = intent.getBooleanExtra("autoconnect", true))
            } catch (t: Throwable) {
                HomingState.log("qr64 解码失败：$t")
            }
            return
        }
        intent.getStringExtra("qr")?.takeIf { it.isNotBlank() }?.let {
            importPayload(it, autoConnect = intent.getBooleanExtra("autoconnect", true))
            return
        }
        val cfg = intent.getStringExtra("cfg")
        if (!cfg.isNullOrBlank()) {
            try {
                val text = String(Base64.decode(cfg, Base64.DEFAULT), Charsets.UTF_8)
                if (text.isNotBlank()) {
                    importPayload(text, autoConnect = intent.getBooleanExtra("autoconnect", false))
                }
            } catch (t: Throwable) {
                HomingState.log("cfg 解码失败：$t")
            }
        }
        intent.getStringExtra("endpoint")?.takeIf { it.isNotBlank() }?.let { ep ->
            val active = repo.active()
            if (active != null && active.isJson) {
                runCatching {
                    val o = org.json.JSONObject(active.raw)
                    HomingProfile.setPeerEndpoint(o, ep)
                    repo.updateRaw(active.id, o.toString(), ep)
                    HomingState.log("已从 intent 把当前配置端点改为 $ep")
                    refreshProfiles()
                }.onFailure { HomingState.log("intent 端点修改失败：${it.message}") }
            }
        }
        if (intent.getBooleanExtra("discover", false)) queryDiscoveryForActive()
        if (intent.getBooleanExtra("connect", false)) {
            repo.active()?.let { connectProfile(it) }
        }
        if (intent.getBooleanExtra("disconnect", false)) disconnect()
    }

    private fun toast(msg: String) {
        Toast.makeText(this, msg, Toast.LENGTH_SHORT).show()
    }
}
