package route

import (
	"testing"
	"time"
)

// One escalation per cooldown: a rebuild kills every wide worker, so a second
// trigger while the first fleet is still coming up would trade live lanes for
// nothing.
func TestRebuildDue(t *testing.T) {
	now := time.Now().Unix()
	if !rebuildDue(0, now) {
		t.Fatal("first trigger must be allowed")
	}
	if !rebuildDue(now-int64(PoolRebuildCooldown/time.Second), now) {
		t.Fatal("trigger at the cooldown edge must be allowed")
	}
	if rebuildDue(now-int64(PoolRebuildCooldown/time.Second)+1, now) {
		t.Fatal("second trigger inside the cooldown must be refused")
	}
}

func TestMaybeEscalateDisabledByEnv(t *testing.T) {
	resetPoolRebuildTimer()
	t.Setenv("AI_ROUTER_POOL_REBUILD", "off")
	maybeEscalatePoolRebuild("test")
	if poolRebuildAt.Load() != 0 {
		t.Fatal("escalation must not arm when disabled")
	}
}
