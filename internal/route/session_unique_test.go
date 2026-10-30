package route

import (
	"fmt"
	"testing"
	"time"
)

// TestSessionPoolNeverSharesAcrossEgresses is the operator's hard rule: one id
// per egress, never the same id on two live egresses. When supply runs out the
// newcomer gets NO identity (""), and the caller refuses that egress — sharing
// links the two IPs and gets both banned as one client.
func TestSessionPoolNeverSharesAcrossEgresses(t *testing.T) {
	now := time.Now()
	clock := &now
	entries := []sessionEntry{
		{id: "ses_onlyaaaaaaaaaaaaaaaaaaaaa", updated: now.Add(-5 * time.Minute).UnixMilli()},
		{id: "ses_onlybbbbbbbbbbbbbbbbbbbbb", updated: now.Add(-6 * time.Minute).UnixMilli()},
	}
	sp := seedPool(t, clock, entries)
	got := map[string]string{}
	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("egress-%d", i)
		id := sp.NextFor(key)
		if id == "" {
			t.Fatalf("egress %s остался без сессии при свободной поставке", key)
		}
		got[key] = id
	}
	if got["egress-0"] == got["egress-1"] {
		t.Fatalf("один id выдан двум egress: %s", sesPrefix(got["egress-0"]))
	}
	for i := 2; i < 5; i++ {
		key := fmt.Sprintf("egress-%d", i)
		if id := sp.NextFor(key); id != "" {
			t.Fatalf("при исчерпании поставки egress %s всё равно получил %s — это связка IP",
				key, sesPrefix(id))
		}
	}
	if n := sp.LiveCount(); n != 2 {
		t.Fatalf("LiveCount=%d, хочу 2", n)
	}
}

// TestSessionPoolReclaimsSilentEgressPin: an egress that stopped calling (warp
// rotated its /64) must not hold an id forever — its pin is reclaimed after
// sessionPinIdle and handed to the newcomer, which keeps supply alive without
// ever giving one id to two LIVE egresses.
func TestSessionPoolReclaimsSilentEgressPin(t *testing.T) {
	now := time.Now()
	clock := &now
	entries := []sessionEntry{{id: "ses_silentaaaaaaaaaaaaaaaaaaa", updated: now.Add(-3 * time.Minute).UnixMilli()}}
	sp := seedPool(t, clock, entries)
	first := sp.NextFor("egress-old")
	if first == "" {
		t.Fatal("первый egress остался без сессии")
	}
	// The newcomer arrives while the old egress is still fresh -> no id.
	if id := sp.NextFor("egress-new"); id != "" {
		t.Fatalf("id выдан второму ЖИВОМУ egress: %s", sesPrefix(id))
	}
	now = now.Add(sessionPinIdle + time.Minute)
	id := sp.NextFor("egress-new")
	if id != first {
		t.Fatalf("молчащий egress не отдал пин: %q != %q", id, first)
	}
}

// TestSessionPoolPinStableWhileLive: on the SAME egress the id must not change
// call to call while the pin is alive — a session that flips per request is
// exactly the rotating-identity fingerprint.
func TestSessionPoolPinStableWhileLive(t *testing.T) {
	now := time.Now()
	clock := &now
	entries := []sessionEntry{
		{id: "ses_stableaaaaaaaaaaaaaaaaaa", updated: now.Add(-2 * time.Minute).UnixMilli()},
		{id: "ses_otherbbbbbbbbbbbbbbbbbbb", updated: now.Add(-3 * time.Minute).UnixMilli()},
	}
	sp := seedPool(t, clock, entries)
	first := sp.NextFor("egress-1")
	for i := 0; i < 5; i++ {
		if id := sp.NextFor("egress-1"); id != first {
			t.Fatalf("сессия сменилась на живом пинe: %q -> %q", first, id)
		}
		now = now.Add(time.Minute)
	}
}

// TestSessionPoolReclaimRespectsCooldown: an id released by a ban must not
// reappear on a different IP inside sessionReuseCooldown even when the new
// egress is desperate — the separation is the only thing that keeps two
// addresses from being provably one client.
func TestSessionPoolReclaimRespectsCooldown(t *testing.T) {
	now := time.Now()
	clock := &now
	entries := []sessionEntry{{id: "ses_burnedaaaaaaaaaaaaaaaaaaa", updated: now.Add(-30 * time.Minute).UnixMilli()}}
	sp := seedPool(t, clock, entries)
	if id := sp.NextFor("egress-a"); id == "" {
		t.Fatal("первый egress без сессии")
	}
	sp.ReleaseEgress("egress-a")   // ban: id retired, now unused
	now = now.Add(5 * time.Minute) // внутри sessionReuseCooldown (10 min)
	if id := sp.NextFor("egress-b"); id != "" {
		t.Fatalf("id после бана ушёл на другой IP внутри кулдауна: %s", sesPrefix(id))
	}
	now = now.Add(sessionReuseCooldown + time.Minute)
	if id := sp.NextFor("egress-b"); id == "" {
		t.Fatal("после кулдауна поставка не восстановилась")
	}
}

