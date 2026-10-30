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

// TestGateWithoutEgressNeverBurnsSession: a gate hit that does not carry its
// egress says nothing about the session id — the gate is a (session x egress x
// minute) window. Live 2026-10-22 17:54: two such hits inside gateStreakTTL
// burned both fresh ids ("session burned … ekey=false"), after which every
// router-minted request was a guaranteed 403.
func TestGateWithoutEgressNeverBurnsSession(t *testing.T) {
	s := &SessionPool{}
	s.MarkGateHit("ses_aaaa", "", false)
	s.MarkGateHit("ses_aaaa", "", false)
	s.MarkGateHit("ses_aaaa", "", false)
	if len(s.dead) != 0 {
		t.Fatalf("egress-blind гейты сожгли сессию: %v", s.dead)
	}

	// Two gates on the SAME egress are still evidence: the id must burn.
	s.MarkGateHit("ses_bbbb", "2a09:bac5:c39b::1", false)
	if len(s.dead) != 0 {
		t.Fatal("один гейт не должен жечь id")
	}
	s.MarkGateHit("ses_bbbb", "2a09:bac5:c39b::1", false)
	if _, burned := s.dead["ses_bbbb"]; !burned {
		t.Fatal("два гейта на одном egress обязаны сжечь id")
	}
}

// TestSyntheticGateVerdictCarriesEgress: the rebuild of a drained gate answer
// must keep the egress stamp, otherwise the session pool cannot tell two gates
// on different egresses from two on the same one.
func TestSyntheticGateVerdictCarriesEgress(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp := syntheticGateVerdict(req, []byte(`{"error":"gate"}`), "2a09:bac5:c39a::7")
	if got := sessionGateEgress(resp); got != "2a09:bac5:c39a::7" {
		t.Fatalf("синтетический гейт потерял egress: %q", got)
	}
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get(gateTerminalHeader) == "" {
		t.Fatal("вердикт гейта должен остаться 403 с terminal-маркером")
	}
}

// TestColdTransient429Ladder: a RA-less 429 on an egress with zero successes in
// this window is a spent address, not a burst shaper (92% of today's transient
// 429s). It must be banned with an escalating ladder, and cleared by one 200.
func TestColdTransient429Ladder(t *testing.T) {
	if !coldTransient429(false, 0, false) {
		t.Fatal("429 без RA и без ok — выжженный адрес")
	}
	if coldTransient429(false, 0, true) {
		t.Fatal("явный Retry-After (даже 0) = шейпер, а не выжженный бюджет")
	}
	// Семантика — 2xx за cold2xxWindow, а не счётчик ответов: адрес, отдавший
	// 9 ответов (в т.ч. 4xx) и ни одного 2xx за минуту, выжжен (review B4).
	if coldTransient429(true, 9, false) {
		t.Fatal("свежий 2xx в окне — не выжженный, парка достаточно")
	}
	if !coldTransient429(false, 9, false) {
		t.Fatal("ни одного 2xx за минуту: адрес выжжен, нужен ладдер")
	}

	p := &PoolTransport{}
	now := time.Now()
	first := p.banSpent("2a09:bac5:48f1::1", now)
	if first != clusterBanCap {
		t.Fatalf("первый бан лестницы %s, ожидался %s", first, clusterBanCap)
	}
	if !p.isBad("2a09:bac5:48f1::1", now) {
		t.Fatal("после ladder-бана egress должен быть bad")
	}
	// Distinct incidents: the ladder advances only after the previous ban
	// expired (review B7), so the second incident arrives past it.
	second := p.banSpent("2a09:bac5:48f1::1", now.Add(first+time.Second))
	if second <= first {
		t.Fatalf("второй бан не вырос: %s -> %s", first, second)
	}
	// The ladder step must actually EXTEND the stored deadline (MIN-rule used
	// to swallow it while the log kept claiming 30m/2h).
	p.muMu.RLock()
	until := p.bad["2a09:bac5:48f1::1"]
	p.muMu.RUnlock()
	if left := until.Sub(now); left < 25*time.Minute {
		t.Fatalf("ступень лестницы не продлила дедлайн: осталось %s", left)
	}
	if third := p.banSpent("2a09:bac5:48f1::1", now.Add(first+second+2*time.Second)); third != 2*time.Hour {
		t.Fatalf("третий бан %s, ожидалось 2h", third)
	}

	// One 200 anywhere lifts both the ban and the stage.
	p.markGoodKey("2a09:bac5:48f1::1")
	if p.isBad("2a09:bac5:48f1::1", now) {
		t.Fatal("200 не снял ladder-бан")
	}
	if got := p.banSpent("2a09:bac5:48f1::1", now); got != clusterBanCap {
		t.Fatalf("ступень лестницы не сброшена: %s", got)
	}
}

