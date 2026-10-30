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

// TestPoolRouterExhaustedReturnsHonest429 wires a Router through a PoolTransport
// whose every backend 429s and asserts the router surfaces a 503 tail —
// never 429 (OMP kills the queue on 429) and never a fabricated
// "repeated proxy lease" transport error.
func TestPoolRouterExhaustedReturnsHonest429(t *testing.T) {
	t.Setenv("OPENCODE_TRANSPORT", "off") // pool lane, not the Bun sidecar
	bad, _ := fakeForwardProxyHits(t, http.StatusTooManyRequests)
	defer bad.Close()

	api := &fakePoolAPI{leases: []string{bad.URL, bad.URL, bad.URL, bad.URL, bad.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Nanosecond}
	prov := &provider.Provider{
		Name:     "opencode",
		Type:     "openai-compatible",
		BaseURL:  "http://upstream/v1",
		AuthMode: "none",
	}

	oldDeadline := poolTryDeadline
	poolTryDeadline = 3 * time.Second
	defer func() { poolTryDeadline = oldDeadline }()
	r := New(map[string]*provider.Provider{"opencode": prov}, WithTransport(pt))
	_, err := r.Call(context.Background(), "deepseek-v4-flash-free", []byte(`{"model":"deepseek-v4-flash-free"}`), false)
	if err == nil {
		t.Fatal("Call succeeded, want 503 tail error")
	}
	var statusErr *UpstreamStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v, want *UpstreamStatusError", err)
	}
	if statusErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 tail (no 429 outward, no repeated-proxy-lease error)", statusErr.Status)
	}
}

// TestPoolRouterSingleAttemptWhenSelfRotating verifies a self-rotating
// transport is given exactly one external attempt: an exhausted pool must
// surface the honest upstream status instead of a second acquire hitting the
// repeated-lease guard.
func TestPoolRouterSingleAttemptWhenSelfRotating(t *testing.T) {
	ok, _ := fakeForwardProxyHits(t, http.StatusOK)
	defer ok.Close()

	api := &fakePoolAPI{leases: []string{ok.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1}
	prov := &provider.Provider{
		Name:     "opencode",
		Type:     "openai-compatible",
		BaseURL:  "http://upstream/v1",
		AuthMode: "none",
	}

	r := New(map[string]*provider.Provider{"opencode": prov}, WithTransport(pt))
	res, err := r.Call(context.Background(), "deepseek-v4-flash-free", []byte(`{"model":"deepseek-v4-flash-free"}`), false)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	defer res.Resp.Body.Close()
	if res.Resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Resp.StatusCode)
	}
	api.mu.Lock()
	n := api.acquireN
	api.mu.Unlock()
	if n != 1 {
		t.Fatalf("acquire called %d times, want exactly 1 (single self-rotating attempt)", n)
	}
}
func TestPoolGateOuterNoReswap(t *testing.T) {
	t.Setenv("OPENCODE_TRANSPORT", "off") // pool lane, not the Bun sidecar
	// P0: an account-gate 403 (FreeTier) must surface honestly with NO outer
	// re-swaps: inner swaps a few egresses (gate is egress-correlated,
	// 2026-09-18: same account+model+minute, one egress 200 while others
	// 403), then goes terminal with the gate reason instead of "pool
	// saturated" 503 once the gate budget is spent.
	const gateBody = `{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}`
	hits := 0
	var mu sync.Mutex
	var srvs []*httptest.Server
	var leases []string
	for range 10 {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(gateBody))
		}))
		defer s.Close()
		srvs = append(srvs, s)
		leases = append(leases, s.URL)
	}
	_ = srvs
	api := &fakePoolAPI{leases: leases}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Nanosecond}
	prov := &provider.Provider{
		Name:     "opencode",
		Type:     "openai-compatible",
		BaseURL:  "http://upstream/v1",
		AuthMode: "none",
	}
	r := New(map[string]*provider.Provider{"opencode": prov}, WithTransport(pt))
	// Gate-shaped request (canonical head + gate pair + stream:true): only such
	// a body makes a 403 an EGRESS verdict. A shape-less body is answered 403
	// by the gate itself and must never be swapped (see
	// TestPoolWeakShapeGateNoEgressBlame).
	_, err := r.Call(context.Background(), "deepseek-v4-flash-free", gateProbeBody("deepseek-v4-flash-free"), true)
	if err == nil {
		t.Fatal("Call succeeded, want honest 403 gate error")
	}
	var statusErr *UpstreamStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v, want *UpstreamStatusError", err)
	}
	if statusErr.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want honest 403 (never 503 for a terminal gate)", statusErr.Status)
	}
	if !strings.Contains(err.Error(), "account gate:") {
		t.Fatalf("gate error must carry the upstream reason, got %v", err)
	}
	mu.Lock()
	h := hits
	mu.Unlock()
	// Inner swaps gate-rejected egresses up to the gate budget, then — for a
	// windowed FreeTier body — waits the 10s park out and re-sweeps up to
	// maxGateResweeps times (2026-10-14: the verdict is a per-IP window, not
	// an account ban; waiting converts closed windows into 200s). ZERO outer
	// re-swaps either way, and the warmup count stays bounded by
	// (1+maxGateSwaps)*(1+maxGateResweeps) — never a full-sweep storm.
	maxWarmups := (1 + maxGateSwaps) * (1 + maxGateResweeps)
	if h < 2 || h > maxWarmups {
		t.Fatalf("gate must cost 2..%d upstream warmups, got %d", maxWarmups, h)
	}
}

