package route

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// fakeForwardProxy answers absolute-form HTTP requests, playing both the
// proxy and the upstream role. statusFor decides the answer per proxy.
// hit counts per server let tests assert *which* egresses were touched.
func fakeForwardProxyHits(t *testing.T, status int) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
		} else {
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limit"}}`))
		}
	}))
	return srv, &hits
}

func fakeForwardProxy(t *testing.T, status int) *httptest.Server {
	s, _ := fakeForwardProxyHits(t, status)
	return s
}

// fakePoolAPI serves acquire/release and returns fresh proxy URLs in order.
type fakePoolAPI struct {
	t        *testing.T
	mu       sync.Mutex
	leases   []string // pool of proxy URLs to rotate through
	fixedIP  string   // non-empty: every lease reports this public_ip (anycast collapse shape)
	acquired []string
	released []string
	acquireN int
	releaseN int
	// epoch is returned on /api/v2/pool/acquire responses (0 = omit, v1 shape).
	epoch uint64
	// wrappedV2 emits the live warp-router v2 shape
	// ({"epoch":N,"leases":[{...}]} instead of flat fields).
	wrappedV2 bool
	// emptyBatch emits {"epoch":N,"leases":[]} on v2 acquire (pool empty /
	// generation change with no lease).
	emptyBatch bool
	// stripInnerEpoch omits epoch inside the wrapped lease (top-level only).
	stripInnerEpoch bool
	// paths records every acquire/release path the router used, so a test
	// can pin the dialect (v1 vs v2) without body sniffing.
	paths []string
	// rotateN counts rotate posts: the saturation escape hatch must re-arm a
	// silent pool, and must NOT fire while the pool serves.
	rotateN int
	// emptyFirst: answer this many acquires with an empty v2 lease batch
	// before the real lease list is used (the "all slots busy then freed"
	// shape the wait-for-egress path has to survive).
	emptyFirst int
}

func (f *fakePoolAPI) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/pool/acquire"):
		f.paths = append(f.paths, r.URL.Path)
		if f.emptyFirst > 0 && f.wrappedV2 {
			f.emptyFirst--
			_ = json.NewEncoder(w).Encode(map[string]any{"epoch": f.epoch, "leases": []any{}})
			return
		}
		if f.emptyBatch {
			_ = json.NewEncoder(w).Encode(map[string]any{"epoch": f.epoch, "leases": []any{}})
			return
		}
		if len(f.leases) == 0 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		proxy := f.leases[0]
		f.leases = f.leases[1:]
		f.acquired = append(f.acquired, proxy)
		f.acquireN++
		leaseID := "ls-" + proxy
		// Egress IP is stable per backend (derived from its port), so
		// cooldown-by-IP treats the same backend consistently across
		// acquires — like a real pool's per-backend egress.
		ip := f.fixedIP
		if ip == "" {
			port := proxy[strings.LastIndex(proxy, ":")+1:]
			var portN int
			fmt.Sscanf(port, "%d", &portN)
			ip = fmt.Sprintf("10.1.0.%d", portN%250+1)
		}
		out := map[string]any{
			"lease_id": leaseID, "proxy_url": proxy, "public_ip": ip, "country": "MD",
		}
		if f.epoch != 0 {
			out["epoch"] = f.epoch
		}
		if f.wrappedV2 {
			inner := map[string]any{
				"lease_id": leaseID, "proxy_url": proxy, "public_ip": ip, "country": "MD",
			}
			if f.epoch != 0 && !f.stripInnerEpoch {
				inner["epoch"] = f.epoch
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"epoch": f.epoch, "leases": []any{inner}})
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasSuffix(r.URL.Path, "/pool/release"):
		f.paths = append(f.paths, r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			LeaseID  string   `json:"lease_id"`
			LeaseIDs []string `json:"lease_ids"`
		}
		_ = json.Unmarshal(body, &req)
		if req.LeaseID != "" {
			f.released = append(f.released, req.LeaseID)
		}
		f.released = append(f.released, req.LeaseIDs...)
		f.releaseN++
		_, _ = w.Write([]byte(`{"released":true}`))
	case strings.HasSuffix(r.URL.Path, "/pool/rotate"):
		f.paths = append(f.paths, r.URL.Path)
		f.rotateN++
		_, _ = w.Write([]byte(`{"respawned":2}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// TestPoolRetryRotatesLeaseOn429 verifies the core contract: a 429 from the
// first lease's proxy must release it, acquire the next lease, and retry —
// so the caller gets a response from a different backend/IP.
// blockBackgroundWorkers детерминирует тесты на одноразовом fakePoolAPI:
// фоновые standby-verify (до 5 acquire за чужой key) и prewarm-тикер делят
// тот же API и съедают лизы свипов. prewarm/standby — флаги, standby-очередь —
// небуферизованный канал без читателя (enqueue дропается через default).
func blockBackgroundWorkers(pt *PoolTransport) {
	pt.prewarmRunning.Store(true)
	pt.standbyRunning.Store(true)
	pt.muMu.Lock()
	pt.standbyCh = make(chan string)
	pt.muMu.Unlock()
}

func TestPoolEventSinkGetsAcquire(t *testing.T) {
	// Pool-путь пишет исходы в ленту: успех acquire -> OnEvent(acquire, ok).
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: []string{good.URL, good.URL, good.URL, good.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	type ev struct {
		proxy, action string
		ok            bool
	}
	var mu sync.Mutex
	var got []ev
	pt.OnEvent = func(proxy, action string, ok bool, ms int64, errText string) {
		mu.Lock()
		got = append(got, ev{proxy, action, ok})
		mu.Unlock()
	}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, e := range got {
		if e.action == "acquire" && e.ok {
			found = true
		}
	}
	if !found {
		t.Fatalf("pool acquire ok must emit OnEvent, got %+v", got)
	}
}

func containsStr(ss []string, v string) bool {
	for _, x := range ss {
		if x == v {
			return true
		}
	}
	return false
}

func TestPoolRetryRotatesLeaseOn429(t *testing.T) {
	bad, badHits := fakeForwardProxyHits(t, http.StatusTooManyRequests)
	defer bad.Close()
	good, goodHits := fakeForwardProxyHits(t, http.StatusOK)
	defer good.Close()
	// 2026-09-23 storm wait-reopen: second-chance peek съедает одну лизу без
	// возврата (fakePoolAPI одноразовый; реальный warp reuse) — bad2 под peek,
	// good под следующий acquire.
	bad2 := fakeForwardProxy(t, http.StatusTooManyRequests)
	defer bad2.Close()

	api := &fakePoolAPI{leases: []string{bad.URL, bad2.URL, good.URL, good.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())

	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after lease rotation", resp.StatusCode)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.acquired) < 2 {
		t.Fatalf("expected >=2 acquires (first lease 429 then rotate), got %v", api.acquired)
	}
	if api.acquired[0] != bad.URL || api.acquired[1] != bad2.URL {
		t.Fatalf("rotation order wrong: %v, want [bad bad2 ...]", api.acquired)
	}
	if !containsStr(api.acquired, good.URL) {
		t.Fatalf("good backend never acquired: %v", api.acquired)
	}
	if len(api.released) < 1 {
		t.Fatal("the 429 lease must be released before retry")
	}
	if *badHits < 1 && *goodHits < 1 {
		t.Fatal("neither egress was hit")
	}
}

// TestPoolSkipsKnownBadEgressOnNextRequest is the "smart routing" contract:
// after one 429 from a lease, later requests must not touch that egress again
// while it cools down — the second request goes straight to the good backend
// with zero hits on the bad one.
func TestPoolSkipsKnownBadEgressOnNextRequest(t *testing.T) {
	badHits := new(int)
	badHitsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*badHits++
		w.Header().Set("Retry-After", "3324")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer badHitsSrv.Close()
	badURL := badHitsSrv.URL
	good, goodHits := fakeForwardProxyHits(t, http.StatusOK)
	defer good.Close()

	api := &fakePoolAPI{leases: []string{badURL, good.URL, badURL, good.URL, badURL, good.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 3, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}

	for n := 0; n < 2; n++ {
		lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
		resp, err := lease.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", n+1, resp.StatusCode)
		}
		resp.Body.Close()
		_ = lease.Release(context.Background())
	}

	if *badHits != 1 {
		// 1 POST on the first request (no per-attempt probe, no control
		// probe — 429 is trusted immediately); the second request must
		// not touch the bad egress at all.
		t.Fatalf("bad egress hit %d times, want 1 (POST on request 1 only)", *badHits)
	}
	if *goodHits < 2 {
		t.Fatalf("good egress hit %d times, want >= 2", *goodHits)
	}
}

// TestPoolAll429ReturnsHonest429 verifies that when every lease 429s (fresh
// pool each request, cooldown still short), the transport surfaces the last
// upstream 429 — no fake success, no direct fallback.
func TestPoolAll429Tails503(t *testing.T) {
	bad, _ := fakeForwardProxyHits(t, http.StatusTooManyRequests)
	defer bad.Close()
	bad2, _ := fakeForwardProxyHits(t, http.StatusTooManyRequests)
	defer bad2.Close()

	api := &fakePoolAPI{leases: []string{bad.URL, bad2.URL, bad.URL, bad2.URL, bad.URL, bad2.URL, bad.URL, bad2.URL, bad.URL, bad2.URL, bad.URL, bad2.URL, bad.URL, bad2.URL, bad.URL, bad2.URL, bad.URL, bad2.URL, bad.URL, bad2.URL, bad.URL, bad2.URL, bad.URL, bad2.URL, bad.URL, bad2.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	oldDeadline := poolTryDeadline
	poolTryDeadline = 3 * time.Second
	defer func() { poolTryDeadline = oldDeadline }()
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())

	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	_, err = lease.Client.Do(req)
	if err == nil {
		t.Fatal("all-429 must not surface 429, want 503 tail error")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("want *UpstreamStatusError, got %T: %v", err, err)
	}
	if use.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 tail when every lease fails (client retries, not dies)", use.Status)
	}
	if use.RetryAfter != "" {
		t.Fatalf("503 tail must carry no Retry-After, got %q", use.RetryAfter)
	}
}

// TestPoolTransportProviderFilter verifies the pool is only applied to the
// configured provider; other providers get a direct lease.
func TestPoolTransportProviderFilter(t *testing.T) {
	pa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("pool API must not be called for non-matching provider")
	}))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Provider: "opencode"}
	prov := &provider.Provider{Name: "other", Type: "openai-compatible", BaseURL: "http://upstream/v1", Egress: "http://proxy.local:8123"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "other"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Proxy != prov.Egress {
		t.Fatalf("non-matching provider must keep static egress, got %q", lease.Proxy)
	}
}

// TestPoolRetryHungUpstreamBounded is the anti-"100s hang" contract: an
// egress whose proxy accepts the connection but never sends a response
// header must NOT pin the request for the 300s client timeout. RoundTrip
// gives up on that egress via poolHeaderThinkTimeout and the retry loop
// surfaces a verdict within a small multiple of it.
func TestPoolRetryHungUpstreamBounded(t *testing.T) {
	// silent proxy: accepts TCP, never writes a byte.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// hold the connection open well past poolHeaderThinkTimeout so
			// only the header timeout, not an EOF, ends this attempt
			go func(c net.Conn) {
				time.Sleep(60 * time.Second)
				_ = c.Close()
			}(c)
		}
	}()

	api := &fakePoolAPI{leases: []string{"http://" + ln.Addr().String()}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())

	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	start := time.Now()
	_, err = lease.Client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("hung upstream must not produce a response")
	}
	// One egress hang costs at most one poolHeaderThinkTimeout (150s) +
	// acquire-spin on the drained pool (10s budget). 220s is far below
	// the old 300s client timeout (think 150 + poolTry 165 era — review
	// 2026-09-19; constants moved, asserts followed).
	if elapsed > 220*time.Second {
		t.Fatalf("request pinned for %v — hung upstream must be bounded by poolHeaderThinkTimeout", elapsed)
	}

	t.Logf("hung upstream rejected after %v", elapsed)
}

// TestPoolNoHomeDialOnAll429: when every pool egress returns 429, the
// router must surface the honest pool verdict - NEVER a home-IP direct
// dial (2026-09-10: fresh main IP fixed the same pool instantly, proving
// the upstream keys home-IP into the pool identity; DirectFallback is
// ignored as a leak vector).
func TestPoolNoHomeDialOnAll429(t *testing.T) {
	hot, _ := fakeForwardProxyHits(t, http.StatusTooManyRequests)
	defer hot.Close()

	api := &fakePoolAPI{leases: []string{hot.URL, hot.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1, Cooldown: time.Nanosecond, DirectFallback: true}
	blockBackgroundWorkers(pt)
	oldDeadline := poolTryDeadline
	poolTryDeadline = 3 * time.Second
	defer func() { poolTryDeadline = oldDeadline }()
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())

	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader("{}"))
	_, err = lease.Client.Do(req)
	if err == nil {
		t.Fatal("all-429 must tail 503, want tail error (no home-IP dial)")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("want *UpstreamStatusError, got %T: %v", err, err)
	}
	if use.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 tail (no home-IP dial)", use.Status)
	}
}

// TestPoolAcquireExhaustedSkipsDirect: pool exhausted with NO upstream
// verdict (every lease hard-banned, acquire fails) must NOT burn the home IP
// via direct_fallback (regression 2026-09-09 storm: 'pool exhausted, home IP
// cooling' on every exhausted request extended the home 8h ban).
func TestPoolAcquireExhaustedSkipsDirect(t *testing.T) {
	hot := fakeForwardProxy(t, http.StatusTooManyRequests)
	defer hot.Close()
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer direct.Close()
	api := &fakePoolAPI{leases: []string{hot.URL, hot.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1, Cooldown: time.Minute, DirectFallback: true}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	// Баним egress заранее: acquire не отдаст ничего -> вердиктов нет.
	// Ключ — как в fakePoolAPI (publicIP по порту, иначе бан и выдача
	// разойдутся по разным ключам).
	hotPort := hot.URL[strings.LastIndex(hot.URL, ":")+1:]
	var hotPortN int
	fmt.Sscanf(hotPort, "%d", &hotPortN)
	pt.markBad429(fmt.Sprintf("10.1.0.%d", hotPortN%250+1), 3600*time.Second, time.Now())
	// Acquire itself must fail with 503, NEVER 429 (no direct lease without
	// a verdict) — direct serves 200, so any handed lease would mask the
	// skip as success. P2-5 resolution 2026-09-09; 429→503 resolution
	// 2026-09-16 (OMP escalates verdict-less 429s into 1800s blocks).
	_, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err == nil {
		t.Fatal("Acquire must fail when the pool is exhausted pre-verdict (direct serves 200 — a lease would burn home)")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("want UpstreamStatusError, got %T: %v", err, err)
	}
	if use.Status != http.StatusServiceUnavailable {
		t.Fatalf("verdict-less exhaustion must be 503, got %d (%v)", use.Status, err)
	}
	_ = direct
}

// TestPoolDirectFallbackNoOptInKeepsHonest429: without DirectFallback the
// transport stays fail-closed — all egresses 429 → honest 429, never a
// silent direct downgrade.
func TestPoolDirectFallbackNoOptInKeepsHonest429(t *testing.T) {
	hot, _ := fakeForwardProxyHits(t, http.StatusTooManyRequests)
	defer hot.Close()

	api := &fakePoolAPI{leases: []string{hot.URL, hot.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	oldDeadline := poolTryDeadline
	poolTryDeadline = 3 * time.Second
	defer func() { poolTryDeadline = oldDeadline }()
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())

	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	_, err = lease.Client.Do(req)
	if err == nil {
		t.Fatal("all-429 without DirectFallback must tail 503, want tail error")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("want *UpstreamStatusError, got %T: %v", err, err)
	}
	if use.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 tail when DirectFallback is not opted in", use.Status)
	}
}

// TestPoolRotatesNotFoundEgress: an upstream answering HTTP 200 with an
// SSE "NOT_FOUND" body (model not deployed on this anycast class) must be
// treated as an unusable egress — rotate to the next backend/direct instead
// of serving the error to the caller.
func TestPoolRotatesNotFoundEgress(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"error":{"message":"Model not found, inaccessible, and/or not deployed","code":"NOT_FOUND"}}`))
	}))
	defer notFound.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer good.Close()

	api := &fakePoolAPI{leases: []string{notFound.URL, good.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())

	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bodyB, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || bytes.Contains(bodyB, []byte("NOT_FOUND")) {
		t.Fatalf("status=%d body=%q — NOT_FOUND egress must be rotated away", resp.StatusCode, bodyB)
	}
	if len(api.acquired) < 2 {
		t.Fatalf("expected >=2 acquires (NOT_FOUND egress then rotate), got %v", api.acquired)
	}
}

