package discovery

import (
	"testing"

	"homing/client/internal/profile"
)

func TestParseTXT(t *testing.T) {
	cases := []struct {
		name string
		in   string
		v4   string
		v6   string
		ts   string
		ok   bool
	}{
		{
			name: "网关默认格式（v4+v6）",
			in:   "v1;v4=203.0.113.10:9612;v6=[2001:db8::10]:51820;ts=2026-11-20T05:09:32Z",
			v4:   "203.0.113.10:9612",
			v6:   "[2001:db8::10]:51820",
			ts:   "2026-11-20T05:09:32Z",
			ok:   true,
		},
		{
			name: "只有 v4（IPv4-only 场景）",
			in:   "v1;v4=1.2.3.4:51820;ts=2026-11-20T00:00:00Z",
			v4:   "1.2.3.4:51820",
			ts:   "2026-11-20T00:00:00Z",
			ok:   true,
		},
		{
			name: "大小写不敏感",
			in:   "V1;V4=1.2.3.4:5;V6=[fd00::1]:6",
			v4:   "1.2.3.4:5",
			v6:   "[fd00::1]:6",
			ok:   true,
		},
		{
			name: "无关的 TXT 记录",
			in:   "google-site-verification=abc123",
			ok:   false,
		},
		{
			name: "空记录",
			in:   "   ",
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v4, v6, ts, ok := ParseTXT(c.in)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if v4 != c.v4 {
				t.Errorf("v4 = %q, want %q", v4, c.v4)
			}
			if v6 != c.v6 {
				t.Errorf("v6 = %q, want %q", v6, c.v6)
			}
			if ts != c.ts {
				t.Errorf("ts = %q, want %q", ts, c.ts)
			}
		})
	}
}

func TestChooseAndAlternate(t *testing.T) {
	r := Result{V4: "1.2.3.4:5", V6: "[2001:db8::1]:6"}

	p4 := &profile.Profile{Discovery: profile.Discovery{Preferred: "v4"}}
	if got := Choose(p4, r); got != r.V4 {
		t.Errorf("preferred=v4 -> %q, want %q", got, r.V4)
	}
	p6 := &profile.Profile{Discovery: profile.Discovery{Preferred: "v6"}}
	if got := Choose(p6, r); got != r.V6 {
		t.Errorf("preferred=v6 -> %q, want %q", got, r.V6)
	}
	// 轮换：当前是 v4 时应给出 v6
	if got := Alternate(p4, r, r.V4); got != r.V6 {
		t.Errorf("alternate(v4) -> %q, want %q", got, r.V6)
	}
	if got := Alternate(p6, r, r.V6); got != r.V4 {
		t.Errorf("alternate(v6) -> %q, want %q", got, r.V4)
	}
	// 只有一个候选时不要返回和当前相同的值
	r2 := Result{V4: "1.2.3.4:5"}
	if got := Alternate(p4, r2, r2.V4); got != "" {
		t.Errorf("唯一候选时 alternate = %q, want 空", got)
	}
}

func TestChoosePriority(t *testing.T) {
	r := Result{V4: "1.2.3.4:5", V6: "[2001:db8::1]:6"}
	cases := []struct {
		pref string
		want string
	}{
		{"v6,v4", r.V6},
		{"v4,v6", r.V4},
		{"v4-only", r.V4},
		{"v6-only", r.V6},
	}
	for _, c := range cases {
		p := &profile.Profile{Discovery: profile.Discovery{Preferred: c.pref}}
		if got := Choose(p, r); got != c.want {
			t.Errorf("preferred=%s -> %q, want %q", c.pref, got, c.want)
		}
	}
	// v4-only 不允许把 v6 当轮换候选
	p := &profile.Profile{Discovery: profile.Discovery{Preferred: "v4-only"}}
	if got := Alternate(p, r, r.V4); got != "" {
		t.Errorf("v4-only alternate 应只剩 v4，实际 %q", got)
	}
	// v6-only 不允许把 v4 当轮换候选
	p = &profile.Profile{Discovery: profile.Discovery{Preferred: "v6-only"}}
	if got := Alternate(p, r, r.V6); got != "" {
		t.Errorf("v6-only alternate 应只剩 v6，实际 %q", got)
	}
}