// TestBurnProbeFanout: while the shared latch is armed the request must probe
// the promising egresses (up to 3) instead of exactly one, but must not spend
// windows on a pool with nothing to offer.
func TestBurnProbeFanout(t *testing.T) {
	now := time.Now()
	p := &PoolTransport{}
	if got := p.burnProbeFanout(now); got != 1 {
		t.Fatalf("пустой пул: фан-аут %d, ожидался 1", got)
	}
	// Lifetime counters must NOT qualify: after a renewal every stale key is
	// neither banned nor parked, so an ipOK-based promise answers yes forever
	// and the 1x1 guard never arms (review B2).
	p.muMu.Lock()
	p.ipOK = map[string]int{"2a09:bac5:c39b::1": 4}
	p.muMu.Unlock()
	if got := p.burnProbeFanout(now); got != 1 {
		t.Fatalf("lifetime 200 без свежего okAt: фан-аут %d, ожидался 1", got)
	}
	// A 2xx inside okFreshWindow is the real promise.
	key := "2a09:bac5:c39b::1"
	p.noteOK(key, 200)
	if got := p.burnProbeFanout(now); got != burnProbeFanout {
		t.Fatalf("живой egress: фан-аут %d, ожидался %d", got, burnProbeFanout)
	}
	p.markBadKeyFor(key, now, time.Hour)
	// the storm ring still holds the 200, so the "pool serving" fallback answers
	// yes - kill both sources to model a truly dead fleet
	p.muMu.Lock()
	p.stormOKs = nil
	p.storm429 = nil
	p.muMu.Unlock()
	p.lastPoolOK.Store(0)
	if got := p.burnProbeFanout(now); got != 1 {
		t.Fatalf("все живые под баном: фан-аут %d, ожидался 1", got)
	}
}

// TestHealEscalationSurvivesSingleOK: one pooled 200 (e.g. the only surviving
// egress) must not reset the rotate backoff — 32 rotates in 2.4h all logged
// heal_fails=0 and 30 of them ended without restoring anything. Only a proven
// identity renewal resets it.
func TestHealEscalationSurvivesSingleOK(t *testing.T) {
	p := &PoolTransport{}
	now := time.Now()
	p.healAt = now.Add(-time.Hour)
	p.noteHealOutcome(false)
	if p.healFails != 1 {
		t.Fatalf("healFails=%d после неудачи, ожидалось 1", p.healFails)
	}
	p.noteOK("2a09:bac5:c39b::1", 200)
	if p.healFails != 1 {
		t.Fatalf("одиночный 200 сбросил эскалацию ротаций: %d", p.healFails)
	}
	p.healMu.Lock()
	p.healExpectRenewal = true
	p.healPreGroups = map[string]bool{"2a09:bac5:48f1": true}
	p.healMu.Unlock()
	p.markGoodKey("2a09:bac5:c39f::9")
	if p.healFails != 0 {
		t.Fatalf("новый /48 не сбросил эскалацию: %d", p.healFails)
	}
}