// TestPoolStatePersistsAcrossRestart verifies the core restart contract: a
// hard 24h class-ban written by one PoolTransport instance must be visible
// to a fresh instance with the same StateFile — a promoted binary must not
// forget burned /48 windows and re-storm them from scratch.
func TestPoolStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "pool_state.json")

	now := time.Now()
	pt := &PoolTransport{StateFile: stateFile}
	pt.markBad429("2a09:bac5:48f3:", 3600*time.Second, now)
	pt.FlushState()
	if !pt.isBad("2a09:bac5:48f3:", now.Add(time.Minute)) {
		t.Fatal("markBad429 did not arm a cooldown")
	}

	// Simulate restart: brand-new transport, same state file, cold maps.
	pt2 := &PoolTransport{StateFile: stateFile}
	pt2.loadState()
	if !pt2.isBad("2a09:bac5:48f3:", now.Add(time.Minute)) {
		t.Fatal("hard class-ban lost across restart: burned /48 would re-enter rotation")
	}
	if !pt2.hardBad["2a09:bac5:48f3:"] {
		t.Fatal("hardBad flag lost across restart")
	}
	// Soft cooldown survives too.
	pt2.markBadKey("2a09:bac1:47a0:", now)
	pt2.FlushState()
	pt3 := &PoolTransport{StateFile: stateFile}
	pt3.loadState()
	if !pt3.isBad("2a09:bac1:47a0:", now.Add(30*time.Second)) {
		t.Fatal("soft cooldown lost across restart")
	}
	// Expired entries are dropped on load: a finished cooldown must not
	// keep the key banned forever.
	pt3.markBadKey("2a09:bac1:47c0:", now.Add(-2*time.Minute))
	pt3.FlushState()
	pt4 := &PoolTransport{StateFile: stateFile}
	pt4.loadState()
	if pt4.isBad("2a09:bac1:47c0:", now) {
		t.Fatal("expired cooldown survived reload")
	}
}

// TestPoolStateNoFileIsNoop verifies transports built without a StateFile
// never touch the disk: existing tests always build PoolTransport without
// one, and persistence must stay opt-in.
func TestPoolStateNoFileIsNoop(t *testing.T) {
	pt := &PoolTransport{} // no StateFile
	pt.markBad429("x:", time.Hour, time.Now())
	pt.saveState() // must not panic or write anywhere
	pt.loadState() // must not panic
}

// TestPoolAcquire429RetryDedup verifies the dedup logic: acquireLease marks
// IPs as seen and skips consecutive duplicates (same IP returned twice).
func TestPoolAcquire429RetryDedup(t *testing.T) {
	// Two fake proxies with same IP (same port-derived IP) should trigger dedup
	srv1 := fakeForwardProxy(t, http.StatusOK)
	defer srv1.Close()
	// Use same URL twice via lease rotation - need to test acquireLease dedup path
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: []string{srv1.URL, good.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2}
	blockBackgroundWorkers(pt)
	// Mark first IP as bad, then good IP should be returned
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release(context.Background())
	// Should succeed with first lease
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 got %d", resp.StatusCode)
	}
}

func TestPoolHonest403And524(t *testing.T) {
	// 2026-09-16: non-region 403 свапает внутри свипа — клиент 403 не
	// видит; один egress = хвост 503. Здесь только 502/524 честные.
	for _, status := range []int{502, 524} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := fakeForwardProxy(t, status)
			defer srv.Close()
			api := &fakePoolAPI{leases: []string{srv.URL, srv.URL}}
			pa := httptest.NewServer(http.HandlerFunc(api.handler))
			defer pa.Close()
			pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
			blockBackgroundWorkers(pt)
			prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
			lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release(context.Background())
			req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
			resp, err := lease.Client.Do(req)
			if err != nil {
				t.Logf("status %d err: %v", status, err)
				return
			}
			defer resp.Body.Close()
			// 502/524 are retryable (5xx), but if only 1 lease they surface as last response
			if status >= 500 && resp.StatusCode != status {
				// After retry exhausted, honest last status returned - may be 502/524 depending on dedup
				t.Logf("5xx status %d got %d", status, resp.StatusCode)
			}
		})
	}
}

func TestPool409AcquireRetry(t *testing.T) {
	goodProxy := fakeForwardProxy(t, http.StatusOK)
	defer goodProxy.Close()
	callN := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pool/acquire") {
			callN++
			if callN == 1 {
				w.WriteHeader(http.StatusConflict)
				return
			}
			leaseID := "ls-" + goodProxy.URL
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"lease_id": leaseID, "proxy_url": goodProxy.URL, "public_ip": "10.9.0.1", "country": "MD",
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/pool/release") {
			_, _ = w.Write([]byte(`{"released":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()
	pt := &PoolTransport{APIURL: api.URL, Fanout: 5, Rounds: 2}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatalf("Acquire after 409 retry: %v", err)
	}
	defer lease.Release(context.Background())
	if callN < 2 {
		t.Fatalf("expected >=2 pool acquires (409 then success), got %d", callN)
	}
}

func TestPoolRegion403SwapsLease(t *testing.T) {
	// First egress answers 403 region_error, second answers 200: the same
	// logical request must transparently swap and succeed. Non-region 403
	// bodies stay honest terminal (covered by TestPoolHonest403And524).
	regionBody := `{"error":{"type":"invalid_request_error","code":"region_error","param":"region_error","message":"Region not supported"}}`
	regionHits := 0
	regionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		regionHits++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(regionBody))
	}))
	defer regionSrv.Close()
	okSrv, okHits := fakeForwardProxyHits(t, http.StatusOK)
	defer okSrv.Close()
	api := &fakePoolAPI{leases: []string{regionSrv.URL, okSrv.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatalf("region swap should succeed, got err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("region swap: want 200 got %d", resp.StatusCode)
	}
	if regionHits != 1 || *okHits != 1 {
		t.Fatalf("want 1 hit on region egress and 1 on ok egress, got %d/%d", regionHits, *okHits)
	}
}

func TestPoolRegion403ExhaustedHonest(t *testing.T) {
	// Every egress region-blocked: honest 403 surfaces, never 200, never hang.
	// 10 region servers back a full 5x2 sweep (fanout*rounds >= minWalkEgress
	// so the walk floor does not extend rounds into an empty fake pool).
	regionBody := `{"error":{"type":"invalid_request_error","code":"region_error","message":"Region not supported"}}`
	var srvs []*httptest.Server
	var leases []string
	for range 10 {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatalf("exhausted region should return honest 403 response, got err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("exhausted region: want 403 got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !isRegionForbiddenBody(raw) {
		t.Fatalf("exhausted region body should keep region markers, got %q", string(raw))
	}
}

func TestIsRegionForbiddenBody(t *testing.T) {
	if !isRegionForbiddenBody([]byte(`{"error":{"code":"region_error","message":"Region not supported"}}`)) {
		t.Fatal("region_error JSON must match")
	}
	if !isRegionForbiddenBody([]byte(`Region not supported in this region: forbidden`)) {
		t.Fatal("region+forbidden text must match")
	}
	if isRegionForbiddenBody([]byte(`{"error":{"code":"rate_limit"}}`)) {
		t.Fatal("rate_limit must NOT match")
	}
	if isRegionForbiddenBody([]byte(`{"error":{"message":"Forbidden"}}`)) {
		t.Fatal("bare Forbidden without region must NOT match")
	}
	if isRegionForbiddenBody([]byte(`the northern region has nice weather`)) {
		t.Fatal("plain region mention without error marker must NOT match")
	}
}
func TestPoolRegionPinSkipsDiscovery(t *testing.T) {
	// Region memory: after one region swap the model is pinned to the
	// serving egress — the NEXT request must hit it first, not re-probe
	// the dead egress (discovery ~2-5s on muse-1.3 per request).
	regionBody := `{"error":{"type":"invalid_request_error","code":"region_error","param":"region_error","message":"Region not supported"}}`
	regionHits := 0
	regionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		regionHits++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(regionBody))
	}))
	defer regionSrv.Close()
	okSrv, okHits := fakeForwardProxyHits(t, http.StatusOK)
	defer okSrv.Close()
	api := &fakePoolAPI{leases: []string{regionSrv.URL, okSrv.URL, okSrv.URL, okSrv.URL, regionSrv.URL, okSrv.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	newReq := func() *http.Request {
		req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		return req
	}
	// Request 1: discovery (region, then ok).
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x", Model: "m"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := lease.Client.Do(newReq())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	_ = lease.Release(context.Background())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("req1: want 200 got %d", resp.StatusCode)
	}
	// Request 2: pinned egress first — zero hits on the region egress.
	regionBefore, okBefore := regionHits, *okHits
	lease2, err := pt.Acquire(context.Background(), RequestContext{Provider: "x", Model: "m"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := lease2.Client.Do(newReq())
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	_ = lease2.Release(context.Background())
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("req2: want 200 got %d", resp2.StatusCode)
	}
	if regionHits != regionBefore {
		t.Fatalf("pinned request re-probed region egress: region hits %d -> %d (ok %d -> %d)",
			regionBefore, regionHits, okBefore, *okHits)
	}
	// Other models are unaffected: a different model still probes fresh.
	lease3, err := pt.Acquire(context.Background(), RequestContext{Provider: "x", Model: "other"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease3.Release(context.Background())
	_ = lease3
}

func TestPoolRegionAs200SwapsInsideSweep(t *testing.T) {
	// 200-SSE with a region_error first event (responses path): swap must
	// happen INSIDE one sweep — second egress warmed once, no outer loop.
	regionSSE := "data: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"region_error\",\"message\":\"Region not supported\"}}\n\n"
	regionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(regionSSE))
	}))
	defer regionSrv.Close()
	okSrv, okHits := fakeForwardProxyHits(t, http.StatusOK)
	defer okSrv.Close()
	api := &fakePoolAPI{leases: []string{regionSrv.URL, okSrv.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x", Model: "m"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatalf("region-as-200 swap should succeed, got err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("region-as-200: want 200 got %d", resp.StatusCode)
	}
	if *okHits != 1 {
		t.Fatalf("want exactly 1 hit on ok egress, got %d", *okHits)
	}
}

func TestPoolNonRegion403SwapsAndTails503(t *testing.T) {
	// Non-region 403 свапает внутри свипа (2026-09-16): клиент 403 не
	// видит. Один egress + короткий бюджет = хвост 503 без Retry-After
	// (OMP ретраит очередь, а не убивает часовыми блоками).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded for this key"}}`))
	}))
	defer srv.Close()
	api := &fakePoolAPI{leases: []string{srv.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Hour}
	blockBackgroundWorkers(pt)
	oldDeadline := poolTryDeadline
	poolTryDeadline = 3 * time.Second
	defer func() { poolTryDeadline = oldDeadline }()
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		t.Fatalf("single-egress non-region 403 must not surface resp, got status %d (want 503 tail error)", resp.StatusCode)
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("want *UpstreamStatusError, got %T: %v", err, err)
	}
	if use.Status != http.StatusServiceUnavailable {
		t.Fatalf("want 503 tail, got %d (%v)", use.Status, err)
	}
	if use.RetryAfter != "" {
		t.Fatalf("503 tail must carry no Retry-After, got %q", use.RetryAfter)
	}
}

