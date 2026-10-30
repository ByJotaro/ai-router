package route

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// batchPoolAPI answers v2 acquire with a fixed batch and records the request
// body (so a test can pin n) plus every release.
type batchPoolAPI struct {
	mu       sync.Mutex
	leases   []poolLeaseResponse
	bodyN    int
	released []string
}

func (b *batchPoolAPI) handler(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/pool/acquire"):
		var req struct {
			N int `json:"n"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.bodyN = req.N
		_ = json.NewEncoder(w).Encode(map[string]any{"epoch": 7, "leases": b.leases})
	case strings.HasSuffix(r.URL.Path, "/pool/release"):
		var req struct {
			LeaseID  string   `json:"lease_id"`
			LeaseIDs []string `json:"lease_ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.LeaseID != "" {
			b.released = append(b.released, req.LeaseID)
		}
		b.released = append(b.released, req.LeaseIDs...)
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// TestPoolAcquirePicksHealthyLeaseFromBatch is the "rotation under load"
// regression: with a mixed batch (two hard-banned /48s first, a healthy one
// third) the router must take the healthy lease instead of Leases[0], ask for
// a batch of poolAcquireBatch, and release everything it did not use.
func TestPoolAcquirePicksHealthyLeaseFromBatch(t *testing.T) {
	api := &batchPoolAPI{leases: []poolLeaseResponse{
		{LeaseID: "l-bad1", ProxyURL: "http://127.0.0.1:9101", PublicIP: "2a09:bac1:47c0:170::32:12f"},
		{LeaseID: "l-bad2", ProxyURL: "http://127.0.0.1:9102", PublicIP: "2a09:bac1:4780:170::a6:b"},
		{LeaseID: "l-good", ProxyURL: "http://127.0.0.1:9103", PublicIP: "2a09:bac5:48f4:1f5::32:12d"},
	}}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	now := time.Now()
	p := &PoolTransport{APIURL: srv.URL, PoolV2: true, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	p.bad = map[string]time.Time{
		"2a09:bac1:47c0:170::32:12f": now.Add(time.Hour),
		"2a09:bac1:4780:170::a6:b":   now.Add(time.Hour),
	}

	l, err := p.acquireLease(context.Background(), 1, "opencode/deepseek-v4.1-flash-free", leaseStamp{sessOK: true})
	if err != nil {
		t.Fatalf("acquireLease: %v", err)
	}
	if l == nil {
		t.Fatal("acquireLease вернул nil lease")
	}
	if l.leaseID != "l-good" {
		t.Fatalf("взяли %q, want l-good (единственный не забаненный)", l.leaseID)
	}
	if api.bodyN != poolAcquireBatch {
		t.Fatalf("n в acquire-запросе = %d, want %d", api.bodyN, poolAcquireBatch)
	}
	got := map[string]bool{}
	for _, id := range api.released {
		got[id] = true
	}
	if !got["l-bad1"] || !got["l-bad2"] {
		t.Fatalf("неиспользованные лизы не отпущены: %v", api.released)
	}
	if got["l-good"] {
		t.Fatalf("выбранный лиз отпущен: %v", api.released)
	}
}

// TestPoolAcquireWaitsOnEmptyBatch pins the capacity wall that produced
// instant "no ready egress" 503s under load (live 2026-10-21 13:45: 7 such
// 503s in 5 min while the pool was merely out of free slots): a v2 acquire
// that answers 200 with an empty leases batch is a TEMPORARY "all slots busy"
// signal and must be retried until a slot frees, not surfaced as a failure.
func TestPoolAcquireWaitsOnEmptyBatch(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/pool/acquire") {
			if n <= 2 {
				_ = json.NewEncoder(w).Encode(map[string]any{"epoch": 9, "leases": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"epoch": 9, "leases": []map[string]any{{
				"lease_id": "l-after-wait", "proxy_url": "http://127.0.0.1:9201",
				"public_ip": "2a09:bac5:48f2:1f5::9:1",
			}}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &PoolTransport{APIURL: srv.URL, PoolV2: true, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	l, err := p.acquireLease(context.Background(), 2, "opencode/deepseek-v4.1-flash-free", leaseStamp{sessOK: true})
	if err != nil {
		t.Fatalf("пустой батч должен пережидаться, а не падать: %v", err)
	}
	if l == nil || l.leaseID != "l-after-wait" {
		t.Fatalf("взяли %v, want l-after-wait (слот освободился)", l)
	}
}

// TestPickLeaseRotatesAcrossEqualHeatLeases pins the tie-break that batch
// rotation lives on: with three healthy leases (heat=0 everywhere) the picker
// used to return the FIRST element of every batch, because warp keeps a stable
// order — live 2026-10-21 14:17: distinct48=3 per batch yet 13 of 14 attempts
// landed on one egress. The least-recently-used lease must win instead.
func TestPickLeaseRotatesAcrossEqualHeatLeases(t *testing.T) {
	p := &PoolTransport{}
	batch := []poolLeaseResponse{
		{LeaseID: "a", ProxyURL: "http://127.0.0.1:9501", PublicIP: "2a09:bac5:48f1:1::1"},
		{LeaseID: "b", ProxyURL: "http://127.0.0.1:9502", PublicIP: "2a09:bac5:48f2:1::1"},
		{LeaseID: "c", ProxyURL: "http://127.0.0.1:9503", PublicIP: "2a09:bac5:48f3:1::1"},
	}
	now := time.Now()
	seen := map[string]int{}
	for i := range 3 {
		chosen, rest := p.pickLease(batch, now, false)
		if len(rest) != 2 {
			t.Fatalf("шаг %d: rest=%d, want 2", i, len(rest))
		}
		seen[chosen.LeaseID]++
		p.markUsed(p.ipKey(chosen.ProxyURL, chosen.PublicIP))
	}
	if len(seen) != 3 {
		t.Fatalf("за 3 запроса выбрано %v, want три разных лиза (батч должен ротировать)", seen)
	}
}

// TestPickLeaseStickyAmongLiveLRUAmongStale pins the M5 tiebreak: among LIVE
// egresses (fresh 2xx) the most recently used one keeps the traffic - the pool
// otherwise abandons a serving /64 after 23.6 200s while clean runs reach 111
// and every re-draw spends a fresh budget. Among STALE candidates the LRU order
// must stay, otherwise warmups never rotate and a live egress is never found.
func TestPickLeaseStickyAmongLiveLRUAmongStale(t *testing.T) {
	now := time.Now()
	liveA, liveB := "2a09:bac5:48f1:1::1", "2a09:bac5:48f2:1::1"
	staleA, staleB := "2a09:bac5:48f3:1::1", "2a09:bac5:48f4:1::1"

	p := &PoolTransport{}
	p.muMu.Lock()
	p.okAt = map[string]int64{
		liveA: now.Add(-2 * time.Minute).UnixNano(),
		liveB: now.Add(-3 * time.Minute).UnixNano(),
	}
	p.usedAt = map[string]time.Time{
		liveA:  now.Add(-1 * time.Minute), // most recently used => sticky
		liveB:  now.Add(-5 * time.Minute),
		staleA: now.Add(-9 * time.Minute),
		staleB: now.Add(-7 * time.Minute), // least recently used => LRU wins
	}
	p.muMu.Unlock()

	batch := []poolLeaseResponse{
		{LeaseID: "b", ProxyURL: "http://127.0.0.1:9502", PublicIP: liveB},
		{LeaseID: "a", ProxyURL: "http://127.0.0.1:9501", PublicIP: liveA},
	}
	if got, _ := p.pickLease(batch, now, false); got.LeaseID != "a" {
		t.Fatalf("среди живых выбран %q: должен побеждать MRU (липкость)", got.LeaseID)
	}

	stale := []poolLeaseResponse{
		{LeaseID: "sb", ProxyURL: "http://127.0.0.1:9504", PublicIP: staleB},
		{LeaseID: "sa", ProxyURL: "http://127.0.0.1:9503", PublicIP: staleA},
	}
	if got, _ := p.pickLease(stale, now, false); got.LeaseID != "sa" {
		t.Fatalf("среди холодных выбран %q: должен побеждать LRU (ротация прогревов)", got.LeaseID)
	}
}
