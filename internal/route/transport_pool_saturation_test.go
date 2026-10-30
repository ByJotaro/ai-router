package route

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// P0 fixes for the 2026-10-17 incident (27 minutes of pool saturation):
// every request probed one egress (1x1 fail-fast), got a 429 and returned 503
// forever, because the two internal escapes were unreachable in that mode —
// rotate needed `rateLimitHits >= 3` (impossible when a request probes exactly
// one egress) and the home-IP leg needed `apiDown()` (the pool API was alive
// and answering acquires). This file pins both escapes and, just as
// important, pins that a serving pool never takes them.

// waitRotates polls the fake pool API until it saw want rotate posts: the
// rotate itself is fire-and-forget (the 1-2 min worker respawn must not block
// the request path).
func waitRotates(t *testing.T, api *fakePoolAPI, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		api.mu.Lock()
		n := api.rotateN
		api.mu.Unlock()
		if n >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	api.mu.Lock()
	n := api.rotateN
	api.mu.Unlock()
	t.Fatalf("rotate calls = %d, want >= %d", n, want)
}

func rotateCount(api *fakePoolAPI) int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.rotateN
}

// TestPoolSaturatedTailRotatesAfterStreak pins the rotate escape: the
// client-visible 503 shape stays byte-identical, but the pool is re-armed
// after satRotateAfter consecutive saturated tails, and not more often than
// satRotateCooldown (a rotate respawns workers for 1-2 min; rotating on the
// first tail was measured to leave the pool flapping in "starting").
func TestPoolSaturatedTailRotatesAfterStreak(t *testing.T) {
	api := &fakePoolAPI{}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1}
	rt := &poolRetryRT{t: pt, prov: "x"}

	for i := 1; i < satRotateAfter; i++ {
		err := rt.saturatedError(1, 0)
		var use *UpstreamStatusError
		if !errors.As(err, &use) {
			t.Fatalf("tail %d: want *UpstreamStatusError, got %T (%v)", i, err, err)
		}
		if use.Status != http.StatusServiceUnavailable {
			t.Fatalf("tail %d: status = %d, want 503 (client retries, does not die)", i, use.Status)
		}
		if use.RetryAfter != "" {
			t.Fatalf("tail %d: 503 must carry no Retry-After (OMP escalates it into hour-class blocks), got %q", i, use.RetryAfter)
		}
		if !strings.Contains(use.Error(), "pool saturated, retry shortly") {
			t.Fatalf("tail %d: client-visible text changed: %v", i, use)
		}
		time.Sleep(50 * time.Millisecond)
		if n := rotateCount(api); n != 0 {
			t.Fatalf("tail %d: rotate fired before the streak threshold (%d calls)", i, n)
		}
	}

	rt.saturatedError(1, 0) // the threshold tail
	waitRotates(t, api, 1)

	rt.saturatedError(1, 0)
	rt.saturatedError(1, 0)
	time.Sleep(200 * time.Millisecond)
	if n := rotateCount(api); n != 1 {
		t.Fatalf("rotate calls = %d, want 1: a respawning pool must not be re-rotated inside the cooldown", n)
	}
}

// TestPoolSaturatedStreakResetsOnPoolOK: any 200 served through a pool lease
// means the pool serves again, so the saturation streak must restart.
func TestPoolSaturatedStreakResetsOnPoolOK(t *testing.T) {
	pt := &PoolTransport{}
	if got := pt.noteSaturatedTail(); got != 1 {
		t.Fatalf("first tail streak = %d, want 1", got)
	}
	if got := pt.noteSaturatedTail(); got != 2 {
		t.Fatalf("second tail streak = %d, want 2 (consecutive tails must count)", got)
	}
	pt.noteOK("10.1.0.7", 200)
	if got := pt.noteSaturatedTail(); got != 1 {
		t.Fatalf("streak after a pooled 200 = %d, want 1 (pool serves again)", got)
	}
}

