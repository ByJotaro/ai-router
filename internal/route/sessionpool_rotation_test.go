package route

import (
	"testing"
	"time"
)

// clockSessionPool seeds a pool whose clock the test drives explicitly, so TTLs
// (dead-map expiry) are testable without sleeping.
func clockSessionPool(t *testing.T, ids ...string) (*SessionPool, *time.Time) {
	t.Helper()
	resetSessionPool()
	t.Cleanup(resetSessionPool)
	sp := getSessionPool()
	now := time.Now()
	clock := &now
	sp.Now = func() time.Time { return *clock }
	sp.Fetch = func(string, time.Duration, time.Time) []sessionEntry {
		out := make([]sessionEntry, 0, len(ids))
		for i, id := range ids {
			out = append(out, sessionEntry{id: id, updated: now.Add(-time.Duration(i+1) * time.Minute).UnixMilli()})
		}
		return out
	}
	return sp, clock
}

// burn burns one session the way the live gate does: two consecutive gate-403s
// on the same egress inside gateStreakTTL.
func burn(sp *SessionPool, id string) {
	sp.MarkGateHit(id, "egress-1", false)
	sp.MarkGateHit(id, "egress-1", false)
}

const (
	sesA = "ses_aaaaaaaaaaaaaaaaaaaaaaaaa"
	sesB = "ses_bbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestSessionPoolRotatesWhenAllBurned is the "restart-only" regression: with
// every harvested session burned, Next() returned "" and the router fell back
// to a generated session — a guaranteed gate-403, i.e. the pool stayed dead
// until the process restarted (which clears `dead`). The pool must instead keep
// rotating: a half-open probe of the longest-burned id.
func TestSessionPoolRotatesWhenAllBurned(t *testing.T) {
	sp, _ := clockSessionPool(t, sesA, sesB)
	if got := sp.Next(); got == "" {
		t.Fatal("пул пуст сразу после посева")
	}
	burn(sp, sesA)
	burn(sp, sesB)
	if n := sp.LiveCount(); n != 0 {
		t.Fatalf("LiveCount = %d, want 0 после сжигания обеих", n)
	}
	got := sp.Next()
	if got == "" {
		t.Fatal("Next() = \"\" при всех сожжённых: ротация умирает до рестарта процесса")
	}
	if got != sesA && got != sesB {
		t.Fatalf("Next() = %q, want одну из сожжённых сессий (half-open проба)", got)
	}
	// Проба, вернувшая 200, оживляет id — и он снова полноценный.
	sp.MarkGateHit(got, "egress-1", true)
	if n := sp.LiveCount(); n != 1 {
		t.Fatalf("LiveCount = %d после 200 на пробе, want 1", n)
	}
}

// TestSessionPoolDeadExpiresAfterTTL proves the dead set is TTL-bounded: a
// session burned by a transient gate verdict must come back on its own instead
// of waiting for a process restart.
func TestSessionPoolDeadExpiresAfterTTL(t *testing.T) {
	sp, clock := clockSessionPool(t, sesA)
	sp.Next()
	burn(sp, sesA)
	if n := sp.LiveCount(); n != 0 {
		t.Fatalf("LiveCount = %d, want 0 сразу после сжигания", n)
	}
	*clock = clock.Add(sessionDeadTTL + time.Second)
	if n := sp.LiveCount(); n != 1 {
		t.Fatalf("LiveCount = %d через sessionDeadTTL, want 1 (смерть должна истекать)", n)
	}
	if got := sp.Next(); got != sesA {
		t.Fatalf("Next() = %q, want %q после истечения TTL", got, sesA)
	}
}

// TestSessionPoolResurrectPicksLongestBurned: when everything is burned the
// resurrection order must be oldest-death-first, so the probe walks the whole
// set instead of hammering the freshest casualty.
func TestSessionPoolResurrectPicksLongestBurned(t *testing.T) {
	sp, clock := clockSessionPool(t, sesA, sesB)
	sp.Next()
	burn(sp, sesA)
	*clock = clock.Add(time.Minute)
	burn(sp, sesB)
	if got := sp.Next(); got != sesA {
		t.Fatalf("первая проба = %q, want %q (сожжена раньше)", got, sesA)
	}
}

// Live 2026-10-23: the pool churns egresses (21 WARP rotates, 320 distinct IPs
// in 7 h) while the harvest holds 40 ids, so pins for rotated-away slots ate
// the supply and every fresh ekey was refused: 6324 x 503 "no free session —
// egress left without identity". A pin must be freed by the POOL's liveness
// signal, not only by 20 min of idleness on that id.
func TestSessionPoolReclaimsUnseenEgressPin(t *testing.T) {
	now := time.Date(2026, 10, 23, 17, 30, 0, 0, time.UTC)
	clock := now
	p := &SessionPool{
		MaxAge: 72 * time.Hour,
		Now:    func() time.Time { return clock },
		Fetch: func(string, time.Duration, time.Time) []sessionEntry {
			return []sessionEntry{
				{id: "ses_aaaa1111", updated: clock.UnixMilli()},
				{id: "ses_bbbb2222", updated: clock.UnixMilli()},
			}
		},
	}
	// Both ids pinned to egresses the pool handed out long ago.
	if a, b := p.NextFor("key-a"), p.NextFor("key-b"); a == "" || b == "" || a == b {
		t.Fatalf("setup pins: %q %q", a, b)
	}
	// The pool last handed them out 10 minutes ago (rotated away since).
	p.NoteEgressSeen("key-a")
	p.NoteEgressSeen("key-b")
	// 15 min: past the 5-min liveness TTL AND the 10-min cooldown-from-last-use
	// (a reclaimed id must not land on a second IP sooner than the uniqueness
	// rule allows).
	clock = now.Add(15 * time.Minute)
	// A brand-new egress must get an identity by reclaiming a dead pin, not be
	// refused (the old code returned "" here -> the caller answers 503).
	if id := p.NextFor("key-c"); id == "" {
		t.Fatal("fresh egress refused although every pin belongs to an egress the pool stopped handing out")
	}
}

// The complement: an egress the pool is still handing out keeps its pin. A live
// IP losing its identity would put a SECOND id on one address.
func TestSessionPoolKeepsPinOfSeenEgress(t *testing.T) {
	now := time.Date(2026, 10, 23, 17, 30, 0, 0, time.UTC)
	clock := now
	p := &SessionPool{
		MaxAge: 72 * time.Hour,
		Now:    func() time.Time { return clock },
		Fetch: func(string, time.Duration, time.Time) []sessionEntry {
			return []sessionEntry{{id: "ses_cccc3333", updated: clock.UnixMilli()}}
		},
	}
	if id := p.NextFor("key-a"); id == "" {
		t.Fatal("setup pin")
	}
	// The pool keeps handing this egress out: refresh its liveness every minute.
	for i := 0; i < 12; i++ {
		clock = clock.Add(time.Minute)
		p.NoteEgressSeen("key-a")
	}
	if id := p.NextFor("key-b"); id != "" {
		t.Fatalf("a live egress lost its pin to a stranger: %q", id)
	}
}

// TestSessionPoolReclaimRespectsLastUseCooldown: the unseen reclaim frees a pin
// after 5 minutes, but the id belongs to the uniqueness rule for 10 minutes
// from its LAST USE. Handing it to a second IP at minute 6 would link the two
// addresses three times sooner than the ban cooldown allows — compress the
// session onto two IPs inside one upstream window.
func TestSessionPoolReclaimRespectsLastUseCooldown(t *testing.T) {
	now := time.Date(2026, 10, 23, 19, 0, 0, 0, time.UTC)
	clock := now
	p := &SessionPool{
		MaxAge: 72 * time.Hour,
		Now:    func() time.Time { return clock },
		Fetch: func(string, time.Duration, time.Time) []sessionEntry {
			return []sessionEntry{{id: "ses_dddd4444", updated: clock.UnixMilli()}}
		},
	}
	if id := p.NextFor("key-a"); id == "" {
		t.Fatal("setup pin")
	}
	p.NoteEgressSeen("key-a")
	clock = now.Add(6 * time.Minute) // past the seen TTL, inside the cooldown
	if id := p.NextFor("key-b"); id != "" {
		t.Fatalf("id reused on a second egress 6 min after its last use: %q", id)
	}
	clock = now.Add(11 * time.Minute) // now outside the cooldown
	if id := p.NextFor("key-b"); id == "" {
		t.Fatal("id must become reusable after the cooldown-from-last-use")
	}
}
