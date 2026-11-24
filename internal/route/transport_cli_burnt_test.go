package route

import (
	"testing"
	"time"
)

func TestGroup64CanonicalAcrossForms(t *testing.T) {
	a := group64("2a09:bac1:5500::3")
	b := group64("2a09:bac1:5500:0:0:0:0:3")
	if a != b {
		t.Fatalf("same /64 canonicalised differently: %q vs %q", a, b)
	}
	if a != "2a09:bac1:5500::/64" {
		t.Fatalf("unexpected canonical form %q", a)
	}
}

func TestFilterBurntLanesDropsWholeGroup(t *testing.T) {
	now := time.Now().UnixMilli()
	t1 := &CLITransport{burnt: map[string]int64{
		"2a09:bac1:76a0:258::/64": now + 10*60*1000, // burned, future
		"2a09:bac1:5500::/64":     now - 1000,       // expired -> swept out
	}}
	in := []lane{
		{proxy: "http://127.0.0.1:8750", ip: "2a09:bac1:76a0:258::1"},
		{proxy: "http://127.0.0.1:8751", ip: "2a09:bac1:76a0:258::2"},
		{proxy: "http://127.0.0.1:8752", ip: "2a09:bac1:5500::3"},
		{proxy: "http://127.0.0.1:8753", ip: "2a09:bac5:bd08:2e28::4"},
	}
	out := t1.filterBurntLanes(in)
	if len(out) != 2 {
		t.Fatalf("kept %d lanes, want 2", len(out))
	}
	seen := map[string]bool{}
	for _, l := range out {
		if l.ip == "2a09:bac1:76a0:258::1" || l.ip == "2a09:bac1:76a0:258::2" {
			t.Fatalf("burned group leaked: %s", l.ip)
		}
		seen[l.ip] = true
	}
	if !seen["2a09:bac1:5500::3"] || !seen["2a09:bac5:bd08:2e28::4"] {
		t.Fatalf("unburned lanes lost: %v", seen)
	}
	if _, ok := t1.burnt["2a09:bac1:5500::/64"]; ok {
		t.Fatalf("expired burnt entry not swept")
	}
}
