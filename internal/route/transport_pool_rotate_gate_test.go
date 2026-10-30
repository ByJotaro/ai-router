package route

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// TestRotateWantedRequiresSilence is the guard the live log asked for: the
// shared-burn latch also arms for a single hours-class Retry-After on ONE
// egress (2026-10-23 04:37:13: ra=80568 on 2a09:bac1:76c0:518::b:2c6) while the
// pool keeps serving. Recycling WARP identities on that arm re-registers every
// egress and dropped the data path with EOF 41 s later.
func TestRotateWantedRequiresSilence(t *testing.T) {
	now := time.Now()
	pt := &PoolTransport{}
	blockBackgroundWorkers(pt)
	armGlobalBurn(pt, time.Hour)

	// Latch armed, pool served a second ago -> not a pool-wide burn.
	pt.lastPoolOK.Store(now.Add(-time.Second).UnixNano())
	if pt.rotateWanted(now) {
		t.Fatal("rotate wants a SILENT pool: a live 200 must veto the latch")
	}

	// Latch armed, pool silent -> the rotate is the only exit that mints
	// fresh identities (2026-10-22: 14/14 IPs replaced, traffic resumed).
	silent := &PoolTransport{}
	blockBackgroundWorkers(silent)
	armGlobalBurn(silent, time.Hour)
	silent.lastPoolOK.Store(now.Add(-10 * time.Minute).UnixNano())
	if !silent.rotateWanted(now) {
		t.Fatal("armed latch + silent pool must rotate")
	}

	// No latch at all, but over half the egresses banned -> rotate.
	banned := &PoolTransport{}
	blockBackgroundWorkers(banned)
	for i := 0; i < 4; i++ {
		banned.markBad429(string(rune('a'+i))+":x", time.Hour, now)
	}
	banned.markBad429("e:x", time.Hour, now)
	if !banned.rotateWanted(now) {
		t.Fatal("majority-banned pool must rotate even without the latch")
	}
}

// TestRefusedLeaseDoesNotRotate: a lease refused for lack of genuine session
// ids is a SUPPLY problem. Rotating re-registers WARP and mints new egresses
// that need ids nobody has — the shortage gets worse, and the pool churns
// identities for nothing.
func TestRefusedLeaseDoesNotRotate(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	api := &fakePoolAPI{leases: []string{up.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	resetSessionPool()
	t.Cleanup(resetSessionPool)
	getSessionPool().Fetch = func(string, time.Duration, time.Time) []sessionEntry { return nil }

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "opencode", Type: "openai-compatible", BaseURL: up.URL, AuthMode: "none"}

	_, err := pt.Acquire(context.Background(), RequestContext{Provider: "opencode"}, prov)
	if err == nil || !strings.Contains(err.Error(), "no unique session") {
		t.Fatalf("err = %v, want a session-supply refusal", err)
	}
	time.Sleep(300 * time.Millisecond)
	api.mu.Lock()
	n := api.rotateN
	api.mu.Unlock()
	if n != 0 {
		t.Fatalf("rotateN=%d: отказ по сессиям не должен крутить WARP", n)
	}
}

// TestRotateWantedMajorityBanNeedsStall: a majority-banned pool that still
// answers 200 must NOT be cycled. Live 2026-10-23 17:55-18:10: banned_share
// 0.55 while 51/57 requests were 200, yet two majority-ban rotates ran and
// their "pool cycled" windows produced ALL six 503s of that interval. Rotating
// is a capacity move (fresh /48 = fresh daily quota) and costs 30-90 s of
// blackout, so it needs evidence of a stall, not just a ban count.
func TestRotateWantedMajorityBanNeedsStall(t *testing.T) {
	now := time.Now()
	mk := func(banned, total int) *PoolTransport {
		pt := &PoolTransport{}
		blockBackgroundWorkers(pt)
		pt.ipOK = map[string]int{}
		for i := 0; i < total; i++ {
			pt.ipOK[fmt.Sprintf("2a09:bac5:%x::1", 100+i)] = 1
		}
		for i := 0; i < banned; i++ {
			pt.markBad429(fmt.Sprintf("2a09:bac5:%x::1", 100+i), time.Hour, now)
		}
		return pt
	}

	// 55% banned, a 200 one second ago -> stay put and keep serving.
	serving := mk(11, 20)
	serving.lastPoolOK.Store(now.Add(-time.Second).UnixNano())
	if serving.rotateWanted(now) {
		t.Fatalf("majority-ban rotate fired while the pool served 200 one second ago (share=%.2f)", serving.bannedShare())
	}

	// Same bans, but nothing has served for three minutes -> rotate.
	stalled := mk(11, 20)
	stalled.lastPoolOK.Store(now.Add(-3 * time.Minute).UnixNano())
	if !stalled.rotateWanted(now) {
		t.Fatal("a majority-banned pool with a three-minute stall must rotate")
	}

	// EVERY known key banned and yet a 200 one second ago: still no rotate.
	// Live 2026-10-23 18:19:40: banned_share=1 since_pool_ok=6s rotated anyway
	// and the blackout answered 503 while the pool was serving.
	burnedButServing := mk(20, 20)
	burnedButServing.lastPoolOK.Store(now.Add(-time.Second).UnixNano())
	if burnedButServing.rotateWanted(now) {
		t.Fatalf("share=%.2f with a live 200 must NOT rotate: capacity is not the problem", burnedButServing.bannedShare())
	}

	// ... and the same fully-banned pool after the stall window does rotate.
	burnedStalled := mk(20, 20)
	burnedStalled.lastPoolOK.Store(now.Add(-2 * time.Minute).UnixNano())
	if !burnedStalled.rotateWanted(now) {
		t.Fatal("a fully-banned pool silent for two minutes must rotate")
	}
}
