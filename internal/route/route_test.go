package route

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

func fakeUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
}

func respBody(t *testing.T, res *Result) map[string]any {
	t.Helper()
	defer res.Resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(res.Resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFallbackToNextProvider(t *testing.T) {
	down := fakeUpstream(t, 500, "")
	defer down.Close()
	up := fakeUpstream(t, 200, `{"id":"ok","object":"chat.completion","choices":[]}`)
	defer up.Close()

	r := New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: down.URL, APIKeys: []string{"k1"}},
		"b": {Name: "b", Type: "openai-compatible", BaseURL: up.URL, APIKeys: []string{"k2"}},
	})
	fallbacks := false
	r.OnEvent = func(typ string, _ any) {
		if typ == "ai.fallback" {
			fallbacks = true
		}
	}
	res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Provider != "b" {
		t.Fatalf("want provider b, got %s", res.Provider)
	}
	m := respBody(t, res)
	if m["object"] != "chat.completion" {
		t.Fatalf("bad body: %v", m)
	}
	if !fallbacks {
		t.Fatal("expected ai.fallback event")
	}
}

func TestExplicitFallbackChain(t *testing.T) {
	down := fakeUpstream(t, 500, "")
	defer down.Close()
	up := fakeUpstream(t, 200, `{"id":"ok","choices":[]}`)
	defer up.Close()

	r := New(map[string]*provider.Provider{
		"main": {Name: "main", Type: "openai-compatible", BaseURL: down.URL, APIKeys: []string{"k1"}, Models: []string{"m"}, Fallback: []string{"alt"}},
		"alt":  {Name: "alt", Type: "openai-compatible", BaseURL: up.URL, APIKeys: []string{"k2"}, Models: []string{"m"}},
	})
	res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Provider != "alt" {
		t.Fatalf("want alt, got %s", res.Provider)
	}
}

func TestNoCandidates(t *testing.T) {
	r := New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", Models: []string{"other"}},
	})
	_, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err != ErrNoCandidates {
		t.Fatalf("want ErrNoCandidates, got %v", err)
	}
}

func TestKeyRotationOn401(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[]}`))
	}))
	defer srv.Close()

	r := New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: srv.URL, APIKeys: []string{"k1", "k2"}},
	})
	res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if calls < 2 {
		t.Fatalf("expected retry with second key, calls=%d", calls)
	}
	if res.Provider != "a" {
		t.Fatalf("want a, got %s", res.Provider)
	}
}

func Test429ExhaustsAllKeys(t *testing.T) {
	srv := fakeUpstream(t, 429, "")
	defer srv.Close()
	r := New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: srv.URL, APIKeys: []string{"k1", "k2"}},
	})
	_, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err == nil || !strings.Contains(err.Error(), "Too Many Requests") {
		t.Fatalf("want 429 error, got %v", err)
	}
}

func TestWildcardProviderServesAnyModel(t *testing.T) {
	srv := fakeUpstream(t, 200, `{"id":"ok","choices":[]}`)
	defer srv.Close()
	r := New(map[string]*provider.Provider{
		"any": {Name: "any", Type: "openai-compatible", BaseURL: srv.URL},
	})
	res, err := r.Call(context.Background(), "some-random-model", []byte(`{"model":"some-random-model"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Provider != "any" {
		t.Fatalf("want any, got %s", res.Provider)
	}
}

func TestModelRoutePrefersExactOverWildcard(t *testing.T) {
	wildcard := fakeUpstream(t, 200, `{"provider":"wildcard"}`)
	defer wildcard.Close()
	exact := fakeUpstream(t, 200, `{"provider":"exact"}`)
	defer exact.Close()
	r := New(map[string]*provider.Provider{
		"wild":  {Name: "wild", Type: "openai-compatible", BaseURL: wildcard.URL},
		"exact": {Name: "exact", Type: "openai-compatible", BaseURL: exact.URL, Models: []string{"m"}},
	})
	res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Resp.Body.Close()
	if res.Provider != "exact" {
		t.Fatalf("want exact provider, got %s", res.Provider)
	}
}

