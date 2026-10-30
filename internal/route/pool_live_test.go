package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestBurnLatchDoesNotCollapseWhilePoolServes pins the live failure of
// 2026-10-22 12:20-12:25: the shared-burn latch was armed (14.6h) while twelve
// egresses kept answering 200, and the 1x1 fail-fast collapse turned every
// request into a single-probe lottery — 45% of requests got 503 in 0.5-2s with
// "(1 throttled)". A pooled 200 inside poolLiveGrace must keep the full sweep.
func TestBurnLatchDoesNotCollapseWhilePoolServes(t *testing.T) {
	p := &PoolTransport{}
	p.muMu.Lock()
	p.globalBurnUntil = time.Now().Add(time.Hour)
	p.muMu.Unlock()

	if !p.burnCollapse(time.Now()) {
		t.Fatal("пул молчит: 1x1 обязан остаться (иначе шторм жжёт все окна)")
	}
	p.lastPoolOK.Store(time.Now().Add(-2 * time.Second).UnixNano())
	if p.burnCollapse(time.Now()) {
		t.Fatal("200 секунду назад, а свип всё ещё схлопнут в один probe — это и была лотерея 503")
	}
	p.lastPoolOK.Store(time.Now().Add(-time.Minute).UnixNano())
	if !p.burnCollapse(time.Now()) {
		t.Fatal("для молчащего пула 1x1 снова обязателен")
	}
}

// TestPickLeasePrefersRecentlyLive: inside one batch the burned and the live
// egresses look identical to the old picker (no bans recorded, heat equal), so
// it drew a lottery. A 200 inside okFreshWindow must win the pick.
func TestPickLeasePrefersRecentlyLive(t *testing.T) {
	p := &PoolTransport{}
	p.muMu.Lock()
	p.okAt = map[string]int64{}
	now := time.Now()
	// c is bordered by two egresses with louder heat/LRU evidence.
	p.okAt["2001:db8:1::1"] = now.Add(-30 * time.Second).UnixNano()
	p.muMu.Unlock()

	ls := []poolLeaseResponse{
		{LeaseID: "a", ProxyURL: "http://127.0.0.1:1", PublicIP: "2001:db8:2::2"},
		{LeaseID: "b", ProxyURL: "http://127.0.0.1:2", PublicIP: "2001:db8:1::1"},
		{LeaseID: "c", ProxyURL: "http://127.0.0.1:3", PublicIP: "2001:db8:3::3"},
	}
	got, _ := p.pickLease(ls, now, false)
	if got.LeaseID != "b" {
		t.Fatalf("выбран %q: лиз с живым 200 обязан побеждать неизвестные", got.LeaseID)
	}
}

// TestPickLeaseHidesBannedBeforeLive: a banned egress stays out even if it is
// the only one with a fresh 200 — bans carry upstream verdicts, not guesses.
func TestPickLeaseHidesBannedBeforeLive(t *testing.T) {
	p := &PoolTransport{}
	now := time.Now()
	p.muMu.Lock()
	p.okAt = map[string]int64{"2001:db8:9::9": now.UnixNano()}
	p.bad = map[string]time.Time{"2001:db8:9::9": now.Add(time.Hour)}
	p.muMu.Unlock()

	ls := []poolLeaseResponse{
		{LeaseID: "bad", ProxyURL: "http://127.0.0.1:9", PublicIP: "2001:db8:9::9"},
		{LeaseID: "ok", ProxyURL: "http://127.0.0.1:8", PublicIP: "2001:db8:8::8"},
	}
	got, _ := p.pickLease(ls, now, false)
	if got.LeaseID != "ok" {
		t.Fatalf("выбран забаненный %q", got.LeaseID)
	}
}

// sanity: the collapse helper must never fire without a burn latch.
func TestBurnCollapseNoLatch(t *testing.T) {
	p := &PoolTransport{}
	if p.burnCollapse(time.Now()) {
		t.Fatal("без латча схлопывания нет")
	}
	_ = httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
}

