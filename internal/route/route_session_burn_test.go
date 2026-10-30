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

// seedSessionPool replaces the process session singleton with one seeded from
// ids (all fresh, so nothing is expired) and returns it for inspection. The
// singleton is dropped again at test end (resetSessionPool is the existing
// harness hook for exactly this).
func seedSessionPool(t *testing.T, ids ...string) *SessionPool {
	t.Helper()
	// Gated lanes go through the Bun sidecar when it is enabled and the lease
	// names an egress; that helper fails with a synthetic 502 in a unit test
	// (measured 2026-10-27: hits=0, err "Bad Gateway"). These tests assert the
	// Go request path, so turn the optional stack off.
	t.Setenv("OPENCODE_TRANSPORT", "off")
	resetSessionPool()
	t.Cleanup(resetSessionPool)
	sp := getSessionPool()
	now := time.Now()
	sp.Now = func() time.Time { return now }
	sp.Fetch = func(string, time.Duration, time.Time) []sessionEntry {
		out := make([]sessionEntry, 0, len(ids))
		for i, id := range ids {
			// Distinct timestamps keep Next/NextFor ordering deterministic
			// (newest first): ids[0] is the sticky pick.
			out = append(out, sessionEntry{id: id, updated: now.Add(-time.Duration(i+1) * time.Minute).UnixMilli()})
		}
		return out
	}
	return sp
}

// leaseStampTransport models what PoolTransport.RoundTrip does on the real
// pool path: stamp the leased egress's OWN session (and its egress id on gate
// verdicts) on the request that is actually sent. Without it these tests would
// depend on a process-wide session default — the cross-egress shared id the
// router no longer produces (2026-10-23).
type leaseStampTransport struct {
	base http.RoundTripper
	ekey string
}

func (t leaseStampTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	if ses := getSessionPool().NextFor(t.ekey); ses != "" {
		setEgressSession(c.Header, ses)
	}
	resp, err := t.base.RoundTrip(c)
	if resp != nil {
		resp.Request = c // sessionGateID reads res.Request
	}
	return resp, err
}

// poolLeaseTransport is the minimal TransportProvider a pool lease needs: a
// client whose transport stamps the egress session (as the real pool path
// does) plus a non-empty, non-"direct-fallback" egress name, so the router's
// isPoolLease branch runs. No pool internals involved.
type poolLeaseTransport struct{ proxy string }

func (t *poolLeaseTransport) Name() string { return "pool-lease" }
func (t *poolLeaseTransport) Acquire(_ context.Context, _ RequestContext, _ *provider.Provider) (*Lease, error) {
	cl := &http.Client{Timeout: 10 * time.Second, Transport: leaseStampTransport{base: http.DefaultTransport, ekey: t.proxy}}
	return &Lease{Client: cl, Proxy: t.proxy}, nil
}

const gateTerminalBody = `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`

func opencodeProvider(t *testing.T, url string) *provider.Provider {
	t.Helper()
	return &provider.Provider{
		Name:     "opencode",
		Type:     "openai-compatible",
		BaseURL:  url,
		AuthMode: "none",
	}
}

// TestGateTerminal403ArmsSessionStreak proves the gateTerminalHeader path of
// tryProvider really marks the served session: one terminal gate 403 must arm
// the SessionPool streak, not burn the id (the gate is egress-correlated and
// often model-scoped, so a single 403 proves nothing).
func TestGateTerminal403ArmsSessionStreak(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set(gateTerminalHeader, "1")
		w.Header().Set(sessionGateEgressHeader, testGateEgress)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(gateTerminalBody))
	}))
	defer srv.Close()

	sp := seedSessionPool(t, "ses_aaaaaaaaaaaaaaaaaaaaaaaaa")
	r := New(map[string]*provider.Provider{"opencode": opencodeProvider(t, srv.URL)},
		WithTransport(&poolLeaseTransport{proxy: "http://127.0.0.1:8888"}))
	_, err := r.Call(context.Background(), "muse-spark-1.3-contributor-free", []byte(`{"model":"muse-spark-1.3-contributor-free"}`), false)
	if err == nil {
		t.Fatal("Call succeeded, want honest terminal 403")
	}
	var statusErr *UpstreamStatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusForbidden {
		t.Fatalf("err = %v, want 403 UpstreamStatusError", err)
	}
	if !strings.Contains(err.Error(), gateTerminalMarker) {
		t.Fatalf("gate marker lost: %v", err)
	}
	if hits != 1 {
		t.Fatalf("terminal gate must cost exactly 1 upstream call, got %d", hits)
	}

	sp.mu.Lock()
	const id = "ses_aaaaaaaaaaaaaaaaaaaaaaaaa"
	_, armed := sp.gateHits[streakKey(id, testGateEgress)]
	_, dead := sp.dead[id]
	sp.mu.Unlock()
	if !armed {
		t.Fatalf("gateHits[%s] not armed, want streak armed, not burned", id)
	}
	if dead {
		t.Fatal("first gate must NOT burn the session id")
	}
	if got := sp.NextFor("http://127.0.0.1:8888"); got != id {
		t.Fatalf("egress must stay pinned to the live id after one gate, got %q", got)
	}
}