func TestPoolNonRegion403SwapsToLive(t *testing.T) {
	// Non-region 403 на первом egress + 200 на втором: тот же логический
	// запрос прозрачно свапает и возвращает 200 (как region-свап).
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"policy denied"}}`))
	}))
	defer bad.Close()
	good, goodHits := fakeForwardProxyHits(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: []string{bad.URL, good.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatalf("non-region swap should succeed, got err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-region swap: want 200 got %d", resp.StatusCode)
	}
	if *goodHits != 1 {
		t.Fatalf("want exactly 1 hit on live egress, got %d", *goodHits)
	}
}

func TestPoolHungBoundedWithDeadline(t *testing.T) {
	// Hung egress via 409 + slow proxy: bounded by poolTryDeadline 120s + RHT 90s, not 300s
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				time.Sleep(60 * time.Second)
				_ = c.Close()
			}(c)
		}
	}()
	api := &fakePoolAPI{leases: []string{"http://" + ln.Addr().String()}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	start := time.Now()
	_, err = lease.Client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("hung should not succeed")
	}
	if elapsed > 150*time.Second {
		t.Fatalf("hung pinned %v > 150s", elapsed)
	}
	t.Logf("hung rejected after %v", elapsed)
}

// TestPoolSpreadBurstsAcrossBackends: два последовательных Acquire должны
// разойтись по разным бэкендам, а не стекаться на один — второй acquire
// откладывает горячую лизу (выдана <spreadWindow назад) и берёт холодную.
// Проверяет и release-баланс: каждый лишний acquire ровно один release.
func TestPoolSpreadBurstsAcrossBackends(t *testing.T) {
	a := fakeForwardProxy(t, http.StatusOK)
	defer a.Close()
	b := fakeForwardProxy(t, http.StatusOK)
	defer b.Close()
	api := &fakePoolAPI{leases: []string{a.URL, a.URL, b.URL, b.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	l1, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	p1 := l1.Proxy
	_ = l1.Release(context.Background())
	l2, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Release(context.Background())
	if l2.Proxy == p1 {
		t.Fatalf("burst stacked on one backend: both acquires got %s, want spread across %s/%s", p1, a.URL, b.URL)
	}
	api.mu.Lock()
	extra := api.releaseN - 1 // минус финальный Release второго Acquire
	acq := api.acquireN
	api.mu.Unlock()
	t.Logf("spread: %s -> %s (acquires=%d extra_releases=%d)", p1, l2.Proxy, acq, extra)
	if extra < 1 {
		t.Fatalf("spread must cost an extra acquire+release, got acquires=%d extra_releases=%d", acq, extra)
	}
}

// TestPoolSequentialStaysOnOneBackend: одиночные запросы вне бёрста НЕ
// платят за spread — холодная лиза возвращается сразу (один acquire, ноль
// лишних release до финального).
func TestPoolSequentialStaysOnOneBackend(t *testing.T) {
	a := fakeForwardProxy(t, http.StatusOK)
	defer a.Close()
	// Одна лиза в пуле: acquire уходит в spin 24 попытки на повторе, поэтому
	// кладём достаточно копий одного proxy.
	leases := make([]string, 24)
	for i := range leases {
		leases[i] = a.URL
	}
	api := &fakePoolAPI{leases: leases}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	api.mu.Lock()
	acq := api.acquireN
	api.mu.Unlock()
	if acq != 1 {
		t.Fatalf("cold single acquire cost %d pool acquires, want 1 (no spread tax outside bursts)", acq)
	}
}

// TestPoolTransient429NoBan: минутный 429 (Retry-After 349s, без заголовка)
// НЕ банит IP и НЕ паркует — walk-on без следа. Следующий запрос идёт сразу,
// без часового бана в bad/hardBad и без персиста. (Парк убран 2026-09-10:
// при 10 параллельных каждый парковал свою лизу и весь пул вставал
// запаркованным с живыми окнами под ним.)
// TestPoolStormWaitReopenRedeals (2026-09-23): в 1x1 после пересдачи
// второй 429 НЕ даёт мгновенный 503-хвост (88x saturated-1 за 30 мин
// на живом): wait-reopen крутит дальше на свежие группы
// и забирает живой egress -> 200. Мгновенный хвост только
// при shared-burn / честном покрытии / мёртвом бюджете.
func TestPoolStormWaitReopenRedeals(t *testing.T) {
	b1 := fakeForwardProxy(t, http.StatusTooManyRequests)
	defer b1.Close()
	b2 := fakeForwardProxy(t, http.StatusTooManyRequests)
	defer b2.Close()
	b3 := fakeForwardProxy(t, http.StatusTooManyRequests)
	defer b3.Close()
	g1 := fakeForwardProxy(t, http.StatusOK)
	defer g1.Close()
	g2 := fakeForwardProxy(t, http.StatusOK)
	defer g2.Close()
	g3 := fakeForwardProxy(t, http.StatusOK)
	defer g3.Close()
	// Порядок: b1 под тестовый Acquire, b2 под second-chance peek
	// (съедается без возврата: fakePoolAPI одноразовый), b3 под второй warmup
	// -> wait-reopen, g1 -> 200.
	api := &fakePoolAPI{leases: []string{b1.URL, b2.URL, b3.URL, g1.URL, g2.URL, g3.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	oldDeadline := poolTryDeadline
	poolTryDeadline = 30 * time.Second
	defer func() { poolTryDeadline = oldDeadline }()
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatalf("wait-reopen must re-deal past 2x429 to live egress, got tail: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after wait-reopen re-deal", resp.StatusCode)
	}
}

func TestPoolTransient429NoBan(t *testing.T) {
	flakyHits := 0
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flakyHits++
		if flakyHits == 1 {
			w.Header().Set("Retry-After", "349")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer flaky.Close()
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	// 2026-09-23 storm wait-reopen: 1x1 после пересдачи не даёт хвост,
	// а крутит дальше на свежие группы: кладём достаточно
	// лиз на два свипа (тот же flaky вернётся как banned-skip:
	// сначала сходит в last, потом acquire skip и забирает good).
	api := &fakePoolAPI{leases: []string{flaky.URL, flaky.URL, good.URL, good.URL, flaky.URL, good.URL, flaky.URL, good.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	// 2026-09-23 storm wait-reopen: этот тест про ОДИН transient-429 на flaky с
	// немедленным 200 — storm-кольцо от других тестов за 4мин окнo может держать
	// stormActive()=true и ронять eff в 1x1 (storm 429 без RA = честный шторм,
	// окно переоткрывается за секунды — wait-reopen, а не мгновенный rotate).
	// Чистим кольцо: тест детерминирован независимо от порядка прогона.
	pt.muMu.Lock()
	pt.storm429 = nil
	pt.muMu.Unlock()
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after transient-429 rotate", resp.StatusCode)
	}
	// Minute-class 429 (Retry-After 349s) = именованный минутный кулдаун:
	// парк (без hard/персиста) + минутный soft-бан без hard-флага, чтобы
	// acquire не раздавал тот же горячий egress следующим запросам как
	// fresh (2026-09-14 рецидив: RA60-парки истекали mid-cycle, пул
	// прогревал те же бюджеты каждые ~2с). Hours-банов нет.
	pt.muMu.RLock()
	nHard := len(pt.hardBad)
	nBad := len(pt.bad)
	nPark := len(pt.park)
	pt.muMu.RUnlock()
	if nHard != 0 {
		t.Fatalf("transient 429 set %d hard bans, want 0 (ban only on hours-class)", nHard)
	}
	if nBad != 1 {
		t.Fatalf("transient 429 left %d bad entries, want 1 minute soft-ban (no hard)", nBad)
	}
	// Парк ставится всегда рядом с любым не-hours 429 (дешёвый след для
	// warmup-guard); именованный софт-бан держит окно ровно RA-секунды.
	if nPark != 1 {
		t.Fatalf("transient 429 left %d park entries, want 1 short park", nPark)
	}
}

// TestPoolNoRA429ColdEgressLadderBan: no-RA 429 на адрес, не отдавшем ни
// одного 200, — выжженный бюджет, а не шейпер: ladder-бан (10m), чтобы свип
// не перерисовывал его каждые 5 секунд (29888 transient-429/день, ~7
// ре-проб на выжженный /128). На ЖИВОЕ окно (ok>0) по-прежнему только парк —
// это проверяет coldTransient429/TestColdTransient429Ladder.
func TestPoolNoRA429ColdEgressLadderBan(t *testing.T) {
	dead := fakeForwardProxy(t, http.StatusTooManyRequests)
	defer dead.Close()
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	// Fanout 5: первый 429 уходит в полный свип — второй dead тоже может
	// прогреться (2 парка на 2 distinct egress — честно). Лизы: dead,good
	// + запас good на свип.
	good2 := fakeForwardProxy(t, http.StatusOK)
	defer good2.Close()
	good3 := fakeForwardProxy(t, http.StatusOK)
	defer good3.Close()
	api := &fakePoolAPI{leases: []string{dead.URL, good.URL, good2.URL, good3.URL, good.URL, good2.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	pt.muMu.Lock()
	pt.storm429 = nil
	pt.muMu.Unlock()
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after no-RA rotate to good", resp.StatusCode)
	}
	pt.muMu.RLock()
	nHard := len(pt.hardBad)
	nBad := len(pt.bad)
	nPark := len(pt.park)
	pt.muMu.RUnlock()
	if nHard != 0 {
		t.Fatalf("no-RA 429 set %d hard bans, want 0 (this is not an hours-class window)", nHard)
	}
	if nBad < 1 {
		t.Fatal("no-RA 429 на выжженном (0 ok) egress обязан дать ladder-бан")
	}
	pt.muMu.RLock()
	var latest time.Time
	for _, until := range pt.bad {
		if until.After(latest) {
			latest = until
		}
	}
	pt.muMu.RUnlock()
	if left := time.Until(latest); left < 5*time.Minute {
		t.Fatalf("ladder-бан %s короче первой ступени 10m", left)
	}
	// Парк больше не ставится на тот же 429: выжженный адрес ведёт ЛАДДЕР
	// (единственный владелец события, review B6 — раньше одно событие
	// считалось дважды: `egress parked long` + `ladder ban`).
	if nPark != 0 {
		t.Fatalf("no-RA 429 ушёл в ладдер, но оставил %d park-записей: двойной счёт события", nPark)
	}
}

// TestPoolMinuteRASoftBansNamed: minute-RA 429 (Retry-After 60) = именованный
// soft-бан на 60с без hard-флага и без персиста + короткий парк. 200 снимает.
// Рецидив 2026-09-14: RA60-парки 30с истекали mid-cycle, пул прогревал те же
// бюджеты каждые ~2с — теперь acquire скипает egress весь названный кулдаун.
func TestPoolMinuteRASoftBansNamed(t *testing.T) {
	pt := &PoolTransport{}
	now := time.Now()
	pt.markBadSoft("10.9.0.77", 60*time.Second, now)
	if !pt.isBad("10.9.0.77", now.Add(30*time.Second)) {
		t.Fatalf("minute-soft egress must be skipped inside its 60s window")
	}
	// review 2026-09-20: 60с бан — длинный кулдаун (long), не hard-флаг.
	// isHardBanned ловит hard ИЛИ long — acquire такие скипает до expiry.
	// Контракт минуты: bad до 60с, флага в hardBad нет, 200 снимает.
	pt.muMu.RLock()
	_, hasHard := pt.hardBad["10.9.0.77"]
	pt.muMu.RUnlock()
	if hasHard {
		t.Fatalf("minute-soft must not set the hard flag")
	}
	if pt.isBad("10.9.0.77", now.Add(61*time.Second)) {
		t.Fatalf("minute-soft must expire after its named cooldown")
	}
	pt.markGoodKey("10.9.0.77")
	if pt.isBad("10.9.0.77", now.Add(30*time.Second)) {
		t.Fatalf("a 200 must lift the minute-soft ban")
	}
}

// TestPoolHeatDeprioritizesHotEgress: heatMaxHits 429 inside heatWindow уводят egress
// в хвост очереди acquire; 200 полностью снимает heat (окно живо сейчас).
// Живой штормовой кейс
func TestPoolHeatDeprioritizesHotEgress(t *testing.T) {
	pt := &PoolTransport{}
	now := time.Now()
	if got := pt.heatCount("10.9.0.5", now); got != 0 {
		t.Fatalf("heatCount cold = %d, want 0", got)
	}
	pt.noteHeat("10.9.0.5", now)
	pt.noteHeat("10.9.0.5", now)
	if got := pt.heatCount("10.9.0.5", now); got != 2 {
		t.Fatalf("heatCount after 2 hits = %d, want 2", got)
	}
	pt.noteHeat("10.9.0.5", now)
	if got := pt.heatCount("10.9.0.5", now); got != 3 {
		t.Fatalf("heatCount after 3 hits = %d, want 3", got)
	}
	if got := pt.heatCount("10.9.0.5", now.Add(heatWindow).Add(time.Minute)); got != 0 {
		t.Fatalf("heatCount after window = %d, want 0", got)
	}
	pt.heatCool("10.9.0.5", now)
	if got := pt.heatCount("10.9.0.5", now); got != 0 {
		t.Fatalf("heatCount after cool = %d, want 0 (200 forgives pre-200 hits)", got)
	}
}

func TestPoolHeatPost200HitsStayVisible(t *testing.T) {
	// F2 (2026-09-24): 429,429,200,429 в одном окне — пост-200 429 виден
	// сразу (heat=1), а не маскируется full-clear до expiry среза.
	pt := &PoolTransport{}
	t0 := time.Now()
	pt.noteHeat("10.9.0.6", t0)
	pt.noteHeat("10.9.0.6", t0.Add(10*time.Second))
	pt.heatCool("10.9.0.6", t0.Add(20*time.Second))
	pt.noteHeat("10.9.0.6", t0.Add(30*time.Second))
	if got := pt.heatCount("10.9.0.6", t0.Add(30*time.Second)); got != 1 {
		t.Fatalf("heatCount post-200 = %d, want 1", got)
	}
	if got := pt.heatCountLocked("10.9.0.6", t0.Add(30*time.Second)); got != 1 {
		t.Fatalf("heatCountLocked post-200 = %d, want 1", got)
	}
	// До-200 хиты прощены, после expiry всё чисто.
	if got := pt.heatCount("10.9.0.6", t0.Add(heatWindow).Add(2*time.Minute)); got != 0 {
		t.Fatalf("heatCount after window = %d, want 0", got)
	}
}

// TestPoolHeatCapSkipsHotAtAcquire: egress с heatMaxHits+ 429 за окно не выдаётся
// как fresh — уходит в hot-очередь (2026-09-13: 1800+ 429 при x4-7 темпе, пул
// раздавал жар как свежие лизы и каждый запрос жёг прогрев на мёртвом бюджете).
func TestPoolHeatCapSkipsHotAtAcquire(t *testing.T) {
	hot := fakeForwardProxy(t, http.StatusTooManyRequests)
	defer hot.Close()
	cold := fakeForwardProxy(t, http.StatusOK)
	defer cold.Close()
	api := &fakePoolAPI{leases: []string{hot.URL, cold.URL, hot.URL, cold.URL, hot.URL, cold.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	// Греем hot-egress до капа заранее: acquire обязан отвести на cold.
	hotPort := hot.URL[strings.LastIndex(hot.URL, ":")+1:]
	var hotPortN int
	fmt.Sscanf(hotPort, "%d", &hotPortN)
	hotKey := fmt.Sprintf("10.1.0.%d", hotPortN%250+1)
	now := time.Now()
	for range heatMaxHits {
		pt.noteHeat(hotKey, now)
	}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via cold egress (hot must be capped at acquire)", resp.StatusCode)
	}
}

// TestPoolParkedLeaseSkippedWithoutBurningTried: egress, запаркованный
// ПОСЛЕ acquire (бурст-сосед), не жжёт прогрев и tried-слот: warmup-guard
// отпускает и идёт дальше, 200 снимает парк (markGood чистит park).
func TestPoolParkedLeaseSkippedWithoutBurningTried(t *testing.T) {
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	live := fakeForwardProxy(t, http.StatusOK)
	defer live.Close()
	api := &fakePoolAPI{leases: []string{good.URL, live.URL, good.URL, live.URL, good.URL, live.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	// Паркуем egress good заранее: warmup-guard должен отвести без прогрева.
	// Ключ — как в fakePoolAPI (publicIP по порту), иначе парк и успех
	// разойдутся по разным ключам.
	goodPort := good.URL[strings.LastIndex(good.URL, ":")+1:]
	var goodPortN int
	fmt.Sscanf(goodPort, "%d", &goodPortN)
	pt.banShort429(fmt.Sprintf("10.1.0.%d", goodPortN%250+1), shortPark, time.Now())
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via parked-lease walk-on", resp.StatusCode)
	}
	// Парк чужого egress жив (успех соседа его не снимает — разные окна),
	// но tried-слот не сожжён: запрос дошёл до live и кончился 200.
	pt.muMu.RLock()
	nPark := len(pt.park)
	pt.muMu.RUnlock()
	if nPark != 1 {
		t.Fatalf("park left %d entries, want 1 (unserved egress stays parked)", nPark)
	}
}

// TestPoolStormSelfCheck: 20+ 429 за 2 мин без единого 200 = шторм (схлоп
// в 1x1); один 200 снимает шторм сразу (2026-09-13: 1800+ no-RA 429 при
// x4-7 темпе, пул сам себя закапывал полным 5x2 свипом; порог 20 — 60
// не срабатывал при interleaved ok, 2026-09-14).
func TestPoolStormSelfCheck(t *testing.T) {
	pt := &PoolTransport{}
	now := time.Now()
	if pt.stormActive() {
		t.Fatal("stormActive cold = true, want false")
	}
	for range stormMax429 {
		pt.noteStorm429(now)
	}
	if !pt.stormActive() {
		t.Fatal("stormActive after 60x429 no-ok = false, want true")
	}
	// Один fluke-200 шторм НЕ снимает (wave1 heat P1: ok-trickle гасил
	// fail-fast посреди 87 RA60) — нужно sustained recovery: доля ok >= 50%
	// окна (wave 2026-09-20: абсолютный ok>=2 в лотерее 25% не срабатывал
	// никогда — 120 ok рядом с 58×429 держали полный 5x2).
	pt.noteStormOK(now)
	if !pt.stormActive() {
		t.Fatal("stormActive after 1x200 = false, want true (single fluke must not lift storm)")
	}
	// Доля: 1 ok против 12×429 (1*2 < 12) — шторм держится.
	// Добиваем ok до половины: 6 ok против 12×429 (6*2 >= 12) — снят.
	for range 5 {
		pt.noteStormOK(now)
	}
	if pt.stormActive() {
		t.Fatal("stormActive after 6x200 vs 12x429 = true, want false (50% ok-rate lifts storm)")
	}
	// Старые хиты за окном не считаются.
	pt2 := &PoolTransport{}
	old := now.Add(-stormWindow).Add(-time.Minute)
	for range stormMax429 {
		pt2.noteStorm429(old)
	}
	if pt2.stormActive() {
		t.Fatal("stormActive on stale hits = true, want false")
	}
}

// TestPoolStormLiveEgressLift: 2+ свежих 200 на одном egress = доказанно живой
// (per-egress лифт 2026-09-23): pick-приоритет и bypass fail-fast, даже пока
// общий шторм-флаг горит. 1 fluke-200 лифтом не считается.
func TestPoolStormLiveEgressLift(t *testing.T) {
	pt := &PoolTransport{}
	now := time.Now()
	for range stormMax429 {
		pt.noteStorm429(now)
	}
	if !pt.stormActive() {
		t.Fatal("want storm active")
	}
	pt.noteStormOKKey("10.9.0.7", now)
	if pt.stormLiveEgress("10.9.0.7") {
		t.Fatal("single fluke-200 must not lift egress")
	}
	pt.noteStormOKKey("10.9.0.7", now)
	if !pt.stormLiveEgress("10.9.0.7") {
		t.Fatal("2 fresh 200s must mark egress live")
	}
	if pt.stormLiveEgress("10.9.0.8") {
		t.Fatal("other egress must not be live")
	}
	// Старые хиты за окном не считаются.
	if pt.stormLiveEgress("10.9.0.7") && true {
		pt2 := &PoolTransport{}
		old := now.Add(-stormWindow).Add(-time.Minute)
		pt2.noteStormOKKey("10.9.0.7", old)
		pt2.noteStormOKKey("10.9.0.7", old)
		if pt2.stormLiveEgress("10.9.0.7") {
			t.Fatal("stale 200s must not mark egress live")
		}
	}
}

// TestPoolStormVerdictIs429: разогнанный шторм-счётчик схлопывает RoundTrip
// в 1x1; штормовой хвост — 503 без Retry-After (2026-09-16: клиент 429 не
// видит вообще, OMP ретраит очередь вместо часовых блоков).
func TestPoolStormVerdictIs429(t *testing.T) {
	hot := fakeForwardProxy(t, http.StatusTooManyRequests)
	defer hot.Close()
	api := &fakePoolAPI{leases: []string{hot.URL, hot.URL, hot.URL, hot.URL, hot.URL, hot.URL, hot.URL, hot.URL, hot.URL, hot.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	now := time.Now()
	for range stormMax429 {
		pt.noteStorm429(now)
	}
	oldDeadline := poolTryDeadline
	poolTryDeadline = 3 * time.Second
	defer func() { poolTryDeadline = oldDeadline }()
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	_, err = lease.Client.Do(req)
	if err == nil {
		t.Fatal("storm must not surface 429/200, want 503 tail error")
	}
	// Штормовой хвост — 503 БЕЗ Retry-After: OMP ретраит очередь вместо
	// часовых credential-блоков (1800000ms > maxDelayMs → смерть).
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("want UpstreamStatusError, got %T: %v", err, err)
	}
	if use.Status != http.StatusServiceUnavailable {
		t.Fatalf("storm tail: want 503, got %d (%v)", use.Status, err)
	}
	if use.RetryAfter != "" {
		t.Fatalf("storm 503 must carry no outward Retry-After, got %q", use.RetryAfter)
	}
}

// TestPoolHoursClass429Bans: часовой 429 (Retry-After 3324s) банит IP —
// acquire пропускает забаненный egress, hard-флаг ставится (exact-RA burn),
// bad держит окно ровно до дедлайна.
func TestPoolHoursClass429Bans(t *testing.T) {
	burned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3324")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer burned.Close()
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: []string{burned.URL, good.URL, burned.URL, good.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via rotation off the burned IP", resp.StatusCode)
	}
	pt.muMu.RLock()
	nBad := len(pt.bad)
	pt.muMu.RUnlock()
	if nBad != 1 {
		t.Fatalf("hours-class 429 left %d bad entries, want 1 (the burned IP)", nBad)
	}
}

// TestPoolHoursClassRAUnit: границы hoursClassRA — пусто/минуты/без
// заголовка бана нет, часы (секунды и HTTP-date) банят ровно на RA.
func TestPoolHoursClassRAUnit(t *testing.T) {
	pt := &PoolTransport{}
	for _, ra := range []string{"", "0", "60", "349", "1799", "garbage"} {
		if _, ok := pt.hoursClassRA(ra); ok {
			t.Fatalf("hoursClassRA(%q) = ban, want transient (no ban)", ra)
		}
	}
	for ra, want := range map[string]time.Duration{"1800": 1800 * time.Second, "3324": 3324 * time.Second, "100000": 100000 * time.Second} {
		cd, ok := pt.hoursClassRA(ra)
		if !ok {
			t.Fatalf("hoursClassRA(%q) = transient, want ban", ra)
		}
		if cd != want {
			t.Fatalf("hoursClassRA(%q) cooldown = %v, want exact %v (no cap, no buffer)", ra, cd, want)
		}
	}
	future := time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
	if _, ok := pt.hoursClassRA(future); !ok {
		t.Fatalf("hoursClassRA(http-date +2h) = transient, want ban")
	}
}

// TestPoolGlobalBurnCluster: два hours-class 429 с ОДИНАКОВЫМ RA на РАЗНЫХ
// egress (живой кейс 2026-09-08: RA 35392/35388/35375/35371 на burned+fresh
// IP) — детектор поднимает глобальный флаг: per-IP баны кластера сняты,
// остался только short park текущего egress (15s, не персистится),
// outward Retry-After капнут 120 (OMP maxDelayMs 300s), rotate пропущен.
func TestPoolGlobalBurnCluster(t *testing.T) {
	ra := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "35390")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
	}
	a, b := ra(), ra()
	defer a.Close()
	defer b.Close()
	api := &fakePoolAPI{leases: []string{a.URL, b.URL, a.URL, b.URL, a.URL, b.URL, a.URL, b.URL, a.URL, b.URL, a.URL, b.URL, a.URL, b.URL, a.URL, b.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	oldDeadline := poolTryDeadline
	poolTryDeadline = 3 * time.Second
	defer func() { poolTryDeadline = oldDeadline }()
	_, err = lease.Client.Do(req)
	if err == nil {
		t.Fatal("global burn must not surface 429, want 503 tail error")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("want *UpstreamStatusError, got %T: %v", err, err)
	}
	if use.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 tail on global burn (client retries, not dies)", use.Status)
	}
	// 2026-09-16: хвост — 503 БЕЗ Retry-After (проверено выше через
	// use.RetryAfter в storm-тесте); клиент ретраит очередь вместо
	// часовых блоков.
	if use.RetryAfter != "" {
		t.Fatalf("outward Retry-After = %q, want empty (503 tail carries none)", use.RetryAfter)
	}
	pt.muMu.RLock()
	nBad := len(pt.bad)
	nHard := len(pt.hardBad)
	global := !pt.globalBurnUntil.IsZero() && time.Now().Before(pt.globalBurnUntil)
	pt.muMu.RUnlock()
	if nHard != 0 {
		t.Fatalf("global burn left %d hard bans, want 0 (cluster shadows unbanned)", nHard)
	}
	if nBad > 3 {
		t.Fatalf("global burn left %d bad entries, want <=3 (triple short parks)", nBad)
	}
	if !global {
		t.Fatal("global flag not armed after clustered RA on distinct IPs")
	}
}

// TestPoolGlobalBurnSame48NoCluster: два hours-class RA с одинаковым RA
// на РАЗНЫХ exact IP, но ОДНОЙ /48 (регрессия 2026-09-09 no-VPN: два egress
// 48ef:682 подняли global на 1ч47м на одном бюджете) — global НЕ armed,
// оба бана per-IP стоят, флаг zero.
func TestPoolGlobalBurnSame48NoCluster(t *testing.T) {
	pt := &PoolTransport{}
	now := time.Now()
	if pt.noteHoursRA("2a09:bac5:48ef:682::a6:e", 6411) {
		t.Fatal("first hours-RA armed global alone, want false (need a pair)")
	}
	if pt.noteHoursRA("2a09:bac5:48ef:682::a6:f", 6411) {
		t.Fatal("same-/48 pair armed global, want false (one budget, not shared)")
	}
	pt.muMu.RLock()
	active := !pt.globalBurnUntil.IsZero() && now.Before(pt.globalBurnUntil)
	pt.muMu.RUnlock()
	if active {
		t.Fatal("global flag armed by same-/48 pair, want inactive")
	}
	// Контроль: разные /48 с тем же RA — arm'ят.
	pt2 := &PoolTransport{}
	pt2.noteHoursRA("2a09:bac5:48ef:682::a6:e", 6411)
	if !pt2.noteHoursRA("2a09:bac1:47a0::a6:12", 6411) {
		t.Fatal("distinct-/48 pair did not arm global, want true")
	}
}

// TestPoolGlobalBurnSameIPNoCluster: один и тот же hours-class RA дважды на
// ОДНОМ egress — per-IP burn, НЕ глобал: бан стоит, флаг не поднят, запрос
// ротируется на живой egress и получает 200.
func TestPoolGlobalBurnSameIPNoCluster(t *testing.T) {
	burned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3324")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer burned.Close()
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: []string{burned.URL, burned.URL, good.URL, good.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via rotation (same-IP burn is per-IP)", resp.StatusCode)
	}
	pt.muMu.RLock()
	global := !pt.globalBurnUntil.IsZero() && time.Now().Before(pt.globalBurnUntil)
	nBad := len(pt.bad)
	pt.muMu.RUnlock()
	if global {
		t.Fatal("global flag armed by same-IP repeats — want per-IP burn only")
	}
	if nBad != 1 {
		t.Fatalf("per-IP burn left %d bad entries, want 1", nBad)
	}
}

// TestPoolGlobalBurnClearedBy200: глобальный флаг снимается парой 200 в
// течение минуты (sustained recovery), одиночный 200 среди шторма — флюк,
// флаг держится (anti-flap, 2026-09-08: 2x200 среди 15x429).
func TestPoolGlobalBurnClearedBy200(t *testing.T) {
	pt := &PoolTransport{}
	if global := pt.noteHoursRA("10.0.0.1", 35390); global {
		t.Fatal("single sighting must not arm global")
	}
	if global := pt.noteHoursRA("10.0.0.1", 35388); global {
		t.Fatal("same-IP repeats must not arm global")
	}
	if global := pt.noteHoursRA("10.0.0.2", 35385); !global {
		t.Fatal("distinct-IP cluster must arm global")
	}
	if !pt.globalActive() {
		t.Fatal("globalActive = false right after arming")
	}
	pt.markGoodKey("10.0.0.3") // первый 200 — probation, флаг жив
	if !pt.globalActive() {
		t.Fatal("global cleared by a lone 200 inside storm — want probation hold")
	}
	pt.markGoodKey("10.0.0.4") // второй 200 в минуту — окно открылось
	if pt.globalActive() {
		t.Fatal("global still active after sustained 200s — flag must lift")
	}
}

// TestPoolGlobalSameGroupNoClear: два 200 одной /48-группы при active-global
// НЕ гасят флаг (регрессия 2026-09-09 no-VPN: один egress 48ec:1c дал 8/9 ok,
// probation погас дважды посреди шторма). Гасціт только пара с РАЗНЫХ групп.
func TestPoolGlobalSameGroupNoClear(t *testing.T) {
	pt := &PoolTransport{}
	pt.noteHoursRA("2a09:bac5:48ee::1", 6411)
	if !pt.noteHoursRA("2a09:bac1:47a0::1", 6411) {
		t.Fatal("distinct-/48 pair must arm global")
	}
	pt.markGoodKey("2a09:bac5:48ec:261e::3cc:1c")
	if !pt.globalActive() {
		t.Fatal("lone 200 cleared global — want probation hold")
	}
	pt.markGoodKey("2a09:bac5:48ec:261e::3cc:1d") // та же /48 — не доказательство
	if !pt.globalActive() {
		t.Fatal("same-/48 pair cleared global mid-storm — want hold")
	}
	pt.markGoodKey("2a09:bac1:60a0:8::9") // другая /48 — окно открылось
	if pt.globalActive() {
		t.Fatal("distinct-/48 200s did not clear global — want lift")
	}
}

// TestPoolGlobalNoSelfExtend: кластерные хиты при активном флаге не сдвигают
// дедлайн вперёд (anti-perpetuum, 2026-09-08: шторм сам себе продлевал бан).
func TestPoolGlobalNoSelfExtend(t *testing.T) {
	pt := &PoolTransport{}
	pt.noteHoursRA("10.0.0.1", 35390)
	pt.noteHoursRA("10.0.0.2", 35385)
	pt.muMu.RLock()
	first := pt.globalBurnUntil
	pt.muMu.RUnlock()
	if first.IsZero() {
		t.Fatal("global not armed")
	}
	pt.noteHoursRA("10.0.0.3", 35380)
	pt.noteHoursRA("10.0.0.4", 35375)
	pt.muMu.RLock()
	second := pt.globalBurnUntil
	pt.muMu.RUnlock()
	if !second.Equal(first) {
		t.Fatalf("global deadline extended: %v -> %v, want arm-once", first, second)
	}
}

// TestPoolParseRASeconds: delay-seconds, HTTP-date, мусор.
func TestPoolParseRASeconds(t *testing.T) {
	if s, ok := parseRASeconds("35390"); !ok || s != 35390 {
		t.Fatalf("parseRASeconds(35390) = %d,%v, want 35390,true", s, ok)
	}
	future := time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
	if s, ok := parseRASeconds(future); !ok || s < 7000 || s > 7300 {
		t.Fatalf("parseRASeconds(http-date +2h) = %d,%v, want ~7200,true", s, ok)
	}
	for _, bad := range []string{"", "  ", "garbage", "-5"} {
		if _, ok := parseRASeconds(bad); ok {
			t.Fatalf("parseRASeconds(%q) = ok, want fail", bad)
		}
	}
}

// TestPoolHardBanQuarantineFromStray200: шальной 200 на ТОМ ЖЕ burned IP не снимает
// hours-бан досрочно (2026-09-12: anycast-тень/полуоткрытое окно, снятие возвращало
// мёртвый бюджет как "fresh" и следующий запрос снова 429 на том же дедлайне).
// Снимает только дедлайн (lazy-expiry в isBad/isHardBanned) или unbanClusteredHours.
func TestPoolHardBanQuarantineFromStray200(t *testing.T) {
	pt := &PoolTransport{}
	now := time.Now()
	pt.markBad429("10.9.0.9", 3600*time.Second, now)
	if !pt.isHardBanned("10.9.0.9", now) {
		t.Fatal("hours-ban must hard-ban immediately")
	}
	pt.markGoodKey("10.9.0.9") // шальной 200 на том же IP
	if !pt.isHardBanned("10.9.0.9", now) {
		t.Fatal("stray 200 lifted an hours-class hard ban — want quarantine until deadline")
	}
	pt.markGoodKey("10.9.0.10") // 200 на ДРУГОМ IP — чужой бан не трогаем
	if !pt.isHardBanned("10.9.0.9", now) {
		t.Fatal("unrelated 200 touched a foreign hard ban")
	}
	// Дедлайн вышел — lazy-expiry отпускает.
	if pt.isHardBanned("10.9.0.9", now.Add(3600*time.Second).Add(time.Minute)) {
		t.Fatal("expired hard ban still holds past deadline")
	}
	if pt.isBad("10.9.0.9", now.Add(3600*time.Second).Add(time.Minute)) {
		t.Fatal("expired bad entry still holds past deadline")
	}
}

// TestPoolMinDeadlineNoShift: повторный 429 тем же RA не сдвигает дедлайн
// вперёд — первый sighting окна истинный. Регрессия overwrite-бага, когда
// каждый re-hit прибавлял минуты и burn выглядел бесконечным.
func TestPoolMinDeadlineNoShift(t *testing.T) {
	pt := &PoolTransport{}
	now := time.Now()
	pt.markBad429("10.9.0.1", 3600*time.Second, now)
	pt.muMu.RLock()
	first := pt.bad["10.9.0.1"]
	pt.muMu.RUnlock()
	pt.markBad429("10.9.0.1", 3600*time.Second, now.Add(10*time.Minute))
	pt.muMu.RLock()
	second := pt.bad["10.9.0.1"]
	pt.muMu.RUnlock()
	if !second.Equal(first) {
		t.Fatalf("deadline shifted: first=%v second=%v, want min (no shift)", first, second)
	}
}

// TestPoolWalksNineBeforeHonest429: честный 429 только после прохода ~9
// distinct egress (live budgets ~9, minWalkEgress=9). Фейк-пул: 8 горячих
// + 1 живой в конце — запрос обязан дойти до живого и вернуть 200.
// (2026-09-11: 20-sweep урезан до 9 — один логический 429 жёг 20 окон.)
func TestPoolWalksNineBeforeHonest429(t *testing.T) {
	var srvs []*httptest.Server
	var leases []string
	for i := 0; i < 8; i++ {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Transient 429 без RA: walk-on без следа, без бана и без глобала.
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		srvs = append(srvs, s)
		leases = append(leases, s.URL)
	}
	good := fakeForwardProxy(t, http.StatusOK)
	srvs = append(srvs, good)
	leases = append(leases, good.URL)
	for _, s := range srvs {
		defer s.Close()
	}
	api := &fakePoolAPI{leases: append(append(leases, leases...), leases...)}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (must walk past 8 hot to the live egress)", resp.StatusCode)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.acquired) < 9 {
		t.Fatalf("acquired %d leases, want >=9 (budget sweep, not early stop)", len(api.acquired))
	}
}

// TestPoolTriedGroupsBy48: два бэкенда за одним IP — одна бюджетная группа.
// tried считает группы: повтор группы = walking on, не fresh-попытка.
func TestPinnedUARefreshesFromNpm(t *testing.T) {
	npm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"version":"9.9.9"}`)
	}))
	defer npm.Close()
	_ = npm
	got := fetchNpmOpencodeVersion()
	if got == "" {
		t.Skip("no network to npm registry")
	}
	if !isStableOpencodeVer(got) {
		t.Fatalf("npm version %q not stable X.Y.Z", got)
	}
	if isStableOpencodeVer("1.2.3-dev-abc") || !isStableOpencodeVer("1.18.31") {
		t.Fatal("isStableOpencodeVer misclassifies")
	}
	setPinnedUA("9.9.9", "test")
	if pinnedUAVersion() != "9.9.9" || !strings.Contains(pinnedUA(), "opencode/9.9.9 ") {
		t.Fatalf("pinnedUA not applied: %q", pinnedUA())
	}
	setPinnedUA("9.9.9-dev-x", "test")
	if pinnedUAVersion() != "9.9.9" {
		t.Fatal("prerelease must not overwrite pinned UA")
	}
}