// TestPoolSilentOpensDirectLegOnlyAfterGrace pins the home-IP escape gate
// (tryDirectFallback):
//   - the second key (AllowHomeIPDirect) is mandatory: direct_fallback alone
//     never opens the home leg, whatever the pool state (2026-10-19: unkeyed
//     direct legs burned the home IP into a 403 «abusive traffic» ban);
//   - a fresh transport with no saturation history never opens the home leg;
//   - a saturated tail alone is not enough either: the grace window has not
//     elapsed, and the pool may still be reopening windows;
//   - once the pool has been silent past poolEmptyGrace, the tail serves the
//     request from the home IP (exactly what saved the 2026-10-17 outage:
//     home answered 200 while every pooled egress was burned);
//   - a pooled 200 starts the grace window over, so the home window is not
//     burned while the pool works.
func TestPoolSilentOpensDirectLegOnlyAfterGrace(t *testing.T) {
	directHits := 0
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHits++
		_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer direct.Close()

	pt := &PoolTransport{APIURL: "http://127.0.0.1:1", DirectFallback: true, AllowHomeIPDirect: true}
	rt := &poolRetryRT{t: pt, prov: "x"}
	body := []byte(`{}`)
	req := func() *http.Request {
		r, _ := http.NewRequest(http.MethodPost, direct.URL+"/v1/chat/completions", strings.NewReader(string(body)))
		return r
	}
	// Same predicate the pool path uses: 429/5xx are retryable, 200 is not.
	retryable := func(code int) bool { return code == http.StatusTooManyRequests || code >= 500 }
	try := func() *http.Response {
		return rt.tryDirectFallback(req(), body, retryable, time.Now().Add(10*time.Second), 1)
	}

	// 0) direct_fallback without allow_home_ip_direct: closed even past grace.
	noKey := &PoolTransport{APIURL: "http://127.0.0.1:1", DirectFallback: true}
	noKey.lastPoolOK.Store(time.Now().Add(-time.Minute).UnixNano())
	if resp := (&poolRetryRT{t: noKey, prov: "x"}).tryDirectFallback(req(), body, retryable, time.Now().Add(10*time.Second), 1); resp != nil {
		resp.Body.Close()
		t.Fatal("home leg opened without allow_home_ip_direct (home IP would burn again)")
	}
	if directHits != 0 {
		t.Fatalf("home-leg hits before the second key = %d, want 0", directHits)
	}
	// 1) Fresh transport: no saturation history -> gate closed.
	if resp := try(); resp != nil {
		resp.Body.Close()
		t.Fatal("home leg must stay closed without any saturation history")
	}
	// 2) One saturated tail, but the pool served a lease a moment ago -> closed.
	pt.noteOK("10.1.0.5", 200)
	pt.noteSaturatedTail()
	if resp := try(); resp != nil {
		resp.Body.Close()
		t.Fatal("home leg must stay closed inside poolEmptyGrace (the pool may still reopen windows)")
	}
	// 3) Pool silent past the grace window -> the home leg serves.
	pt.lastPoolOK.Store(time.Now().Add(-time.Minute).UnixNano())
	resp := try()
	if resp == nil {
		t.Fatal("pool silent past the grace window must open the home leg (the sure escape from a burned pool)")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("home-leg status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	if directHits != 1 {
		t.Fatalf("home-leg hits = %d, want 1", directHits)
	}
	// 4) A pooled 200 starts the grace window over: the home leg closes again.
	pt.noteOK("10.1.0.5", 200)
	pt.noteSaturatedTail()
	if resp := try(); resp != nil {
		resp.Body.Close()
		t.Fatal("a fresh pooled 200 must close the home leg again (protect the home window)")
	}
	if directHits != 1 {
		t.Fatalf("home-leg hits = %d, want still 1", directHits)
	}
}

// TestPoolHealthyNeverRotatesOrUsesDirectLeg: a serving pool must never spend
// the home (IP,UA) window and never thrash the pool with rotates — the
// regression that made the escape hatches unsafe the first time round.
func TestPoolHealthyNeverRotatesOrUsesDirectLeg(t *testing.T) {
	good, goodHits := fakeForwardProxyHits(t, http.StatusOK)
	defer good.Close()
	directHits := 0
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHits++
		_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer direct.Close()
	leases := make([]string, 200)
	for i := range leases {
		leases[i] = good.URL
	}
	api := &fakePoolAPI{leases: leases}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Minute, DirectFallback: true}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: direct.URL + "/v1"}
	for i := range 3 {
		lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
		if err != nil {
			t.Fatalf("request %d: healthy pool must acquire, got %v", i, err)
		}
		req, _ := http.NewRequest(http.MethodPost, direct.URL+"/v1/chat/completions", strings.NewReader(`{}`))
		if _, err = lease.Client.Do(req); err != nil {
			t.Fatalf("request %d: healthy pool must serve, got %v", i, err)
		}
		_ = lease.Release(context.Background())
	}
	if *goodHits != 3 {
		t.Fatalf("pooled hits = %d, want 3 (all traffic through the pool)", *goodHits)
	}
	if directHits != 0 {
		t.Fatalf("direct-leg hits = %d, want 0 on a healthy pool", directHits)
	}
	if n := rotateCount(api); n != 0 {
		t.Fatalf("rotate calls = %d, want 0 on a healthy pool", n)
	}
}
