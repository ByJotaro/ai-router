package route

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

func zenProv() *provider.Provider {
	return &provider.Provider{Name: "opencode", BaseURL: "https://opencode.ai/zen/v1"}
}

// Full CLI set is synthesized when the inbound client sent nothing (omp
// harness, curl): stable session (cache affinity) + cli/default + fresh
// per-call request id (live A/B 2026-09-10: gate keys on session presence,
// all four header variants 200, no-session 400).
func TestSetOpencodeHeadersSynthesizes(t *testing.T) {
	t.Setenv("AI_ROUTER_SESSION_DB", "off")
	resetSessionPool()
	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/responses", nil)
	setOpencodeHeadersAt(req, context.Background(), zenProv(), directHomeIPKey)
	if got := req.Header.Get("x-opencode-session"); !strings.HasPrefix(got, "ses_") || len(got) < 10 {
		t.Fatalf("x-opencode-session = %q, want ses_<id>", got)
	}
	if got := req.Header.Get("x-opencode-client"); got != "cli" {
		t.Fatalf("x-opencode-client = %q, want cli", got)
	}
	// Verified passing recipe (2026-10-13): project=global, request msg_*,
	// and BOTH session headers carry the same real ses_* value.
	if got := req.Header.Get("x-opencode-project"); got != "global" {
		t.Fatalf("x-opencode-project = %q, want global", got)
	}
	// x-opencode-request is OPTIONAL on the gate (bisect 2026-10-23: dropping
	// it still passes), and a fabricated msg_* is a gate signal while a shared
	// one links egresses — so with no genuine pairing the header is omitted.
	if got := req.Header.Get("x-opencode-request"); got != "" {
		t.Fatalf("x-opencode-request = %q, want omitted without a genuine paired id", got)
	}
	if got := req.Header.Get("x-opencode-session-id"); got != req.Header.Get("x-opencode-session") {
		t.Fatalf("x-opencode-session-id = %q, want same as x-opencode-session", got)
	}
}

// Anonymous fallback (AI_ROUTER_SESSION_DB=off, no harvest): the session stays
// process-stable (prompt-cache affinity) and no request id is invented. With
// the DB enabled genuine ids are harvested instead — see
// TestSetOpencodeHeadersHarvestsRequestID.
func TestSetOpencodeHeadersStableFallback(t *testing.T) {
	t.Setenv("AI_ROUTER_SESSION_DB", "off")
	resetSessionPool()
	// Drop the singleton harvest cache (another test may have warmed it);
	// with the DB off the next NextFor re-fetches and finds nothing.
	p := getMsgIDPool()
	p.mu.Lock()
	p.cached = nil
	p.loadedAt = time.Time{}
	p.mu.Unlock()
	r1, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/responses", nil)
	setOpencodeHeadersAt(r1, context.Background(), zenProv(), directHomeIPKey)
	r2, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/responses", nil)
	setOpencodeHeadersAt(r2, context.Background(), zenProv(), directHomeIPKey)
	a, b := r1.Header.Get("x-opencode-session"), r2.Header.Get("x-opencode-session")
	if a == "" || a != b {
		t.Fatalf("fallback not stable: %q vs %q", a, b)
	}
	// No fabricated ids: with no harvest there is no genuine request id, and
	// the lane accepts the request without the header (measured 2026-10-23).
	if c, d := r1.Header.Get("x-opencode-request"), r2.Header.Get("x-opencode-request"); c != "" || d != "" {
		t.Fatalf("request id must stay omitted without harvest: %q vs %q", c, d)
	}
}

// With the TUI DB enabled the request id must be a REAL harvested msg_*
// value (the lane 403s generated ids), not the msg_<24 random> fallback.
//
// Hermetic: the db is a fixture shaped like opencode.db, so the harvest path
// runs on a machine without a TUI (CI) instead of being skipped there.
func TestSetOpencodeHeadersHarvestsRequestID(t *testing.T) {
	now := time.Now()
	ses := "ses_" + strings.Repeat("a", 26) // the 30-char shape the gate accepts
	msg := "msg_" + strings.Repeat("b", 24)
	t.Setenv("AI_ROUTER_SESSION_DB", mkHarvestDB(t, ses, msg, now))
	resetSessionPool() // harvest the fixture, not a seeded supply
	t.Cleanup(resetSessionPool)
	pool := getMsgIDPool()
	pool.DBPath = ""
	pool.MaxAge = 0
	pool.Now = nil
	pool.cached = nil
	pool.loadedAt = time.Time{}
	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/inference/v1/responses", nil)
	setOpencodeHeadersAt(req, context.Background(), zenProv(), directHomeIPKey)
	if got := req.Header.Get("x-opencode-session"); got != ses {
		t.Fatalf("session = %q, want the harvested %q", got, ses)
	}
	got := req.Header.Get("x-opencode-request")
	if got != msg {
		t.Fatalf("request id = %q, want the harvested %q", got, msg)
	}
	t.Logf("harvested session %s -> request id %s", ses, got)
}