// TestHoursClusterDoesNotLatchWhileServing: two hours-class RAs from two /48s are
// two burned egresses, not proof that the whole pool is dead. While a pooled 200
// is younger than poolLiveGrace the latch must NOT arm (it would collapse every
// request to one probe); the burned egress is banned on its own instead.
func TestHoursClusterDoesNotLatchWhileServing(t *testing.T) {
	now := time.Now()
	raSecs := 56000
	groupA, groupB := "2a09:bac5:48f1::1", "2a09:bac5:48f2::2"

	serving := &PoolTransport{}
	serving.lastPoolOK.Store(now.Add(-3 * time.Second).UnixNano())
	for _, k := range []string{groupA, groupB} {
		if global, _ := serving.noteHoursRAGuarded(k, raSecs, now); global {
			t.Fatalf("латч армится, хотя пул только что отдал 200 (egress %s)", k)
		}
	}
	if until, _ := serving.globalStatus(); !until.IsZero() {
		t.Fatalf("латч всё же взведён до %s", until)
	}

	silent := &PoolTransport{}
	silent.lastPoolOK.Store(now.Add(-10 * time.Minute).UnixNano())
	if global, _ := silent.noteHoursRAGuarded(groupA, raSecs, now); global {
		t.Fatal("одного наблюдения для shared-латча мало (нужен кластер)")
	}
	if global, _ := silent.noteHoursRAGuarded(groupB, raSecs, now); !global {
		t.Fatal("молчащий пул + кластер hours-RA обязан взводить латч")
	}
	if !silent.burnCollapse(time.Now()) {
		t.Fatal("для молчащего пула с латчем свип обязан схлопнуться")
	}
}

// TestPoolServingIgnoresCadence: the guard must not collapse the sweep just because
// the last 200 is older than 15s. Measured live load is 1-16 req/min (4-60s gaps),
// and the 15s-only test let the 1x1 regime survive (714 of 717 tails "(1 throttled)").
func TestPoolServingIgnoresCadence(t *testing.T) {
	p := &PoolTransport{}
	now := time.Now()
	p.lastPoolOK.Store(now.Add(-40 * time.Second).UnixNano())
	p.muMu.Lock()
	p.stormOKs = []int64{now.Add(-20 * time.Second).UnixNano(), now.Add(-35 * time.Second).UnixNano()}
	p.storm429 = []int64{now.Add(-25 * time.Second).UnixNano()}
	p.muMu.Unlock()
	if !p.poolServing() {
		t.Fatal("2 ok против 1 хита в окне — пул жив, схлопывать нельзя")
	}
	if p.burnCollapse(now) {
		t.Fatal("без латча схлопывания нет")
	}

	burned := &PoolTransport{}
	burned.lastPoolOK.Store(now.Add(-40 * time.Second).UnixNano())
	burned.muMu.Lock()
	burned.storm429 = []int64{now.Add(-5 * time.Second).UnixNano(), now.Add(-9 * time.Second).UnixNano()}
	burned.muMu.Unlock()
	if burned.poolServing() {
		t.Fatal("200-хвост 40с при свежих 429 без единого ok — пул молчит")
	}
}

// TestPoolServingRequires2xx: a 403/4xx through a lease must not count as the pool
// serving (live mix had 26 gate403 per 5 min lifting the guard on dead budgets).
func TestPoolServingRequires2xx(t *testing.T) {
	p := &PoolTransport{}
	p.noteOK("2001:db8:1::1", 403)
	if p.poolServingRecently(time.Minute) {
		t.Fatal("403 не доказывает, что пул жив")
	}
	p.noteOK("2001:db8:1::1", 204)
	if !p.poolServingRecently(time.Minute) {
		t.Fatal("204 — это 2xx, пул жив")
	}
}

// TestOKAtClearedOn429: freshness must not survive the egress's own 429, otherwise
// pickLease keeps re-picking a burned egress for okFreshWindow (10 min).
func TestOKAtClearedOn429(t *testing.T) {
	p := &PoolTransport{}
	now := time.Now()
	p.noteOK("2001:db8:1::1", 200)
	if _, seen := p.okAge("2001:db8:1::1"); !seen {
		t.Fatal("предусловие: 200 обязан записать свежесть")
	}
	rt := &poolRetryRT{t: p, prov: "opencode"}
	rt.noteThrottleHit("2001:db8:1::1", []byte(`{}`), now)
	if _, seen := p.okAge("2001:db8:1::1"); seen {
		t.Fatal("после 429 egress больше не «недавно живой»")
	}
}

// TestClusteredHoursBanCapped: a shared hours deadline (RA reported by two /48s)
// must not ban an egress for the full 12-16h — live had 89 bans of ~45100s while 19
// of 20 egresses served 200s in the same 10 minutes.
func TestClusteredHoursBanCapped(t *testing.T) {
	if clusterBanCap > time.Hour {
		t.Fatal("кластерный бан не должен превышать час")
	}
	if clusterBanCap < 2*time.Minute {
		t.Fatal("слишком короткий бан — окно будет ре-прогреваться каждый свип")
	}
}

