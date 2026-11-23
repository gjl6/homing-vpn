#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""归途 Homing 客户端 —— UOS / Linux 桌面图形界面（Python3 + Gtk3/PyGObject）。

设计原则：
  * GUI 只做「拼命令 + 读状态 + 展示」，所有网络操作交给 root 助手 /usr/libexec/homing/homing-helper；
  * 需要 root 的动作通过 pkexec（polkit auth_admin_keep，5 分钟内不重复弹窗）；
  * 状态从 /run/homing/status.json（0644）读取，每秒轮询，不需要 root；
  * 端点发现（discover）与配置解析（parse-conf）不需要 root，直接调用助手。

用法：
  homing-gui              正常启动
  homing-gui --selftest   无头冒烟测试：构建界面、跑一轮刷新、打印 SELFTEST OK 后退出
"""

import json
import os
import re
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

import gi

gi.require_version("Gtk", "3.0")
gi.require_version("Gdk", "3.0")
gi.require_version("Pango", "1.0")
from gi.repository import Gdk, GLib, Gtk, Pango  # noqa: E402

APP_NAME = "归途客户端"
APP_ID = "homing_gui.py"
HELPER = "/usr/libexec/homing/homing-helper"
PKEXEC = "/usr/bin/pkexec"
STATUS_PATH = "/run/homing/status.json"
LOG_PATH = "/var/log/homing/homing.log"

CONFIG_DIR = os.path.join(
    os.environ.get("XDG_CONFIG_HOME", os.path.expanduser("~/.config")), "homing"
)
PROFILE_DIR = os.path.join(CONFIG_DIR, "profiles")
CURRENT_FILE = os.path.join(CONFIG_DIR, "current")

LOG_MAX_LINES = 500
HANDSHAKE_WARN_SECONDS = 180

CSS = """
.status-ok   { color: #2e7d32; font-weight: bold; }
.status-off  { color: #b71c1c; font-weight: bold; }
.status-busy { color: #ef6c00; font-weight: bold; }
.dim         { color: #666666; }
.mono        { font-family: monospace; }
.profile-name { font-weight: 600; }
.profile-detail { color: #666666; font-size: 11px; }
list row:selected .profile-name,
list row:selected .profile-detail,
row:selected .profile-name,
row:selected .profile-detail { color: #ffffff; }
"""

SELF_API_FALLBACK = "http://10.8.0.1:51822"


# ---------------------------------------------------------------- 基础工具


def run_cmd(argv, input_text=None, timeout=90):
    """执行命令，返回 (rc, stdout, stderr)。永不抛异常。"""
    try:
        proc = subprocess.run(
            argv,
            input=input_text,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            encoding="utf-8",
            errors="replace",
            timeout=timeout,
        )
        return proc.returncode, (proc.stdout or "").strip(), (proc.stderr or "").strip()
    except FileNotFoundError:
        return 127, "", "找不到命令 %s（请确认 homing-client 已安装）" % argv[0]
    except subprocess.TimeoutExpired:
        return 124, "", "命令超时（%s 秒）: %s" % (timeout, " ".join(argv))
    except OSError as exc:
        return 126, "", "执行失败: %s" % exc


def human_bytes(num):
    try:
        num = float(num)
    except (TypeError, ValueError):
        return "-"
    if num < 1024:
        return "%d B" % int(num)
    for unit in ("KB", "MB", "GB", "TB"):
        num /= 1024.0
        if num < 1024:
            return "%.1f %s" % (num, unit)
    return "%.1f PB" % num


def human_age(seconds):
    """把秒数变成「3 秒前 / 2 分钟前 / 1 小时前」。"""
    try:
        seconds = int(seconds)
    except (TypeError, ValueError):
        return "从未"
    if seconds < 0:
        return "从未"
    if seconds < 60:
        return "%d 秒前" % seconds
    if seconds < 3600:
        return "%d 分钟前" % (seconds // 60)
    if seconds < 86400:
        return "%d 小时前" % (seconds // 3600)
    return "%d 天前" % (seconds // 86400)


def slugify(name):
    slug = re.sub(r"[^0-9a-zA-Z\u4e00-\u9fff._-]+", "-", (name or "").strip())
    slug = slug.strip("-._") or "profile"
    return slug[:48]


def ensure_dirs():
    for path in (CONFIG_DIR, PROFILE_DIR):
        try:
            os.makedirs(path, exist_ok=True)
        except OSError:
            pass


def profile_files():
    ensure_dirs()
    try:
        names = [n for n in os.listdir(PROFILE_DIR) if n.endswith(".json")]
    except OSError:
        return []
    names.sort()
    return [os.path.join(PROFILE_DIR, n) for n in names]


def load_json(path):
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return json.load(handle)
    except (OSError, ValueError):
        return None


def save_json(path, data):
    ensure_dirs()
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(data, handle, ensure_ascii=False, indent=2)
    os.replace(tmp, path)


def profile_summary(data):
    """配置列表第二行：优先显示 Endpoint，其次公告板记录，最后显示隧道地址。"""
    if not isinstance(data, dict):
        return "未设置端点"
    peers = data.get("peers") or []
    peer = peers[0] if peers and isinstance(peers[0], dict) else {}
    endpoint = (peer.get("endpoint") or "").strip()
    disc = data.get("discovery") or {}
    record = (disc.get("record") or "").strip() if isinstance(disc, dict) else ""
    parts = []
    if endpoint:
        parts.append("Endpoint " + endpoint)
    if record:
        parts.append("公告板 " + record)
    if not parts:
        addrs = data.get("addresses") or []
        if addrs:
            parts.append(str(addrs[0]))
    return " · ".join(parts) or "未设置端点"


def self_api_of(data):
    if isinstance(data, dict):
        api = (data.get("selfApi") or "").strip()
        if api:
            return api
    return SELF_API_FALLBACK


def http_json(url, payload=None, timeout=3.5):
    """极简 JSON 请求；payload 为 None 时 GET，否则 POST。"""
    body = None
    headers = {"Accept": "application/json"}
    if payload is not None:
        body = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json; charset=utf-8"
    req = urllib.request.Request(url, data=body, headers=headers,
                                 method="POST" if payload is not None else "GET")
    # 不要走系统 http_proxy，直接访问隧道内 10.8.0.1
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(req, timeout=timeout) as resp:
        raw = resp.read().decode("utf-8", "replace")
    return json.loads(raw) if raw else {}


def api_error_message(exc):
    if isinstance(exc, urllib.error.HTTPError):
        try:
            data = json.loads(exc.read().decode("utf-8", "replace"))
            if isinstance(data, dict) and data.get("error"):
                return str(data["error"])
        except Exception:  # noqa: BLE001
            pass
        return "服务端返回 HTTP %s" % exc.code
    return str(exc) or "连接失败"


def load_current_slug():
    try:
        with open(CURRENT_FILE, "r", encoding="utf-8") as handle:
            return handle.read().strip()
    except OSError:
        return ""


def save_current_slug(slug):
    try:
        with open(CURRENT_FILE, "w", encoding="utf-8") as handle:
            handle.write(slug)
    except OSError:
        pass


# ---------------------------------------------------------------- 主窗口


class HomingApp(Gtk.Window):
    def __init__(self):
        super().__init__(title=APP_NAME)
        self.set_wmclass(APP_ID, APP_NAME)
        self.set_default_size(1080, 720)
        self.set_position(Gtk.WindowPosition.CENTER)
        self.set_border_width(0)
        self.connect("destroy", self._on_destroy)

        self._busy = False
        self._profiles = []          # [(path, slug, name)]
        self._current_path = None
        self._status = {}            # 上次读到的 status.json
        self._last_endpoint = ""
        self._last_warn = 0.0
        self._tick_id = None
        self._helper_ok = os.access(HELPER, os.X_OK)
        self._last_discovery = {}
        self._warned_no_handshake = False
        self._last_event = ""
        self._loading_autofollow = False
        self._loading_autostart = False
        self._loading_proxy = False
        self._proxy_status = {}
        self._proxy_loading = False
        self._proxy_updating = False
        self._proxy_last_fetch = 0.0
        self._last_running_profile = None

        self._install_css()
        self._build_ui()
        self._reload_profiles()

        if not self._helper_ok:
            self._log("未找到特权助手 %s —— 请先安装 homing-client deb 包" % HELPER)

        self._refresh_autostart()
        self._tick_id = GLib.timeout_add(1000, self._tick)

    # ------------------------------------------------------------ UI 构建

    def _install_css(self):
        provider = Gtk.CssProvider()
        try:
            provider.load_from_data(CSS.encode("utf-8"))
            screen = Gdk.Screen.get_default()
            if screen is not None:
                Gtk.StyleContext.add_provider_for_screen(
                    screen, provider, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION
                )
        except GLib.Error:
            pass

    def _build_ui(self):
        header = Gtk.HeaderBar()
        header.set_show_close_button(True)
        header.set_title(APP_NAME)
        header.set_subtitle("全部流量回到网关再出网 · 双栈隧道 · 端点自动跟随")
        self.set_titlebar(header)

        btn_import = Gtk.Button(label="导入配置")
        btn_import.set_tooltip_text("导入归途 JSON 配置（旧 .conf 会自动转换）")
        btn_import.connect("clicked", self._on_import_clicked)
        header.pack_start(btn_import)

        btn_paste = Gtk.Button(label="粘贴配置")
        btn_paste.set_tooltip_text("粘贴 [Interface]/[Peer] 配置文本")
        btn_paste.connect("clicked", self._on_paste_clicked)
        header.pack_start(btn_paste)

        btn_edit = Gtk.Button(label="编辑")
        btn_edit.set_tooltip_text("编辑当前配置：改名 / 改端点 / 改公告板记录")
        btn_edit.connect("clicked", self._on_edit_clicked)
        header.pack_start(btn_edit)

        btn_delete = Gtk.Button(label="删除")
        btn_delete.set_tooltip_text("删除当前选中的配置")
        btn_delete.connect("clicked", self._on_delete_clicked)
        header.pack_start(btn_delete)

        btn_log = Gtk.Button(label="查看日志")
        btn_log.set_tooltip_text("查看 /var/log/homing/homing.log（排查“连上但没网”用）")
        btn_log.connect("clicked", self._on_view_log_clicked)
        header.pack_end(btn_log)

        root = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=10)
        root.set_border_width(10)
        self.add(root)

        # 左侧：配置列表
        left = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6)
        left.set_size_request(300, -1)
        left_title_row = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=6)
        left_title = Gtk.Label(label="配置")
        left_title.set_xalign(0)
        left_title.set_markup("<b>配置</b>")
        left_title_row.pack_start(left_title, False, False, 0)
        self._lbl_profile_count = Gtk.Label(label="")
        self._lbl_profile_count.set_xalign(1)
        self._lbl_profile_count.set_markup("<span foreground='#777777'></span>")
        left_title_row.pack_end(self._lbl_profile_count, False, False, 0)
        left.pack_start(left_title_row, False, False, 0)

        scroller = Gtk.ScrolledWindow()
        scroller.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
        scroller.set_shadow_type(Gtk.ShadowType.IN)
        self._profile_list = Gtk.ListBox()
        self._profile_list.set_selection_mode(Gtk.SelectionMode.SINGLE)
        self._profile_list.connect("row-selected", self._on_profile_selected)
        scroller.add(self._profile_list)
        left.pack_start(scroller, True, True, 0)

        self._empty_hint = Gtk.Label(
            label="还没有配置。\n点上方「导入配置」或「粘贴配置」。"
        )
        self._empty_hint.set_no_show_all(True)
        self._empty_hint.set_line_wrap(True)
        self._empty_hint.set_xalign(0)
        left.pack_start(self._empty_hint, False, False, 0)
        root.pack_start(left, False, False, 0)

        # 右侧
        right = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=10)
        root.pack_start(right, True, True, 0)

        right.pack_start(self._build_status_frame(), False, False, 0)
        right.pack_start(self._build_endpoint_frame(), False, False, 0)
        right.pack_start(self._build_log_frame(), True, True, 0)

    def _build_status_frame(self):
        frame = Gtk.Frame(label="连接状态")
        grid = Gtk.Grid()
        grid.set_border_width(10)
        grid.set_row_spacing(6)
        grid.set_column_spacing(14)
        frame.add(grid)

        self._lbl_state = Gtk.Label()
        self._lbl_state.set_xalign(0)
        self._lbl_state.set_markup("<span foreground='#b71c1c'>● 未连接</span>")
        self._lbl_endpoint = Gtk.Label(label="-")
        self._lbl_handshake = Gtk.Label(label="从未")
        self._lbl_traffic = Gtk.Label(label="-")
        self._lbl_addrs = Gtk.Label(label="-")
        self._lbl_addrs.set_line_wrap(True)
        self._lbl_addrs.set_xalign(0)
        self._lbl_profile_name = Gtk.Label(label="-")
        self._lbl_backend = Gtk.Label(label="-")

        rows = [
            ("状态", self._lbl_state),
            ("实现", self._lbl_backend),
            ("当前配置", self._lbl_profile_name),
            ("当前 Endpoint", self._lbl_endpoint),
            ("最近握手", self._lbl_handshake),
            ("收 / 发", self._lbl_traffic),
            ("隧道地址", self._lbl_addrs),
        ]
        for row, (name, widget) in enumerate(rows):
            tag = Gtk.Label(label=name + "：")
            tag.set_xalign(1)
            tag.set_markup("<span foreground='#555555'>%s</span>" % GLib.markup_escape_text(name + "："))
            grid.attach(tag, 0, row, 1, 1)
            widget.set_xalign(0)
            grid.attach(widget, 1, row, 1, 1)

        btn_box = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        self._btn_connect = Gtk.Button(label="连接")
        self._btn_connect.get_style_context().add_class("suggested-action")
        self._btn_connect.connect("clicked", self._on_connect_clicked)
        self._btn_disconnect = Gtk.Button(label="断开")
        self._btn_disconnect.connect("clicked", self._on_disconnect_clicked)
        self._btn_disconnect.set_sensitive(False)
        btn_box.pack_start(self._btn_connect, False, False, 0)
        btn_box.pack_start(self._btn_disconnect, False, False, 0)
        grid.attach(btn_box, 0, len(rows), 2, 1)

        # 开机自动连接开关（systemd 单元，授权一次长期有效）
        auto_row = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        self._switch_autostart = Gtk.Switch()
        self._switch_autostart.set_tooltip_text("开机时自动用「当前配置」建立隧道（需要管理员授权一次）")
        self._switch_autostart.connect("state-set", self._on_autostart_toggled)
        auto_row.pack_start(self._switch_autostart, False, False, 0)
        auto_label = Gtk.Label(label="开机自动连接（使用当前选中的配置）")
        auto_label.set_xalign(0)
        auto_row.pack_start(auto_label, True, True, 0)
        self._lbl_autostart = Gtk.Label(label="")
        self._lbl_autostart.set_xalign(1)
        self._lbl_autostart.set_markup("<span foreground='#777777'>读取中…</span>")
        auto_row.pack_start(self._lbl_autostart, False, False, 0)
        grid.attach(auto_row, 0, len(rows) + 1, 2, 1)

        # 代理出口自助开关（服务端「自助」权限 + 全局出口开启后才可用）
        proxy_row = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        self._switch_proxy = Gtk.Switch()
        self._switch_proxy.set_sensitive(False)
        self._switch_proxy.set_tooltip_text("服务端允许后，可自行开关本机是否走代理出口")
        self._switch_proxy.connect("state-set", self._on_proxy_toggled)
        proxy_row.pack_start(self._switch_proxy, False, False, 0)
        proxy_label = Gtk.Label(label="代理出口")
        proxy_label.set_xalign(0)
        proxy_row.pack_start(proxy_label, True, True, 0)
        self._lbl_proxy = Gtk.Label(label="未连接")
        self._lbl_proxy.set_xalign(1)
        self._lbl_proxy.set_ellipsize(Pango.EllipsizeMode.END)
        self._lbl_proxy.set_tooltip_text("")
        proxy_row.pack_end(self._lbl_proxy, False, False, 0)
        grid.attach(proxy_row, 0, len(rows) + 2, 2, 1)
        return frame

    def _build_endpoint_frame(self):
        frame = Gtk.Frame(label="端点（家宽动态端口自动发现）")
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6)
        box.set_border_width(10)
        frame.add(box)

        disc_row = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        self._lbl_disc = Gtk.Label(label="还没查询")
        self._lbl_disc.set_xalign(0)
        disc_row.pack_start(self._lbl_disc, True, True, 0)
        btn_discover = Gtk.Button(label="重新发现")
        btn_discover.connect("clicked", self._on_discover_clicked)
        disc_row.pack_start(btn_discover, False, False, 0)
        box.pack_start(disc_row, False, False, 0)

        cand_row = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        self._lbl_v4 = Gtk.Label(label="IPv4 候选：-")
        self._lbl_v4.set_xalign(0)
        self._btn_use_v4 = Gtk.Button(label="用 v4")
        self._btn_use_v4.connect("clicked", self._on_use_v4_clicked)
        self._btn_use_v4.set_sensitive(False)
        self._lbl_v6 = Gtk.Label(label="IPv6 候选：-")
        self._lbl_v6.set_xalign(0)
        self._btn_use_v6 = Gtk.Button(label="用 v6")
        self._btn_use_v6.connect("clicked", self._on_use_v6_clicked)
        self._btn_use_v6.set_sensitive(False)
        cand_row.pack_start(self._lbl_v4, True, True, 0)
        cand_row.pack_start(self._btn_use_v4, False, False, 0)
        cand_row.pack_start(self._lbl_v6, True, True, 0)
        cand_row.pack_start(self._btn_use_v6, False, False, 0)
        box.pack_start(cand_row, False, False, 0)

        manual_row = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        self._entry_endpoint = Gtk.Entry()
        self._entry_endpoint.set_placeholder_text("手动指定端点，如 203.0.113.10:51820 或 [2001:db8::1]:51820")
        manual_row.pack_start(self._entry_endpoint, True, True, 0)
        btn_swap = Gtk.Button(label="热换")
        btn_swap.set_tooltip_text("只改对端端点，不重建隧道、不断流")
        btn_swap.connect("clicked", self._on_swap_clicked)
        manual_row.pack_start(btn_swap, False, False, 0)
        box.pack_start(manual_row, False, False, 0)

        self._chk_autofollow = Gtk.CheckButton(label="自动跟随端点变化（服务端端口变化时自动热换）")
        self._chk_autofollow.connect("toggled", self._on_autofollow_toggled)
        box.pack_start(self._chk_autofollow, False, False, 0)
        return frame

    def _build_log_frame(self):
        frame = Gtk.Frame(label="日志")
        scroller = Gtk.ScrolledWindow()
        scroller.set_policy(Gtk.PolicyType.AUTOMATIC, Gtk.PolicyType.AUTOMATIC)
        scroller.set_shadow_type(Gtk.ShadowType.IN)
        self._log_view = Gtk.TextView()
        self._log_view.set_editable(False)
        self._log_view.set_cursor_visible(False)
        self._log_view.set_wrap_mode(Gtk.WrapMode.WORD_CHAR)
        self._log_view.get_style_context().add_class("mono")
        self._log_buffer = self._log_view.get_buffer()
        scroller.add(self._log_view)
        frame.add(scroller)
        self._log_scroller = scroller
        return frame

    # ------------------------------------------------------------ 配置管理

    def _reload_profiles(self, keep=None):
        try:
            ensure_dirs()
            current_slug = keep or load_current_slug()
            self._profiles = []
            for path in profile_files():
                data = load_json(path) or {}
                name = data.get("name") or os.path.splitext(os.path.basename(path))[0]
                slug = os.path.splitext(os.path.basename(path))[0]
                self._profiles.append({
                    "path": path,
                    "slug": slug,
                    "name": name,
                    "summary": profile_summary(data),
                })

            for child in self._profile_list.get_children():
                self._profile_list.remove(child)

            selected_row = None
            for prof in self._profiles:
                try:
                    row = Gtk.ListBoxRow()
                    box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=1)
                    box.set_margin_start(6)
                    box.set_margin_end(6)
                    box.set_margin_top(4)
                    box.set_margin_bottom(4)

                    name_label = Gtk.Label()
                    name_label.set_xalign(0)
                    name_label.set_ellipsize(Pango.EllipsizeMode.END)
                    name_label.get_style_context().add_class("profile-name")
                    name_label.set_markup(
                        "<b>%s</b>" % GLib.markup_escape_text(prof["name"])
                    )

                    detail_label = Gtk.Label(label=prof["summary"])
                    detail_label.set_xalign(0)
                    detail_label.set_ellipsize(Pango.EllipsizeMode.END)
                    detail_label.get_style_context().add_class("profile-detail")

                    box.pack_start(name_label, False, False, 0)
                    box.pack_start(detail_label, False, False, 0)
                    row.add(box)
                    row.profile = prof
                    row.name_label = name_label
                    row.detail_label = detail_label
                    row.set_tooltip_text("%s\n%s" % (prof["path"], prof["summary"]))
                    self._profile_list.add(row)
                    if prof["slug"] == current_slug:
                        selected_row = row
                except Exception as exc:  # noqa: BLE001 - 单个条目异常不能拖垮整个列表
                    self._log("加载配置条目失败 %s: %s" % (prof.get("path"), exc))

            self._lbl_profile_count.set_markup(
                "<span foreground='#777777'>%d 个</span>" % len(self._profiles)
            )
            self._empty_hint.set_visible(len(self._profiles) == 0)
            if self._profiles:
                if selected_row is None:
                    selected_row = self._profile_list.get_row_at_index(0)
                if selected_row is not None:
                    self._profile_list.select_row(selected_row)
            else:
                self._current_path = None
            self._refresh_profile_states()
            self._update_profile_widgets()
        except Exception as exc:  # noqa: BLE001
            self._log("刷新配置列表失败: %s" % exc)

    def _refresh_profile_states(self):
        """根据守护进程当前 profile，在配置列表里标出「使用中」。"""
        running_name = self._status.get("profile") if self._status.get("running") else ""
        for row in self._profile_list.get_children():
            prof = getattr(row, "profile", None)
            if not prof or not hasattr(row, "name_label"):
                continue
            in_use = bool(running_name) and prof["name"] == running_name
            prefix = "<span foreground='#2e7d32'>● </span>" if in_use else ""
            row.name_label.set_markup(
                prefix + "<b>%s</b>" % GLib.markup_escape_text(prof["name"])
            )
            detail = prof.get("summary") or ""
            row.detail_label.set_text(detail + (" · 使用中" if in_use else ""))

    def _on_profile_selected(self, _listbox, row):
        if row is None or not hasattr(row, "profile"):
            return
        self._current_path = row.profile["path"]
        save_current_slug(row.profile["slug"])
        self._update_profile_widgets()
        data = load_json(self._current_path) or {}
        disc = data.get("discovery") or {}
        self._loading_autofollow = True
        self._chk_autofollow.set_active(bool(disc.get("autoFollow")))
        self._loading_autofollow = False

    def _update_profile_widgets(self):
        data = load_json(self._current_path) if self._current_path else None
        running = bool(self._status.get("running"))
        self._btn_connect.set_sensitive(
            bool(self._current_path) and not running and not self._busy and self._helper_ok
        )
        self._btn_disconnect.set_sensitive(running and not self._busy and self._helper_ok)
        if data:
            self._lbl_profile_name.set_text(data.get("name") or "-")
        else:
            self._lbl_profile_name.set_text("-")

    def _save_profile(self, name, data):
        """导入保存。**不覆盖同名配置**：重名时自动改名 "名字 (2)"，避免把已有配置冲掉。"""
        ensure_dirs()
        base = (name or "").strip() or "配置"
        final_name = base
        slug = slugify(final_name)
        path = os.path.join(PROFILE_DIR, slug + ".json")
        n = 2
        while os.path.exists(path) and n < 100:
            final_name = "%s (%d)" % (base, n)
            slug = slugify(final_name)
            path = os.path.join(PROFILE_DIR, slug + ".json")
            n += 1
        data = dict(data)
        data["name"] = final_name
        data.setdefault("discovery", {})
        save_json(path, data)
        self._reload_profiles(keep=slug)
        if final_name != base:
            self._log("已保存配置：%s（同名已存在，自动改名为「%s」）" % (path, final_name))
        else:
            self._log("已保存配置：%s" % path)
        return path

    # ------------------------------------------------------------ 编辑（改名/改内容）

    def _on_edit_clicked(self, _btn):
        if not self._current_path:
            self._log("先选择一个配置再编辑")
            return
        data = load_json(self._current_path)
        if not data:
            self._log("读取配置失败：%s" % self._current_path)
            return

        dialog = Gtk.Dialog(title="编辑配置", parent=self, modal=True)
        dialog.add_buttons(
            Gtk.STOCK_CANCEL, Gtk.ResponseType.CANCEL,
            Gtk.STOCK_OK, Gtk.ResponseType.OK,
        )
        dialog.set_default_size(820, 640)
        area = dialog.get_content_area()
        area.set_border_width(10)
        area.set_spacing(8)

        name_row = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        name_row.pack_start(Gtk.Label(label="配置名字："), False, False, 0)
        name_entry = Gtk.Entry()
        name_entry.set_text(str(data.get("name") or ""))
        name_row.pack_start(name_entry, True, True, 0)
        area.pack_start(name_row, False, False, 0)

        hint = Gtk.Label()
        hint.set_markup(
            "<span foreground='#666666'>下面是完整配置 JSON，可以直接改："
            "<b>peers[].endpoint</b>（端点）、<b>discovery.record</b>（公告板记录）、"
            "<b>discovery.autoFollow</b>、<b>endpoints</b>（候选端点列表）、"
            "<b>probe.*</b>（探针：interval / idleInterval / idleAfter / failures / timeoutMs / "
            "recoveryInterval / softGrace）。保存前会校验 JSON。</span>"
        )
        hint.set_line_wrap(True)
        hint.set_xalign(0)
        area.pack_start(hint, False, False, 0)

        scroller = Gtk.ScrolledWindow()
        scroller.set_policy(Gtk.PolicyType.AUTOMATIC, Gtk.PolicyType.AUTOMATIC)
        scroller.set_shadow_type(Gtk.ShadowType.IN)
        view = Gtk.TextView()
        view.get_style_context().add_class("mono")
        view.set_wrap_mode(Gtk.WrapMode.WORD_CHAR)
        view.get_buffer().set_text(json.dumps(data, ensure_ascii=False, indent=2))
        scroller.add(view)
        area.pack_start(scroller, True, True, 0)
        dialog.show_all()

        response = dialog.run()
        buffer = view.get_buffer()
        text = buffer.get_text(buffer.get_start_iter(), buffer.get_end_iter(), False)
        new_name = name_entry.get_text().strip()
        dialog.destroy()
        if response != Gtk.ResponseType.OK:
            return
        try:
            new_data = json.loads(text)
        except ValueError as exc:
            self._log("JSON 格式错误，未保存：%s" % exc)
            return
        self._save_edited_profile(self._current_path, new_data, new_name or data.get("name"))

    def _save_edited_profile(self, old_path, data, new_name):
        """保存编辑结果；名字变了就重命名文件（同名冲突时自动加序号）。返回新路径。"""
        if not isinstance(data, dict):
            self._log("配置无效：不是 JSON 对象")
            return None
        if not data.get("peers"):
            self._log("配置无效：缺少 peers")
            return None
        new_name = (new_name or data.get("name") or "").strip() or "配置"
        data["name"] = new_name
        data.setdefault("discovery", {})

        old_slug = os.path.splitext(os.path.basename(old_path))[0]
        base_slug = slugify(new_name)
        slug = base_slug
        new_path = os.path.join(PROFILE_DIR, slug + ".json")
        n = 2
        while os.path.abspath(new_path) != os.path.abspath(old_path) and os.path.exists(new_path):
            slug = "%s-%d" % (base_slug, n)
            new_path = os.path.join(PROFILE_DIR, slug + ".json")
            n += 1
        try:
            save_json(new_path, data)
            if os.path.abspath(new_path) != os.path.abspath(old_path):
                try:
                    os.remove(old_path)
                except OSError:
                    pass
        except OSError as exc:
            self._log("保存失败：%s" % exc)
            return None
        self._current_path = new_path
        save_current_slug(slug)
        self._reload_profiles(keep=slug)
        self._log("已保存配置：%s" % new_path)
        return new_path

    # ------------------------------------------------------------ 导入/粘贴

    def _on_import_clicked(self, _btn):
        dialog = Gtk.FileChooserDialog(
            title="选择配置文件",
            parent=self,
            action=Gtk.FileChooserAction.OPEN,
        )
        dialog.add_buttons(
            Gtk.STOCK_CANCEL, Gtk.ResponseType.CANCEL,
            Gtk.STOCK_OPEN, Gtk.ResponseType.OK,
        )
        filt = Gtk.FileFilter()
        filt.set_name("归途配置 (*.json *.conf)")
        filt.add_pattern("*.json")
        filt.add_pattern("*.conf")
        dialog.add_filter(filt)
        all_filter = Gtk.FileFilter()
        all_filter.set_name("所有文件")
        all_filter.add_pattern("*")
        dialog.add_filter(all_filter)

        response = dialog.run()
        path = dialog.get_filename()
        dialog.destroy()
        if response != Gtk.ResponseType.OK or not path:
            return
        try:
            with open(path, "r", encoding="utf-8", errors="replace") as handle:
                text = handle.read()
        except OSError as exc:
            self._log("读取文件失败: %s" % exc)
            return
        name = os.path.splitext(os.path.basename(path))[0]
        self._import_text(text, name)

    def _on_paste_clicked(self, _btn):
        dialog = Gtk.Dialog(title="粘贴配置", parent=self, modal=True)
        dialog.add_buttons(
            Gtk.STOCK_CANCEL, Gtk.ResponseType.CANCEL,
            Gtk.STOCK_OK, Gtk.ResponseType.OK,
        )
        dialog.set_default_size(620, 520)
        area = dialog.get_content_area()
        area.set_spacing(8)
        area.set_border_width(10)

        name_row = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        name_row.pack_start(Gtk.Label(label="配置名字："), False, False, 0)
        entry_name = Gtk.Entry()
        entry_name.set_text("我的网关")
        name_row.pack_start(entry_name, True, True, 0)
        area.pack_start(name_row, False, False, 0)

        scroller = Gtk.ScrolledWindow()
        scroller.set_policy(Gtk.PolicyType.AUTOMATIC, Gtk.PolicyType.AUTOMATIC)
        text_view = Gtk.TextView()
        text_view.get_style_context().add_class("mono")
        text_view.set_wrap_mode(Gtk.WrapMode.WORD_CHAR)
        scroller.add(text_view)
        try:
            text_view.get_buffer().set_text(Gtk.Clipboard.get(Gdk.SELECTION_CLIPBOARD).wait_for_text() or "")
        except Exception:  # noqa: BLE001 - 剪贴板不可用就留空
            pass
        area.pack_start(scroller, True, True, 0)
        dialog.show_all()

        response = dialog.run()
        buffer = text_view.get_buffer()
        text = buffer.get_text(buffer.get_start_iter(), buffer.get_end_iter(), False)
        name = entry_name.get_text().strip() or "我的网关"
        dialog.destroy()
        if response == Gtk.ResponseType.OK:
            self._import_text(text, name)

    def _import_text(self, text, name):
        text = (text or "").strip()
        if not text:
            self._log("配置内容为空")
            return
        # parse-conf：不需要 root，直接调用助手把 .conf 转成 profile JSON
        rc, out, err = run_cmd([HELPER, "parse-conf"], input_text=text, timeout=30)
        if rc != 0:
            self._log("配置解析失败：%s" % (err or out or "未知错误"))
            return
        try:
            data = json.loads(out)
        except ValueError:
            self._log("配置解析结果不是合法 JSON")
            return
        path = self._save_profile(name, data)
        self._log("已导入「%s」，点「连接」即可建立隧道" % name)
        _ = path

    def _on_delete_clicked(self, _btn):
        if not self._current_path:
            return
        dialog = Gtk.MessageDialog(
            parent=self,
            flags=Gtk.DialogFlags.MODAL,
            type=Gtk.MessageType.QUESTION,
            buttons=Gtk.ButtonsType.OK_CANCEL,
            text="删除当前配置？",
        )
        dialog.format_secondary_text(self._current_path)
        response = dialog.run()
        dialog.destroy()
        if response != Gtk.ResponseType.OK:
            return
        try:
            os.remove(self._current_path)
            self._log("已删除 %s" % self._current_path)
        except OSError as exc:
            self._log("删除失败: %s" % exc)
        self._current_path = None
        save_current_slug("")
        self._reload_profiles()

    # ------------------------------------------------------------ 连接/断开

    def _on_connect_clicked(self, _btn):
        if not self._current_path:
            self._log("先选择一个配置（或导入 .conf）")
            return
        self._save_autofollow()
        self._busy = True
        self._set_state_label("正在连接…", "busy")
        self._update_profile_widgets()
        argv = [PKEXEC, HELPER, "up", "--daemon", "--profile", self._current_path]
        self._log("$ " + " ".join(argv))
        self._run_async(argv, self._after_connect)

    def _after_connect(self, rc, out, err):
        self._busy = False
        if rc == 0:
            self._log("连接成功：%s" % (out or "隧道已建立"))
        elif rc in (126, 127):
            self._log("已取消授权或授权失败（退出码 %d）：%s" % (rc, err or out))
        else:
            self._log("连接失败（退出码 %d）：%s" % (rc, err or out))
        self._tick()
        self._update_profile_widgets()

    def _on_disconnect_clicked(self, _btn):
        self._busy = True
        self._update_profile_widgets()
        argv = [PKEXEC, HELPER, "down"]
        self._log("$ " + " ".join(argv))
        self._run_async(argv, self._after_disconnect)

    def _after_disconnect(self, rc, out, err):
        self._busy = False
        if rc == 0:
            self._log("已断开：%s" % (out or "隧道已清理"))
        else:
            self._log("断开失败（退出码 %d）：%s" % (rc, err or out))
        self._tick()
        self._update_profile_widgets()

    # ------------------------------------------------------------ 开机自动连接

    def _refresh_autostart(self):
        """读一次开机自启状态（helper autostart status，不需要 root）。"""
        if not self._helper_ok:
            self._lbl_autostart.set_markup("<span foreground='#b71c1c'>助手未安装</span>")
            return

        def worker():
            rc, out, _err = run_cmd([HELPER, "autostart", "status"], timeout=15)

            def apply():
                enabled = False
                name = ""
                if rc == 0:
                    try:
                        info = json.loads(out)
                        enabled = bool(info.get("enabled"))
                        name = info.get("profile") or ""
                    except ValueError:
                        pass
                self._loading_autostart = True
                self._switch_autostart.set_active(enabled)
                self._loading_autostart = False
                if enabled:
                    self._lbl_autostart.set_markup(
                        "<span foreground='#2e7d32'>已开启%s</span>" % (("（%s）" % name) if name else "")
                    )
                else:
                    self._lbl_autostart.set_markup("<span foreground='#777777'>未开启</span>")
                return False

            GLib.idle_add(apply)

        threading.Thread(target=worker, daemon=True).start()

    def _on_autostart_toggled(self, switch, state):
        if getattr(self, "_loading_autostart", False):
            return False
        if not self._helper_ok:
            self._log("未找到特权助手，无法设置开机自启")
            return False
        if not self._current_path:
            self._log("先选择一个配置，再开启开机自动连接")
            self._loading_autostart = True
            switch.set_active(False)
            self._loading_autostart = False
            self._lbl_autostart.set_markup("<span foreground='#777777'>未开启</span>")
            return False
        want = bool(state)
        self._log("设置开机自动连接：%s" % ("开启" if want else "关闭"))
        if want:
            argv = [PKEXEC, HELPER, "autostart", "on", "--profile", self._current_path]
        else:
            argv = [PKEXEC, HELPER, "autostart", "off"]
        self._log("$ " + " ".join(argv))
        self._run_async(argv, self._after_autostart)
        return False  # 让回调里刷新真实状态

    def _after_autostart(self, rc, out, err):
        if rc == 0:
            self._log("已保存：%s" % (out or "设置成功"))
        elif rc in (126, 127):
            self._log("已取消授权或授权失败（退出码 %d）" % rc)
        else:
            self._log("设置失败（退出码 %d）：%s" % (rc, err or out))
        self._refresh_autostart()

    # ------------------------------------------------------------ 端点

    def _on_discover_clicked(self, _btn):
        if not self._current_path:
            self._log("先选择一个配置")
            return
        argv = [HELPER, "discover", "--profile", self._current_path]
        self._log("$ " + " ".join(argv))
        self._run_async(argv, self._after_discover)

    def _after_discover(self, rc, out, err):
        if rc not in (0, 1):
            self._log("端点发现失败：%s" % (err or out))
            return
        try:
            res = json.loads(out)
        except ValueError:
            self._log("端点发现返回异常：%s" % out)
            return
        self._apply_discovery(res)

    def _apply_discovery(self, res):
        v4 = (res.get("v4") or "").strip()
        v6 = (res.get("v6") or "").strip()
        self._lbl_disc.set_text(
            "公告记录：%s    更新时间：%s%s"
            % (
                res.get("record") or "-",
                res.get("ts") or "-",
                ("    错误：" + res["error"]) if res.get("error") else "",
            )
        )
        self._lbl_v4.set_text("IPv4 候选：%s" % (v4 or "-"))
        self._lbl_v6.set_text("IPv6 候选：%s" % (v6 or "-"))
        self._btn_use_v4.set_sensitive(bool(v4))
        self._btn_use_v6.set_sensitive(bool(v6))
        self._last_discovery = res

    def _on_use_v4_clicked(self, _btn):
        self._hot_swap(getattr(self, "_last_discovery", {}).get("v4", ""))

    def _on_use_v6_clicked(self, _btn):
        self._hot_swap(getattr(self, "_last_discovery", {}).get("v6", ""))

    def _on_swap_clicked(self, _btn):
        self._hot_swap(self._entry_endpoint.get_text().strip())

    def _hot_swap(self, endpoint):
        endpoint = (endpoint or "").strip()
        if not endpoint:
            self._log("端点为空，先「重新发现」或手动填写")
            return
        argv = [PKEXEC, HELPER, "set-endpoint", endpoint]
        self._log("$ " + " ".join(argv))
        self._run_async(argv, lambda rc, out, err: self._after_hot_swap(rc, out, err, endpoint))

    def _after_hot_swap(self, rc, out, err, endpoint):
        if rc == 0:
            self._log("端点已热换为 %s（不重建隧道）" % endpoint)
        else:
            self._log("热换失败（退出码 %d）：%s" % (rc, err or out))
        self._tick()

    def _on_view_log_clicked(self, _btn):
        """查看守护进程日志（0644，普通用户可读）。"""
        path = "/var/log/homing/homing.log"
        try:
            with open(path, "r", encoding="utf-8", errors="replace") as handle:
                lines = handle.read().splitlines()
            text = "\n".join(lines[-200:]) or "(日志为空)"
        except FileNotFoundError:
            text = "还没有日志：%s\\n\\n先在主界面点「连接」建立一次隧道。" % path
        except PermissionError:
            text = ("没有权限读取 %s\\n\\n请在终端执行：\\nsudo tail -n 200 %s" % (path, path))
        except OSError as exc:
            text = "读取日志失败: %s" % exc

        dialog = Gtk.Dialog(title="隧道日志", parent=self, modal=True)
        dialog.add_buttons(Gtk.STOCK_CLOSE, Gtk.ResponseType.CLOSE)
        dialog.set_default_size(900, 560)
        area = dialog.get_content_area()
        area.set_border_width(8)
        tip = Gtk.Label()
        tip.set_markup("<span foreground='#555555'>%s（最后 200 行）</span>" % GLib.markup_escape_text(path))
        tip.set_xalign(0)
        area.pack_start(tip, False, False, 0)
        scroller = Gtk.ScrolledWindow()
        scroller.set_policy(Gtk.PolicyType.AUTOMATIC, Gtk.PolicyType.AUTOMATIC)
        view = Gtk.TextView()
        view.set_editable(False)
        view.set_wrap_mode(Gtk.WrapMode.WORD_CHAR)
        view.get_style_context().add_class("mono")
        view.get_buffer().set_text(text)
        scroller.add(view)
        area.pack_start(scroller, True, True, 0)
        dialog.show_all()
        dialog.run()
        dialog.destroy()

    def _on_autofollow_toggled(self, check):
        if getattr(self, "_loading_autofollow", False):
            return
        self._save_autofollow()

    def _save_autofollow(self):
        if not self._current_path:
            return
        data = load_json(self._current_path)
        if not data:
            return
        disc = data.get("discovery") or {}
        want = bool(self._chk_autofollow.get_active())
        if bool(disc.get("autoFollow")) == want:
            return
        disc["autoFollow"] = want
        data["discovery"] = disc
        try:
            save_json(self._current_path, data)
            self._log("已%s「自动跟随端点」（守护进程下一次连接时生效）" % ("开启" if want else "关闭"))
        except OSError as exc:
            self._log("保存配置失败: %s" % exc)

    # ------------------------------------------------------------ 代理出口自助

    def _self_api_base(self):
        data = load_json(self._current_path) if self._current_path else {}
        return self_api_of(data)

    def _update_proxy_row(self, running):
        if not running:
            self._proxy_status = {}
            self._proxy_loading = False
            self._loading_proxy = True
            self._switch_proxy.set_active(False)
            self._switch_proxy.set_sensitive(False)
            self._loading_proxy = False
            self._lbl_proxy.set_text("未连接")
            self._lbl_proxy.set_tooltip_text("")
            return
        now = time.time()
        if self._proxy_loading or now - self._proxy_last_fetch < 8:
            return
        self._proxy_last_fetch = now
        self._proxy_loading = True
        threading.Thread(target=self._fetch_proxy_status, daemon=True).start()

    def _fetch_proxy_status(self):
        api = self._self_api_base().rstrip("/") + "/api/self/proxy"
        try:
            data = http_json(api)
        except Exception as exc:  # noqa: BLE001
            GLib.idle_add(self._proxy_fetch_failed, api_error_message(exc))
            return
        GLib.idle_add(self._apply_proxy_status, data)

    def _apply_proxy_status(self, st):
        self._proxy_loading = False
        if not isinstance(st, dict):
            st = {}
        self._proxy_status = st
        self._loading_proxy = True
        self._switch_proxy.set_active(bool(st.get("enabled")))
        self._switch_proxy.set_sensitive(bool(st.get("allowed")))
        self._loading_proxy = False
        if not st.get("globalEnabled"):
            text = "未开启（管理员未开启全局出口）"
        elif not st.get("selfService"):
            text = "未授权（请联系管理员）"
        elif st.get("allowed"):
            text = "已开启 · 可自行关闭" if st.get("enabled") else "已关闭 · 可自行开启"
        else:
            text = st.get("message") or "当前不可用"
        self._lbl_proxy.set_text(text)
        self._lbl_proxy.set_tooltip_text(st.get("message") or text)
        return False

    def _proxy_fetch_failed(self, message):
        self._proxy_loading = False
        self._switch_proxy.set_sensitive(False)
        self._lbl_proxy.set_text("控制接口暂不可用")
        self._lbl_proxy.set_tooltip_text(message or "")
        return False

    def _on_proxy_toggled(self, _switch, state):
        if self._loading_proxy:
            return False
        st = self._proxy_status or {}
        if not st.get("allowed"):
            self._log("代理出口不可用：%s" % (st.get("message") or "请联系管理员开启权限"))
            return True
        if self._proxy_updating:
            return True
        self._proxy_updating = True
        self._switch_proxy.set_sensitive(False)
        self._lbl_proxy.set_text("正在保存…")
        enabled = bool(state)
        threading.Thread(target=self._post_proxy, args=(enabled,), daemon=True).start()
        return True

    def _post_proxy(self, enabled):
        api = self._self_api_base().rstrip("/") + "/api/self/proxy"
        try:
            data = http_json(api, {"enabled": enabled})
        except Exception as exc:  # noqa: BLE001
            GLib.idle_add(self._apply_proxy_post, enabled, None, api_error_message(exc))
            return
        GLib.idle_add(self._apply_proxy_post, enabled, data, None)

    def _apply_proxy_post(self, enabled, st, error):
        self._proxy_updating = False
        if error:
            self._log("代理出口切换失败：%s" % error)
            self._proxy_last_fetch = 0.0
            self._proxy_fetch_failed(error)
            return False
        self._apply_proxy_status(st or {})
        self._log("代理出口已%s（约 10 秒生效）" % ("开启" if enabled else "关闭"))
        return False

    # ------------------------------------------------------------ 状态轮询

    def _tick(self):
        status = load_json(STATUS_PATH) or {}
        if not isinstance(status, dict):
            status = {}
        self._status = status
        running = bool(status.get("running"))

        if running:
            endpoint = status.get("endpoint") or "-"
            age = status.get("handshakeAgeSeconds")
            try:
                age_int = int(age) if age is not None else -1
            except (TypeError, ValueError):
                age_int = -1
            if age_int < 0:
                handshake = "从未握手"
            else:
                handshake = human_age(age_int)
            probe = status.get("probe") or {}
            probe_state = probe.get("state") or ""
            if probe.get("enabled") is False:
                handshake += " · 探针已关闭"
            elif probe_state == "suspect":
                handshake += " · 探针疑似故障（%s 次无回包）" % (probe.get("failures") or "?")
            elif probe_state == "idle":
                handshake += " · 探针空闲档（%ss）" % (probe.get("intervalSeconds") or "?")
            elif probe_state == "active":
                handshake += " · 探针活跃档（%ss）" % (probe.get("intervalSeconds") or "?")
            # 隧道起来了但还没握手 = 实际没网，用橙色提示，别让用户以为连上了
            if age_int < 0 or age_int > 120:
                self._set_state_label("● 已连接（未握手）", "busy")
                if not self._warned_no_handshake:
                    self._warned_no_handshake = True
                    self._log("已连接但还没有握手：端点可能不通，稍等会自动尝试备用端点（见下方日志）")
            else:
                self._set_state_label("● 已连接", "ok")
                self._warned_no_handshake = False
            backend = status.get("backend") or ""
            self._lbl_backend.set_text(
                {"kernel": "内核隧道", "userspace": "用户态隧道"}.get(backend, backend or "-")
            )
            self._lbl_endpoint.set_text(endpoint)
            self._lbl_handshake.set_text(handshake)
            self._lbl_traffic.set_text(
                "%s / %s" % (human_bytes(status.get("rxBytes")), human_bytes(status.get("txBytes")))
            )
            addrs = status.get("addresses") or []
            self._lbl_addrs.set_text("\n".join(addrs) if addrs else "-")
            if status.get("profile"):
                self._lbl_profile_name.set_text(status.get("profile"))

            # 守护进程的关键事件（握手成功/端点回退/发现失败…）直接显示在日志区
            events = status.get("events") or []
            if events:
                if self._last_event != events[-1]:
                    if self._last_event in events:
                        new_events = events[events.index(self._last_event) + 1:]
                    else:
                        new_events = events[-3:]
                    for item in new_events:
                        self._log("· " + item)
                    self._last_event = events[-1]

            if self._last_endpoint and endpoint != self._last_endpoint:
                self._log("端点已变化：%s → %s" % (self._last_endpoint, endpoint))
            self._last_endpoint = endpoint

            now = time.time()
            if age_int >= HANDSHAKE_WARN_SECONDS and (now - self._last_warn) > 60:
                self._last_warn = now
                self._log("警告：已 %s 没有握手，正在等待重连或自动轮换端点" % human_age(age_int))

            disc = status.get("discovery") or {}
            if disc.get("v4") or disc.get("v6"):
                self._lbl_v4.set_text("IPv4 候选：%s" % (disc.get("v4") or "-"))
                self._lbl_v6.set_text("IPv6 候选：%s" % (disc.get("v6") or "-"))
                self._btn_use_v4.set_sensitive(bool(disc.get("v4")))
                self._btn_use_v6.set_sensitive(bool(disc.get("v6")))
                tip = "自动跟随：%s" % ("开" if disc.get("autoFollow") else "关")
                if disc.get("lastErr"):
                    tip += "    最近错误：" + str(disc.get("lastErr"))[:120]
                self._lbl_disc.set_text(tip)
        else:
            self._set_state_label("● 未连接", "off")
            self._warned_no_handshake = False
            self._last_event = ""
            self._lbl_backend.set_text("-")
            self._lbl_endpoint.set_text("-")
            self._lbl_handshake.set_text("从未")
            self._lbl_traffic.set_text("-")
            self._lbl_addrs.set_text("-")
            if self._last_endpoint:
                self._last_endpoint = ""
            if self._current_path:
                data = load_json(self._current_path) or {}
                self._lbl_profile_name.set_text(data.get("name") or "-")

        running_profile = status.get("profile") if running else ""
        if running_profile != self._last_running_profile:
            self._last_running_profile = running_profile
            self._refresh_profile_states()
        self._update_proxy_row(running)
        self._update_profile_widgets()
        return True

    def _set_state_label(self, text, kind):
        color = {"ok": "#2e7d32", "off": "#b71c1c", "busy": "#ef6c00"}.get(kind, "#333333")
        self._lbl_state.set_markup("<span foreground='%s'>%s</span>" % (color, GLib.markup_escape_text(text)))

    # ------------------------------------------------------------ 日志

    def _log(self, message):
        stamp = time.strftime("%H:%M:%S")
        line = "[%s] %s\n" % (stamp, message)
        end = self._log_buffer.get_end_iter()
        self._log_buffer.insert(end, line)
        lines = self._log_buffer.get_line_count()
        if lines > LOG_MAX_LINES:
            start = self._log_buffer.get_start_iter()
            cut = self._log_buffer.get_iter_at_line(lines - LOG_MAX_LINES)
            self._log_buffer.delete(start, cut)
        mark = self._log_buffer.create_mark(None, self._log_buffer.get_end_iter(), False)
        self._log_view.scroll_mark_onscreen(mark)
        self._log_buffer.delete_mark(mark)

    # ------------------------------------------------------------ 异步执行

    def _run_async(self, argv, on_done):
        def worker():
            rc, out, err = run_cmd(argv, timeout=120)
            GLib.idle_add(on_done, rc, out, err)

        thread = threading.Thread(target=worker, daemon=True)
        thread.start()

    # ------------------------------------------------------------ 退出

    def _on_destroy(self, _widget):
        if self._tick_id is not None:
            GLib.source_remove(self._tick_id)
            self._tick_id = None
        Gtk.main_quit()

    def confirm_quit_if_running(self):
        """窗口关闭前：隧道还在跑就询问。返回 True 表示可以退出。"""
        if not bool(self._status.get("running")):
            return True
        dialog = Gtk.MessageDialog(
            parent=self,
            flags=Gtk.DialogFlags.MODAL,
            type=Gtk.MessageType.QUESTION,
            buttons=Gtk.ButtonsType.NONE,
            text="隧道仍在运行，要断开再退出吗？",
        )
        dialog.format_secondary_text("选择「保持运行」会保留 VPN 隧道，后台守护进程继续工作。")
        dialog.add_button("保持运行并退出", Gtk.ResponseType.NO)
        dialog.add_button("断开并退出", Gtk.ResponseType.YES)
        dialog.add_button("取消", Gtk.ResponseType.CANCEL)
        response = dialog.run()
        dialog.destroy()
        if response == Gtk.ResponseType.CANCEL:
            return False
        if response == Gtk.ResponseType.YES:
            rc, out, err = run_cmd([PKEXEC, HELPER, "down"], timeout=120)
            self._log("退出前断开：rc=%d %s" % (rc, out or err))
        return True


# ---------------------------------------------------------------- 入口


def main(argv):
    selftest = "--selftest" in argv
    app = HomingApp()
    app.show_all()
    app._tick()
    if selftest:
        def _finish():
            print("SELFTEST OK")
            app.destroy()
            return False

        GLib.timeout_add(400, _finish)
        Gtk.main()
        return 0

    # 关闭窗口时确认
    app.connect("delete-event", _on_delete_event)
    Gtk.main()
    return 0


def _on_delete_event(window, _event):
    if window.confirm_quit_if_running():
        return False
    return True


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
