package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// TestPoolWindowedGateDroughtRotates pins the 2026-10-22 recurrence: gate
// bursts every ~1-2h (22:09, 22:32, 23:34, 01:35, 03:37, 05:11) meant the
// current WARP generation had run out of clean identities while the process
// kept serving — only a manual warp-router restart (or the saturation streak
// rotate, which a gate tail never incremented) revived it. A terminal windowed
// FreeTier gate must therefore re-arm the pool itself: honest 403 for this
// request, identity recycle for the next one.
func TestPoolWindowedGateDroughtRotates(t *testing.T) {
	t.Setenv("OPENCODE_TRANSPORT", "off") // pool lane, not the Bun sidecar
	const gateBody = `{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}`
	api := &fakePoolAPI{}
	var srvs []*httptest.Server
	for range 10 {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(gateBody))
		}))
		defer s.Close()
		srvs = append(srvs, s)
		api.leases = append(api.leases, s.URL)
	}
	_ = srvs
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
	// Short deadline keeps the windowed re-sweeps (gateEgressBan+1s each) out
	// of the test: the terminal verdict is what arms the heal.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// Gate-shaped request: only a shaped 403 is an egress/identity drought. A
	// shape-less body is answered 403 by the gate itself and must not rotate
	// the pool (TestPoolWeakShapeGateNoEgressBlame).
	if _, err := r.Call(ctx, "deepseek-v4-flash-free", gateProbeBody("deepseek-v4-flash-free"), true); err == nil {
		t.Fatal("Call succeeded, want the honest terminal gate error")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		api.mu.Lock()
		n := api.rotateN
		api.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("windowed gate drought must heal-rotate the pool (rotateN=0)")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
