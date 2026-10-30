package route

import (
	"strings"
	"testing"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// TestPinnedUATupleMatchesOneShippedBuild pins the UA triple to the CLI build
// captured on 2026-10-22 (opencode 1.19.4 + provider-utils 4.0.23 + bun 1.4.4).
// A tuple that never shipped together (the old 4.0.40/1.3.14 pair) is a stable
// non-client fingerprint: no real client can ever produce it.
func TestPinnedUATupleMatchesOneShippedBuild(t *testing.T) {
	ua := pinnedUA()
	if !strings.Contains(ua, "ai-sdk/provider-utils/4.0.23") {
		t.Fatalf("UA не совпадает с захваченным CLI: %q", ua)
	}
	if !strings.Contains(ua, "runtime/bun/1.4.4") {
		t.Fatalf("UA не совпадает с захваченным CLI: %q", ua)
	}
	if !strings.HasPrefix(ua, "opencode/") {
		t.Fatalf("UA без префикса opencode: %q", ua)
	}
}

// TestLeaseUAPinsOpencodeUpstream: OMP's own UA must never reach the free lane.
func TestLeaseUAPinsOpencodeUpstream(t *testing.T) {
	oc := &provider.Provider{Name: "opencode", BaseURL: "https://opencode.ai/inference/v1"}
	other := &provider.Provider{Name: "kilocode", BaseURL: "https://kilocode.example/v1"}
	if got := leaseUA(oc, "1.2.3.4:8120", "ai-sdk/9.9.9 runtime/bun/2.0"); got != pinnedUA() {
		t.Fatalf("opencode-апстрим отправил клиентский UA: %q", got)
	}
	if got := leaseUA(other, "", "kilo/1.0"); got != "kilo/1.0" {
		t.Fatalf("не-opencode UA перетёрт: %q", got)
	}
}
