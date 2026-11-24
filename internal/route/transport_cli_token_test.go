package route

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A 401 from warp-router (it rotates its API token) must reload the token and
// retry, not lose the main half until ai-router restarts.
func TestLanesNowReloadsTokenOn401(t *testing.T) {
	var authorized int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer new-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		atomic.StoreInt32(&authorized, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backends": []map[string]any{{"http_addr": "127.0.0.1:9001", "public_ip": "2a09:main", "state": "up"}},
		})
	}))
	defer srv.Close()

	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("api:\n  auth_token: \"new-token\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := &CLITransport{apiURL: srv.URL, token: "old-token", warpCfgPath: cfg}
	lanes := tr.lanesNow()
	if atomic.LoadInt32(&authorized) != 1 {
		t.Fatal("token was not reloaded after 401")
	}
	if len(lanes) != 1 || lanes[0].ip != "2a09:main" {
		t.Fatalf("main half lost after token rotation: %+v", lanes)
	}
}

// A failed main read must keep the LAST GOOD view: a wide-only fleet is a set
// of burned lanes, and serving it means instant 429 on every address.
func TestLanesNowKeepsLastGoodOnMainFailure(t *testing.T) {
	var fail int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&fail) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backends": []map[string]any{{"http_addr": "127.0.0.1:9001", "public_ip": "2a09:main", "state": "up"}},
		})
	}))
	defer srv.Close()

	tr := &CLITransport{apiURL: srv.URL}
	if got := tr.lanesNow(); len(got) != 1 {
		t.Fatalf("initial view: %+v", got)
	}
	atomic.StoreInt32(&fail, 1)
	tr.mu.Lock()
	tr.fetched = time.Now().Add(-2 * time.Minute) // expire the cache
	tr.mu.Unlock()

	got := tr.lanesNow()
	if len(got) != 1 || got[0].ip != "2a09:main" {
		t.Fatalf("partial view replaced the last good one: %+v", got)
	}
}
