package gui

import (
	"log"
	"strings"
	"testing"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/keys"
	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/route"
)

func testLog() *log.Logger {
	return log.New(discard{}, "", 0)
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// fakeRouter строит роутер с одним фейк-провайдером и одним пулом ключей.
func fakeRouter(t *testing.T, apiKeys []string) *route.Router {
	t.Helper()
	p := &provider.Provider{
		Name:    "fake",
		Type:    "openai-compatible",
		BaseURL: "http://127.0.0.1:9/v1",
		APIKeys: apiKeys,
		Models:  []string{"m1", "m2"},
		Tier:    "api",
	}
	return route.New(map[string]*provider.Provider{"fake": p})
}

func fakeApp(t *testing.T, r *route.Router, ks *keys.Store) *App {
	t.Helper()
	cfg := config.Default()
	cfg.Hub.URL = "" // без сети в юнит-тестах
	return NewApp(r, ks, cfg, "test-config.yaml", "test", testLog())
}

func TestAppStatus(t *testing.T) {
	r := fakeRouter(t, []string{"sk-a", "sk-b"})
	ks := keys.New(nil, nil)
	if err := ks.Create(config.KeyConf{Name: "dev", Key: "vk-dev-123", QuotaTokens: 1000}); err != nil {
		t.Fatal(err)
	}
	app := fakeApp(t, r, ks)

	st, err := app.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Providers != 1 || st.Models != 5 || st.Keys != 1 {
		t.Fatalf("status = %+v", st)
	}
	if st.Version != "test" || st.HubOnline {
		t.Fatalf("status version/hub = %+v", st)
	}
	if st.UptimeS < 0 {
		t.Fatalf("uptime negative: %+v", st)
	}
}

func TestAppEndpoints(t *testing.T) {
	r := fakeRouter(t, []string{"sk-a", "sk-b"})
	app := fakeApp(t, r, keys.New(nil, nil))

	eps, err := app.Endpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 4 {
		t.Fatalf("endpoints = %d, want runtime + catalog entries", len(eps))
	}
	var ep EndpointView
	for _, candidate := range eps {
		if candidate.Name == "fake" {
			ep = candidate
			break
		}
	}
	if ep.Name != "fake" || ep.BaseURL != "http://127.0.0.1:9/v1" || ep.Keys != 2 {
		t.Fatalf("endpoint = %+v", ep)
	}
	if ep.Breaker != "closed" || ep.KeysHealthy != 2 || ep.KeysBackoff != 0 {
		t.Fatalf("endpoint health = %+v", ep)
	}

	// 429 на один ключ → он уходит в backoff (rate-limit), другой остаётся жив.
	pl := r.Pool("fake")
	key, err := pl.Pick()
	if err != nil {
		t.Fatal(err)
	}
	pl.Report(key, 429, "")

	eps, _ = app.Endpoints()
	for _, candidate := range eps {
		if candidate.Name == "fake" {
			ep = candidate
			break
		}
	}
	if ep.KeysHealthy != 1 || ep.KeysBackoff != 1 {
		t.Fatalf("endpoint after 429 = %+v", ep)
	}
	if ep.BreakerFailures != 0 {
		t.Fatalf("breaker must stay closed, got %+v", ep)
	}
}

func TestAppKeysAndUsage(t *testing.T) {
	ks := keys.New(nil, nil)
	if err := ks.Create(config.KeyConf{Name: "dev", Key: "vk-dev-1234567890", QuotaTokens: 1000, QuotaReqs: 100, BudgetUSD: 100}); err != nil {
		t.Fatal(err)
	}
	if err := ks.Create(config.KeyConf{Name: "prod", Key: "vk-prod-1234567890", QuotaTokens: 5000, QuotaReqs: 0}); err != nil {
		t.Fatal(err)
	}
	if err := ks.Revoke("vk-prod-1234567890"); err != nil {
		t.Fatal(err)
	}
	if err := ks.RecordUsageByKey("vk-dev-1234567890", "", 250, 50); err != nil {
		t.Fatal(err)
	}
	app := fakeApp(t, fakeRouter(t, nil), ks)

	kvs, err := app.Keys()
	if err != nil {
		t.Fatal(err)
	}
	if len(kvs) != 2 {
		t.Fatalf("keys = %d, want 2", len(kvs))
	}
	dev := kvs[0]
	if dev.Name != "dev" || strings.Contains(dev.Key, "1234567890") {
		t.Fatalf("key must be masked, got %+v", dev)
	}
	if dev.QuotaTokens != 1000 || dev.UsageTokens != 300 || dev.TokenPct != 0.3 {
		t.Fatalf("dev quota = %+v", dev)
	}
	prod := kvs[1]
	if !prod.Revoked || prod.ReqPct != -1 {
		t.Fatalf("prod = %+v", prod)
	}

	u, err := app.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if u.Keys != 2 || u.Revoked != 1 || u.Reqs != 1 || u.Tokens != 300 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestAppTestNoModels(t *testing.T) {
	app := fakeApp(t, route.New(nil), keys.New(nil, nil))
	tr, err := app.Test("")
	if err == nil {
		t.Fatalf("expected error for empty router, got %+v", tr)
	}
	if !strings.Contains(err.Error(), "моделей") {
		t.Fatalf("unexpected error: %v", err)
	}
}