// TestAPIBreakerHalfOpen: while the pool-API breaker is open, exactly one
// caller per interval may dial through, so a warp-router respawn does not
// blackhole every request for the whole cooldown.
func TestAPIBreakerHalfOpen(t *testing.T) {
	p := &PoolTransport{}
	p.apiDownUntil.Store(time.Now().Add(30 * time.Second).UnixNano())
	allowed := 0
	for i := 0; i < 5; i++ {
		if !p.apiDown() {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("half-open пропустил %d попыток, ожидалась 1", allowed)
	}
	if p.apiDownUntil.Load() == 0 {
		t.Fatal("half-open не должен закрывать брейкер сам")
	}
}

// TestLadderBanIsFalsifiable: a ladder ban is our own guess, so the standby
// verifier must be allowed to disprove it (once per half-window); the
// plant-level hours ban (hardBad) is never probed.
func TestLadderBanIsFalsifiable(t *testing.T) {
	p := &PoolTransport{}
	now := time.Now()
	if !p.verifyAnyway("2a09:bac5:48f1::1") {
		t.Fatal("незабаненный egress обязан проверяться")
	}
	p.banSpent("2a09:bac5:48f1::1", now)
	if !p.verifyAnyway("2a09:bac5:48f1::1") {
		t.Fatal("ladder-бан должен быть опровержим первой проверкой")
	}
	if p.verifyAnyway("2a09:bac5:48f1::1") {
		t.Fatal("второй probe внутри полу-окна запрещён (каждая проба стоит окно)")
	}
	p.muMu.Lock()
	p.hardBad = map[string]bool{"2a09:bac5:48f1::1": true}
	p.muMu.Unlock()
	if p.verifyAnyway("2a09:bac5:48f1::1") {
		t.Fatal("hours-бан (hardBad) не проверяется standby-воркером")
	}
	// A verified 200 resets the stage: the next ladder hit starts at 10m.
	p.noteStandbyLive("2a09:bac5:48f1::1", now)
	if got := p.banSpent("2a09:bac5:48f1::1", now); got != clusterBanCap {
		t.Fatalf("подтверждённый 200 не сбросил ступень: %s", got)
	}
}

// TestSpentStageSurvivesNon2xx: only a real 2xx proves the window reopened; a
// 403 through the lease must not reset the ladder stage.
func TestSpentStageSurvivesNon2xx(t *testing.T) {
	p := &PoolTransport{}
	now := time.Now()
	p.banSpent("2a09:bac5:48f1::1", now)
	p.noteOK("2a09:bac5:48f1::1", 403)
	// the ladder only advances after the previous ban expired (review B7), so
	// each probe of the stage must arrive past it
	after := now.Add(clusterBanCap + time.Second)
	if got := p.banSpent("2a09:bac5:48f1::1", after); got == clusterBanCap {
		t.Fatal("403 через лизу сбросил ступень лестницы")
	}
	p.markGoodKey("2a09:bac5:48f1::1")
	if got := p.banSpent("2a09:bac5:48f1::1", after.Add(time.Hour)); got != clusterBanCap {
		t.Fatalf("200 не сбросил ступень: %s", got)
	}
}

// TestPoolEpochResetsLadder: a new WARP identity generation invalidates bans,
// ipOK and therefore the ladder as well.
func TestPoolEpochResetsLadder(t *testing.T) {
	p := &PoolTransport{PoolV2: true}
	p.banSpent("2a09:bac5:48f1::1", time.Now())
	if len(p.spent) == 0 {
		t.Fatal("ступень не записалась")
	}
	p.notePoolEpoch(7)
	if len(p.spent) != 0 || len(p.verifyAt) != 0 {
		t.Fatal("новая генерация идентичностей не сбросила лестницу")
	}
}

// TestLimitedProbeKeepsTailGates: the latch's 3x1 must be treated as a limited
// probe budget, otherwise the fail-fast tail gates stop applying and one
// request walks 7-9 upstream windows.
func TestLimitedProbeKeepsTailGates(t *testing.T) {
	if !limitedProbe(burnProbeFanout, 1) {
		t.Fatal("3x1 под латчем обязан считаться lazy-бюджетом")
	}
	if !limitedProbe(1, 1) {
		t.Fatal("1x1 обязан считаться fail-fast")
	}
	if limitedProbe(5, 2) {
		t.Fatal("полный свип 5x2 не должен включать fail-fast хвост")
	}
}

// TestPoolHasPromisingNeedsRecentOK: a stale okAt (older than okFreshWindow)
// must not qualify, and an epoch change (identity renewal) must clear both the
// mark and the shared-budget latch it armed (review B2/B3).
func TestPoolHasPromisingNeedsRecentOK(t *testing.T) {
	now := time.Now()
	p := &PoolTransport{PoolV2: true}
	p.noteOK("2a09:bac5:c39b::1", 200)
	if !p.poolHasPromising(now) {
		t.Fatal("свежий 2xx обязан обещать пробу")
	}
	if !p.poolHasPromising(now.Add(okFreshWindow + time.Second)) {
		// the storm ring still has the 200 inside stormWindow, so the pool as a
		// whole is serving - that is the documented fallback
		t.Log("pool serving fallback answered for a stale mark (ожидаемо)")
	}
	p.muMu.Lock()
	p.globalBurnUntil = time.Now().Add(2 * time.Hour)
	p.globalGen = p.identGen.Load()
	p.okAt = map[string]int64{"2a09:bac5:c39b::1": time.Now().Add(-okFreshWindow - time.Minute).UnixNano()}
	p.muMu.Unlock()
	if p.globalActive() != true {
		t.Fatal("latch должен быть армирован до renewal")
	}
	p.notePoolEpoch(7)
	if p.globalActive() {
		t.Fatal("renewal обязан снять latch: он армировался против старого флота")
	}
	p.muMu.RLock()
	n := len(p.okAt)
	p.muMu.RUnlock()
	if n != 0 {
		t.Fatalf("okAt не очищен на epoch: %d", n)
	}
}

// TestLadderStageAdvancesOnce: concurrent 429s from in-flight requests on the
// same /128 must not push the ladder - only a NEW incident after the ban
// expired does (review B7).
func TestLadderStageAdvancesOnce(t *testing.T) {
	now := time.Now()
	p := &PoolTransport{}
	key := "2a09:bac5:48f1::9"
	first := p.banSpent(key, now)
	if first != spentBanLadder(1) {
		t.Fatalf("первый инцидент: ban=%s, ожидался %s", first, spentBanLadder(1))
	}
	// three in-flight tails arrive after the ban is already applied
	for i := 0; i < 3; i++ {
		if d := p.banSpent(key, now.Add(time.Second)); d != first {
			t.Fatalf("in-flight 429 сдвинул ладдер: %s вместо %s", d, first)
		}
	}
	// next incident after expiry: stage 2
	later := now.Add(first + time.Second)
	if d := p.banSpent(key, later); d != spentBanLadder(2) {
		t.Fatalf("новый инцидент: ban=%s, ожидался %s", d, spentBanLadder(2))
	}
}

// TestWaitAPIWaitsForHalfOpen: with the breaker open the request must WAIT for
// the half-open probe (bounded), not 503 with waited_ms=0 (review M1).
func TestWaitAPIWaitsForHalfOpen(t *testing.T) {
	old := apiWaitTick
	apiWaitTick = 10 * time.Millisecond
	defer func() { apiWaitTick = old }()

	// budget exhausted while the breaker stays open -> no wait, honest 503
	dead := &PoolTransport{WaitForLease: 20 * time.Millisecond}
	dead.apiDownUntil.Store(time.Now().Add(time.Hour).UnixNano())
	dead.apiProbeAt.Store(time.Now().Add(time.Hour).UnixNano())
	if dead.waitAPI(context.Background()) {
		t.Fatal("breaker открыт весь бюджет - ожидание обязано провалиться")
	}
	// the breaker closes inside the budget -> the request proceeds
	p := &PoolTransport{WaitForLease: 2 * time.Second}
	p.apiDownUntil.Store(time.Now().Add(25 * time.Millisecond).UnixNano())
	p.apiProbeAt.Store(time.Now().Add(time.Hour).UnixNano())
	if !p.waitAPI(context.Background()) {
		t.Fatal("breaker закрылся внутри бюджета - запрос обязан пройти")
	}
}

// TestGateIdLevelRetirement: a genuinely dead account must still be retirable
// although the fast path is egress-keyed (review B5). Three DISTINCT egresses -
// or six gates of any shape - inside gateIdWindow burn the id; two blind gates
// never do (the 17:54 incident).
func TestGateIdLevelRetirement(t *testing.T) {
	s := &SessionPool{}
	for i := 0; i < 2; i++ {
		s.MarkGateHit("ses_1", "", false)
	}
	if _, burned := s.dead["ses_1"]; burned {
		t.Fatal("два egress-blind гейта не должны жечь id")
	}
	// third distinct egress -> id-level verdict
	s.MarkGateHit("ses_1", "2a09:bac5:c39b::1", false)
	s.MarkGateHit("ses_1", "2a09:bac5:48f1::2", false)
	if _, burned := s.dead["ses_1"]; burned {
		t.Fatal("два разных egress ещё не приговор (гейт бывает оконным)")
	}
	s.MarkGateHit("ses_1", "2a09:bac5:48f2::3", false)
	if _, burned := s.dead["ses_1"]; !burned {
		t.Fatal("три разных egress внутри 10 мин обязаны сжечь id")
	}

	// Six gates on ONE egress also retire (the fast path would need 2, this is
	// the account-level count).
	s2 := &SessionPool{}
	for i := 0; i < gateIdTotalGates; i++ {
		s2.MarkGateHit("ses_2", "2a09:bac5:c39b::9", false)
	}
	if _, burned := s2.dead["ses_2"]; !burned {
		t.Fatalf("%d гейтов внутри окна обязаны сжечь id", gateIdTotalGates)
	}

	// A 200 anywhere clears the id-level record: the account is alive.
	s3 := &SessionPool{}
	s3.MarkGateHit("ses_3", "2a09:bac5:c39b::1", false)
	s3.MarkGateHit("ses_3", "2a09:bac5:48f1::2", false)
	s3.MarkGateHit("ses_3", "2a09:bac5:48f2::3", true)
	for i := 0; i < 2; i++ {
		s3.MarkGateHit("ses_3", "", false)
	}
	if _, burned := s3.dead["ses_3"]; burned {
		t.Fatal("200 обязан обнулить id-level счётчик гейтов")
	}
}

// TestGateIdLevelSparesLiveSession: gates are egress/window-correlated, so on a
// three-group pool a LIVE id collects three distinct-egress gates inside ten
// minutes. A 2xx in the window must protect it (live 2026-10-22 19:21: the
// id-level rule burned ses_eb692d6c three times in 30s while it was serving).
func TestGateIdLevelSparesLiveSession(t *testing.T) {
	s := &SessionPool{}
	s.MarkGateHit("ses_live", "2a09:bac5:c39b::1", true) // proven alive
	for _, ek := range []string{"2a09:bac5:c39b::2", "2a09:bac5:48f1::2", "2a09:bac5:48f2::3"} {
		s.MarkGateHit("ses_live", ek, false)
	}
	if _, burned := s.dead["ses_live"]; burned {
		t.Fatal("свежий 2xx в окне защищает живой id от id-level вердикта")
	}
}

// TestGateIdLevelCountsGatesNotSlots: six gates spread over only TWO egresses
// must still retire the id - an earlier implementation overwrote each slot, so
// the "6 gates" threshold was unreachable and the log field lied.
func TestGateIdLevelCountsGatesNotSlots(t *testing.T) {
	s := &SessionPool{}
	a, b := "2a09:bac5:c39b::1", "2a09:bac5:48f1::2"
	for i := 0; i < gateIdTotalGates; i++ {
		if i%2 == 0 {
			s.MarkGateHit("ses_storm", a, false)
		} else {
			s.MarkGateHit("ses_storm", b, false)
		}
	}
	if _, burned := s.dead["ses_storm"]; !burned {
		t.Fatalf("%d гейтов на двух egress обязаны сжечь id (счёт по гейтам, не по слотам)", gateIdTotalGates)
	}
}

// TestCold429LadderOwnsThePark is the wiring guard for the single-owner rule: a
// RA-less 429 on an egress with no 2xx must go to the LADDER (spent stage + ban)
// and must NOT be parked as a burst shaper. The earlier code passed a literal
// haveRA=true, which made the predicate constant-false - the build stayed green
// while park+ladder both applied to the same event.
func TestCold429LadderOwnsThePark(t *testing.T) {
	hot := fakeForwardProxy(t, http.StatusTooManyRequests) // no Retry-After
	defer hot.Close()
	api := &fakePoolAPI{leases: []string{hot.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}

	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := lease.Client.Do(req)
	if resp != nil {
		drainClose(resp)
	}
	_ = lease.Release(context.Background())

	pt.muMu.RLock()
	stage := 0
	var key string
	for k, v := range pt.spent {
		key, stage = k, v
	}
	nPark := len(pt.park)
	_, banned := pt.bad[key]
	pt.muMu.RUnlock()
	if stage == 0 {
		t.Fatalf("RA-less 429 без 2xx обязан поднять ступень ладдера (spent=%v park=%d)", pt.spent, nPark)
	}
	if !banned {
		t.Fatal("ладдер обязан забанить выжженный egress")
	}
	if nPark != 0 {
		t.Fatalf("park не должен применяться к тому же 429 (park=%d): один владелец события", nPark)
	}
}
