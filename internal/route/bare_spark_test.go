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

// Bare muse-spark-1.3-contributor-free must resolve keyless opencode BEFORE
// the fireworks wildcard in the same free tier (config order must not let a
// wildcard shadow a typed provider). Guards the resolve/CascadeChoose half
// of the 2026-09-18 bare-404: the other half (gate masking) is covered by
// TestBareSparkTerminalGateStopsCascade below.
func TestBareSparkResolvesOpencodeBeforeFireworks(t *testing.T) {
	fw := fakeUpstream(t, 404, `{"error":"not found"}`)
	defer fw.Close()
	oc := fakeUpstream(t, 200, `{"id":"ok","choices":[]}`)
	defer oc.Close()
	r := New(map[string]*provider.Provider{
		// Live declaration order: wildcard fireworks (idx 7) long before
		// typed opencode (idx 66) — the split must still put opencode first.
		"fireworks": {Name: "fireworks", Type: "openai-compatible", BaseURL: fw.URL, APIKeys: []string{"k"}},
		"opencode":  {Name: "opencode", Type: "openai-compatible", BaseURL: oc.URL, Models: []string{"muse-spark-1.3-contributor-free"}},
	}, WithProvidersOrder([]string{"fireworks", "opencode"}))
	got := r.resolve("muse-spark-1.3-contributor-free")
	if len(got) != 2 {
		t.Fatalf("resolve = %d candidates, want 2", len(got))
	}
	if got[0].Name != "opencode" || got[1].Name != "fireworks" {
		t.Fatalf("bare order = [%s %s], want [opencode fireworks]", got[0].Name, got[1].Name)
	}
	res, err := r.Call(context.Background(), "muse-spark-1.3-contributor-free", []byte(`{"model":"muse-spark-1.3-contributor-free"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	defer res.Resp.Body.Close()
	if res.Provider != "opencode" {
		t.Fatalf("bare call hit %q, want opencode", res.Provider)
	}
}

// Terminal account-gate 403 from the first typed candidate must STOP the
// cascade and surface honest 403 — never fall through to the wildcard and
// mask the gate as a misleading 404 (live 2026-09-18: bare spark opencode
// gate-403 then fireworks Not Found, client saw the 404). Wired through a
// fake pool transport like production (opencode always has pool): the inner
// sweep decides terminal (gate budget spent) and tryProvider marks it with
// the gate-terminal marker that Call stops on.
func TestBareSparkTerminalGateStopsCascade(t *testing.T) {
	const gateBody = `{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}`
	var leases []string
	for range 10 {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(gateBody))
		}))
		defer s.Close()
		leases = append(leases, s.URL)
	}
	api := &fakePoolAPI{leases: leases}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Nanosecond}
	fw := fakeUpstream(t, 404, `{"error":"not found"}`)
	defer fw.Close()
	r := New(map[string]*provider.Provider{
		"fireworks": {Name: "fireworks", Type: "openai-compatible", BaseURL: fw.URL, APIKeys: []string{"k"}},
		"opencode":  {Name: "opencode", Type: "openai-compatible", BaseURL: "http://upstream/v1", Models: []string{"muse-spark-1.3-contributor-free"}},
	}, WithTransport(pt), WithProvidersOrder([]string{"fireworks", "opencode"}))
	_, err := r.Call(context.Background(), "muse-spark-1.3-contributor-free", []byte(`{"model":"muse-spark-1.3-contributor-free"}`), false)
	if err == nil {
		t.Fatal("Call succeeded, want honest terminal 403")
	}
	var use *UpstreamStatusError
	if !errors.As(err, &use) {
		t.Fatalf("err = %v, want *UpstreamStatusError", err)
	}
	if use.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (never masked wildcard 404)", use.Status)
	}
	if !strings.Contains(err.Error(), "account gate:") {
		t.Fatalf("gate error must carry the upstream reason, got %v", err)
	}
	if strings.Contains(err.Error(), "fireworks") {
		t.Fatalf("gate must stop before the wildcard, got %v", err)
	}
}

// Plain 403 without the terminal marker still cascades (region-style swap
// path depends on it): typed 403 then wildcard 200 must succeed via the
// wildcard, proving the Call stop is marker-scoped, not status-scoped.
func TestPlain403StillCascadesToWildcard(t *testing.T) {
	typed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"region not served here"}`))
	}))
	wild := fakeUpstream(t, 200, `{"id":"ok","choices":[]}`)
	defer wild.Close()
	r := New(map[string]*provider.Provider{
		"wild":  {Name: "wild", Type: "openai-compatible", BaseURL: wild.URL},
		"typed": {Name: "typed", Type: "openai-compatible", BaseURL: typed.URL, Models: []string{"m"}},
	}, WithProvidersOrder([]string{"wild", "typed"}))
	res, err := r.Call(context.Background(), "m", []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("plain 403 must cascade, got %v", err)
	}
	defer res.Resp.Body.Close()
	if res.Provider != "wild" {
		t.Fatalf("want wildcard fallback, got %s", res.Provider)
	}
}
