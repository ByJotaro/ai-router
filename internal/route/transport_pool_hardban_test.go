package route

import (
	"testing"
	"time"
)

// TestHardBansCoverPool pins the short-park gate: a handful of hours-class bans
// must not mute the wait/re-sweep escape (2026-10-21 live: 6 hard bans of 20
// egresses disabled it permanently — every 429 became an instant 503 while 13
// healthy egresses served), while a majority of bans (or a global burn) does.
func TestHardBansCoverPool(t *testing.T) {
	now := time.Now()
	ban := func(p *PoolTransport, n int) {
		p.muMu.Lock()
		defer p.muMu.Unlock()
		if p.bad == nil {
			p.bad = map[string]time.Time{}
			p.hardBad = map[string]bool{}
		}
		for i := range n {
			key := "10.0.0." + string(rune('a'+i))
			p.bad[key] = now.Add(time.Hour)
			p.hardBad[key] = true
		}
	}
	short := &PoolTransport{}
	ban(short, 6)
	if short.hardBansCoverPool(now) {
		t.Fatalf("6 hard bans из 20 не должны глушить короткое ожидание")
	}
	majority := &PoolTransport{}
	ban(majority, poolHardBanMajority)
	if !majority.hardBansCoverPool(now) {
		t.Fatalf("%d hard bans должны глушить ожидание", poolHardBanMajority)
	}
	burn := &PoolTransport{globalBurnUntil: now.Add(time.Hour)}
	if !burn.hardBansCoverPool(now) {
		t.Fatal("активный global burn должен глушить ожидание")
	}
	// Истёкшие баны не считаются.
	expired := &PoolTransport{}
	ban(expired, 6)
	expired.muMu.Lock()
	for k := range expired.bad {
		expired.bad[k] = now.Add(-time.Minute)
	}
	expired.muMu.Unlock()
	if expired.hardBansCoverPool(now) {
		t.Fatal("истёкшие баны не должны считаться покрытием")
	}
}