// mkHarvestDB builds a temp opencode.db with one live session row and one
// message row inside it, so the session harvest and the msg-id harvest agree
// on the same identity.
func mkHarvestDB(t *testing.T, ses, msg string, now time.Time) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE session (id TEXT, time_updated INTEGER)`,
		`CREATE TABLE message (id TEXT, session_id TEXT, time_created INTEGER)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO session (id, time_updated) VALUES (?, ?)`, ses, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message (id, session_id, time_created) VALUES (?, ?, ?)`, msg, ses, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return p
}

// Inbound real session affinity must be forwarded verbatim (sticky routing).
func TestSetOpencodeHeadersForwards(t *testing.T) {
	in := http.Header{}
	in.Set("x-opencode-session", "ses_abc123")
	in.Set("x-opencode-client", "tui")
	in.Set("x-opencode-request", "req_1")
	in.Set("x-opencode-project", "proj_9")
	ctx := ContextWithClientHeaders(context.Background(), in)
	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/responses", nil)
	setOpencodeHeaders(req, ctx, zenProv())
	if got := req.Header.Get("x-opencode-session"); got != "ses_abc123" {
		t.Fatalf("x-opencode-session = %q, want ses_abc123", got)
	}
	for k, want := range map[string]string{"x-opencode-client": "tui", "x-opencode-request": "req_1", "x-opencode-project": "proj_9"} {
		if got := req.Header.Get(k); got != want {
			t.Fatalf("%s = %q, want %q (forwarded)", k, got, want)
		}
	}
}

