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

// TestAcquireGeneratesSessionWhenHarvestEmpty: the pool lane's identity is the
// harvested session when the pool has one, else the SAME per-egress generator
// the direct lane has shipped since 2026-10-24 (ses_ + 12 hex + 14 alnum — the
// 30-char shape the free gate validates). Refusing identity-less egresses
// instead left every pool-lane provider without supply once the harvest went
// off by default (2026-11-07): opencode-zen/opencode-temp answered 503
// "session supply exhausted" for 8 egresses (measured 2026-11-11 17:33) while
// the direct lane served 200s through that same generator. A supply fallback is
// not a pool-burn signal either, so nothing here may rotate WARP.
func TestAcquireGeneratesSessionWhenHarvestEmpty(t *testing.T) {
	resetSessionPool()
	t.Cleanup(resetSessionPool)
	getSessionPool().Fetch = func(string, time.Duration, time.Time) []sessionEntry { return nil } // empty harvest

	var ups []*httptest.Server
	for range 2 {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		ups = append(ups, up)
		defer up.Close()
	}
	api := &fakePoolAPI{leases: []string{ups[0].URL, ups[1].URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "opencode", Type: "openai-compatible", BaseURL: "http://upstream/v1", AuthMode: "none"}

	seen := map[string]bool{}
	for i := range 2 {
		lease, err := pt.acquireLease(context.Background(), pt.reqSeq.Add(1), "deepseek-v4.1-flash-free", stampFor(prov))
		if err != nil {
			t.Fatalf("acquire %d: empty harvest must fall back to the per-egress generator: %v", i, err)
		}
		if lease == nil || lease.session == "" {
			t.Fatalf("acquire %d: lease without a session identity", i)
		}
		if !strings.HasPrefix(lease.session, "ses_") || len(lease.session) != 30 {
			t.Fatalf("session %q is not the 30-char ses_ shape the free gate validates", lease.session)
		}
		if seen[lease.session] {
			t.Fatalf("session %q reused across distinct egresses: a shared id links two IPs", lease.session)
		}
		seen[lease.session] = true
	}
	time.Sleep(300 * time.Millisecond)
	api.mu.Lock()
	n := api.rotateN
	api.mu.Unlock()
	if n != 0 {
		t.Fatalf("rotateN=%d: a session-supply fallback must not cycle WARP", n)
	}
}
