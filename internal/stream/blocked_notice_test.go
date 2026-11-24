package stream

import (
	"strings"
	"testing"
)

// A turn whose only call names an undeclared tool must tell the client which
// tool was refused: an all-blocked turn used to reach OMP as finish=tool_calls
// with zero calls ("Assistant returned empty stop after retry cap", measured
// 2026-11-20 with scout agents asked to run Playwright).
func TestBlockedNotice(t *testing.T) {
	got := blockedNotice(map[string]bool{"bash": true}, map[string]bool{"read": true, "write": true})
	if !strings.Contains(got, "bash") || !strings.Contains(got, "read") {
		t.Fatalf("notice must name the refused and the available tools: %q", got)
	}
	if blockedNotice(nil, map[string]bool{"read": true}) != "" {
		t.Fatal("no blocked calls must produce no notice")
	}
}