// Non-zen providers must not get the headers.
func TestSetOpencodeHeadersSkipsOthers(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://api.deepseek.com/v1/chat/completions", nil)
	setOpencodeHeaders(req, context.Background(), &provider.Provider{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1"})
	if got := req.Header.Get("x-opencode-session"); got != "" {
		t.Fatalf("non-zen got session header %q", got)
	}
}

// TestSetOpencodeHeadersCLIParity pins the two headers the 2026-10-22 MITM
// capture shows on EVERY real CLI request but that ours omitted: the free lane
// went out as an anonymous stranger (no Authorization, no Accept).
func TestSetOpencodeHeadersCLIParity(t *testing.T) {
	free := &provider.Provider{Name: "opencode", BaseURL: "https://opencode.ai/inference/v1", AuthMode: "none"}
	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/inference/v1/chat/completions", nil)
	setOpencodeHeaders(req, context.Background(), free)
	if got := req.Header.Get("Authorization"); got != "Bearer public" {
		t.Fatalf("Authorization=%q, хочу %q", got, "Bearer public")
	}
	if got := req.Header.Get("Accept"); got != "*/*" {
		t.Fatalf("Accept=%q, хочу %q", got, "*/*")
	}

	keyed := &provider.Provider{Name: "opencode", BaseURL: "https://opencode.ai/inference/v1", AuthMode: "bearer", APIKeys: []string{"sk-test"}}
	req2, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/inference/v1/chat/completions", nil)
	req2.Header.Set("Authorization", "Bearer sk-test")
	setOpencodeHeaders(req2, context.Background(), keyed)
	if got := req2.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Fatalf("ключ перетёрт на %q", got)
	}
}

// TestOpencodeHeadersPerLaneSessions: lanes without a pool lease (direct
// home-IP leg, direct fallback, /responses adapter) must pin their OWN egress
// session, not borrow a process-wide default. A shared default is one id on
// every IP — the cross-IP link the free tier must never see — and the
// 2026-10-23 audit of the pool path could not see it, because the pool stamps
// over the header before sending.
func TestOpencodeHeadersPerLaneSessions(t *testing.T) {
	t.Setenv("AI_ROUTER_SESSION_DB", "off")
	now := time.Now()
	ids := []sessionEntry{
		{id: "ses_lane00000000000000000001", updated: now.UnixMilli()},
		{id: "ses_lane00000000000000000002", updated: now.Add(-time.Minute).UnixMilli()},
	}
	sp := seedPool(t, &now, ids)

	a1, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	setOpencodeHeadersAt(a1, context.Background(), zenProv(), "egress-a")
	a2, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	setOpencodeHeadersAt(a2, context.Background(), zenProv(), "egress-a")
	b1, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	setOpencodeHeadersAt(b1, context.Background(), zenProv(), "egress-b")

	sa, sb := a1.Header.Get("x-opencode-session"), b1.Header.Get("x-opencode-session")
	if sa == "" || sb == "" {
		t.Fatalf("обе полосы обязаны получить сессию: %q / %q", sa, sb)
	}
	if sa != a2.Header.Get("x-opencode-session") {
		t.Fatalf("сессия полосы нестабильна: %q vs %q", sa, a2.Header.Get("x-opencode-session"))
	}
	if sa == sb {
		t.Fatalf("два egress получили ОДНУ сессию %q — кросс-IP связка", sa)
	}
	if got := sp.NextFor("egress-a"); got != sa {
		t.Fatalf("полоса должна использовать пин своего egress: %q, пул отдаёт %q", sa, got)
	}
	if got := a1.Header.Get("x-opencode-session-id"); got != sa {
		t.Fatalf("x-opencode-session-id = %q, want %q", got, sa)
	}

	// No key = no invented session: the caller stamps (pool hot path) or omits.
	// A process-wide default here is exactly the bug this test locks out.
	plain, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	setOpencodeHeaders(plain, context.Background(), zenProv())
	if got := plain.Header.Get("x-opencode-session"); got != "" {
		t.Fatalf("без egress-ключа сессия выдумываться не должна: %q", got)
	}
	if got := plain.Header.Get("x-opencode-client"); got != "cli" {
		t.Fatalf("остальные CLI-заголовки обязаны ставиться: client=%q", got)
	}
}

// TestDirectLaneSendsOwnEgressSession is the lane-level guard for the bug the
// pool-path audit could not see: Router.doRequest (direct home-IP leg and
// direct fallback) used to stamp the process-wide pool default (alive[0]) on
// every request, so the same session id went out from the home IP while it also
// lived on a WARP egress. The lane must ship the id its own egress owns.
func TestDirectLaneSendsOwnEgressSession(t *testing.T) {
	t.Setenv("AI_ROUTER_SESSION_DB", "off")
	now := time.Now()
	entries := []sessionEntry{
		{id: "ses_direct0000000000000000001", updated: now.UnixMilli()},
		{id: "ses_direct0000000000000000002", updated: now.Add(-time.Minute).UnixMilli()},
	}
	sp := seedPool(t, &now, entries)
	// A pool egress owns alive[0] first: the old global fallback shipped
	// exactly alive[0], so this makes the leak observable instead of masked.
	poolID := sp.NextFor("warp-egress-1")
	if poolID == "" {
		t.Fatal("pool egress не получил id — тест не различает баг")
	}

	var got, ownerAtSend string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-opencode-session")
		// Ownership AT SEND TIME is the discriminator: a shared process-wide
		// default is unowned (or owned by another egress) when it goes out.
		sp.mu.Lock()
		ownerAtSend = sp.owner[got]
		sp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer up.Close()

	prov := &provider.Provider{Name: "opencode", Type: "openai-compatible", BaseURL: up.URL, AuthMode: "none"}
	if _, _, err := (&Router{}).doRequest(context.Background(), up.Client(), prov, nil, []byte(`{"model":"m","messages":[]}`), directHomeIPKey, ""); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	want := sp.NextFor(directHomeIPKey)
	if ownerAtSend != directHomeIPKey {
		t.Fatalf("сессия %q на момент отправки принадлежала %q, а не домашнему egress — утечка общего id", got, ownerAtSend)
	}
	if got == "" || got != want {
		t.Fatalf("direct lane session = %q, want the home egress's own id %q", got, want)
	}
	if got == poolID {
		t.Fatalf("home lane отправил сессию пула %q — одна сессия на двух IP", got)
	}
}
