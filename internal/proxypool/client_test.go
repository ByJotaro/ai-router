package proxypool_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ByJotaro/ai-router/internal/proxypool"
)

// fakeEgress implements the hub egress "wrapped {ok,result}" HTTP contract:
// POST /actions/{action}, Authorization: Bearer <token>, body {"params":{...}}.
// It records each request and routes the action to a stub handler.
type fakeEgress struct {
	mu      sync.Mutex
	token   string
	seen    []call
	totalID int
}

type call struct {
	action string
	bearer string
	params map[string]any
}

func newFake(token string) *fakeEgress { return &fakeEgress{token: token} }

func (f *fakeEgress) records() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]call, len(f.seen))
	copy(out, f.seen)
	return out
}

func (f *fakeEgress) record(action, bearer string, params map[string]any) {
	f.mu.Lock()
	f.seen = append(f.seen, call{action: action, bearer: bearer, params: params})
	f.mu.Unlock()
}

// nextAcquire alternates the channel id so consecutive acquires never share a
// channel — emulating the egress pool's per_request rotation.
func (f *fakeEgress) nextAcquire() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.totalID++
	id := f.totalID % 2 // 1,0,1,0...
	name := "chan-A"
	if id == 0 {
		name = "chan-B"
	}
	return map[string]any{
		"lease_id":   "ls_" + itoa(f.totalID),
		"id":         name,
		"ttl_s":      300,
		"socks":      "127.0.0.1:1080",
		"http":       "127.0.0.1:8118",
		"public_ip":  fmt.Sprintf("203.0.113.%d", id+1),
		"latency_ms": int64(10 + id),
		"geo":        map[string]any{"country": "DE", "city": "Frankfurt", "asn": "AS13335"},
	}
}

func (f *fakeEgress) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/actions/", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		action := r.URL.Path[len("/actions/"):]
		f.record(action, r.Header.Get("Authorization"), body.Params)

		write := func(result map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
		}
		switch action {
		case "status":
			write(map[string]any{
				"pool": map[string]any{
					"total": 2, "alive": 2, "dead": 0, "connecting": 0,
					"connect_rate": 1.0, "failures_24h": 3, "degraded": false,
					"avg_latency_ms":     42,
					"last_latency_ms":    50,
					"latency_updated_at": 1700000000,
				},
				"active_account_id": "acc-1",
				"public_ip":         "203.0.113.99",
			})
		case "accounts.list":
			write(map[string]any{"accounts": []map[string]any{
				{"id": "acc-1", "virtual_ip": "10.0.0.1", "healthy": true, "latency_ms": 42,
					"geo": map[string]any{"country": "DE", "city": "Frankfurt"}},
				{"id": "acc-2", "virtual_ip": "10.0.0.2", "healthy": false, "fail_count": 2},
			}})
		case "servers.list":
			write(map[string]any{"servers": []map[string]any{
				{"name": "FRA", "status": "ok", "latency_ms": 40,
					"geo": map[string]any{"country": "DE", "city": "Frankfurt"}},
			}})
		case "proxies.acquire":
			write(f.nextAcquire())
		case "proxies.release":
			write(map[string]any{"ok": true, "usage": 3})
		case "rotate":
			write(map[string]any{"ok": true, "public_ip": "203.0.113.7"})
		case "connect", "disconnect":
			write(map[string]any{"ok": true})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]any{"code": "not_found"}})
		}
	})
	return mux
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func serve(t *testing.T, fake *fakeEgress, cfg proxypool.Config) *proxypool.Client {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	cfg.BaseURL = srv.URL
	return proxypool.New(cfg)
}

func TestStatus(t *testing.T) {
	fake := newFake("mt_secret")
	c := serve(t, fake, proxypool.Config{Token: "mt_secret"})

	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Total != 2 || st.Alive != 2 || st.Dead != 0 {
		t.Errorf("pool counts wrong: %+v", st)
	}
	if st.ConnectRate != 1.0 || st.Failures24h != 3 || st.Degraded {
		t.Errorf("pool metrics wrong: %+v", st)
	}
	if st.AvgLatencyMs != 42 || st.LastLatencyMs != 50 {
		t.Errorf("latency wrong: +%v", st)
	}
	if st.ActiveAccountID != "acc-1" || st.PublicIP != "203.0.113.99" {
		t.Errorf("active/public wrong: %s / %s", st.ActiveAccountID, st.PublicIP)
	}
	if len(st.Accounts) != 2 || st.Accounts[0].ID != "acc-1" || !st.Accounts[0].Healthy {
		t.Errorf("accounts wrong: %+v", st.Accounts)
	}
	if len(st.Servers) != 1 || st.Servers[0].Name != "FRA" {
		t.Errorf("servers wrong: %+v", st.Servers)
	}
	rec := fake.records()
	if len(rec) < 1 || rec[0].bearer != "Bearer mt_secret" {
		t.Errorf("expected bearer auth, got %+v", rec)
	}
}