// TestPeekedGate403BurnsOnSecondConsecutive exercises the peeked account-gate
// branch (403 whose body is a gate but carries no gateTerminalHeader, so the
// outer loop peeks it itself) and the streak rule end-to-end: the first 403
// only arms the streak, the SECOND consecutive one burns the id, and the
// harvest pool rotates the egress to the next live id.
func TestPeekedGate403BurnsOnSecondConsecutive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(sessionGateEgressHeader, testGateEgress)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(gateTerminalBody))
	}))
	defer srv.Close()

	first, second := "ses_bbbbbbbbbbbbbbbbbbbbbbbbb", "ses_ccccccccccccccccccccccccc"
	sp := seedSessionPool(t, first, second)
	r := New(map[string]*provider.Provider{"opencode": opencodeProvider(t, srv.URL)},
		WithTransport(&poolLeaseTransport{proxy: "http://127.0.0.1:8889"}))
	callGate := func(n int) *UpstreamStatusError {
		t.Helper()
		_, err := r.Call(context.Background(), "muse-spark-1.3-contributor-free", []byte(`{"model":"muse-spark-1.3-contributor-free"}`), false)
		if err == nil {
			t.Fatalf("call %d: Call succeeded, want honest terminal 403", n)
		}
		var statusErr *UpstreamStatusError
		if !errors.As(err, &statusErr) || statusErr.Status != http.StatusForbidden {
			t.Fatalf("call %d: err = %v, want 403 UpstreamStatusError", n, err)
		}
		if !strings.Contains(err.Error(), gateTerminalMarker) {
			t.Fatalf("call %d: peeked gate must carry the terminal marker, got %v", n, err)
		}
		return statusErr
	}
	callGate(1)
	// The served id is whichever the egress got pinned to (spread is hashed,
	// not "newest first"): read it back instead of assuming the seed order.
	served := sp.NextFor("http://127.0.0.1:8889")
	other := first
	if served == first {
		other = second
	}
	if served != first && served != second {
		t.Fatalf("served id = %q, want one of the seeded ids", served)
	}
	sp.mu.Lock()
	_, armed1 := sp.gateHits[streakKey(served, testGateEgress)]
	_, dead := sp.dead[served]
	sp.mu.Unlock()
	if !armed1 || dead {
		t.Fatalf("after the 1st peeked gate: armed=%v dead=%v, want streak armed, not burned", armed1, dead)
	}

	callGate(2)
	sp.mu.Lock()
	_, armed2 := sp.gateHits[streakKey(served, testGateEgress)]
	_, dead = sp.dead[served]
	sp.mu.Unlock()
	if !dead {
		t.Fatalf("second consecutive gate must burn %s (armed=%v dead=%v)", served, armed2, dead)
	}
	if armed2 {
		t.Fatalf("gateHits[%s] still armed, want burned streak deleted", served)
	}
	if got := sp.NextFor("http://127.0.0.1:8889"); got != other {
		t.Fatalf("after the burn the pool must rotate to the next live id %q, got %q", other, got)
	}
}

// TestGate403StreakClearedBy200 pins the load-bearing pairing: a 200 between
// two gates clears the streak, so an unrelated later gate cannot burn a
// session that just proved itself alive on some egress.
func TestGate403StreakClearedBy200(t *testing.T) {
	const id = "ses_eeeeeeeeeeeeeeeeeeeeeeeeee"
	sp := seedSessionPool(t, id)
	res := gateResponse(t, id)
	noteSessionGate(sp, res)
	sp.mu.Lock()
	if _, armed := sp.gateHits[streakKey(id, testGateEgress)]; !armed {
		sp.mu.Unlock()
		t.Fatal("first gate must arm the streak")
	}
	sp.mu.Unlock()

	// ...a 200 in between (the tryProvider success path)...
	okRes := gateResponse(t, id)
	sp.MarkGateHit(sessionGateID(okRes), sessionGateEgress(okRes), true)
	// ...and the next gate must not burn: the streak was cleared.
	noteSessionGate(sp, res)
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if _, dead := sp.dead[id]; dead {
		t.Fatal("a 200 between gates must clear the streak, not burn the id")
	}
	if _, armed := sp.gateHits[streakKey(id, testGateEgress)]; !armed {
		t.Fatalf("gateHits[%s] not armed, want re-armed by the post-200 gate", id)
	}
}

// TestSecondConsecutiveGateBurnsWithout200 proves the burn half of the streak
// rule goes through the same helper: no 200 in between, second gate kills.
func TestSecondConsecutiveGateBurnsWithout200(t *testing.T) {
	const id = "ses_ffffffffffffffffffffffffff"
	sp := seedSessionPool(t, id)
	res := gateResponse(t, id)
	noteSessionGate(sp, res)
	noteSessionGate(sp, res)
	sp.mu.Lock()
	_, dead := sp.dead[id]
	sp.mu.Unlock()
	if !dead {
		t.Fatalf("two consecutive gates must burn %s", id)
	}
	// Burning the only id must NOT strand the egress on an empty session
	// (2026-10-21): "" made the router fall back to a generated session, which
	// the gate always rejects — the pool then stayed dead until a process
	// restart cleared `dead`. Rotation now hands the burned id back as a
	// half-open probe, so the egress keeps a real id in flight.
	if got := sp.NextFor("http://127.0.0.1:8890"); got != id {
		t.Fatalf("NextFor after burn = %q, want the resurrection probe %q", got, id)
	}
}

