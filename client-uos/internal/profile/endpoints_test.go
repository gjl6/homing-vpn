package profile

import "testing"

func TestCandidateEndpoints(t *testing.T) {
	p := &Profile{
		Peers: []Peer{{PublicKey: "k", Endpoint: "203.0.113.10:9612"}},
		Endpoints: []string{
			"192.168.1.10:51830",
			"203.0.113.10:9612", // 和主端点重复，应被去重
			"[2001:db8::10]:51820",
			"  ",
		},
	}
	got := p.CandidateEndpoints()
	want := []string{
		"203.0.113.10:9612",
		"192.168.1.10:51830",
		"[2001:db8::10]:51820",
	}
	if len(got) != len(want) {
		t.Fatalf("CandidateEndpoints = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个端点 = %q，期望 %q", i, got[i], want[i])
		}
	}
}