func TestPoolGateThenNonRegionKeepsGate(t *testing.T) {
	gateBody := `{"type":"error","error":{"type":"FreeTierError","message":"free tier can only be used from within OpenCode"}}`
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(gateBody))
	}))
	defer gate.Close()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer plain.Close()
	api := &fakePoolAPI{leases: []string{gate.URL, plain.URL, plain.URL, plain.URL, plain.URL, plain.URL, plain.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	prov := &provider.Provider{Name: "opencode", Type: "openai-compatible", BaseURL: "http://upstream/v1", AuthMode: "none"}
	r := New(map[string]*provider.Provider{"opencode": prov}, WithTransport(pt))
	_, err := r.Call(context.Background(), "deepseek-v4-flash-free", []byte(`{"model":"deepseek-v4-flash-free"}`), false)
	if err == nil {
		t.Fatal("Call succeeded, want honest 403 gate error")
	}
	var statusErr *UpstreamStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v, want *UpstreamStatusError", err)
	}
	if statusErr.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want honest 403 (gate must survive non-region 403)", statusErr.Status)
	}
	if !strings.Contains(err.Error(), gateTerminalMarker) {
		t.Fatalf("gate marker lost after non-region 403, got %v", err)
	}
}

func TestPoolRouterRegionExhaustedNoReswap(t *testing.T) {
	// P0: an inner decided sweep (synthetic region verdict, marked with
	// regionExhaustedHeader) must surface honestly — the outer loop must
	// NOT run up to 5 more full sweeps on it (60 warmups on a decided 403).
	regionBody := `{"error":{"type":"invalid_request_error","code":"region_error","message":"Region not supported"}}`
	hits := 0
	var srvs []*httptest.Server
	var leases []string
	for range 10 {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(regionBody))
		}))
		defer s.Close()
		srvs = append(srvs, s)
		leases = append(leases, s.URL)
	}
	_ = srvs
	api := &fakePoolAPI{leases: leases}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Nanosecond}
	prov := &provider.Provider{
		Name:        "opencode",
		Type:        "openai-compatible",
		BaseURL:     "http://upstream/v1",
		AuthMode:    "none",
		ErrorPolicy: map[string]string{"403": "none"},
	}
	r := New(map[string]*provider.Provider{"opencode": prov}, WithTransport(pt))
	before := hits
	_, err := r.Call(context.Background(), "deepseek-v4-flash-free", []byte(`{"model":"deepseek-v4-flash-free"}`), false)
	if err == nil {
		t.Fatal("Call succeeded, want honest 403")
	}
	var statusErr *UpstreamStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v, want *UpstreamStatusError", err)
	}
	if statusErr.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want honest 403", statusErr.Status)
	}
	// One sweep only: 10 warmups + initial lease, never 5 more outer sweeps.
	if got := hits - before; got > 12 {
		t.Fatalf("outer re-swapped a decided sweep: %d upstream warmups, want <= 12", got)
	}
}

// TestPoolWeakShapeGateNoEgressBlame pins the shape verdict: a 403 produced by
// a request that never carried the free-tier shape is the gate answering the
// BODY, not the egress. It must cost exactly one upstream call, must not
// ban/park the egress and must not arm the session streak — before this guard
// a single GUI combo/health click re-registered the pool.
//
// The weak body can no longer be expressed with `"stream": false`: the chat
// lane forces stream:true on the wire (chat_destream.go) and the verdict counts
// it, so a plain non-stream JSON body IS gate-shaped now. The remaining case the
// guard covers is a body the shaping cannot fix — invalid JSON never gets the
// canonical head or the gate tool pair, so its 403 must still be blamed on the
// body.
func TestPoolWeakShapeGateNoEgressBlame(t *testing.T) {
	const gateBody = `{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}`
	hits := 0
	var mu sync.Mutex
	var leases []string
	for range 6 {
		st := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(gateBody))
		}))
		defer st.Close()
		leases = append(leases, st.URL)
	}
	api := &fakePoolAPI{leases: leases}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 3, Rounds: 1, Cooldown: time.Nanosecond}
	prov := &provider.Provider{
		Name:     "opencode",
		Type:     "openai-compatible",
		BaseURL:  "http://upstream/v1",
		AuthMode: "none",
	}
	r := New(map[string]*provider.Provider{"opencode": prov}, WithTransport(pt))
	// Invalid JSON: neither the canonical head nor the gate tool pair can be
	// injected, so the shape verdict stays false.
	_, err := r.Call(context.Background(), "deepseek-v4-flash-free",
		[]byte(`{"model":"deepseek-v4-flash-free","messages":[{"role":"user","content":"hi"}]} #`), false)
	if err == nil {
		t.Fatal("Call succeeded, want the honest 403")
	}
	mu.Lock()
	h := hits
	mu.Unlock()
	if h != 1 {
		t.Fatalf("shape-less 403 cost %d upstream calls, want exactly 1 (no swap storm)", h)
	}
	if n := len(pt.bad); n != 0 {
		t.Fatalf("bad-map after a shape-less 403 = %d entries, want 0 (egress not blamed)", n)
	}
}