// TestNoteSessionGateNoSessionIsNoop keeps the helper silent for detached
// responses and non-opencode upstreams (no session header): nothing must be
// recorded, and the pool must not be asked about an empty id.
func TestNoteSessionGateNoSessionIsNoop(t *testing.T) {
	sp := seedSessionPool(t, "ses_gggggggggggggggggggggggggg")
	for _, res := range []*http.Response{
		nil,
		{StatusCode: http.StatusForbidden, Header: make(http.Header)},
		func() *http.Response {
			req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader("{}"))
			return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Request: req}
		}(),
	} {
		noteSessionGate(sp, res)
	}
	noteSessionGate(nil, gateResponse(t, "ses_gggggggggggggggggggggggggg"))
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.gateHits) != 0 || len(sp.dead) != 0 {
		t.Fatalf("gateHits=%v dead=%v, want nothing recorded without a session id", sp.gateHits, sp.dead)
	}
}

// gateResponse builds a response whose Request carries the session id, exactly
// as setOpencodeHeaders stamps it on the upstream request (so res.Request
// carries it back into tryProvider). egress (optional) is the ipKey the pool
// stamps on gate-terminal responses via X-Pool-Egress.
// testGateEgress is the egress stamped on gate verdicts in tests: the gate
// streak is keyed on (session, egress), so a verdict without one arms nothing.
const testGateEgress = "2a09:bac5:c39b:2628::3cd:31"

func gateResponse(t *testing.T, session string, egress ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-opencode-session", session)
	res := &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: http.NoBody, Request: req}
	ek := testGateEgress
	if len(egress) > 0 && egress[0] != "" {
		ek = egress[0]
	}
	res.Header.Set(sessionGateEgressHeader, ek)
	return res
}

// TestGateStreakKeysBySessionAndEgress pins the egress-keyed contract: the
// gate is egress-correlated, so gates on DIFFERENT egresses must not burn the
// shared session id, while a second gate on the SAME egress does.
func TestGateStreakKeysBySessionAndEgress(t *testing.T) {
	const id = "ses_1111222233334444555566667"
	sp := seedSessionPool(t, id)
	noteSessionGate(sp, gateResponse(t, id, "2a09:bac1::1"))
	noteSessionGate(sp, gateResponse(t, id, "2a09:bac1::2"))
	sp.mu.Lock()
	_, deadA := sp.dead[id]
	sp.mu.Unlock()
	if deadA {
		t.Fatal("gates on different egresses must NOT burn the shared session id")
	}
	// Second gate on the FIRST egress still burns: repeat evidence is real.
	noteSessionGate(sp, gateResponse(t, id, "2a09:bac1::1"))
	sp.mu.Lock()
	_, deadB := sp.dead[id]
	sp.mu.Unlock()
	if !deadB {
		t.Fatal("second gate on the same egress must burn the id")
	}
}

// TestGateStreakExpiresAfterTTL: a stale arm (> gateStreakTTL) says nothing
// about now, so a late second gate must re-arm, not burn.
func TestGateStreakExpiresAfterTTL(t *testing.T) {
	const id = "ses_77778888999900001111222234"
	sp := seedSessionPool(t, id)
	noteSessionGate(sp, gateResponse(t, id))
	// Move the clock past the TTL: a late second gate must re-arm, not burn.
	sp.Now = func() time.Time { return time.Now().Add(gateStreakTTL + time.Second) }
	noteSessionGate(sp, gateResponse(t, id))
	sp.mu.Lock()
	_, dead := sp.dead[id]
	sp.mu.Unlock()
	if dead {
		t.Fatal("a second gate outside gateStreakTTL must NOT burn the id")
	}
}

// TestBurnRevivedBy200: the in-process burn is not a death sentence — a 200
// anywhere proves the id alive and revives it into the pick.
func TestBurnRevivedBy200(t *testing.T) {
	const id = "ses_3333444455556666777788889"
	sp := seedSessionPool(t, id)
	noteSessionGate(sp, gateResponse(t, id))
	noteSessionGate(sp, gateResponse(t, id))
	sp.mu.Lock()
	_, dead := sp.dead[id]
	sp.mu.Unlock()
	if !dead {
		t.Fatal("two consecutive gates must burn for the test to prove revival")
	}
	okRes := gateResponse(t, id)
	sp.MarkGateHit(sessionGateID(okRes), sessionGateEgress(okRes), true)
	sp.mu.Lock()
	_, deadAfter := sp.dead[id]
	sp.mu.Unlock()
	if deadAfter {
		t.Fatal("a 200 must revive a burned id (burn is egress/minute-scoped, not terminal)")
	}
}