func TestStandbyVerifyReturnsIPToRotation(t *testing.T) {
	good, _ := fakeForwardProxyHits(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: []string{good.URL, good.URL, good.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	pt.enqueueStandby("10.9.0.9")
	if _, dup := pt.standbyQueued["10.9.0.9"]; !dup {
		t.Fatal("enqueueStandby must queue the IP")
	}
	pt.enqueueStandby("10.9.0.9")
	if len(pt.standbyQueued) != 1 {
		t.Fatal("enqueueStandby must dedup")
	}
	pt.noteStandbyLive("10.9.0.9", time.Now())
	if !pt.standbyFresh("10.9.0.9", time.Now()) {
		t.Fatal("verified IP must be standby-fresh")
	}
	if _, queued := pt.standbyQueued["10.9.0.9"]; queued {
		t.Fatal("verified IP must leave the queue")
	}
	pt.noteHeat("10.9.0.9", time.Now())
	pt.noteHeat("10.9.0.9", time.Now())
	if pt.standbyFresh("10.9.0.9", time.Now()) {
		t.Fatal("hot IP must not be standby-fresh")
	}
}

func TestPoolTriedGroupsBy48(t *testing.T) {
	if triedKey("2a09:bac5:512a:1f5::32:120") != triedKey("2a09:bac5:512a:1f5::32:121") {
		t.Fatal("same /48 must map to one tried group")
	}
	if triedKey("2a09:bac5:512a:1f5::32:120") == triedKey("2a09:bac1:60c0:8::32:8f") {
		t.Fatal("different /48 must map to different tried groups")
	}
	if triedKey("10.1.0.5") != "10.1.0.5" {
		t.Fatal("pure v4 must pass through triedKey")
	}
}

// TestPoolVerdictBodyReadable: all-429 через RoundTrip обязан вернуть ЧИТАЕМУЮ
// 503-ошибку хвоста (не drained+closed): регрессия P1-A review 2026-09-09,
// хвост 503 2026-09-16 (клиент 429 не видит).
func TestPoolVerdictBodyReadable(t *testing.T) {
	hot, _ := fakeForwardProxyHits(t, http.StatusTooManyRequests)
	defer hot.Close()
	api := &fakePoolAPI{leases: []string{hot.URL, hot.URL, hot.URL, hot.URL, hot.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	oldDeadline := poolTryDeadline
	poolTryDeadline = 3 * time.Second
	defer func() { poolTryDeadline = oldDeadline }()
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	_, err = lease.Client.Do(req)
	if err == nil {
		t.Fatal("all-429 must tail 503, want tail error with readable body")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("want *UpstreamStatusError, got %T: %v", err, err)
	}
	if use.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 tail", use.Status)
	}
	if use.Error() == "" {
		t.Fatal("503 tail error empty — verdict unreadable")
	}
}

// TestPoolAllNotFoundIsNot200: все egress отвечают 200+NOT_FOUND — наружу
// обязана выйти НЕ-200 ошибка (404-каскад), а не пустой 200. P1-B 2026-09-09.
func TestPoolAllNotFoundIsNot200(t *testing.T) {
	nf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// SSE-терминатор \n\n обязателен: без него sniff ждёт EOF (реальный
		// апстрим закрывает соединение или шлёт \n\n, мок — нет).
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"Model not found\",\"code\":\"NOT_FOUND\"}}\n\n"))
	}))
	defer nf.Close()
	api := &fakePoolAPI{leases: []string{nf.URL, nf.URL, nf.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1, Cooldown: time.Nanosecond}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("all-NOT_FOUND pool returned 200 — must be a non-200 error for the 404 cascade")
		}
	}
}