func TestAcquireParamsAndResult(t *testing.T) {
	fake := newFake("t")
	c := serve(t, fake, proxypool.Config{
		Token: "t", Strategy: "best_latency", PerRequest: true, RotateAfter: 10,
	})

	pr, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if pr.LeaseID == "" || pr.HTTP != "127.0.0.1:8118" || pr.Socks != "127.0.0.1:1080" {
		t.Errorf("proxy parse wrong: %+v", pr)
	}
	if pr.Geo.Country != "DE" || pr.PublicIP == "" {
		t.Errorf("geo/public IP wrong: %+v", pr)
	}

	// Params must carry strategy + rotation.
	var got *call
	for _, rc := range fake.records() {
		if rc.action == "proxies.acquire" {
			got = &rc
			break
		}
	}
	if got == nil {
		t.Fatal("acquire not seen")
	}
	if got.params["strategy"] != "best_latency" {
		t.Errorf("strategy param=%v", got.params["strategy"])
	}
	rot, ok := got.params["rotation"].(map[string]any)
	if !ok || rot["per_request"] != true || rot["rotate_after"] != float64(10) {
		t.Errorf("rotation params=%v", got.params["rotation"])
	}
}

func TestReleaseAndRotateParams(t *testing.T) {
	fake := newFake("t")
	c := serve(t, fake, proxypool.Config{Token: "t"})

	if err := c.Release(context.Background(), "ls_77"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := c.Rotate(context.Background(), "sticky"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	rec := fake.records()
	if len(rec) < 2 {
		t.Fatalf("expected 2 calls, got %d", len(rec))
	}
	if rec[0].action != "proxies.release" || rec[0].params["lease_id"] != "ls_77" {
		t.Errorf("release wrong: %+v", rec[0])
	}
	if rec[1].action != "rotate" || rec[1].params["mode"] != "sticky" {
		t.Errorf("rotate wrong: %+v", rec[1])
	}
}

func TestPerRequestDoesNotRepeatChannel(t *testing.T) {
	fake := newFake("t")
	c := serve(t, fake, proxypool.Config{Token: "t", PerRequest: true})

	first, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	second, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("per_request returned the same channel twice: %s", first.ID)
	}
}

func TestProxiesLogWritten(t *testing.T) {
	fake := newFake("t")
	c := serve(t, fake, proxypool.Config{Token: "t"})

	var mu sync.Mutex
	var logs []string
	c.SetLog(func(proxy, action string, ok bool, _ int64, errText string) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, action+"|"+proxy+"|"+fmt.Sprint(ok)+"|"+errText)
	})

	pr, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := c.Release(context.Background(), pr.LeaseID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := c.Rotate(context.Background(), ""); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 3 {
		t.Fatalf("expected 3 log entries, got %d: %v", len(logs), logs)
	}
	if logs[0] != "acquire|chan-A|true|" && logs[0] != "acquire|chan-B|true|" {
		t.Errorf("acquire log entry wrong: %q", logs[0])
	}
	if logs[1] != "release|"+"ls_1"+"|true|" && logs[1] != "release|ls_2|true|" {
		t.Errorf("release log entry wrong: %q", logs[1])
	}
	if expected := "rotate||true|"; logs[2] != expected {
		t.Errorf("rotate log entry wrong: %q", logs[2])
	}
}

func TestCallToleratesBareResult(t *testing.T) {
	// egress вернул сырой результат без конверта — acquire должен сработать.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease_id":"ls_1","id":"chan-A","http":"127.0.0.1:8118","socks":"127.0.0.1:1080"}`))
	}))
	t.Cleanup(srv.Close)
	c := proxypool.New(proxypool.Config{BaseURL: srv.URL, Token: "t"})
	pr, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire bare: %v", err)
	}
	if pr.LeaseID != "ls_1" || pr.ID != "chan-A" {
		t.Errorf("lease wrong: %+v", pr)
	}
}

func TestCallHTTPErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	t.Cleanup(srv.Close)
	c := proxypool.New(proxypool.Config{BaseURL: srv.URL, Token: "t"})
	_, err := c.Acquire(context.Background())
	if err == nil || !strings.Contains(err.Error(), "http 500") {
		t.Fatalf("want http 500, got %v", err)
	}
}

func TestCallBareNumberEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`42`))
	}))
	t.Cleanup(srv.Close)
	c := proxypool.New(proxypool.Config{BaseURL: srv.URL, Token: "t"})
	_, err := c.Acquire(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bad envelope") {
		t.Fatalf("want bad envelope, got %v", err)
	}
}

func TestCallStringErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"pool drained"}`))
	}))
	t.Cleanup(srv.Close)
	c := proxypool.New(proxypool.Config{BaseURL: srv.URL, Token: "t"})
	_, err := c.Acquire(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pool drained") {
		t.Fatalf("want pool drained, got %v", err)
	}
}
