package route

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMain keeps the route package hermetic in two ways:
//
//  1. The optional Bun sidecar (buntransport, enabled unless
//     OPENCODE_TRANSPORT=off) spawns a child process. In a unit test that child
//     outlives the test binary, so `go test` reports "Test I/O incomplete ...
//     WaitDelay expired" even when every test passed, and pool tests silently
//     bypass the transport they are pinning. The switch is forced off here;
//     the sidecar's own tests live in internal/buntransport.
//  2. Session ids are harvested from the local opencode.db. A clean machine
//     (CI) has no such db, so the supply is empty and every lease that needs a
//     session identity fails ("no unique session available for egress"). The
//     seed below gives the process pool synthetic, correctly shaped ids;
//     tests that pin a specific supply still install their own Fetch through
//     seedSessionPool.
func TestMain(m *testing.M) {
	// Unconditional: a shell that exported OPENCODE_TRANSPORT=on would
	// otherwise leak a sidecar child and break the test binary's exit. The
	// sidecar itself is covered in internal/buntransport.
	_ = os.Setenv("OPENCODE_TRANSPORT", "off")
	sessionPoolSeed = func(sp *SessionPool) {
		// A real harvest wins: this seed exists only for machines that have no
		// opencode.db (CI), where the default harvest returns nothing.
		if os.Getenv("AI_ROUTER_SESSION_DB") == "off" {
			return
		}
		if _, err := os.Stat(opencodeDBPath()); err == nil {
			return
		}
		sp.Fetch = func(_ string, _ time.Duration, now time.Time) []sessionEntry {
			// Tests that disable the harvest (AI_ROUTER_SESSION_DB=off) must
			// still see an empty supply.
			if os.Getenv("AI_ROUTER_SESSION_DB") == "off" {
				return nil
			}
			out := make([]sessionEntry, 0, 64)
			for i := range 64 {
				out = append(out, sessionEntry{
					// ses_ + 26 chars = the 30-char shape the free gate
					// validates (see fetchSessionIDs).
					id:      fmt.Sprintf("ses_%026d", i),
					updated: now.Add(-time.Duration(i+1) * time.Second).UnixMilli(),
				})
			}
			return out
		}
	}
	os.Exit(m.Run())
}