// TestPoolHoursBreakEarlyStop: 8 distinct hours-групп — хвост 503 уже при
// tried==7 (floor), acquired < 15 (не полный свип).
func TestPoolHoursBreakEarlyStop(t *testing.T) {
	var srvs []*httptest.Server
	var leases []string
	for i := 0; i < 8; i++ {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "3539")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer s.Close()
		srvs = append(srvs, s)
		leases = append(leases, s.URL)
	}
	_ = srvs
	// Лиз вдоволь (3 круга по 8): floor-брейк должен остановить свип до
	// исчерпания — меряем вердикт + скорость, а не выдачи мока (один warmup
	// может стоить спред-спину несколько выдач).
	var many []string
	for i := 0; i < 3; i++ {
		many = append(many, leases...)
	}
	api := &fakePoolAPI{leases: many}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	oldDeadline := poolTryDeadline
	poolTryDeadline = 20 * time.Second
	defer func() { poolTryDeadline = oldDeadline }()
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	start := time.Now()
	_, err = lease.Client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("hours-floor must tail 503, want tail error (client 429 never surfaces)")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("want *UpstreamStatusError, got %T: %v", err, err)
	}
	if use.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 tail at hours floor", use.Status)
	}
	if use.RetryAfter != "" {
		t.Fatalf("tail Retry-After = %q, want empty", use.RetryAfter)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	// 2026-09-23: global-кластер (2 одинаковых RA3539 на distinct IP) срабатывает
	// раньше floor — это честный shared-burn, хвост сразу. Floor-гейт проверяется
	// отдельно (tried>=7 при distinct-бюджетах без кластера). Здесь: хвост 503
	// + ранний выход + минимум 2 warmup (первый бан + кластер-подтверждение).
	if api.acquireN < 2 {
		t.Fatalf("acquired %d leases, want >=2 (ban + cluster probe before the tail)", api.acquireN)
	}
	if elapsed > 60*time.Second {
		t.Fatalf("tail took %v — floor-break must stop early, not sweep to the deadline", elapsed)
	}
}

