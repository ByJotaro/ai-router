package route

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// free429Body is the free-tier quota verdict: unlike the account gate (403) it
// is bound to (session, egress), so the router retires the id and replays the
// SAME request instead of showing the caller a 429.
const free429Body = `{"type":"error","error":{"type":"FreeUsageLimitError","message":"rate limit"}}`

// sidRecorder is an upstream that answers per attempt: the verdict for the
// n-th call and a record of the session id each call carried.
type sidRecorder struct {
	mu     sync.Mutex
	sids   []string
	bodies []string
	reply  func(n int) (int, string)
}

func (s *sidRecorder) handle(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, 4096)
	n, _ := r.Body.Read(body)
	s.mu.Lock()
	s.sids = append(s.sids, r.Header.Get("x-opencode-session"))
	s.bodies = append(s.bodies, string(body[:n]))
	idx := len(s.sids)
	s.mu.Unlock()
	status, payload := s.reply(idx)
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "3600")
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(payload))
}

func (s *sidRecorder) snapshot() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sids...), append([]string(nil), s.bodies...)
}

func newSwapRouter(t *testing.T, url string) *Router {
	t.Helper()
	// proxy "" keeps the request on lease.Client: a non-empty egress makes
	// doRequest hand the call to the optional Bun sidecar (buntransport.Do),
	// which is neither needed nor reachable in a unit test.
	return New(map[string]*provider.Provider{"opencode": opencodeProvider(t, url)},
		WithTransport(&poolLeaseTransportFor{key: directHomeIPKey}))
}

// poolLeaseTransportFor is poolLeaseTransport with an explicit stamp key and no
// egress name, so the lease stays on the Go HTTP path.
type poolLeaseTransportFor struct{ key string }

func (t *poolLeaseTransportFor) Name() string { return "pool-lease-test" }
func (t *poolLeaseTransportFor) Acquire(_ context.Context, _ RequestContext, _ *provider.Provider) (*Lease, error) {
	cl := &http.Client{Timeout: 10 * time.Second, Transport: leaseStampTransport{base: http.DefaultTransport, ekey: t.key}}
	return &Lease{Client: cl}, nil
}

// TestFree429SwapsSessionInRequest is the regression for the "this session is
// always 429 while others work" symptom: a spent id must cost an internal
// rotation, not a client-visible 429.
func TestFree429SwapsSessionInRequest(t *testing.T) {
	rec := &sidRecorder{reply: func(n int) (int, string) {
		if n == 1 {
			return http.StatusTooManyRequests, free429Body
		}
		return http.StatusOK, `{"id":"ok","object":"chat.completion","choices":[]}`
	}}
	srv := httptest.NewServer(http.HandlerFunc(rec.handle))
	defer srv.Close()

	const idA, idB, idC = "ses_aaaaaaaaaaaaaaaaaaaaaaaaa", "ses_bbbbbbbbbbbbbbbbbbbbbbbbb", "ses_ccccccccccccccccccccccccc"
	sp := seedSessionPool(t, idA, idB, idC)

	r := newSwapRouter(t, srv.URL)
	fallbacks := 0
	r.OnEvent = func(typ string, _ any) {
		if typ == "ai.fallback" {
			fallbacks++
		}
	}
	res, err := r.Call(context.Background(), "muse-spark-1.3-contributor-free",
		[]byte(`{"model":"muse-spark-1.3-contributor-free"}`), false)
	if err != nil {
		sids, bodies := rec.snapshot()
		t.Fatalf("call must survive a burned session id: %v (upstream hits=%d sids=%v bodies=%v)", err, len(sids), sids, bodies)
	}
	if res.Resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Resp.StatusCode)
	}
	_ = res.Resp.Body.Close()

	sids, _ := rec.snapshot()
	if len(sids) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (burned id + one live id)", len(sids))
	}
	if sids[0] == sids[1] || sids[1] == "" {
		t.Fatalf("session ids on the wire = %v, want a different id per attempt", sids)
	}
	for _, id := range sids {
		if !strings.HasPrefix(id, "ses_") {
			t.Fatalf("wire id %q is not a harvested session", id)
		}
	}

	sp.mu.Lock()
	_, deadFirst := sp.dead[sids[0]]
	_, deadSecond := sp.dead[sids[1]]
	deadCount := len(sp.dead)
	sp.mu.Unlock()
	if !deadFirst {
		t.Fatalf("the 429 id %q must be retired so no other conversation inherits it", sids[0])
	}
	if deadSecond {
		t.Fatal("the id that answered 200 must stay live")
	}
	if deadCount != 1 {
		t.Fatalf("dead ids = %d, want exactly the one that answered 429", deadCount)
	}
	if live := sp.LiveCount(); live != 2 {
		t.Fatalf("live ids = %d, want 2 of the 3 seeded", live)
	}
	if fallbacks != 0 {
		t.Fatalf("ai.fallback fired %d times for an internal swap, want 0", fallbacks)
	}
	if got := sp.NextFor(directHomeIPKey); got != sids[1] {
		t.Fatalf("egress pin = %q, want the id that answered 200 (%q)", got, sids[1])
	}
}

