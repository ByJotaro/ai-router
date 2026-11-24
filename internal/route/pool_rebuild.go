package route

import (
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// poolRebuildAt is the last successful escalation, unix seconds. One rebuild
// per cooldown: a rebuild kills every wide worker and re-mints tunnels, so a
// second call while the first fleet is still coming up would trade live lanes
// for nothing.
var poolRebuildAt atomic.Int64

// PoolRebuildScript is the fleet rebuild entry point (wide_start.ps1 -Force).
// Exported only so the daemon can point it at another path when the checkout
// moves; the default matches the live host.
var PoolRebuildScript = `D:\omp_watch\vpnpool\wide_start.ps1`

// PoolRebuildCooldown is how long the router waits after one escalation before
// it may trigger another.
const PoolRebuildCooldown = 10 * time.Minute

// rebuildDue reports whether an escalation may run now. Pure so the window can
// be tested without spawning anything.
func rebuildDue(last, now int64) bool {
	if last <= 0 {
		return true
	}
	return now-last >= int64(PoolRebuildCooldown/time.Second)
}

// maybeEscalatePoolRebuild turns "every lane answered 429, there is nothing
// left to swap" into the one action that actually restores capacity: a WARP
// tunnel rebuild, i.e. fresh egress addresses with a fresh free-tier
// allowance. Before it existed the only cure was a manual warp-router restart
// (measured repeatedly on 2026-11-20: a rebuild restored 200s within a minute,
// while session/egress swaps alone could not).
func maybeEscalatePoolRebuild(reason string) {
	if os.Getenv("AI_ROUTER_POOL_REBUILD") == "off" {
		return
	}
	now := time.Now().Unix()
	// No wall suppression: a shared day boundary is per-address allowance
	// evidence, not a fleet closure (measured 2026-11-20 21:30: 200s kept
	// flowing while three burned /64s named 00:00Z).
	//
	// Nor may a fleet with a working lane be rebuilt: the burn is per slot's
	// address, and killing 45 workers because one request exhausted its sweep
	// trades a working fleet for a 1-2 min hole (measured: 21 lanes with
	// successes, two workhorses at 101/58 OK while 14 slots were parked).
	if ok := lastLaneSuccessUnix.Load(); ok > 0 && now-ok < int64(PoolRebuildCooldown/time.Second) {
		slog.Warn("429 past every swap, but lanes are still serving: no fleet rebuild", "reason", reason, "last_ok_s_ago", now-ok)
		return
	}
	last := poolRebuildAt.Load()
	if !rebuildDue(last, now) {
		return
	}
	if !poolRebuildAt.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("429 past every swap: rebuilding the egress fleet", "reason", reason, "cooldown_ms", PoolRebuildCooldown.Milliseconds())
	go func() {
		if err := spawnPoolRebuild(PoolRebuildScript); err != nil {
			slog.Warn("egress fleet rebuild failed to start", "err", err)
		}
	}()
}

// resetPoolRebuildTimer is used by tests to start from a clean window.
func resetPoolRebuildTimer() { poolRebuildAt.Store(0) }
