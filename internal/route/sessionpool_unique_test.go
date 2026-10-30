package route

import (
	"testing"
	"time"
)

// TestNextForNeverSharesSessionAcrossEgresses pins the rotation contract the
// free tier makes mandatory: every egress carries its OWN session id, so two
// IPs can never be linked by a common identity (a ban on one would follow the
// other). The previous selector hashed the egress key into the fresh window,
// so two keys could collide on one id; the reservation map makes that
// impossible while the harvest is wide enough.
func TestNextForNeverSharesSessionAcrossEgresses(t *testing.T) {
	sp, _ := clockSessionPool(t, sesA, sesB,
		"ses_ccccccccccccccccccccccccc",
		"ses_ddddddddddddddddddddddddd")
	egresses := []string{"2a09:bac5:a6f2:292d::1", "2a09:bac5:a6f3:1973::2",
		"2a09:bac5:a6f4:6e::3", "2a09:bac5:a6f7:1973::4"}
	seen := map[string]string{}
	for _, ekey := range egresses {
		id := sp.NextFor(ekey)
		if id == "" {
			t.Fatalf("NextFor(%q) = \"\": пул не отдал сессию при 4 живых id", ekey)
		}
		if prev, dup := seen[id]; dup && prev != ekey {
			t.Fatalf("сессия %s выдана ДВУМ egress: %q и %q — идентичности связаны", sesPrefix(id), prev, ekey)
		}
		seen[id] = ekey
	}
	if len(seen) != len(egresses) {
		t.Fatalf("уникальных сессий %d из %d egress", len(seen), len(egresses))
	}
	// Stability: the same egress keeps its own id on every later call.
	for _, ekey := range egresses {
		if got := sp.NextFor(ekey); got != seen[got] && got == "" {
			t.Fatalf("NextFor(%q) потерял привязку", ekey)
		}
		if got := sp.NextFor(ekey); seen[got] != ekey {
			t.Fatalf("NextFor(%q) вернул %s, закреплённую за %q", ekey, sesPrefix(got), seen[got])
		}
	}
}

// TestNextForRepinsAfterBurn: once the pinned id dies, that egress must get a
// DIFFERENT live id (never the burned one, never another egress' id).
func TestNextForRepinsAfterBurn(t *testing.T) {
	sp, clock := clockSessionPool(t, sesA, sesB, "ses_ccccccccccccccccccccccccc")
	ekey := "2a09:bac5:a6f2:292d::1"
	first := sp.NextFor(ekey)
	if first == "" {
		t.Fatal("пустое пиннинг-значение")
	}
	*clock = clock.Add(time.Second)
	burn(sp, first)
	next := sp.NextFor(ekey)
	if next == "" {
		t.Fatal("NextFor после сжигания = \"\": egress остался без сессии")
	}
	if next == first {
		t.Fatalf("egress пере-прибит к сожжённой сессии %s", sesPrefix(first))
	}
	other := sp.NextFor("2a09:bac5:a6f3:1973::2")
	if other == next {
		t.Fatalf("после пере-пиннинга сессия %s роздана двум egress", sesPrefix(next))
	}
}

// TestNextForRefusesSharingWhenHarvestNarrow replaces the old "sharing is the
// last resort" contract: sharing one id across two egresses is the cross-IP
// link the operator forbids, so the second egress must get NOTHING and the
// caller refuses that egress (an empty value used to become a fabricated
// session, which the gate answers 403 to on every egress — a worse outcome).
func TestNextForRefusesSharingWhenHarvestNarrow(t *testing.T) {
	sp, _ := clockSessionPool(t, sesA)
	first := sp.NextFor("egress-b")
	if first == "" {
		t.Fatal("первый egress остался без сессии при живой поставке")
	}
	if second := sp.NextFor("egress-a"); second != "" {
		t.Fatalf("второму ЖИВОМУ egress выдан id %s — это связка IP", sesPrefix(second))
	}
	// Same egress asks again: still its own id (stability until death/ban).
	if again := sp.NextFor("egress-b"); again != first {
		t.Fatalf("сессия живого egress сменилась: %q -> %q", first, again)
	}
}

// TestNextForUniqueDespiteHashCollision is the red-proof of the reservation
// map: these two egress keys collide under the old FNV-1a hash mod 2 (both
// hash to slot 0), so the hash selector handed them the SAME session id even
// though two fresh ids were available — the exact cross-IP link this pool must
// never create.
func TestNextForUniqueDespiteHashCollision(t *testing.T) {
	sp, _ := clockSessionPool(t, sesA, sesB)
	a := sp.NextFor("2a09:bac5:a6f2:292d::0")
	b := sp.NextFor("2a09:bac5:a6f2:292d::2")
	if a == "" || b == "" {
		t.Fatalf("пустые значения при двух живых id: %q / %q", a, b)
	}
	if a == b {
		t.Fatalf("коллизия хеша выдала одну сессию %s двум egress — IP связаны", sesPrefix(a))
	}
}

// TestReleaseEgressRotatesSession: a burned IP identity must come back with a
// DIFFERENT session (the banned identity's id must not follow it), while the
// freed id stays available to other egresses.
func TestReleaseEgressRotatesSession(t *testing.T) {
	sp, _ := clockSessionPool(t, sesA, sesB, "ses_ccccccccccccccccccccccccc")
	ekey := "2a09:bac5:a6f2:292d::1"
	first := sp.NextFor(ekey)
	if first == "" {
		t.Fatal("пустой пиннинг")
	}
	sp.ReleaseEgress(ekey)
	after := sp.NextFor(ekey)
	if after == "" {
		t.Fatal("после ReleaseEgress сессия не выдана")
	}
	if after == first {
		t.Fatalf("забаненный egress вернулся с той же сессией %s", sesPrefix(first))
	}
	other := sp.NextFor("2a09:bac5:a6f3:1973::9")
	if other == "" {
		t.Fatal("второй egress без сессии")
	}
	if other == after {
		t.Fatalf("сессия %s роздана двум egress после релиза", sesPrefix(other))
	}
}

// TestRetiredSessionNotReusedAcrossEgresses: when an egress loses its session
// (banned identity), that id must not surface on ANOTHER egress right away —
// one id seen from two IPs minutes apart is the cross-identity link the free
// tier bans on. After the cooldown the id may roam again (a real client
// changing networks looks exactly like that).
func TestRetiredSessionNotReusedAcrossEgresses(t *testing.T) {
	sp, clock := clockSessionPool(t, sesA, sesB)
	a := "2a09:bac5:a6f2:292d::0"
	b := "2a09:bac5:a6f2:292d::2"
	first := sp.NextFor(a)
	sp.ReleaseEgress(a)
	// Immediately: egress B must take the OTHER id, not the retired one.
	second := sp.NextFor(b)
	if second == first {
		t.Fatalf("retired id %s сразу выдано другому egress", sesPrefix(first))
	}
	// After the cooldown the retired id is allowed to roam again.
	*clock = clock.Add(sessionReuseCooldown + time.Minute)
	c := "2a09:bac5:a6f3:1973::3"
	third := sp.NextFor(c)
	if third == "" {
		t.Fatal("после кулдауна сессия не выдана")
	}
	if third != first && third != second {
		t.Fatalf("неожиданный id %s", sesPrefix(third))
	}
}