// TestFree429SwapIsBounded: a provider that 429s every session must not loop.
// The budget is the base attempt count plus maxSessionSwaps, and only that many
// distinct ids may be touched.
func TestFree429SwapIsBounded(t *testing.T) {
	rec := &sidRecorder{reply: func(int) (int, string) {
		return http.StatusTooManyRequests, free429Body
	}}
	srv := httptest.NewServer(http.HandlerFunc(rec.handle))
	defer srv.Close()

	const idA, idB, idC = "ses_ddddddddddddddddddddddddd", "ses_eeeeeeeeeeeeeeeeeeeeeeeee", "ses_fffffffffffffffffffffffff"
	seedSessionPool(t, idA, idB, idC)

	r := newSwapRouter(t, srv.URL)
	_, err := r.Call(context.Background(), "muse-spark-1.3-contributor-free",
		[]byte(`{"model":"muse-spark-1.3-contributor-free"}`), false)
	if err == nil {
		t.Fatal("call succeeded, want an honest 429")
	}
	var statusErr *UpstreamStatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want 429 UpstreamStatusError", err)
	}

	sids, _ := rec.snapshot()
	// 4 base attempts (pool transport) + 2 session swaps, never more.
	if len(sids) != 6 {
		t.Fatalf("upstream calls = %d, want 6 (4 attempts + 2 swaps)", len(sids))
	}
	seen := map[string]bool{}
	for _, s := range sids {
		seen[s] = true
	}
	if len(seen) < 3 {
		t.Fatalf("distinct ids touched = %d (%v), want at least 3 (one per swap)", len(seen), seen)
	}
	for _, id := range []string{idA, idB} {
		if !seen[id] {
			t.Fatalf("swap did not reach %q: %v", id, seen)
		}
	}
}

// TestMintedSessionIsNotCachedBy429: with an empty harvest pool the gate still
// needs SOME session, so the router mints a throwaway id. A 429 on it must
// retire nothing from the (empty) pool and the call must stay bounded.
func TestMintedSessionIsNotCachedBy429(t *testing.T) {
	rec := &sidRecorder{reply: func(int) (int, string) {
		return http.StatusTooManyRequests, free429Body
	}}
	srv := httptest.NewServer(http.HandlerFunc(rec.handle))
	defer srv.Close()

	sp := seedSessionPool(t) // empty harvest pool

	r := newSwapRouter(t, srv.URL)
	_, err := r.Call(context.Background(), "muse-spark-1.3-contributor-free",
		[]byte(`{"model":"muse-spark-1.3-contributor-free"}`), false)
	if err == nil {
		t.Fatal("call succeeded, want 429")
	}
	var statusErr *UpstreamStatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want 429", err)
	}
	sids, _ := rec.snapshot()
	if len(sids) == 0 || len(sids) > 6 {
		t.Fatalf("upstream calls = %d, want 1..6 (base attempts + swaps)", len(sids))
	}
	if got := sp.LiveCount(); got != 0 {
		t.Fatalf("live harvested ids = %d, want 0", got)
	}
}

// TestSessionSwapDoesNotFireOutsideOpencode: a 429 from a generic provider is a
// real rate limit, never a session verdict.
func TestSessionSwapDoesNotFireOutsideOpencode(t *testing.T) {
	rec := &sidRecorder{reply: func(int) (int, string) {
		return http.StatusTooManyRequests, free429Body
	}}
	srv := httptest.NewServer(http.HandlerFunc(rec.handle))
	defer srv.Close()

	const idA, idB = "ses_ggggggggggggggggggggggggg", "ses_hhhhhhhhhhhhhhhhhhhhhhhhh"
	sp := seedSessionPool(t, idA, idB)

	plain := &provider.Provider{Name: "plain", Type: "openai-compatible", BaseURL: srv.URL, AuthMode: "none"}
	r := New(map[string]*provider.Provider{"plain": plain},
		WithTransport(&poolLeaseTransport{proxy: testGateEgress}))
	_, err := r.Call(context.Background(), "some-model", []byte(`{"model":"some-model"}`), false)
	if err == nil {
		t.Fatal("call succeeded, want 429")
	}
	sp.mu.Lock()
	deadCount := len(sp.dead)
	sp.mu.Unlock()
	if deadCount != 0 {
		t.Fatalf("a non-opencode 429 retired %d session id(s), want 0", deadCount)
	}
	if live := sp.LiveCount(); live != 2 {
		t.Fatalf("live ids = %d, want the 2 seeded ones untouched", live)
	}
}
