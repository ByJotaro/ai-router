package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// armGlobalBurn puts the transport into the live "shared budget burned" state
// (2026-10-21 21:12 storm: globalUntil=03:01 while the pool was dead).
func armGlobalBurn(pt *PoolTransport, d time.Duration) {
	pt.muMu.Lock()
	pt.globalBurnUntil = time.Now().Add(d)
	pt.globalRA = int(d.Seconds())
	pt.muMu.Unlock()
}

// TestPoolHealRotateFiresDuringGlobalBurn pins the bug that cost hours of 503:
// with the shared-deadline flag armed, acquire-rotate was SKIPPED ("fresh
// identities share the deadline"), so a burned pool stayed dead until a human
// restarted warp-router. A rotate re-registers WARP devices and returns NEW
// /48 prefixes (measured 2026-10-22 00:31: 14/14 IPs changed), so it must fire
// even under a global burn.
func TestPoolHealRotateFiresDuringGlobalBurn(t *testing.T) {
	api := &fakePoolAPI{epoch: 3, wrappedV2: true, emptyBatch: true}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond,
		PoolV2: true, WaitForLease: 2 * time.Second}
	blockBackgroundWorkers(pt)
	armGlobalBurn(pt, time.Hour)

	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	start := time.Now()
	_, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("пустой пул без единого лisa обязан вернуть ошибку, а не вечный успех")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		api.mu.Lock()
		n := api.rotateN
		api.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rotate не вызван под global burn (rotateN=0) — именно этот skip убивал пул")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if elapsed < time.Second {
		t.Fatalf("запрос обязан ЖДАТЬ egress вместо мгновенного 503, elapsed=%v", elapsed)
	}
}

// TestPoolHealRotateKeepsGlobalBurnUntilRenewal: a rotate re-registers the
// WARP devices but can land on the SAME /48 prefixes (measured 2026-10-22
// 10:14: two fresh accounts came up in 2a09:bac1:47a0, already burned for
// hours). Clearing the shared-burn latch on the rotate alone drops the router
// out of fail-fast probing on a claim, so the flag must survive until a 200
// proves service through a prefix that did not exist before the rotate.
func TestPoolHealRotateKeepsGlobalBurnUntilRenewal(t *testing.T) {
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: []string{good.URL}, epoch: 2, wrappedV2: true}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond, PoolV2: true}
	blockBackgroundWorkers(pt)
	armGlobalBurn(pt, time.Hour)
	if !pt.globalActive() {
		t.Fatal("предусловие: burn должен быть активен")
	}
	pt.healRotate("test")
	deadline := time.Now().Add(3 * time.Second)
	for {
		api.mu.Lock()
		n := api.rotateN
		api.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ротация не дошла до warp-router")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !pt.globalActive() {
		t.Fatal("burn снят самой ротацией, без доказанного обновления префиксов")
	}
	pt.noteIdentityGroup("2a09:bac1:47a0:170::32:46") // prefix absent before the rotate
	if pt.globalActive() {
		t.Fatal("burn не снят после доказанного обновления префиксов")
	}
}

// TestPoolHealRotateBacksOff: a heal that does not restore service must not
// flap the pool every request — the gap escalates.
func TestPoolHealRotateBacksOff(t *testing.T) {
	api := &fakePoolAPI{epoch: 1, wrappedV2: true}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, PoolV2: true}
	blockBackgroundWorkers(pt)
	pt.healRotate("first")
	deadline := time.Now().Add(2 * time.Second)
	for {
		api.mu.Lock()
		n := api.rotateN
		api.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("первый heal не отработал")
		}
		time.Sleep(20 * time.Millisecond)
	}
	pt.healRotate("second — must be suppressed by the gap")
	time.Sleep(150 * time.Millisecond)
	api.mu.Lock()
	n := api.rotateN
	api.mu.Unlock()
	if n != 1 {
		t.Fatalf("rotateN=%d, want 1 (второй heal обязан ждать gap)", n)
	}
}