// TestSessionPoolPrunesOutlivedState: retiredAt/dead are lifetime maps on a
// process that rotates for weeks. An entry past its window means nothing (the
// cooldown expired, the burn TTL expired), so keeping it only grows memory at
// the ban rate (thousands per storm hour). Pruning must not change rotation:
// the id stays usable once its cooldown is over.
func TestSessionPoolPrunesOutlivedState(t *testing.T) {
	now := time.Now()
	clock := &now
	ids := []sessionEntry{
		{id: "ses_prune00000000000000000001", updated: now.UnixMilli()},
		{id: "ses_prune00000000000000000002", updated: now.UnixMilli()},
	}
	sp := seedPool(t, clock, ids)
	if sp.DeadTTL == 0 {
		sp.DeadTTL = sessionDeadTTL
	}

	// Ban the egress: its id is retired, no reuse inside the cooldown.
	first := sp.NextFor("egress-a")
	if first == "" {
		t.Fatal("no id for egress-a")
	}
	sp.ReleaseEgress("egress-a")
	sp.mu.Lock()
	if _, ok := sp.retiredAt[first]; !ok {
		sp.mu.Unlock()
		t.Fatal("release must retire the id")
	}
	sp.mu.Unlock()

	// Burn the other id.
	sp.mu.Lock()
	sp.dead["ses_prune00000000000000000002"] = now
	sp.mu.Unlock()

	// Move past every window, then touch the pool (nextLocked prunes).
	now = now.Add(sessionPinIdle + time.Minute)
	if id := sp.NextFor("egress-b"); id == "" {
		t.Fatal("egress-b must still get an id after the windows")
	}
	sp.mu.Lock()
	retiredLeft, deadLeft := len(sp.retiredAt), len(sp.dead)
	sp.mu.Unlock()
	if retiredLeft != 0 {
		t.Fatalf("retiredAt не чистится: %d записей после sessionPinIdle", retiredLeft)
	}
	if deadLeft != 0 {
		t.Fatalf("dead не чистится: %d записей после TTL", deadLeft)
	}
}

// TestSessionPoolPinSurvivesHarvestRefresh: the harvest cache is a hint for NEW
// assignments, never a verdict on an id a live egress is already using. A
// transient empty/short refresh used to run the "no alive ids" branch first,
// which REPLACED the pin silently — live 2026-10-23 05:17:19 → 05:18:26: the
// same egress 2a09:bac5:a6f3:f37::184:23e went from ses_e9a0c8b4 to
// ses_e9a198ab with no unpin event (i.e. a new identity on the same IP for no
// reason, the churn this rotation must never do).
func TestSessionPoolPinSurvivesHarvestRefresh(t *testing.T) {
	now := time.Now()
	clock := &now
	ids := []sessionEntry{
		{id: "ses_pin000000000000000000001", updated: now.UnixMilli()},
		{id: "ses_pin000000000000000000002", updated: now.UnixMilli()},
	}
	sp := seedPool(t, clock, ids)
	mine := sp.NextFor("egress-a")
	if mine == "" {
		t.Fatal("no id for egress-a")
	}
	other := sp.NextFor("egress-b")
	if other == "" || other == mine {
		t.Fatalf("second egress must get its own id (got %q, mine %q)", other, mine)
	}

	// Harvest refresh comes back EMPTY (db read truncated / minter rewriting).
	sp.Fetch = func(string, time.Duration, time.Time) []sessionEntry { return nil }
	sp.loadedAt = now.Add(-2 * sessionRefresh)
	if got := sp.NextFor("egress-a"); got != mine {
		t.Fatalf("live egress lost its session on an empty refresh: %q -> %q", mine, got)
	}

	// Harvest comes back with ids only OUTSIDE maxAge (rescue path).
	sp.Fetch = func(string, time.Duration, time.Time) []sessionEntry {
		return []sessionEntry{
			{id: mine, updated: now.Add(-30 * 24 * time.Hour).UnixMilli()},
			{id: other, updated: now.Add(-30 * 24 * time.Hour).UnixMilli()},
		}
	}
	sp.loadedAt = now.Add(-2 * sessionRefresh)
	if got := sp.NextFor("egress-a"); got != mine {
		t.Fatalf("rescue path replaced a live pin: %q -> %q", mine, got)
	}

	// Burned id is the ONLY reason to re-pin.
	sp.mu.Lock()
	sp.dead[mine] = now
	sp.Fetch = func(string, time.Duration, time.Time) []sessionEntry { return ids }
	delete(sp.dead, mine) // expired TTL counts as alive again...
	sp.dead[mine] = now   // ...so burn it again for this assertion
	sp.loadedAt = time.Time{}
	sp.mu.Unlock()
	if got := sp.NextFor("egress-a"); got == mine {
		t.Fatalf("burned id must not stay pinned to the egress: %q", got)
	}
}
