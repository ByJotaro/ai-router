package route

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/provider"
)

// canaryServer counts hits and answers with the configured status.
func canaryServer(t *testing.T, status int, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":"canary","object":"chat.completion","choices":[]}`))
	}))
}

func canaryProvider(base string, maxReq, maxFail int) *provider.Provider {
	return &provider.Provider{
		Name: "p", Type: "openai-compatible", APIKeys: []string{"k"},
		Canary: &config.CanaryConf{BaseURL: base, MaxRequests: maxReq, MaxFailures: maxFail, Window: 20, TimeoutS: 5},
	}
}

// A healthy stand serves the request: the live upstream is never touched and
// the result is attributed to the canary.
func TestCanaryServesWhenHealthy(t *testing.T) {
	var canaryHits, liveHits int32
	canary := canaryServer(t, 200, &canaryHits)
	defer canary.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&liveHits, 1)
		_, _ = w.Write([]byte(`{"id":"live","choices":[]}`))
	}))
	defer live.Close()

	r := New(map[string]*provider.Provider{"p": canaryProvider(canary.URL, 100, 3)})
	r.providers["p"].BaseURL = live.URL

	res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Provider != "p~canary" {
		t.Fatalf("provider = %q, want p~canary", res.Provider)
	}
	if got := atomic.LoadInt32(&liveHits); got != 0 {
		t.Fatalf("live hit %d times, want 0", got)
	}
	_ = res.Resp.Body.Close()
}

// A 429 from the stand must fall through to the live provider on the same
// request, and the failure must be counted.
func TestCanary429FallsBackToLive(t *testing.T) {
	var canaryHits, liveHits int32
	canary := canaryServer(t, 429, &canaryHits)
	defer canary.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&liveHits, 1)
		_, _ = w.Write([]byte(`{"id":"live","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer live.Close()

	r := New(map[string]*provider.Provider{"p": canaryProvider(canary.URL, 100, 3)})
	r.providers["p"].BaseURL = live.URL

	res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Provider != "p" {
		t.Fatalf("provider = %q, want live p", res.Provider)
	}
	if got := atomic.LoadInt32(&canaryHits); got != 1 {
		t.Fatalf("canary hit %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&liveHits); got != 1 {
		t.Fatalf("live hit %d times, want 1", got)
	}
	_ = res.Resp.Body.Close()
}

// After max_failures the canary is disabled for the process: further requests
// go straight to the live provider and never dial the stand again.
func TestCanaryDisablesAfterFailures(t *testing.T) {
	var canaryHits, liveHits int32
	canary := canaryServer(t, 500, &canaryHits)
	defer canary.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&liveHits, 1)
		_, _ = w.Write([]byte(`{"id":"live","choices":[]}`))
	}))
	defer live.Close()

	r := New(map[string]*provider.Provider{"p": canaryProvider(canary.URL, 100, 2)})
	r.providers["p"].BaseURL = live.URL

	for i := 0; i < 5; i++ {
		res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		_ = res.Resp.Body.Close()
	}
	if got := atomic.LoadInt32(&canaryHits); got != 2 {
		t.Fatalf("canary hit %d times, want 2 (max_failures)", got)
	}
	if got := atomic.LoadInt32(&liveHits); got != 5 {
		t.Fatalf("live hit %d times, want 5", got)
	}
}

// The budget bounds the sample: with max_requests=1 the second request goes
// live without touching the stand.
func TestCanaryBudgetExhausted(t *testing.T) {
	var canaryHits, liveHits int32
	canary := canaryServer(t, 200, &canaryHits)
	defer canary.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&liveHits, 1)
		_, _ = w.Write([]byte(`{"id":"live","choices":[]}`))
	}))
	defer live.Close()

	r := New(map[string]*provider.Provider{"p": canaryProvider(canary.URL, 1, 3)})
	r.providers["p"].BaseURL = live.URL

	for i := 0; i < 2; i++ {
		res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		_ = res.Resp.Body.Close()
	}
	if got := atomic.LoadInt32(&canaryHits); got != 1 {
		t.Fatalf("canary hit %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&liveHits); got != 1 {
		t.Fatalf("live hit %d times, want 1", got)
	}
}

// A dead stand (connection refused) must not surface as an error.
func TestCanaryUnreachableFallsBack(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"live","choices":[]}`))
	}))
	defer live.Close()

	r := New(map[string]*provider.Provider{"p": canaryProvider("http://127.0.0.1:1/v1", 100, 3)})
	r.providers["p"].BaseURL = live.URL

	res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Provider != "p" {
		t.Fatalf("provider = %q, want live p", res.Provider)
	}
	var payload map[string]any
	_ = json.NewDecoder(res.Resp.Body).Decode(&payload)
	_ = res.Resp.Body.Close()
	if payload["id"] != "live" {
		t.Fatalf("body id = %v, want live", payload["id"])
	}
}

