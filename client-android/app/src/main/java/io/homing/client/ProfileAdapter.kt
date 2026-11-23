package io.homing.client

import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.widget.TextView
import androidx.recyclerview.widget.RecyclerView
import com.google.android.material.button.MaterialButton
import com.google.android.material.card.MaterialCardView

/**
 * 配置列表：每张卡片一条配置，显示名称/端点/公告板，并提供「连接、编辑、更多」。
 */
class ProfileAdapter(
    private val onClickProfile: (Profile) -> Unit,
    private val onConnect: (Profile) -> Unit,
    private val onEdit: (Profile) -> Unit,
    private val onMore: (Profile, View) -> Unit,
) : RecyclerView.Adapter<ProfileAdapter.Holder>() {

    private var items: List<Profile> = emptyList()
    private var activeId: String? = null
    private var connectedId: String? = null

    fun submit(profiles: List<Profile>, activeId: String?, connectedId: String?) {
        items = profiles
        this.activeId = activeId
        this.connectedId = connectedId
        notifyDataSetChanged()
    }

    override fun onCreateViewHolder(parent: ViewGroup, viewType: Int): Holder {
        val v = LayoutInflater.from(parent.context).inflate(R.layout.item_profile, parent, false)
        return Holder(v)
    }

    override fun getItemCount(): Int = items.size

    override fun onBindViewHolder(holder: Holder, position: Int) {
        holder.bind(items[position])
    }

    inner class Holder(view: View) : RecyclerView.ViewHolder(view) {
        private val card: MaterialCardView = view.findViewById(R.id.cardProfile)
        private val name: TextView = view.findViewById(R.id.textProfileName)
        private val activeBadge: TextView = view.findViewById(R.id.textActiveBadge)
        private val summary: TextView = view.findViewById(R.id.textProfileSummary)
        private val connectBtn: MaterialButton = view.findViewById(R.id.btnItemConnect)
        private val editBtn: MaterialButton = view.findViewById(R.id.btnItemEdit)
        private val moreBtn: MaterialButton = view.findViewById(R.id.btnItemMore)

        fun bind(profile: Profile) {
            name.text = profile.displayName()
            summary.text = profile.summary()
            val active = profile.id == activeId
            val connected = profile.id == connectedId
            // 连接中的卡片显示「使用中」，其它仅当前选中显示「当前」
            activeBadge.visibility = if (active || connected) View.VISIBLE else View.GONE
            activeBadge.text = if (connected) "使用中" else "当前"
            connectBtn.text = if (connected) "断开" else "连接"
            card.strokeWidth = if (connected) dp(2) else if (active) dp(2) else dp(1)
            card.setOnClickListener { onClickProfile(profile) }
            connectBtn.setOnClickListener { onConnect(profile) }
            editBtn.setOnClickListener { onEdit(profile) }
            moreBtn.setOnClickListener { onMore(profile, it) }
        }

        private fun dp(v: Int): Int = (itemView.resources.displayMetrics.density * v).toInt()
    }
}