func TestModelRouteRejectsNonMatchingExplicitModels(t *testing.T) {
	r := New(map[string]*provider.Provider{
		"only": {Name: "only", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", Models: []string{"other"}},
	})
	_, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err != ErrNoCandidates {
		t.Fatalf("want ErrNoCandidates, got %v", err)
	}
}

func TestPinnedProviderModel(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"pinned","choices":[]}`))
	}))
	defer srv.Close()

	// Two providers serve "m"; pinned syntax must hit "beta" regardless of the
	// sorted-name ordering (alpha would normally win).
	r := New(map[string]*provider.Provider{
		"alpha": {Name: "alpha", Type: "openai-compatible", BaseURL: srv.URL, APIKeys: []string{"k"}, Models: []string{"m"}},
		"beta":  {Name: "beta", Type: "openai-compatible", BaseURL: srv.URL, APIKeys: []string{"k"}, Models: []string{"m"}},
	})
	res, err := r.Call(context.Background(), "beta/m", []byte(`{"model":"beta/m"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Provider != "beta" {
		t.Fatalf("want pinned provider beta, got %s", res.Provider)
	}
	if gotModel != "m" {
		t.Fatalf("want upstream model m, got %q", gotModel)
	}
	res.Resp.Body.Close()
}

func TestPinnedProviderNoSuchProvider(t *testing.T) {
	r := New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
	})
	_, err := r.Call(context.Background(), "nope/m", []byte(`{"model":"nope/m"}`), false)
	if err != ErrNoCandidates {
		t.Fatalf("want ErrNoCandidates, got %v", err)
	}
}

func TestPinnedProviderModelNotAllowed(t *testing.T) {
	r := New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", Models: []string{"other"}},
	})
	_, err := r.Call(context.Background(), "a/m", []byte(`{"model":"a/m"}`), false)
	if err != ErrNoCandidates {
		t.Fatalf("want ErrNoCandidates, got %v", err)
	}
}

func TestDeterministicProviderOrder(t *testing.T) {
	srvA := fakeUpstream(t, 200, `{"id":"a","choices":[]}`)
	defer srvA.Close()
	srvB := fakeUpstream(t, 200, `{"id":"b","choices":[]}`)
	defer srvB.Close()
	// Equal-standing providers must be tried in sorted name order every time,
	// not in random map-iteration order.
	r := New(map[string]*provider.Provider{
		"zeta":  {Name: "zeta", Type: "openai-compatible", BaseURL: srvB.URL, APIKeys: []string{"k"}, Models: []string{"m"}},
		"alpha": {Name: "alpha", Type: "openai-compatible", BaseURL: srvA.URL, APIKeys: []string{"k"}, Models: []string{"m"}},
	})
	for i := 0; i < 5; i++ {
		res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if res.Provider != "alpha" {
			t.Fatalf("want deterministic alpha, got %s", res.Provider)
		}
		res.Resp.Body.Close()
	}
}

// TestWithReasoningMaxAlias: GUI "max" maps to the family's real top level —
// muse-spark/mimo accept xhigh, "max" earns a live 400 (2026-09-10).
func TestWithReasoningMaxAlias(t *testing.T) {
	out := WithReasoning([]byte(`{"model":"muse-spark-1.3-contributor-free","messages":[]}`), "max")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["reasoning_effort"] != "xhigh" {
		t.Fatalf("muse max -> %v, want xhigh", m["reasoning_effort"])
	}
	out2 := WithReasoning([]byte(`{"model":"x-preview-f-free","messages":[]}`), "max")
	var m2 map[string]any
	if err := json.Unmarshal(out2, &m2); err != nil {
		t.Fatal(err)
	}
	if m2["reasoning_effort"] != "max" {
		t.Fatalf("x-preview max -> %v, want max", m2["reasoning_effort"])
	}
	// Client-explicit wins: never override.
	out3 := WithReasoning([]byte(`{"model":"muse-spark-1.3-contributor-free","reasoning_effort":"low"}`), "max")
	var m3 map[string]any
	if err := json.Unmarshal(out3, &m3); err != nil {
		t.Fatal(err)
	}
	if m3["reasoning_effort"] != "low" {
		t.Fatalf("explicit low -> %v, want low", m3["reasoning_effort"])
	}
}

