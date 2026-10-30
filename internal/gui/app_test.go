package gui

import (
	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openTestStore открывает временный SQLite-стор для тестов журналов.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAppLogs(t *testing.T) {
	s := openTestStore(t)
	_, err := s.DB().Exec(
		`INSERT INTO request_log(ts, rid, model, requested_model, provider, status, http_status, error, duration_ms)
		 VALUES('2026-08-01T10:00:00Z','rid-1','m-a','m-a','prov-1','ok',200,'',12)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB().Exec(
		`INSERT INTO request_log(ts, rid, model, requested_model, provider, status, http_status, error, duration_ms)
		 VALUES('2026-08-01T10:05:00Z','rid-2','m-b','m-b','prov-2','error',500,'upstream 502',99)`)
	if err != nil {
		t.Fatal(err)
	}
	app := fakeApp(t, fakeRouter(t, nil), nil).withStore(s)

	logs, err := app.Logs(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Fatalf("Logs = %d rows, want 2", len(logs))
	}
	if logs[0].RID != "rid-2" || logs[0].Status != "error" || logs[0].HTTPStatus != 500 {
		t.Fatalf("newest row first, got %+v", logs[0])
	}

	found, err := app.LogsByQuery("prov-2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Provider != "prov-2" {
		t.Fatalf("LogsByQuery(prov-2) = %+v", found)
	}
	byErr, err := app.LogsByQuery("502", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(byErr) != 1 {
		t.Fatalf("LogsByQuery(502) = %+v", byErr)
	}
}

func TestWarpPoolStatsLive(t *testing.T) {
	// warpPoolStats ходит в warp-router HTTP: фейковый /api/v1/pool/stats.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/pool/stats" {
			t.Errorf("path = %s, want /api/v1/pool/stats", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok123" {
			t.Errorf("auth = %q, want Bearer tok123", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":true,"state":"ready","size":2,"up":2,"unique_egresses":2,"acquire_total":10,"acquire_ok":9,"acquire_fail":1,"rotate_total":1,"restarts":0,"front_http":"http://127.0.0.1:8118","front_socks":"127.0.0.1:1080","backends":[{"id":"b1","state":"up","public_ip":"1.2.3.4","requests":7}]}`))
	}))
	defer srv.Close()
	app := &App{cfg: &config.Config{Providers: map[string]config.ProviderConf{
		"opencode": {Transport: config.TransportConf{Mode: "pool", APIURL: srv.URL, Token: "tok123"}},
	}}}
	st, apiURL := app.warpPoolStats()
	if apiURL != srv.URL {
		t.Fatalf("apiURL = %s, want %s", apiURL, srv.URL)
	}
	if st.Err != "" {
		t.Fatalf("warp stats err: %s", st.Err)
	}
	if !st.Enabled || st.State != "ready" || st.Up != 2 || st.UniqueEgress != 2 {
		t.Fatalf("warp stats wrong: %+v", st)
	}
	if st.AcquireTotal != 10 || st.RotateTotal != 1 || st.FrontHTTP != "http://127.0.0.1:8118" {
		t.Fatalf("warp counters/ports wrong: %+v", st)
	}
	if len(st.Backends) != 1 || st.Backends[0].Requests != 7 {
		t.Fatalf("warp backends wrong: %+v", st.Backends)
	}
	// Недоступный warp: stats с Err, не нули-ошибка.
	app2 := &App{cfg: &config.Config{Providers: map[string]config.ProviderConf{
		"opencode": {Transport: config.TransportConf{Mode: "pool", APIURL: "http://127.0.0.1:1", Token: "x"}},
	}}}
	st2, _ := app2.warpPoolStats()
	if st2.Err == "" {
		t.Fatal("dead warp must surface Err, not silent zeros")
	}
}

