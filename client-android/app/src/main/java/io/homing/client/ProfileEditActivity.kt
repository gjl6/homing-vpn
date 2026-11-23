package io.homing.client

import android.os.Bundle
import android.widget.ArrayAdapter
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import com.google.android.material.appbar.MaterialToolbar
import com.google.android.material.button.MaterialButton
import com.google.android.material.dialog.MaterialAlertDialogBuilder
import com.google.android.material.switchmaterial.SwitchMaterial
import com.google.android.material.textfield.MaterialAutoCompleteTextView
import com.google.android.material.textfield.TextInputEditText

/**
 * 编辑单条配置：名称、端点、MTU、DNS、AllowedIPs、Keepalive、
 * 公告板（记录/自动跟随/优先协议/故障重查间隔），以及高级原始配置。
 */
class ProfileEditActivity : AppCompatActivity() {


    private lateinit var repo: ProfileRepository
    private lateinit var profile: Profile

    private lateinit var etName: TextInputEditText
    private lateinit var etEndpoint: TextInputEditText
    private lateinit var etMtu: TextInputEditText
    private lateinit var etKeepalive: TextInputEditText
    private lateinit var etDns: TextInputEditText
    private lateinit var etAllowedIps: TextInputEditText
    private lateinit var etRecord: TextInputEditText
    private lateinit var swAutoFollow: SwitchMaterial
    private lateinit var dropdownPreferred: MaterialAutoCompleteTextView
    private lateinit var etInterval: TextInputEditText
    private lateinit var swProbeEnabled: SwitchMaterial
    private lateinit var etProbeInterval: TextInputEditText
    private lateinit var etProbeIdleInterval: TextInputEditText
    private lateinit var etProbeIdleAfter: TextInputEditText
    private lateinit var etProbeFailures: TextInputEditText
    private lateinit var etProbeTimeout: TextInputEditText
    private lateinit var etProbeRecoveryInterval: TextInputEditText
    private lateinit var etProbeSoftGrace: TextInputEditText
    private lateinit var etRaw: TextInputEditText
    private lateinit var layoutDiscovery: android.view.View
    private lateinit var textConfNotice: TextView