// TestPoolWaitsForEgressThenServes is the user-facing contract: instead of a
// 503 in two milliseconds (which burns one of the client's ten retries), the
// request WAITS for one egress and succeeds — and says so in WaitedMs so the
// journal can show it as a separate category.
func TestPoolWaitsForEgressThenServes(t *testing.T) {
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: []string{good.URL, good.URL, good.URL}, epoch: 5, wrappedV2: true, emptyFirst: 6}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond,
		PoolV2: true, WaitForLease: 14 * time.Second}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	l, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatalf("слот освободился — запрос обязан дождаться, а не упасть: %v", err)
	}
	if l == nil {
		t.Fatal("nil lease")
	}
	if l.WaitedMs <= 0 {
		t.Fatalf("WaitedMs=%d: журнал не увидит категорию «обслужен после ожидания»", l.WaitedMs)
	}
	_ = l.Release(context.Background())
}

// TestPoolBannedShareCountsHardBans: the "over half the pool banned" trigger
// must count hours-class SET entries (hardBad) — they are the bans that make a
// pool hopeless.
func TestPoolBannedShareCountsHardBans(t *testing.T) {
	pt := &PoolTransport{}
	pt.muMu.Lock()
	now := time.Now()
	pt.ipOK = map[string]int{"a": 3, "b": 1, "c": 2, "d": 0}
	pt.bad = map[string]time.Time{"a": now.Add(time.Hour), "b": now.Add(time.Hour)}
	pt.hardBad = map[string]bool{"a": true, "b": true}
	pt.muMu.Unlock()
	if got := pt.bannedShare(); got != 0.5 {
		t.Fatalf("bannedShare=%.2f, want 0.50 (2 из 4)", got)
	}
	pt.muMu.Lock()
	pt.hardBad["c"] = true
	pt.muMu.Unlock()
	if got := pt.bannedShare(); got < 0.74 || got > 0.76 {
		t.Fatalf("bannedShare=%.2f, want 0.75 (hardBad — тоже бан)", got)
	}
}

// TestPoolWaitDisabledKeepsFailFast: direct-mode stands and existing callers
// rely on the old behaviour — with no wait budget the exhausted pool answers
// immediately (the wait path must stay strictly opt-in via config).
func TestPoolWaitDisabledKeepsFailFast(t *testing.T) {
	api := &fakePoolAPI{epoch: 7, wrappedV2: true, emptyBatch: true}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond, PoolV2: true}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	start := time.Now()
	_, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err == nil || !strings.Contains(err.Error(), "empty leases batch") {
		t.Fatalf("want distinct empty-batch error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 9*time.Second {
		t.Fatalf("без wait_for_lease запрос обязан падать быстро, elapsed=%v", elapsed)
	}
}

// TestHealRotateRenewalRequiresNewPrefix: a rotate that returns the SAME /48
// prefixes changed nothing upstream — the shared budget is the same, so the
// router must not claim a new generation (which would clear the burn flag and
// reset per-model lockouts on a lie). Renewal is proven only by serving a 200
// through a prefix that did not exist before the rotate.
func TestHealRotateRenewalRequiresNewPrefix(t *testing.T) {
	pt := &PoolTransport{}
	pt.muMu.Lock()
	pt.ipOK = map[string]int{"2a09:bac5:48f4:682::a6:c": 1}
	pt.muMu.Unlock()
	pt.muMu.RLock()
	pre := pt.egressGroupsLocked()
	pt.muMu.RUnlock()
	pt.healMu.Lock()
	pt.healPreGroups = pre
	pt.healExpectRenewal = true
	pt.healMu.Unlock()
	armGlobalBurn(pt, time.Hour)

	pt.noteIdentityGroup("2a09:bac5:48f4:682::a6:d") // same /48 as before
	if got := pt.identGen.Load(); got != 0 {
		t.Fatalf("генерация выросла на СТАРОМ префиксе (gen=%d) — это ложное «обновление»", got)
	}
	if !pt.globalActive() {
		t.Fatal("burn снят без доказанного обновления префиксов")
	}
	pt.noteIdentityGroup("2a09:bac1:47a0:170::32:46") // new /48
	if got := pt.identGen.Load(); got != 1 {
		t.Fatalf("новый префикс не поднял генерацию (gen=%d)", got)
	}
	if pt.globalActive() {
		t.Fatal("burn не снят после доказанного обновления префиксов")
	}
}