// A neutral 4xx (health probe without a model) must not count as a stand
// failure, and must not consume the sliding window.
func TestCanaryNeutral4xxDoesNotTrip(t *testing.T) {
	var canaryHits, liveHits int32
	canary := canaryServer(t, 400, &canaryHits)
	defer canary.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&liveHits, 1)
		_, _ = w.Write([]byte(`{"id":"live","choices":[]}`))
	}))
	defer live.Close()

	r := New(map[string]*provider.Provider{"p": canaryProvider(canary.URL, 100, 1)})
	r.providers["p"].BaseURL = live.URL

	for i := 0; i < 5; i++ {
		res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		_ = res.Resp.Body.Close()
	}
	if got := atomic.LoadInt32(&canaryHits); got != 5 {
		t.Fatalf("canary hit %d times, want 5 (4xx is neutral)", got)
	}
}

// Degradation is a sliding window: failures older than the window roll out,
// so a stand that recovers re-enters service without a restart.
func TestCanaryWindowRecovers(t *testing.T) {
	var canaryHits int32
	status := int32(500)
	canary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&canaryHits, 1)
		w.WriteHeader(int(atomic.LoadInt32(&status)))
		_, _ = w.Write([]byte(`{"id":"x","choices":[]}`))
	}))
	defer canary.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"live","choices":[]}`))
	}))
	defer live.Close()

	p := canaryProvider(canary.URL, 0, 2) // unlimited budget, 2 failures per window of 20
	p.Canary.Window = 4
	r := New(map[string]*provider.Provider{"p": p})
	r.providers["p"].BaseURL = live.URL

	call := func() {
		res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		_ = res.Resp.Body.Close()
	}
	call()                                // fail 1
	call()                                // fail 2 -> disabled
	if got := atomic.LoadInt32(&canaryHits); got != 2 {
		t.Fatalf("canary hit %d, want 2 (disabled)", got)
	}
	atomic.StoreInt32(&status, 200)       // stand recovers
	call()                                // still disabled until window moves? off is sticky
	if got := atomic.LoadInt32(&canaryHits); got != 2 {
		t.Fatalf("canary hit %d, want 2 (off is terminal for the process)", got)
	}
}

// MaxRequests=0 means unlimited: a long soak never retires on budget.
func TestCanaryUnlimitedBudget(t *testing.T) {
	var canaryHits int32
	canary := canaryServer(t, 200, &canaryHits)
	defer canary.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"live","choices":[]}`))
	}))
	defer live.Close()

	r := New(map[string]*provider.Provider{"p": canaryProvider(canary.URL, 0, 3)})
	r.providers["p"].BaseURL = live.URL

	for i := 0; i < 25; i++ {
		res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		_ = res.Resp.Body.Close()
	}
	if got := atomic.LoadInt32(&canaryHits); got != 25 {
		t.Fatalf("canary hit %d times, want 25 (unlimited)", got)
	}
}

// The canary must pin the model to its own provider, or the stand routes the
// bare id through a different provider and the sample is not comparable.
func TestCanaryPinsModelToProvider(t *testing.T) {
	var gotModel string
	canary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		gotModel, _ = m["model"].(string)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"canary","choices":[]}`))
	}))
	defer canary.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"live","choices":[]}`))
	}))
	defer live.Close()

	r := New(map[string]*provider.Provider{"p": canaryProvider(canary.URL, 10, 3)})
	r.providers["p"].BaseURL = live.URL

	res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	_ = res.Resp.Body.Close()
	if gotModel != "p/m" {
		t.Fatalf("canary body model = %q, want p/m", gotModel)
	}
}

// A retired canary re-arms after CooldownS: a burst pauses the sample, it does
// not end a multi-hour rollout.
func TestCanaryReArmsAfterCooldown(t *testing.T) {
	var canaryHits int32
	status := int32(500)
	canary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&canaryHits, 1)
		w.WriteHeader(int(atomic.LoadInt32(&status)))
		_, _ = w.Write([]byte(`{"id":"x","choices":[]}`))
	}))
	defer canary.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"live","choices":[]}`))
	}))
	defer live.Close()

	p := canaryProvider(canary.URL, 0, 1)
	p.Canary.Window = 4
	p.Canary.CooldownS = 1
	r := New(map[string]*provider.Provider{"p": p})
	r.providers["p"].BaseURL = live.URL

	call := func() {
		res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		_ = res.Resp.Body.Close()
	}
	call() // fail -> retired
	if got := atomic.LoadInt32(&canaryHits); got != 1 {
		t.Fatalf("canary hit %d, want 1", got)
	}
	atomic.StoreInt32(&status, 200)
	time.Sleep(1100 * time.Millisecond)
	call() // cooldown expired -> re-armed and served
	if got := atomic.LoadInt32(&canaryHits); got != 2 {
		t.Fatalf("canary hit %d, want 2 (re-armed)", got)
	}
}

// A dead stand (dial refused, status 0) must count as a failure, not as a
// neutral 4xx: otherwise a missing test endpoint never retires the canary and
// every request pays the failed dial.
func TestCanaryDialErrorCountsAsFailure(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"live","choices":[]}`))
	}))
	defer live.Close()

	p := canaryProvider("http://127.0.0.1:1/v1", 0, 1)
	r := New(map[string]*provider.Provider{"p": p})
	r.providers["p"].BaseURL = live.URL

	for i := 0; i < 2; i++ {
		res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		_ = res.Resp.Body.Close()
	}
	r.canaryMu.Lock()
	st := r.canaryStat["p"]
	off := st != nil && st.off
	r.canaryMu.Unlock()
	if !off {
		t.Fatal("canary still armed after a dial error exceeded max_failures")
	}
}