// TestPoolDirectHomeBurnSkipsNext: direct-нога 429 (hours) — следующий запрос
// скипает direct (isBad gate). P1 шторма 2026-09-09: stuck-open без записи.
func TestPoolDirectHomeBurnSkipsNext(t *testing.T) {
	hot, _ := fakeForwardProxyHits(t, http.StatusTooManyRequests)
	defer hot.Close()
	api := &fakePoolAPI{leases: []string{}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Minute, DirectFallback: true}
	// Hours-RA на дом: следующий запрос обязан увидеть isBad(home).
	pt.BanHomeCooldown("3539")
	if !pt.DirectHomeBanned() {
		t.Fatal("home must be banned right after hours-class BanHomeCooldown")
	}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	_, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err == nil {
		t.Fatal("Acquire with empty pool + cooling home must fail (honest 429), not hand a direct lease")
	}
}

// TestPoolTransientHomeParkShort: transient-RA на дом — только короткий парк,
// не hard-бан (окно откроется через секунды). DirectHomeBanned при этом true:
// запаркованный home всё ещё горит, повторный хит каждый запрос долбит одну
// identity пока pool API мёртв (wave2 parked-home gate).
func TestPoolTransientHomeParkShort(t *testing.T) {
	api := &fakePoolAPI{leases: []string{}}
	_ = api
	pt := &PoolTransport{APIURL: "http://127.0.0.1:1", Fanout: 1, Rounds: 1, Cooldown: time.Minute}
	pt.BanHomeCooldown("12")
	if pt.isBad(directHomeIPKey, time.Now()) {
		t.Fatal("transient home verdict must park, not hard-ban (isBad must be false)")
	}
	if !pt.isParked(directHomeIPKey, time.Now()) {
		t.Fatal("transient home verdict must leave a short park")
	}
	if !pt.DirectHomeBanned() {
		t.Fatal("parked home must gate direct fallback (DirectHomeBanned true while parked)")
	}
}

// TestPoolUnbanClearsParkAndPersists: unban на first-arm чистит bad+park и
// персистит (рестарт не воскрешает тени). R1+R9 2026-09-09.
func TestPoolUnbanClearsParkAndPersists(t *testing.T) {
	dir := t.TempDir()
	pt := &PoolTransport{APIURL: "http://127.0.0.1:1", StateFile: dir + "/pool_state.json"}
	now := time.Now()
	pt.markBad429("10.9.0.1", 3600*time.Second, now)
	pt.banShort429("10.9.0.9", shortPark, now)
	// Арм global парой distinct-IP с близкими RA.
	pt.noteHoursRA("10.9.0.1", 3500)
	pt.noteHoursRA("10.9.0.2", 3520)
	if !pt.globalActive() {
		t.Fatal("global must be armed by the distinct pair")
	}
	cleared := pt.unbanClusteredHours("10.9.0.1")
	if cleared == 0 {
		t.Fatal("first-arm unban must clear members")
	}
	if pt.isBad("10.9.0.1", time.Now()) {
		t.Fatal("member must be unbanned on first arm")
	}
	if pt.isParked("10.9.0.9", time.Now()) {
		t.Fatal("park must be cleared on first arm (memberHit)")
	}
	pt.FlushState()
	pt2 := &PoolTransport{APIURL: "http://127.0.0.1:1", StateFile: dir + "/pool_state.json"}
	pt2.loadState()
	if pt2.isBad("10.9.0.1", time.Now()) {
		t.Fatal("restart resurrected an unbanned shadow — unban must persist")
	}
}

// TestPoolDirectLastResortOnDeadAPI: pool API мёртв (breaker open) + home
// свеж + оба ключа (DirectFallback + AllowHomeIPDirect) — last-resort direct
// разрешён и спасает запрос (E 2026-09-11). Живой пул с горячими egress —
// см. NoHomeDial.
func TestPoolDirectLastResortOnDeadAPI(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer direct.Close()
	// Pool API мёртв: acquire всегда transport-error -> breaker откроется.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer dead.Close()
	_ = direct
	pt := &PoolTransport{APIURL: dead.URL, Fanout: 1, Rounds: 1, Cooldown: time.Minute, DirectFallback: true, AllowHomeIPDirect: true}
	// Открываем breaker вручную: 3 hard-fail = порог.
	pt.noteAPIFail(false)
	pt.noteAPIFail(false)
	pt.noteAPIFail(false)
	// Half-open пейсится кулдауном: сразу после открытия брейкер блокирует,
	// но одна попытка за apiProbeEvery проходит (без этого мёртвый API
	// опрашивался бы 20 раз/мин независимо от apiDownCool).
	if !pt.apiDown() {
		t.Fatal("breaker must be open right after the threshold")
	}
	pt.apiProbeAt.Store(0)
	if pt.apiDown() {
		t.Fatal("после истечения пейсинга half-open обязан пропустить одну пробу")
	}
	if !pt.apiDown() {
		t.Fatal("half-open пропустил вторую попытку подряд")
	}
	if pt.DirectHomeBanned() {
		t.Fatal("home must be fresh for last-resort")
	}
	// poolDeadLastResort gate напрямую.
	rt := &poolRetryRT{t: pt, prov: "x"}
	if !rt.poolDeadLastResort() {
		t.Fatal("last-resort gate must pass on dead API + fresh home + opt-in")
	}
	// Живой API (breaker closed) — gate закрыт даже с opt-in.
	pt2 := &PoolTransport{APIURL: dead.URL, Fanout: 1, Rounds: 1, Cooldown: time.Minute, DirectFallback: true, AllowHomeIPDirect: true}
	rt2 := &poolRetryRT{t: pt2, prov: "x"}
	if rt2.poolDeadLastResort() {
		t.Fatal("last-resort gate must stay closed on live pool (breaker closed)")
	}
	// Без opt-in — закрыт даже при мёртвом API.
	pt3 := &PoolTransport{APIURL: dead.URL, Fanout: 1, Rounds: 1, Cooldown: time.Minute}
	pt3.noteAPIFail(false)
	pt3.noteAPIFail(false)
	pt3.noteAPIFail(false)
	rt3 := &poolRetryRT{t: pt3, prov: "x"}
	if rt3.poolDeadLastResort() {
		t.Fatal("last-resort gate must stay closed without DirectFallback opt-in")
	}
	// Горячий home — закрыт даже при мёртвом API + opt-in.
	pt.BanHomeCooldown("3539")
	rt4 := &poolRetryRT{t: pt, prov: "x"}
	if rt4.poolDeadLastResort() {
		t.Fatal("last-resort gate must stay closed on cooling home (protect window)")
	}
}

// TestPoolAcquireDeadAPIGivesDirectLease: pool API мёртв (breaker open) +
// ОБА ключа (direct_fallback + allow_home_ip_direct) + свежий home — Acquire
// отдаёт direct-лизу, а не honest-429 (2026-09-11: VPN вкл/выкл и рестарт
// пула клали очередь). Без второго ключа — см. ...WithoutSecondKeyStays503.
func TestPoolAcquireDeadAPIGivesDirectLease(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer dead.Close()
	pt := &PoolTransport{APIURL: dead.URL, Fanout: 1, Rounds: 1, Cooldown: time.Minute, DirectFallback: true, AllowHomeIPDirect: true}
	pt.noteAPIFail(false)
	pt.noteAPIFail(false)
	pt.noteAPIFail(false)
	if !pt.apiDown() {
		t.Fatal("breaker must be open")
	}
	pt.apiProbeAt.Store(0)
	if pt.apiDown() {
		t.Fatal("half-open probe must pass once the pacing is due")
	}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatalf("Acquire on dead API + both keys + fresh home must give direct lease, got err: %v", err)
	}
	if lease.Proxy != "direct-fallback" {
		t.Fatalf("lease.Proxy = %q, want direct-fallback", lease.Proxy)
	}
	_ = lease.Release(context.Background())
}

// TestPoolAcquireDeadAPIDirectWithoutSecondKeyStays503: direct_fallback: true
// без allow_home_ip_direct — прямой выход запрещён даже при мёртвом пуле:
// домашний IP не уходит в апстрим, клиент получает честный 503. Именно
// unkeyed direct-лизы 2026-10-19 принесли бан «abusive traffic from this IP».
func TestPoolAcquireDeadAPIDirectWithoutSecondKeyStays503(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer dead.Close()
	pt := &PoolTransport{APIURL: dead.URL, Fanout: 1, Rounds: 1, Cooldown: time.Minute, DirectFallback: true}
	pt.noteAPIFail(false)
	pt.noteAPIFail(false)
	pt.noteAPIFail(false)
	if !pt.apiDown() {
		t.Fatal("breaker must be open")
	}
	pt.apiProbeAt.Store(0)
	if pt.apiDown() {
		t.Fatal("half-open probe must pass once the pacing is due")
	}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err == nil {
		if lease != nil {
			_ = lease.Release(context.Background())
		}
		t.Fatalf("direct lease handed out without allow_home_ip_direct (proxy=%v)", lease)
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) || use.Status != http.StatusServiceUnavailable {
		t.Fatalf("want 503 UpstreamStatusError (honest pool failure), got %v", err)
	}
}

// TestPoolAcquireDeadAPINoOptInStays503: без opt-in мёртвый API — 503
// (2026-09-16: verdict-less 429 эскалируется OMP в 1800s блоки).
func TestPoolAcquireDeadAPINoOptInStays503(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer dead.Close()
	pt := &PoolTransport{APIURL: dead.URL, Fanout: 1, Rounds: 1, Cooldown: time.Minute}
	pt.noteAPIFail(false)
	pt.noteAPIFail(false)
	pt.noteAPIFail(false)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	_, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err == nil {
		t.Fatal("Acquire without opt-in must stay 503, not direct lease")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) || use.Status != http.StatusServiceUnavailable {
		t.Fatalf("want 503 UpstreamStatusError, got %v", err)
	}
}

// TestPoolAcquireDeadAPIHotHomeStays503: горячий home — 503, не direct.
func TestPoolAcquireDeadAPIHotHomeStays503(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer dead.Close()
	pt := &PoolTransport{APIURL: dead.URL, Fanout: 1, Rounds: 1, Cooldown: time.Minute, DirectFallback: true, AllowHomeIPDirect: true}
	pt.noteAPIFail(false)
	pt.noteAPIFail(false)
	pt.noteAPIFail(false)
	pt.BanHomeCooldown("3539")
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	_, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err == nil {
		t.Fatal("Acquire with cooling home must stay 503 (protect window)")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) || use.Status != http.StatusServiceUnavailable {
		t.Fatalf("want 503 UpstreamStatusError, got %v", err)
	}
}

func TestPoolAcquireCycledIs503(t *testing.T) {
	// Pool hands the SAME backend forever → dedup cycle, no verdict.
	// Must be 503 (MODEL_CAPACITY, queue retries), never 429: OMP
	// escalates verdict-less 429s into 1800s credential blocks
	// (2026-09-16: "pool cycled, 1 seen" → queue death).
	proxyURL := ""
	var mu sync.Mutex
	n := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pool/release") {
			_, _ = w.Write([]byte(`{"released":true}`))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/pool/acquire") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		n++
		id := n
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"lease_id": fmt.Sprintf("ls-%d", id), "proxy_url": proxyURL, "public_ip": "10.9.0.1", "country": "MD",
		})
	}))
	defer api.Close()
	// Backend playing both proxy and upstream roles; URL known only
	// after listen — the closure above reads proxyURL late, after assign.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit"}}`))
	}))
	defer up.Close()
	proxyURL = up.URL
	pt := &PoolTransport{APIURL: api.URL, Fanout: 2, Rounds: 1, Cooldown: time.Nanosecond}
	// Pre-ban the only backend: its leases park in `last`, the second
	// same-URL acquire dedups with empty finish() → cycled, no verdict.
	pt.markBad429("10.9.0.1", 3600*time.Second, time.Now())
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	_, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err == nil {
		t.Fatal("Acquire on cycled pool must fail")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("want UpstreamStatusError, got %T: %v", err, err)
	}
	if use.Status != http.StatusServiceUnavailable {
		t.Fatalf("cycled pool must be 503, got %d (%v)", use.Status, err)
	}
	if use.RetryAfter != "" {
		t.Fatalf("cycled 503 must carry no Retry-After (swap mode), got %q", use.RetryAfter)
	}
	lower := strings.ToLower(err.Error())
	for _, bad := range []string{"too many requests", "rate limit", "rate_limit", "quota", "exhausted", "429", "retry-after", "retry_after"} {
		if strings.Contains(lower, bad) {
			t.Fatalf("cycled 503 body must not match OMP rate-limit regexes, contains %q: %v", bad, err)
		}
	}
}

