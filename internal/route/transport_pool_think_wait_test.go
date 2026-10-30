package route

import (
	"testing"
	"time"
)

// TestPoolThinkWaitLeavesRoomForASecondEgress: live 2026-10-23 19:00-19:30 all
// 30 503s were "pool retry budget exhausted (100s) with no upstream response":
// the first attempt waited the full 90s think timeout, so the second egress —
// which answered in 2s — never got a turn.
func TestPoolThinkWaitLeavesRoomForASecondEgress(t *testing.T) {
	full := 100 * time.Second
	if got := poolThinkWait(full, 8<<10); got != poolSmallThinkWait {
		t.Fatalf("small request: got %v, want %v", got, poolSmallThinkWait)
	}
	if got := poolThinkWait(full, 1<<20); got != poolHeaderThinkTimeout {
		t.Fatalf("large request keeps the long think wait: got %v, want %v", got, poolHeaderThinkTimeout)
	}
	// Unknown length (-1) is treated as large: never shorten what we cannot size.
	if got := poolThinkWait(full, -1); got != poolHeaderThinkTimeout {
		t.Fatalf("unknown length: got %v, want %v", got, poolHeaderThinkTimeout)
	}
	// Budget-aware: the wait never exceeds what is left, so the attempt loop
	// still ends inside poolTryDeadline.
	if got := poolThinkWait(20*time.Second, 1<<20); got != 20*time.Second {
		t.Fatalf("remaining budget must cap the wait: got %v", got)
	}
	// A small request with half the budget left gets the smaller of the two.
	if got := poolThinkWait(30*time.Second, 8<<10); got != 30*time.Second {
		t.Fatalf("got %v, want the remaining budget", got)
	}
}