// TestPublishedVendorPrefixedIDBeatsPin: a vendor-prefixed id that some
// provider publishes verbatim must not be hijacked by the "provider/model"
// pin when another provider owns the first segment (live 2026-10-17:
// kilocode publishes "openai/gpt-5" while provider "openai" allows "gpt-5").
func TestPublishedVendorPrefixedIDBeatsPin(t *testing.T) {
	type hit struct {
		provider string
		model    string
	}
	var hits []hit
	newSrv := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			hits = append(hits, hit{provider: name, model: req.Model})
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id":"ok","choices":[]}`))
		}))
	}
	paid := newSrv("openai")
	defer paid.Close()
	pub := newSrv("kilocode")
	defer pub.Close()

	wild := newSrv("wild")
	defer wild.Close()

	r := New(map[string]*provider.Provider{
		"openai":   {Name: "openai", Type: "openai-compatible", BaseURL: paid.URL, APIKeys: []string{"sk-test"}, Models: []string{"gpt-5", "gpt-4o"}},
		"kilocode": {Name: "kilocode", Type: "openai-compatible", BaseURL: pub.URL, APIKeys: []string{"k"}, Models: []string{"openai/gpt-5", "poolside/laguna-s-2.1:free"}},
		// Wildcard provider: serves anything, must never outrank the publisher
		// of a composite id (live: fireworks answering "pool: no transport").
		"wild": {Name: "wild", Type: "openai-compatible", BaseURL: wild.URL, APIKeys: []string{"k"}},
	}, WithProvidersOrder([]string{"wild", "openai", "kilocode"}))
	res, err := r.Call(context.Background(), "openai/gpt-5", []byte(`{"model":"openai/gpt-5"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	res.Resp.Body.Close()
	if res.Provider != "kilocode" {
		t.Fatalf("published id must ride its publisher, got %q", res.Provider)
	}
	if len(hits) != 1 || hits[0].model != "openai/gpt-5" {
		t.Fatalf("upstream must see the composite id, hits=%+v", hits)
	}

	// A composite nobody publishes still pins (normal provider/model
	// addressing), even when the first segment also names a provider.
	hits = nil
	res2, err := r.Call(context.Background(), "openai/gpt-4o", []byte(`{"model":"openai/gpt-4o"}`), false)
	if err != nil {
		t.Fatalf("call pin: %v", err)
	}
	res2.Resp.Body.Close()
	if res2.Provider != "openai" {
		t.Fatalf("unpublished composite must keep pinning, got %q", res2.Provider)
	}
	if len(hits) != 1 || hits[0].model != "gpt-4o" {
		t.Fatalf("pin strips the provider prefix upstream, hits=%+v", hits)
	}
}

// cancelRT reports a cancel-class transport error without the caller's
// context being cancelled — the exact shape the pool produces when it kills
// its own per-attempt context (transport_pool.go:4143-4148).
type cancelRT struct{}

func (cancelRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, context.Canceled }

type cancelLeaseTransport struct{}

func (cancelLeaseTransport) Name() string { return "cancel-lease" }

func (cancelLeaseTransport) Acquire(context.Context, RequestContext, *provider.Provider) (*Lease, error) {
	return &Lease{Client: &http.Client{Transport: cancelRT{}, Timeout: 2 * time.Second}, Proxy: "http://127.0.0.1:8889"}, nil
}

// TestCallNeverReturnsNilNilResult: an internal cancel (client ctx alive) used
// to leave Call returning (nil, nil) — every caller checks only err and then
// dereferences the result (gateway.go:454/520, hubmod.go:150).
func TestCallNeverReturnsNilNilResult(t *testing.T) {
	r := New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", APIKeys: []string{"k"}},
	}, WithTransport(cancelLeaseTransport{}))
	res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err == nil {
		if res == nil {
			t.Fatal("Call returned (nil, nil)")
		}
		t.Fatalf("Call returned a result for a cancelled attempt: %+v", res)
	}
	if res != nil {
		t.Fatalf("result must be nil with an error, got %+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled in chain", err)
	}
}

// TestProviderEditKeepsKeyHealthAndBreaker: saving a provider from the GUI
// rebuilds its pool/breaker. Live health must survive that rebuild — before
// the fix every save cleared 429 cooldowns/backoff and closed a provider's
// breaker, so the next request re-hit the burned budget (r20 F1).
func TestProviderEditKeepsKeyHealthAndBreaker(t *testing.T) {
	prov := func() *provider.Provider {
		// Fresh value each call: Add stores the pointer it is given.
		return &provider.Provider{Name: "a", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", APIKeys: []string{"k1", "k2"}}
	}
	r := New(map[string]*provider.Provider{"a": prov()})

	pl := r.pools["a"]
	if pl == nil {
		t.Fatal("no pool for provider a")
	}
	key, err := pl.Pick()
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	pl.Report(key, http.StatusTooManyRequests, "")
	if key.Failures == 0 || key.BackoffLevel == 0 {
		t.Fatalf("429 did not mark the key: failures=%d backoff=%d", key.Failures, key.BackoffLevel)
	}
	hash, failures, backoff := key.KeyHash(), key.Failures, key.BackoffLevel

	br := r.breakers["a"]
	if br == nil {
		t.Fatal("no breaker for provider a")
	}
	for i := 0; i < 8; i++ {
		br.RecordFailure("boom")
	}
	if br.Allow(time.Now()) {
		t.Fatal("breaker stayed closed after 8 failures")
	}

	r.Add(prov())

	pl2 := r.pools["a"]
	if pl2 == nil {
		t.Fatal("pool disappeared after Add")
	}
	k2 := pl2.KeyByHash(hash)
	if k2 == nil {
		t.Fatal("key hash lost across Add")
	}
	if k2.Failures != failures || k2.BackoffLevel != backoff {
		t.Fatalf("key health lost across Add: failures=%d backoff=%d, want %d/%d", k2.Failures, k2.BackoffLevel, failures, backoff)
	}
	if r.breakers["a"] != br {
		t.Fatal("breaker instance replaced by Add — open state lost")
	}
	if r.breakers["a"].Allow(time.Now()) {
		t.Fatal("breaker reopened after Add")
	}
}