func TestAppProxiesLogs(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.DB().Exec(
		`INSERT INTO proxies_log(ts, proxy, action, ok, latency_ms, error)
		 VALUES('2026-08-01T10:00:00Z','socks5://h1','rotate',1,150,'')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO proxies_log(ts, proxy, action, ok, latency_ms, error)
		 VALUES('2026-08-01T10:05:00Z','socks5://h2','check',0,0,'timeout')`); err != nil {
		t.Fatal(err)
	}
	app := fakeApp(t, fakeRouter(t, nil), nil).withStore(s)

	rows, err := app.ProxiesLogs(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Proxy != "socks5://h2" || rows[0].OK {
		t.Fatalf("ProxiesLogs = %+v", rows)
	}
	if !rows[1].OK {
		t.Fatalf("first row must be ok=true: %+v", rows)
	}
}

func TestAppDailyUsage(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()
	days := []string{
		now.AddDate(0, 0, -2).Format("2006-01-02"),
		now.AddDate(0, 0, -1).Format("2006-01-02"),
		now.Format("2006-01-02"),
	}
	for i, day := range days {
		reqs := int64(i + 1)
		if _, err := s.DB().Exec(
			`INSERT INTO daily_usage(day, requests, prompt_tokens, completion_tokens, cache_read, cache_write, cost_usd)
			 VALUES(?,?,?,?,?,?,?)`,
			day, reqs, 100*reqs, 50*reqs, 25*reqs, 10*reqs, 7*reqs); err != nil {
			t.Fatal(err)
		}
	}
	app := fakeApp(t, fakeRouter(t, nil), nil).withStore(s)

	v, err := app.DailyUsage(14)
	if err != nil {
		t.Fatal(err)
	}
	if v.Days != 3 || v.Requests != 6 || v.Prompt != 600 || v.Completion != 300 || v.CacheRead != 150 || v.CacheWrite != 60 || v.CostUSD != 42 {
		t.Fatalf("DailyUsage = %+v", v)
	}
	if len(v.Series) != 3 || v.Series[0].Day != days[0] || v.Series[2].Day != days[2] {
		t.Fatalf("series = %+v", v.Series)
	}
}
func TestAppUsageAnalyticsLight(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -400).Format("2006-01-02")
	today := now.Format("2006-01-02")
	if _, err := s.DB().Exec(
		`INSERT INTO daily_usage(day, requests, prompt_tokens, completion_tokens, cache_read, cache_write, cost_usd)
		 VALUES(?,?,100,50,0,0,7)`,
		old, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO daily_usage(day, requests, prompt_tokens, completion_tokens, cache_read, cache_write, cost_usd)
		 VALUES(?,?,200,100,0,0,14)`,
		today, 20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO request_log(ts, rid, model, requested_model, provider, status, http_status, error, duration_ms)
		 VALUES('2026-08-01T10:00:00Z','rid-1','m-a','m-a','prov-1','ok',200,'',12)`); err != nil {
		t.Fatal(err)
	}
	app := fakeApp(t, fakeRouter(t, nil), nil).withStore(s)
	v, err := app.UsageAnalyticsLight(365)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Series) != 1 || v.Series[0].Day != today {
		t.Fatalf("light series = %+v", v.Series)
	}
	if len(v.DailyTrend) != 1 || v.DailyTrend[0].Date != today || v.DailyTrend[0].Total != 300 {
		t.Fatalf("light trend = %+v", v.DailyTrend)
	}
	if len(v.ByModel) != 0 || len(v.ByProvider) != 0 || len(v.ActivityMap) != 0 || len(v.Weekly) != 0 {
		t.Fatalf("light must skip heavy aggregates: %+v", v)
	}
}

func TestAppGetConfigSaveConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`server:
    host: 127.0.0.1
    port: 9001
admin_token: abc123
providers:
    fake:
        type: openai-compatible
        base_url: http://127.0.0.1:9/v1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(fakeRouter(t, nil), nil, cfg, path, "test", testLog())

	m, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	server, ok := m["server"].(map[string]any)
	if !ok || server["port"].(int) != 9001 {
		t.Fatalf("GetConfig server = %#v", m["server"])
	}
	if _, present := m["admin_token"]; present {
		t.Fatalf("GetConfig must redact admin_token, got %#v", m["admin_token"])
	}
	provs, ok := m["providers"].(map[string]any)
	if !ok || len(provs) != 1 {
		t.Fatalf("GetConfig providers = %#v", m["providers"])
	}

	// Правка порта и провайдера → сохранение → файл меняется.
	server["port"] = 9123
	res, err := app.SaveConfig(m)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("SaveConfig: %s", res.Message)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Server.Port != 9123 {
		t.Fatalf("port after save = %d, want 9123", reloaded.Server.Port)
	}
	if _, ok := reloaded.Providers["fake"]; !ok {
		t.Fatalf("provider lost after save: %+v", reloaded.Providers)
	}
	if reloaded.AdminToken != "abc123" {
		t.Fatalf("admin_token must survive an edit that never touched it, got %q", reloaded.AdminToken)
	}
}

// Editing admin_token in the GUI form must actually persist the new value:
// GetConfig redacts it, so blank means "unchanged" but a typed value must win.
func TestAppSaveConfigChangesAdminToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n    port: 9001\nadmin_token: old-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(fakeRouter(t, nil), nil, cfg, path, "test", testLog())
	m, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	m["admin_token"] = "new-secret"
	res, err := app.SaveConfig(m)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("SaveConfig: %s", res.Message)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AdminToken != "new-secret" {
		t.Fatalf("admin_token = %q, want new-secret", reloaded.AdminToken)
	}
}

// Virtual key secrets (config.yaml `keys[].key`) must be redacted from
// GetConfig and preserved by SaveConfig when the GUI never touched them.
func TestAppGetConfigRedactsVirtualKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`server:
    port: 9001
keys:
    - name: alice
      key: sk-alice-secret
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(fakeRouter(t, nil), nil, cfg, path, "test", testLog())
	m, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	ks, ok := m["keys"].([]any)
	if !ok || len(ks) != 1 {
		t.Fatalf("GetConfig keys = %#v", m["keys"])
	}
	k0 := ks[0].(map[string]any)
	if v, present := k0["key"]; present && v != "" {
		t.Fatalf("GetConfig must redact virtual key secret, got %#v", v)
	}
	res, err := app.SaveConfig(m)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("SaveConfig: %s", res.Message)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Keys) != 1 || reloaded.Keys[0].Key != "sk-alice-secret" {
		t.Fatalf("virtual key secret lost after save: %+v", reloaded.Keys)
	}
}

func TestAppSaveConfigRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n    port: 9001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(fakeRouter(t, nil), nil, cfg, path, "test", testLog())

	bad := map[string]any{
		"server": map[string]any{"port": "not-a-number"},
	}
	res, err := app.SaveConfig(bad)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatalf("SaveConfig must reject invalid port, got %s", res.Message)
	}
}

func TestAppReloadConfigStub(t *testing.T) {
	app := fakeApp(t, fakeRouter(t, nil), nil)
	res, err := app.ReloadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// GUI does not own a separate daemon process: ReloadConfig must not claim
	// success it cannot deliver.
	if res.OK {
		t.Fatalf("ReloadConfig must be truthful about not owning a daemon, got %+v", res)
	}
}