// TestHomeLegMarksEgressSource: request_log.proxy carries the last LANE lease, so a
// home-leg success used to look exactly like a pooled one (2026-10-22 16:15: 101
// "direct last-resort ok" against 37 rows tagged with lane proxies — the outage hid
// behind the fallback). The direct leg must stamp its identity on the response.
func TestHomeLegMarksEgressSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := &PoolTransport{DirectFallback: true, AllowHomeIPDirect: true}
	p.satTailStreak.Store(1)
	p.lastPoolOK.Store(time.Now().Add(-time.Minute).UnixNano())
	rt := &poolRetryRT{t: p, prov: ""}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp := rt.tryDirectFallback(req, []byte(`{"stream":true}`), func(st int) bool { return st == 429 || st >= 500 }, time.Now().Add(20*time.Second), 1)
	if resp == nil {
		t.Fatal("предусловие: пул молчит + оба ключа выставлены — домашний лиз обязан отдать ответ")
	}
	defer drainClose(resp)
	if got := resp.Header.Get(egressSourceHeader); got != directHomeIPKey {
		t.Fatalf("ответ домашнего лиз не помечен: %q", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// TestHomeLegDoesNotRenewIdentities: the direct home-IP key is not a pool identity.
// Live 2026-10-22 16:15:23 logged "identities renewed — new egress prefix served
// group=direct-home-ip global_cleared=true" right before "direct last-resort ok",
// i.e. every successful home-leg request bumped identGen, cleared the shared-burn
// latch and dropped identity-keyed ban state.
func TestHomeLegDoesNotRenewIdentities(t *testing.T) {
	p := &PoolTransport{}
	now := time.Now()
	p.muMu.Lock()
	p.globalBurnUntil = now.Add(time.Hour)
	p.globalRA = 45000
	p.muMu.Unlock()

	p.healMu.Lock()
	p.healExpectRenewal = true
	p.healPreGroups = map[string]bool{"2a09:bac5:48f1": true}
	p.healMu.Unlock()
	genBefore := p.identGen.Load()

	p.markGoodKey(directHomeIPKey)

	if got := p.identGen.Load(); got != genBefore {
		t.Fatalf("домашний лиз поднял поколение идентичностей %d -> %d", genBefore, got)
	}
	if until, _ := p.globalStatus(); until.IsZero() {
		t.Fatal("домашний лиз снял shared-латч: баны и дедлайн пула потеряны")
	}
	if !p.healExpectRenewal {
		t.Fatal("ожидание реального обновления /48 сброшено домашним лизом")
	}

	// A REAL new prefix must still renew identities.
	p.markGoodKey("2a09:bac5:48f9::7")
	if got := p.identGen.Load(); got != genBefore+1 {
		t.Fatalf("новый /48 не поднял поколение: %d", got)
	}
}

// TestHomeLegRequiresBothKeys: policy "pool only" (2026-10-22) — the router must
// never reach the upstream from our own IP unless BOTH keys are on. With
// allow_home_ip_direct:false (live config) the last-resort dial must not even run.
func TestHomeLegRequiresBothKeys(t *testing.T) {
	cases := []struct {
		fallback, allow, want bool
		name                  string
	}{
		{false, true, false, "только allow"},
		{true, false, false, "только direct_fallback"},
		{false, false, false, "оба выключены"},
		{true, true, true, "оба включены"},
	}
	for _, c := range cases {
		p := &PoolTransport{DirectFallback: c.fallback, AllowHomeIPDirect: c.allow}
		if got := p.homeDirectAllowed(); got != c.want {
			t.Fatalf("%s: homeDirectAllowed=%v, ожидалось %v", c.name, got, c.want)
		}
	}

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	p := &PoolTransport{DirectFallback: true, AllowHomeIPDirect: false}
	p.satTailStreak.Store(1)
	p.lastPoolOK.Store(time.Now().Add(-time.Minute).UnixNano())
	rt := &poolRetryRT{t: p}
	if rt.poolDeadLastResort() {
		t.Fatal("при allow_home_ip_direct=false пул не имеет права отдавать домашний лиз")
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp := rt.tryDirectFallback(req, []byte(`{"stream":true}`),
		func(st int) bool { return st == 429 || st >= 500 }, time.Now().Add(20*time.Second), 1); resp != nil {
		t.Fatalf("домашний лиз сработал при выключенном allow_home_ip_direct: status=%d", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("в апстрим ушло %d прямых запросов с нашего IP", n)
	}
}