    private var baselineRaw: String = ""
    private var baselineName: String = ""

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_profile_edit)

        repo = ProfileRepository(this)
        val id = intent.getStringExtra(EXTRA_PROFILE_ID)
        val found = repo.find(id)
        if (found == null) {
            finish()
            return
        }
        profile = found

        findViewById<MaterialToolbar>(R.id.toolbarEdit).setNavigationOnClickListener { finish() }

        etName = findViewById(R.id.etName)
        etEndpoint = findViewById(R.id.etEndpoint)
        etMtu = findViewById(R.id.etMtu)
        etKeepalive = findViewById(R.id.etKeepalive)
        etDns = findViewById(R.id.etDns)
        etAllowedIps = findViewById(R.id.etAllowedIps)
        etRecord = findViewById(R.id.etRecord)
        swAutoFollow = findViewById(R.id.swAutoFollow)
        dropdownPreferred = findViewById(R.id.dropdownPreferred)
        etInterval = findViewById(R.id.etInterval)
        swProbeEnabled = findViewById(R.id.swProbeEnabled)
        etProbeInterval = findViewById(R.id.etProbeInterval)
        etProbeIdleInterval = findViewById(R.id.etProbeIdleInterval)
        etProbeIdleAfter = findViewById(R.id.etProbeIdleAfter)
        etProbeFailures = findViewById(R.id.etProbeFailures)
        etProbeTimeout = findViewById(R.id.etProbeTimeout)
        etProbeRecoveryInterval = findViewById(R.id.etProbeRecoveryInterval)
        etProbeSoftGrace = findViewById(R.id.etProbeSoftGrace)
        etRaw = findViewById(R.id.etRaw)
        layoutDiscovery = findViewById(R.id.layoutDiscovery)
        textConfNotice = findViewById(R.id.textConfNotice)

        if (!profile.isJson) {
            layoutDiscovery.visibility = android.view.View.GONE
            textConfNotice.visibility = android.view.View.VISIBLE
        }

        val preferredAdapter = ArrayAdapter(
            this,
            android.R.layout.simple_list_item_1,
            PREFERRED_LABELS,
        )
        dropdownPreferred.setAdapter(preferredAdapter)

        loadFields()

        findViewById<MaterialButton>(R.id.btnSave).setOnClickListener { save() }
        findViewById<MaterialButton>(R.id.btnDelete).setOnClickListener { confirmDelete() }
    }

    private fun loadFields() {
        val fields = ProfileFields.from(profile)
        baselineRaw = fields.raw
        baselineName = fields.name
        etName.setText(fields.name)
        etEndpoint.setText(fields.endpoint)
        etMtu.setText(fields.mtu)
        etKeepalive.setText(fields.keepalive)
        etDns.setText(fields.dns)
        etAllowedIps.setText(fields.allowedIps)
        etRecord.setText(fields.record)
        swAutoFollow.isChecked = fields.autoFollow
        dropdownPreferred.setText(preferredLabel(fields.preferred), false)
        etInterval.setText(fields.interval)
        swProbeEnabled.isChecked = fields.probeEnabled
        etProbeInterval.setText(fields.probeInterval)
        etProbeIdleInterval.setText(fields.probeIdleInterval)
        etProbeIdleAfter.setText(fields.probeIdleAfter)
        etProbeFailures.setText(fields.probeFailures)
        etProbeTimeout.setText(fields.probeTimeout)
        etProbeRecoveryInterval.setText(fields.probeRecoveryInterval)
        etProbeSoftGrace.setText(fields.probeSoftGrace)
        etRaw.setText(profile.raw)
    }

    private fun save() {
        val name = etName.text?.toString()?.trim().orEmpty()
        val rawText = etRaw.text?.toString()?.trim().orEmpty()
        val rawChanged = rawText != baselineRaw.trim()
        val endpoint = etEndpoint.text?.toString()?.trim().orEmpty()

        if (endpoint.isNotBlank() && !endpoint.contains(':')) {
            toast("端点格式不对，应为 host:port 或 [IPv6]:port")
            return
        }
        val mtuText = etMtu.text?.toString()?.trim().orEmpty()
        if (mtuText.isNotBlank() && (mtuText.toIntOrNull() ?: 0) !in 576..1500) {
            toast("MTU 应为 576–1500 之间的整数")
            return
        }
        val keepaliveText = etKeepalive.text?.toString()?.trim().orEmpty()
        if (keepaliveText.isNotBlank() && (keepaliveText.toIntOrNull() ?: -1) < 0) {
            toast("Keepalive 应为非负整数（秒）")
            return
        }
        var preferred = preferredValue(dropdownPreferred.text?.toString()?.trim().orEmpty())
        if (preferred !in PREFERRED_VALUES) preferred = "auto"
        val intervalText = etInterval.text?.toString()?.trim().orEmpty()
        if (intervalText.isNotBlank() && (intervalText.toIntOrNull() ?: 0) < HomingProfile.MIN_INTERVAL_SEC) {
            toast("故障重查间隔至少 ${HomingProfile.MIN_INTERVAL_SEC} 秒")
            return
        }
        val probeIntervalText = etProbeInterval.text?.toString()?.trim().orEmpty()
        if (probeIntervalText.isNotBlank()) {
            val v = probeIntervalText.toIntOrNull() ?: 0
            if (v !in HomingProfile.Probe.MIN_PROBE_INTERVAL_SEC..HomingProfile.Probe.MAX_PROBE_INTERVAL_SEC) {
                toast("探针间隔应为 ${HomingProfile.Probe.MIN_PROBE_INTERVAL_SEC}–${HomingProfile.Probe.MAX_PROBE_INTERVAL_SEC} 秒")
                return
            }
        }
        val probeFailuresText = etProbeFailures.text?.toString()?.trim().orEmpty()
        if (probeFailuresText.isNotBlank()) {
            val v = probeFailuresText.toIntOrNull() ?: 0
            if (v !in HomingProfile.Probe.MIN_FAILURES..HomingProfile.Probe.MAX_FAILURES) {
                toast("连续失败次数应为 ${HomingProfile.Probe.MIN_FAILURES}–${HomingProfile.Probe.MAX_FAILURES}")
                return
            }
        }
        val probeTimeoutText = etProbeTimeout.text?.toString()?.trim().orEmpty()
        if (probeTimeoutText.isNotBlank()) {
            val v = probeTimeoutText.toIntOrNull() ?: 0
            if (v !in HomingProfile.Probe.MIN_TIMEOUT_MS..HomingProfile.Probe.MAX_TIMEOUT_MS) {
                toast("探针超时应为 ${HomingProfile.Probe.MIN_TIMEOUT_MS}–${HomingProfile.Probe.MAX_TIMEOUT_MS} 毫秒")
                return
            }
        }
        val probeIdleIntervalText = etProbeIdleInterval.text?.toString()?.trim().orEmpty()
        if (probeIdleIntervalText.isNotBlank()) {
            val v = probeIdleIntervalText.toIntOrNull() ?: 0
            if (v !in HomingProfile.Probe.MIN_IDLE_INTERVAL_SEC..HomingProfile.Probe.MAX_IDLE_INTERVAL_SEC) {
                toast("空闲探针间隔应为 ${HomingProfile.Probe.MIN_IDLE_INTERVAL_SEC}–${HomingProfile.Probe.MAX_IDLE_INTERVAL_SEC} 秒")
                return
            }
        }
        val probeIdleAfterText = etProbeIdleAfter.text?.toString()?.trim().orEmpty()
        if (probeIdleAfterText.isNotBlank()) {
            val v = probeIdleAfterText.toIntOrNull() ?: 0
            if (v !in HomingProfile.Probe.MIN_IDLE_AFTER_SEC..HomingProfile.Probe.MAX_IDLE_AFTER_SEC) {
                toast("空闲判定应为 ${HomingProfile.Probe.MIN_IDLE_AFTER_SEC}–${HomingProfile.Probe.MAX_IDLE_AFTER_SEC} 秒")
                return
            }
        }
        val probeRecoveryIntervalText = etProbeRecoveryInterval.text?.toString()?.trim().orEmpty()
        if (probeRecoveryIntervalText.isNotBlank()) {
            val v = probeRecoveryIntervalText.toIntOrNull() ?: 0
            if (v !in HomingProfile.Probe.MIN_RECOVERY_INTERVAL_SEC..HomingProfile.Probe.MAX_RECOVERY_INTERVAL_SEC) {
                toast("快探间隔应为 ${HomingProfile.Probe.MIN_RECOVERY_INTERVAL_SEC}–${HomingProfile.Probe.MAX_RECOVERY_INTERVAL_SEC} 秒")
                return
            }
        }
        val probeSoftGraceText = etProbeSoftGrace.text?.toString()?.trim().orEmpty()
        if (probeSoftGraceText.isNotBlank()) {
            val v = probeSoftGraceText.toIntOrNull() ?: 0
            if (v !in HomingProfile.Probe.MIN_SOFT_GRACE_SEC..HomingProfile.Probe.MAX_SOFT_GRACE_SEC) {
                toast("软恢复宽限应为 ${HomingProfile.Probe.MIN_SOFT_GRACE_SEC}–${HomingProfile.Probe.MAX_SOFT_GRACE_SEC} 秒")
                return
            }
        }

        val newRaw: String = try {
            if (rawChanged) {
                // 直接改了原始配置：以原始文本为准
                val normalized = HomingProfile.normalize(rawText)
                if (name.isNotBlank()) {
                    if (HomingProfile.isJson(normalized)) {
                        val o = org.json.JSONObject(normalized)
                        o.put("name", name)
                        o.toString()
                    } else normalized
                } else normalized
            } else {
                val fields = ProfileFields(
                    name = name.ifBlank { baselineName },
                    endpoint = endpoint,
                    mtu = mtuText,
                    dns = etDns.text?.toString()?.trim().orEmpty(),
                    allowedIps = etAllowedIps.text?.toString()?.trim().orEmpty(),
                    keepalive = keepaliveText,
                    record = if (profile.isJson) etRecord.text?.toString()?.trim().orEmpty() else "",
                    autoFollow = swAutoFollow.isChecked,
                    preferred = preferred,
                    interval = intervalText,
                    probeEnabled = swProbeEnabled.isChecked,
                    probeInterval = probeIntervalText,
                    probeIdleInterval = probeIdleIntervalText,
                    probeIdleAfter = probeIdleAfterText,
                    probeFailures = probeFailuresText,
                    probeTimeout = probeTimeoutText,
                    probeRecoveryInterval = probeRecoveryIntervalText,
                    probeSoftGrace = probeSoftGraceText,
                    raw = baselineRaw,
                    isJson = profile.isJson,
                )
                if (profile.isJson) ProfileFields.applyJson(fields, baselineRaw)
                else ProfileFields.applyConf(fields, baselineRaw)
            }
        } catch (e: Exception) {
            toast("配置无效：${e.message}")
            return
        }

        val updated = profile.copy(
            name = name.ifBlank { profile.displayName() },
            raw = newRaw,
            lastEndpoint = if (HomingProfile.isJson(newRaw)) {
                runCatching { HomingProfile.peerEndpoint(org.json.JSONObject(newRaw)) }.getOrNull()
            } else {
                HomingProfile.confEndpoint(newRaw)
            },
        )
        repo.update(updated)
        HomingState.log("已保存配置「${updated.displayName()}」")
        // 如果改的是当前正在使用的配置，按新配置重连
        if (updated.id == repo.activeId && HomingState.tunnelState == "UP") {
            HomingState.log("当前配置已修改，正在重连…")
            HomingService.startAction(this, HomingService.ACTION_CONNECT)
        }
        toast("已保存")
        finish()
    }

    private fun confirmDelete() {
        MaterialAlertDialogBuilder(this)
            .setTitle("删除配置")
            .setMessage("确定删除「${profile.displayName()}」？")
            .setNegativeButton("取消", null)
            .setPositiveButton("删除") { _, _ ->
                if (profile.id == HomingState.connectedProfileId) {
                    HomingService.startAction(this, HomingService.ACTION_DISCONNECT)
                }
                repo.delete(profile.id)
                HomingState.log("已删除配置「${profile.displayName()}」")
                toast("已删除")
                finish()
            }
            .show()
    }

    private fun toast(msg: String) {
        android.widget.Toast.makeText(this, msg, android.widget.Toast.LENGTH_SHORT).show()
    }

    companion object {
        const val EXTRA_PROFILE_ID = "profile_id"

        private val PREFERRED_VALUES = listOf("auto", "v6,v4", "v4,v6", "v4-only", "v6-only", "v4", "v6")
        private val PREFERRED_LABELS = listOf(
            "自动（有 IPv6 优先 v6）",
            "IPv6 优先（v6 → v4）",
            "IPv4 优先（v4 → v6）",
            "仅 IPv4",
            "仅 IPv6",
            "IPv4 优先（兼容旧值）",
            "IPv6 优先（兼容旧值）"
        )

        private fun preferredValue(label: String): String {
            val i = PREFERRED_LABELS.indexOf(label)
            return if (i >= 0) PREFERRED_VALUES[i] else label.trim().lowercase()
        }

        private fun preferredLabel(value: String): String {
            val v = value.trim().lowercase().ifBlank { "auto" }
            val i = PREFERRED_VALUES.indexOf(v)
            return if (i >= 0) PREFERRED_LABELS[i] else PREFERRED_LABELS[0]
        }
    }
}