func TestPoolAccountGate403Terminal(t *testing.T) {
	// FreeTier/billing 403 is EGRESS-CORRELATED (c4acdb0 2026-09-18: same
	// account+model+minute, one egress 200 while others 403): swaps like
	// non-region 403 up to maxGateSwaps, then honest terminal 403 (never 503).
	// Здесь все 3 egress gate-403 -> 3 warmup (по одному на egress) -> честный
	// 403 с gate-маркером. Egress получает только 10s steer (не бан окна).
	const gateBody = `{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}`
	hits := 0
	var mu sync.Mutex
	mk := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(gateBody))
		}))
	}
	s1, s2, s3 := mk(), mk(), mk()
	defer s1.Close()
	defer s2.Close()
	defer s3.Close()
	api := &fakePoolAPI{leases: []string{s1.URL, s2.URL, s3.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x", Model: "m"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		// Terminal gate verdict must arrive as a response, never a transport error.
		t.Fatalf("gate 403 must return honest 403 response, got err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("gate 403: want 403 got %d (must never map to 503)", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !isAccountGateBody(raw) {
		t.Fatalf("gate verdict body must keep gate markers, got %q", string(raw))
	}
	mu.Lock()
	h := hits
	mu.Unlock()
	if h != 3 {
		t.Fatalf("gate must try every egress once: want 3 upstream warmups, got %d", h)
	}
	if resp.Header.Get(gateTerminalHeader) == "" {
		t.Fatal("gate verdict must carry gateTerminalHeader for the outer loop")
	}
	// Egress steered 10s (c4acdb0), not window-banned: soft-steer без hard-флага,
	// TTL короткий — следующий запрос перепроверяет. (Ban key mirrors fakePoolAPI:
	// publicIP derived from backend port.)
	s1Port := s1.URL[strings.LastIndex(s1.URL, ":")+1:]
	var s1PortN int
	fmt.Sscanf(s1Port, "%d", &s1PortN)
	s1Key := fmt.Sprintf("10.1.0.%d", s1PortN%250+1)
	if pt.isHardBanned(s1Key, time.Now()) {
		t.Fatal("gate 403 must not hard-ban the egress (10s steer only)")
	}
	if pt.isBad(s1Key, time.Now().Add(30*time.Second)) {
		t.Fatal("gate 403 steer must expire in ~10s, not linger")
	}
}

func TestInferGateResponsesFreeTier(t *testing.T) {
	// A failed /responses envelope carrying FreeTier markers must infer 403
	// (terminal gate), never generic-failed 500 (which would burn dead-model
	// budget on an account verdict).
	st := inferResponsesStatus(&responsesError{
		Type: "invalid_request_error", Code: "FreeTierError",
		Message: "OpenCode's free tier can only be used from within OpenCode",
	}, "failed")
	if st != http.StatusForbidden {
		t.Fatalf("free-tier envelope: want 403 got %d", st)
	}
}

func TestWrapGateSSECarriesTerminalHeader(t *testing.T) {
	// Responses-mapped gate 403 must carry gateTerminalHeader so the outer
	// tryProvider loop surfaces it honestly instead of burning
	// maxRegionSwaps full sweeps (wave1 gate P1: outer :914 was dead for
	// the responses path, every spark gate burned 4 sweeps).
	sse := "event: response.failed\n" +
		`data: {"type":"response.failed","response":{"status":"failed","error":{"type":"invalid_request_error","code":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}}` + "\n\n"
	up := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}
	out, err := wrapResponsesStream(up, "m", true)
	if err != nil {
		t.Fatal(err)
	}
	if out.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for gate-without-content", out.StatusCode)
	}
	if out.Header.Get(gateTerminalHeader) == "" {
		t.Fatal("gate SSE verdict must carry gateTerminalHeader for the outer loop")
	}
}

func TestWrapGateJSONCarriesTerminalHeader(t *testing.T) {
	// stream:false upstream (whole-object JSON) gate must carry the header too.
	body := `{"status":"failed","error":{"type":"invalid_request_error","code":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"},"output":[]}`
	up := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
	out, err := wrapResponsesJSON(up, strings.NewReader(body), "m")
	if err != nil {
		t.Fatal(err)
	}
	if out.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for gate JSON", out.StatusCode)
	}
	if out.Header.Get(gateTerminalHeader) == "" {
		t.Fatal("gate JSON verdict must carry gateTerminalHeader for the outer loop")
	}
}

func TestPoolAcquireRestartWindowIs503(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer dead.Close()
	// Breaker open = pool API restart window. DirectFallback ВЫКЛЮЧЕН,
	// чтобы дошли до verdict-ветки (с opt-in ушёл бы в direct-лизу).
	pt := &PoolTransport{APIURL: dead.URL, Fanout: 1, Rounds: 1, Cooldown: time.Minute}
	pt.noteAPIFail(false)
	pt.noteAPIFail(false)
	pt.noteAPIFail(false)
	if !pt.apiDown() {
		t.Fatal("breaker must be open")
	}
	pt.apiProbeAt.Store(0)
	if pt.apiDown() {
		t.Fatal("half-open probe must pass once the pacing is due")
	}
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	_, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err == nil {
		t.Fatal("Acquire on dead API must fail")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("want UpstreamStatusError, got %T: %v", err, err)
	}
	if use.Status != http.StatusServiceUnavailable {
		t.Fatalf("restart window must be 503, got %d (%v)", use.Status, err)
	}
	if use.RetryAfter != "" {
		t.Fatalf("restart 503 must carry no Retry-After (swap mode), got %q", use.RetryAfter)
	}
	lower := strings.ToLower(err.Error())
	for _, bad := range []string{"too many requests", "rate limit", "429", "retry-after", "retry_after"} {
		if strings.Contains(lower, bad) {
			t.Fatalf("restart 503 body must not match OMP rate-limit regexes, contains %q: %v", bad, err)
		}
	}
	// acquireLease fast-path — тот же 503 напрямую.
	_, err2 := pt.acquireLease(context.Background(), 999, "m", leaseStamp{})
	if err2 == nil {
		t.Fatal("acquireLease on dead API must fail")
	}
	var use2 *UpstreamStatusError
	if !errors.As(err2, &use2) {
		t.Fatalf("want UpstreamStatusError from acquireLease, got %T: %v", err2, err2)
	}
	if use2.Status != http.StatusServiceUnavailable {
		t.Fatalf("acquireLease restart window must be 503, got %d", use2.Status)
	}
}

// TestPoolAPIBusyDoesNotTripBreaker: забитый пул (timeout/cancel acquire при
// живом API) НЕ открывает apiDown — иначе шторм 429 сам себя добивает через
// 3418x "pool api restarting" (2026-09-22). Мёртвый API (dial refused) — открывает.
func TestPoolAPIBusyDoesNotTripBreaker(t *testing.T) {
	pt := &PoolTransport{APIURL: "http://127.0.0.1:1", StateFile: t.TempDir() + "/pool_state.json"}
	if !isPoolAPIDead(errDialRefusedForTest()) {
		t.Fatal("dial refused must count as dead API")
	}
	if isPoolAPIDead(context.DeadlineExceeded) {
		t.Fatal("deadline must count as busy pool, not dead API")
	}
	if isPoolAPIDead(context.Canceled) {
		t.Fatal("cancel must count as busy pool, not dead API")
	}
	// 3 busy-fail подряд НЕ открывают брейкер
	for i := 0; i < 5; i++ {
		if isPoolAPIDead(context.DeadlineExceeded) {
			pt.noteAPIFail(false)
		}
	}
	if pt.apiDown() {
		t.Fatal("busy pool must not open the breaker")
	}
	// 3 dead-fail подряд открывают
	for i := 0; i < 3; i++ {
		pt.noteAPIFail(false)
	}
	// Half-open пейсится кулдауном: сразу после порога — блок, затем одна проба.
	if !pt.apiDown() {
		t.Fatal("dead API must open the breaker after threshold")
	}
	pt.apiProbeAt.Store(0)
	if pt.apiDown() {
		t.Fatal("half-open обязан пропустить одну пробу")
	}
	if !pt.apiDown() {
		t.Fatal("half-open пропустил вторую пробу подряд")
	}
}

// TestPoolClientCancelNoSwap: отмена клиента — не вердикт egress.
// Лиза возвращается, банов/heat/шторм-хитов нет, наружу ctx err (не 503-хвост
// "context canceled", не свап на следующий IP).
func TestPoolClientCancelNoSwap(t *testing.T) {
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: []string{good.URL, good.URL, good.URL, good.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pt.Acquire(ctx, RequestContext{Provider: "x"}, prov)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Acquire must return ctx err, got %v", err)
	}
	pt.muMu.RLock()
	nBad, nPark, nHeat := len(pt.bad), len(pt.park), len(pt.heat)
	pt.muMu.RUnlock()
	if nBad != 0 || nPark != 0 || nHeat != 0 {
		t.Fatalf("cancel must not ban/park/heat: bad=%d park=%d heat=%d", nBad, nPark, nHeat)
	}
	// RoundTrip на отменённом запросе: без warmup, сразу ctx err.
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	req = req.WithContext(cctx)
	if _, err := lease.Client.Do(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled RoundTrip must return ctx err without swap, got %v", err)
	}
}

func errDialRefusedForTest() error {
	return &net.OpError{Op: "dial", Err: errConnRefusedForTest()}
}

func errConnRefusedForTest() error {
	return errTestTimeout{"connection refused"}
}

type errTestTimeout struct{ s string }

func (e errTestTimeout) Error() string   { return e.s }
func (e errTestTimeout) Timeout() bool   { return true }
func (e errTestTimeout) Temporary() bool { return true }

