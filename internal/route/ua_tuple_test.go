package route

import (
	"testing"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// TestPinnedUATupleMatchesOneShippedBuild pins the UA triple to the CLI build
// captured 2026-11-11 (opencode 1.19.5 + provider-utils 4.0.23 + bun 1.4.0):
// that tuple answers 200 on the free lane, while the previous capture
// (1.19.4 + 4.0.23 + 1.4.4) started drawing 429s the same day. A tuple that
// never shipped together (the old 4.0.40/1.3.14 pair) is a stable non-client
// fingerprint: no real client can ever produce it.
// The runtime pin is ignored on purpose: this test pins the DEFAULT, and
// another test's setPinnedUA must not leak into it.
func TestPinnedUATupleMatchesOneShippedBuild(t *testing.T) {
	prev, _ := pinnedUAVer.Load().(string)
	pinnedUAVer.Store("")
	t.Cleanup(func() { pinnedUAVer.Store(prev) })

	const captured = "opencode/1.19.5 ai-sdk/provider-utils/4.0.23 runtime/bun/1.4.0"
	if got := pinnedUA(); got != captured {
		t.Fatalf("UA не совпадает с захваченным CLI (%q): %q", captured, got)
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
