package route

import (
	"fmt"
	"testing"
	"time"
)

// seedPool builds a pool with an explicit Fetch list and a driven clock.
func seedPool(t *testing.T, clock *time.Time, entries []sessionEntry) *SessionPool {
	t.Helper()
	resetSessionPool()
	t.Cleanup(resetSessionPool)
	sp := getSessionPool()
	sp.Now = func() time.Time { return *clock }
	sp.Fetch = func(string, time.Duration, time.Time) []sessionEntry { return entries }
	return sp
}

// TestSessionPoolNextForPrefersFreshWindow pins two rules at once: a fresh id
// is handed out before stale ones, and NO two egresses share an id while the
// harvest has more than one candidate. The old version of this test asserted
// the opposite of the second rule (all 20 egresses got the single fresh id) —
// that sharing is exactly the cross-IP link the free tier must never see.
func TestSessionPoolNextForPrefersFreshWindow(t *testing.T) {
	now := time.Now()
	clock := &now
	fresh := "ses_fresh00000000000000000000"
	entries := []sessionEntry{{id: fresh, updated: now.Add(-6 * time.Minute).UnixMilli()}}
	for i := 0; i < 20; i++ {
		entries = append(entries, sessionEntry{
			id:      fmt.Sprintf("ses_stale%02d0000000000000000000", i),
			updated: now.Add(-time.Duration(3+i) * time.Hour).UnixMilli(),
		})
	}
	sp := seedPool(t, clock, entries)
	got := map[string]string{}
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("egress-%d", i)
		id := sp.NextFor(key)
		if id == "" {
			t.Fatalf("egress %s без сессии", key)
		}
		if prev, dup := got[id]; dup {
			t.Fatalf("сессия %s выдана двум egress: %s и %s", sesPrefix(id), prev, key)
		}
		got[id] = key
	}
	if _, ok := got[fresh]; !ok {
		t.Fatalf("свежий id не выдан ни одному egress: %v", got)
	}
	if len(got) != 20 {
		t.Fatalf("уникальных сессий %d из 20", len(got))
	}
}

// TestSessionPoolProvenLiveOutranksStaleMtime: an id that served a 200 today
// ranks with the fresh set regardless of its TUI mtime — it must be handed out
// (and, being gate-valid, a merely old id may be used too, but never twice).
func TestSessionPoolProvenLiveOutranksStaleMtime(t *testing.T) {
	now := time.Now()
	clock := &now
	fresh := "ses_fresh11111111111111111111"
	proven := "ses_proven2222222222222222222"
	stale := "ses_stale3333333333333333333"
	entries := []sessionEntry{
		{id: fresh, updated: now.Add(-5 * time.Minute).UnixMilli()},
		{id: proven, updated: now.Add(-150 * time.Hour).UnixMilli()},
		{id: stale, updated: now.Add(-30 * time.Hour).UnixMilli()},
	}
	sp := seedPool(t, clock, entries)
	sp.MarkGateHit(proven, "egress-7", true) // 200 proves it live
	seen := map[string]int{}
	for i := 0; i < 3; i++ {
		id := sp.NextFor(fmt.Sprintf("egress-%d", i))
		if id == "" {
			t.Fatalf("egress-%d без сессии", i)
		}
		seen[id]++
	}
	if seen[proven] == 0 {
		t.Fatalf("доказанно живой id не попал в выборку: %v", seen)
	}
	if seen[fresh] == 0 {
		t.Fatalf("свежий id не попал в выборку: %v", seen)
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("id %s выдан %d egress", sesPrefix(id), n)
		}
	}
}

// TestSessionBurnForcesReharvest: a burned id must trigger a fresh db read on
// the next selection — a TUI session minted seconds ago has to enter the pool
// instead of waiting out the 30s cache, otherwise rotation walks stale ids.
func TestSessionBurnForcesReharvest(t *testing.T) {
	now := time.Now()
	clock := &now
	resetSessionPool()
	t.Cleanup(resetSessionPool)
	sp := getSessionPool()
	sp.Now = func() time.Time { return *clock }
	fetches := 0
	sp.Fetch = func(string, time.Duration, time.Time) []sessionEntry {
		fetches++
		return []sessionEntry{{id: sesA, updated: now.Add(-time.Minute).UnixMilli()}}
	}
	sp.NextFor("egress-1")
	before := fetches
	sp.MarkGateHit(sesA, "egress-1", false)
	sp.MarkGateHit(sesA, "egress-1", false) // burn: 2 гейта на ОДНОМ egress
	sp.NextFor("egress-2")
	if fetches == before {
		t.Fatalf("после сжигания не было перечитывания db (fetches=%d)", fetches)
	}
}

// TestSessionPoolStaleRescueBeatsGeneratedSession is the exact live shape of
// 2026-10-22: every harvested id is older than sessionMaxAge, so selection was
// empty and the router handed out a GENERATED session — which the free-tier gate
// rejects on every egress (403 "can only be used from within OpenCode"). An old
// but genuine TUI id must win over a fabricated one.
func TestSessionPoolStaleRescueBeatsGeneratedSession(t *testing.T) {
	now := time.Now()
	clock := &now
	entries := []sessionEntry{
		{id: "ses_old1aaaaaaaaaaaaaaaaaaaaa", updated: now.Add(-116 * time.Hour).UnixMilli()},
		{id: "ses_old2bbbbbbbbbbbbbbbbbbbbb", updated: now.Add(-150 * time.Hour).UnixMilli()},
		{id: "ses_old3ccccccccccccccccccccc", updated: now.Add(-183 * time.Hour).UnixMilli()},
	}
	sp := seedPool(t, clock, entries)
	got := sp.NextFor("egress-1")
	if got == "" {
		t.Fatal("пул отдал пустую сессию — роутер сгенерирует свою и получит гарантированный 403")
	}
	if got != "ses_old1aaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("rescue обязан взять САМЫЙ СВЕЖИЙ просроченный id, получили %q", got)
	}
	if n := sp.LiveCount(); n == 0 {
		t.Fatal("LiveCount=0 при живом rescue-пуле")
	}
}