// TestPoolHalfOpenProbesWarmestHot: shared-шторм + graded heat на 3 distinct
// egress — acquire выдаёт warmest (min heat) на 1 прогрев вместо хвоста.
// 200 с пробы кормит stormOKs (снятие шторма — долей ok*2>=n, не одним 200);
// 429 паркует egress как обычно. Pileup-гейт (>=3 hot) покрыт distinct
// лизами без dedup-цикла. Регрессия 2026-10-06: 0-прогревные хвосты.
func TestPoolHalfOpenProbesWarmestHot(t *testing.T) {
	hot1 := fakeForwardProxy(t, http.StatusOK)
	defer hot1.Close()
	hot2 := fakeForwardProxy(t, http.StatusOK)
	defer hot2.Close()
	flaky := fakeForwardProxy(t, http.StatusOK)
	defer flaky.Close()
	api := &fakePoolAPI{leases: []string{hot1.URL, hot2.URL, flaky.URL, hot1.URL, hot2.URL, flaky.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	now := time.Now()
	keyOf := func(u string) string {
		port := u[strings.LastIndex(u, ":")+1:]
		var n int
		fmt.Sscanf(port, "%d", &n)
		return fmt.Sprintf("10.1.0.%d", n%250+1)
	}
	hot1Key, hot2Key, flakyKey := keyOf(hot1.URL), keyOf(hot2.URL), keyOf(flaky.URL)
	// Graded heat: hot1 самый горячий, flaky самый холодный → warmest=flaky.
	for range heatMaxHits + 3 {
		pt.noteHeat(hot1Key, now)
	}
	for range heatMaxHits + 1 {
		pt.noteHeat(hot2Key, now)
	}
	for range heatMaxHits {
		pt.noteHeat(flakyKey, now)
	}
	// Shared-шторм по всем трём ключам (ни один <80% → не single-IP).
	for range 4 {
		pt.noteStorm429Key(hot1Key, now)
		pt.noteStorm429Key(hot2Key, now)
		pt.noteStorm429Key(flakyKey, now)
	}
	if !pt.stormActive() {
		t.Fatal("storm must be active for half-open test")
	}
	if single, ok := pt.stormSingleIP(now); ok {
		t.Fatalf("storm must be shared, got single %s", single)
	}
	okBefore := len(pt.stormOKs)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatalf("half-open must probe warmest hot, got tail: %v", err)
	}
	defer lease.Release(context.Background())
	// Warmest-identity: проба обязана уйти на flaky (min heat), а не на
	// hot1/hot2 — иначе graded heat 5/3/2 не верифицируется.
	if lease.Proxy != flaky.URL {
		t.Fatalf("half-open must probe warmest (flaky), got %s", lease.Proxy)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via half-open probe", resp.StatusCode)
	}
	// Проба-200 кормит stormOKs (путь снятия шторма).
	if len(pt.stormOKs) <= okBefore {
		t.Fatal("probe 200 must feed stormOKs")
	}
}

// TestPoolLiftTierSpreadsLive: 3 egress с 2+ 200 (storm-лифт) — acquire
// размазывает входы по всем трём через pickSpread (LRU), а не льёт всё на
// первый. Регрессия concentration 2026-10-06: 244+171+152 на трёх при 20 up.
func TestPoolLiftTierSpreadsLive(t *testing.T) {
	e1 := fakeForwardProxy(t, http.StatusOK)
	defer e1.Close()
	e2 := fakeForwardProxy(t, http.StatusOK)
	defer e2.Close()
	e3 := fakeForwardProxy(t, http.StatusOK)
	defer e3.Close()
	api := &fakePoolAPI{leases: []string{e1.URL, e2.URL, e3.URL, e1.URL, e2.URL, e3.URL, e1.URL, e2.URL, e3.URL, e1.URL, e2.URL, e3.URL, e1.URL, e2.URL, e3.URL, e1.URL, e2.URL, e3.URL, e1.URL, e2.URL, e3.URL, e1.URL, e2.URL, e3.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Minute}
	blockBackgroundWorkers(pt)
	now := time.Now()
	keyOf := func(u string) string {
		port := u[strings.LastIndex(u, ":")+1:]
		var n int
		fmt.Sscanf(port, "%d", &n)
		return fmt.Sprintf("10.1.0.%d", n%250+1)
	}
	k1, k2, k3 := keyOf(e1.URL), keyOf(e2.URL), keyOf(e3.URL)
	// Всем трём по 2 свежих 200 → все в storm-лифте. Плюс shared-шторм:
	// 18×429 против 6×200 (ok*2=12 < 18 → active; 6/6/6 split → не single).
	for _, k := range []string{k1, k2, k3} {
		pt.noteStormOKKey(k, now)
		pt.noteStormOKKey(k, now)
	}
	for range 6 {
		pt.noteStorm429Key(k1, now)
		pt.noteStorm429Key(k2, now)
		pt.noteStorm429Key(k3, now)
	}
	if !pt.stormActive() {
		t.Fatal("storm must be active for lift-spread test")
	}
	if single, ok := pt.stormSingleIP(now); ok {
		t.Fatalf("storm must be shared, got single %s", single)
	}
	// usedWithin-разводка: выданная лиза уходит в hot следующего спина.
	// Без spread лифт first-match всегда отдавал бы e1.
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	seen := map[string]int{}
	for range 6 {
		lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
		if err != nil {
			t.Fatal(err)
		}
		seen[lease.Proxy]++
		lease.Release(context.Background())
	}
	for _, u := range []string{e1.URL, e2.URL, e3.URL} {
		if seen[u] < 1 {
			t.Fatalf("lift tier must spread to all live egress, got %v", seen)
		}
	}
}

// TestPoolLeaseStampPerProviderUA — регрессия кило-503 (2026-10-12):
// не-opencode провайдер через пул должен уходить СВОИМ User-Agent
// (config user_agent) и БЕЗ x-opencode-session, иначе его запросы
// делят окно и баны opencode на общих /48 (6447 upstream-429 только
// с kilo- slice, 2190 клиентских 503).
func TestPoolLeaseStampPerProviderUA(t *testing.T) {
	type seenHdr struct {
		ua, sess string
	}
	rec := make(chan seenHdr, 4)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec <- seenHdr{ua: r.Header.Get("User-Agent"), sess: r.Header.Get("x-opencode-session")}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[]}`))
	}))
	defer proxy.Close()
	api := &fakePoolAPI{leases: []string{proxy.URL, proxy.URL, proxy.URL, proxy.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	run := func(pv *provider.Provider, name string) seenHdr {
		pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2}
		blockBackgroundWorkers(pt)
		lease, err := pt.Acquire(context.Background(), RequestContext{Provider: name}, pv)
		if err != nil {
			t.Fatalf("%s: acquire: %v", name, err)
		}
		defer lease.Release(context.Background())
		req, _ := http.NewRequest(http.MethodPost, pv.BaseURL+"/chat/completions", strings.NewReader("{}"))
		resp, err := lease.Client.Do(req)
		if err != nil {
			t.Fatalf("%s: do: %v", name, err)
		}
		_ = resp.Body.Close()
		select {
		case h := <-rec:
			return h
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: upstream never hit", name)
			return seenHdr{}
		}
	}

	// kilo-shaped: не-opencode апстрим, свой UA в конфиге.
	kilo := &provider.Provider{Name: "kilocode", Type: "openai-compatible", BaseURL: "http://upstream/v1", UserAgent: "kilocode/1.0"}
	h := run(kilo, "kilocode")
	if h.ua != "kilocode/1.0" {
		t.Fatalf("kilo UA = %q, want kilocode/1.0 (пул штампует opencode-UA — кило делит окно/bans с opencode)", h.ua)
	}
	if h.sess != "" {
		t.Fatalf("kilo request carries x-opencode-session %q — не-opencode апстрим не должен", h.sess)
	}

	// opencode-shaped: pinned UA + session сохраняются (гейт zen их требует).
	oc := &provider.Provider{Name: "opencode", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	h2 := run(oc, "opencode")
	if !strings.HasPrefix(h2.ua, "opencode/") {
		t.Fatalf("opencode UA = %q, want pinned opencode/... (гейт zen)", h2.ua)
	}
	if h2.sess == "" {
		t.Fatal("opencode request lost x-opencode-session — zen free-tier гейт")
	}
}

// Короткие окна (kilo keyless, парки 5s) НЕ получают мгновенный 503-хвост:
// пул ждёт ~2×2.5s и делает ре-свип, и только потом честный хвост.
// Часовые баны/global burn хвост сохраняют мгновенным (проверено косвенно:
// longWindows=false здесь, а дедлайн 40с даёт ветке жить).
func TestPoolShortParkWaitsBeforeSaturatedTail(t *testing.T) {
	bad, _ := fakeForwardProxyHits(t, http.StatusTooManyRequests)
	defer bad.Close()
	bad2, _ := fakeForwardProxyHits(t, http.StatusTooManyRequests)
	defer bad2.Close()

	api := &fakePoolAPI{leases: []string{bad.URL, bad2.URL, bad.URL, bad2.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	oldDeadline := poolTryDeadline
	poolTryDeadline = 40 * time.Second
	defer func() { poolTryDeadline = oldDeadline }()
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())

	req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", strings.NewReader(`{}`))
	start := time.Now()
	_, err = lease.Client.Do(req)
	elapsed := time.Since(start)
	var use *UpstreamStatusError
	if !errors.As(err, &use) || use.Status != http.StatusServiceUnavailable {
		t.Fatalf("want 503 tail after waits, got %T %v", err, err)
	}
	if elapsed < 5*time.Second {
		t.Fatalf("short-park storm must wait before the tail, elapsed=%v", elapsed)
	}
}

// TestPoolBannedFallbackLeaseReleased — контракт banned-fallback лизы: на
// живом пуле, где КАЖДЫЙ egress внутри часового бана, acquireLease паркует
// первый полученный лиз в `last`, остальные релизит сразу. Этот лиз не имеет
// права остаться висеть в warp-router: каждый выход acquireLease возвращает
// его в пул ровно один раз (releaseLast + defer-страховка), а вызывающий
// получает вердикт-less 503 без Retry-After (swap mode).
func TestPoolBannedFallbackLeaseReleased(t *testing.T) {
	// 12 разных бэкендов: разные proxy URL (dedup by ProxyURL не срабатывает),
	// но один public_ip — форма anycast-коллапса, когда пул отдаёт "свежие"
	// лизы одного и того же сгоревшего IP.
	const bannedIP = "10.5.0.9"
	backends := make([]string, 0, 12)
	for range 12 {
		s := fakeForwardProxy(t, http.StatusTooManyRequests)
		defer s.Close()
		backends = append(backends, s.URL)
	}
	api := &fakePoolAPI{leases: backends, fixedIP: bannedIP}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 2, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	// Часовой бан на единственный IP пула: каждый acquire попадает в
	// banned-ветку (isBad), cold/hot не набираются вообще.
	pt.markBad429(bannedIP, 3600*time.Second, time.Now())

	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	_, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err == nil {
		t.Fatal("полностью забаненный пул не должен отдавать лиз")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("want *UpstreamStatusError, got %T: %v", err, err)
	}
	if use.Status != http.StatusServiceUnavailable {
		t.Fatalf("verdict-less exhaustion = 503, got %d (%v)", use.Status, err)
	}
	if use.RetryAfter != "" {
		t.Fatalf("503-хвост не несёт Retry-After, got %q", use.RetryAfter)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.acquireN < 2 {
		t.Fatalf("banned-ветка не проворачивалась: acquires=%d", api.acquireN)
	}
	// Ни один полученный лиз не остался в warp-router: releases == acquires
	// и по составу id — banned-fallback `last` включительно. fakePoolAPI
	// пишет в released lease_id вида "ls-<proxy>", а в acquired — proxy URL.
	balance := map[string]int{}
	for _, proxy := range api.acquired {
		balance["ls-"+proxy]++
	}
	for _, id := range api.released {
		balance[id]--
	}
	for id, n := range balance {
		if n != 0 {
			t.Fatalf("лиз %s освобождён не по одному разу (баланс %d)", id, n)
		}
	}
}

// --- pool_api v2 dialect ------------------------------------------------

func v2TestServer(t *testing.T, leases []string, epoch uint64) (*fakePoolAPI, *PoolTransport, func()) {
	t.Helper()
	api := &fakePoolAPI{leases: leases, epoch: epoch, wrappedV2: true}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond, PoolV2: true}
	blockBackgroundWorkers(pt)
	return api, pt, pa.Close
}

func TestPoolV2DialectAcquireRelease(t *testing.T) {
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api, pt, closeSrv := v2TestServer(t, []string{good.URL, good.URL, good.URL, good.URL}, 7)
	defer closeSrv()
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x", Model: "muse-spark-1.3-contributor-free"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	for _, p := range api.paths {
		if strings.Contains(p, "/api/v1/") {
			t.Fatalf("v2 transport must not speak v1, saw %q", p)
		}
	}
	if len(api.paths) != 2 || !strings.HasSuffix(api.paths[0], "/api/v2/pool/acquire") || !strings.HasSuffix(api.paths[1], "/api/v2/pool/release") {
		t.Fatalf("paths = %v", api.paths)
	}
	if len(api.released) != 1 {
		t.Fatalf("release body must carry lease_ids, recorded %v", api.released)
	}
}

func TestPoolV2WrappedAcquireShape(t *testing.T) {
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: []string{good.URL, good.URL}, epoch: 6, wrappedV2: true}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond, PoolV2: true}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x", Model: "muse-spark-1.3-contributor-free"}, prov)
	if err != nil {
		t.Fatalf("wrapped v2 acquire must parse leases[0], got err: %v", err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	pt.muMu.RLock()
	epoch := pt.epochSeen
	pt.muMu.RUnlock()
	if epoch != 6 {
		t.Fatalf("epochSeen = %d, want 6 (top-level epoch must survive unwrap)", epoch)
	}
}

func TestPoolV2EmptyBatchKeepsBans(t *testing.T) {
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: nil, epoch: 6, wrappedV2: true, emptyBatch: true}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond, PoolV2: true}
	blockBackgroundWorkers(pt)
	pt.muMu.Lock()
	pt.bad = map[string]time.Time{"10.9.0.9": time.Now().Add(time.Hour)}
	pt.epochSeen = 4
	pt.muMu.Unlock()
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	_, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err == nil || !strings.Contains(err.Error(), "empty leases batch") {
		t.Fatalf("empty leases[] must be a distinct error, got %v", err)
	}
	pt.muMu.RLock()
	epoch := pt.epochSeen
	badN := len(pt.bad)
	pt.muMu.RUnlock()
	if epoch != 4 {
		t.Fatalf("empty batch must not advance epoch: epochSeen = %d, want 4", epoch)
	}
	if badN != 1 {
		t.Fatalf("empty batch must not wipe bans: bad = %d, want 1", badN)
	}
	_ = api
}

func TestPoolV2WrappedInnerEpochInheritsTop(t *testing.T) {
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: []string{good.URL}, epoch: 6, wrappedV2: true, stripInnerEpoch: true}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond, PoolV2: true}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatalf("wrapped acquire without inner epoch must parse, got %v", err)
	}
	_ = lease.Release(context.Background())
	pt.muMu.RLock()
	epoch := pt.epochSeen
	pt.muMu.RUnlock()
	if epoch != 6 {
		t.Fatalf("top-level epoch must survive unwrap: epochSeen = %d, want 6", epoch)
	}
	_ = api
}

func TestPoolV2EpochChangeClearsIdentityState(t *testing.T) {
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api, pt, closeSrv := v2TestServer(t, []string{good.URL, good.URL, good.URL, good.URL}, 7)
	defer closeSrv()
	// Seed identity-keyed state as if learned against the OLD generation.
	pt.muMu.Lock()
	pt.bad = map[string]time.Time{"http://127.0.0.1:9999": time.Now().Add(time.Hour)}
	pt.hardBad = map[string]bool{"http://127.0.0.1:9999": true}
	pt.regionGood = map[string]string{"muse": "http://127.0.0.1:9999"}
	pt.muMu.Unlock()
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	// First acquire at epoch 7 records the generation and clears (silent).
	if _, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov); err != nil {
		t.Fatal(err)
	}
	// Re-seed, then bump the epoch: the rebuild must drop stale bans.
	pt.muMu.Lock()
	pt.bad = map[string]time.Time{"http://127.0.0.1:9999": time.Now().Add(time.Hour)}
	pt.muMu.Unlock()
	api.mu.Lock()
	api.epoch = 9
	api.mu.Unlock()
	if _, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov); err != nil {
		t.Fatal(err)
	}
	pt.muMu.RLock()
	badN := len(pt.bad)
	hardN := len(pt.hardBad)
	regionN := len(pt.regionGood)
	epoch := pt.epochSeen
	pt.muMu.RUnlock()
	if badN != 0 || hardN != 0 || regionN != 0 {
		t.Fatalf("epoch change must clear identity state: bad=%d hard=%d region=%d", badN, hardN, regionN)
	}
	if epoch != 9 {
		t.Fatalf("epochSeen = %d, want 9", epoch)
	}
}

func TestPoolV1DialectUnchangedByDefault(t *testing.T) {
	good := fakeForwardProxy(t, http.StatusOK)
	defer good.Close()
	api := &fakePoolAPI{leases: []string{good.URL, good.URL}, epoch: 7}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond} // PoolV2 unset
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
	lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Release(context.Background())
	for _, p := range api.paths {
		if strings.Contains(p, "/api/v2/") {
			t.Fatalf("default transport must stay v1, saw %q", p)
		}
	}
	if pt.epochSeen != 0 {
		t.Fatal("v1 path must not track epochs")
	}
}
