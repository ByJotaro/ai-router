package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/keys"
	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/route"
)

func TestAppRisksQuotaHigh(t *testing.T) {
	ks := keys.New(nil, nil)
	if err := ks.Create(config.KeyConf{Name: "dev", Key: "vk-risks-123456", QuotaTokens: 100}); err != nil {
		t.Fatal(err)
	}
	if err := ks.RecordUsageByKey("vk-risks-123456", "", 90, 0); err != nil {
		t.Fatal(err)
	}
	app := fakeApp(t, fakeRouter(t, nil), ks).withStore(openTestStore(t))

	risks, err := app.Risks()
	if err != nil {
		t.Fatal(err)
	}
	if len(risks) == 0 {
		t.Fatal("ожидался high-риск по квоте токенов")
	}
	hasHigh, hasMsg := false, false
	for _, r := range risks {
		if r.Level == "high" {
			hasHigh = true
		}
		if strings.Contains(r.Msg, "dev") {
			hasMsg = true
		}
	}
	if !hasHigh || !hasMsg {
		t.Fatalf("risks = %+v", risks)
	}
}

func TestAppRisksEmpty(t *testing.T) {
	app := fakeApp(t, fakeRouter(t, nil), keys.New(nil, nil)).withStore(openTestStore(t))
	risks, err := app.Risks()
	if err != nil {
		t.Fatal(err)
	}
	// Чистый роутер, без ключей/ошибок — допускаем пустой список (или только хаб).
	for _, r := range risks {
		if r.Msg == "" {
			t.Fatalf("risk with empty msg: %+v", r)
		}
	}
}

func TestAppTestKey(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer up.Close()

	s := openTestStore(t)
	r := route.New(map[string]*provider.Provider{
		"fake": {Name: "fake", Type: "openai-compatible", BaseURL: up.URL, APIKeys: []string{"sk-a"}},
	})
	ks := keys.New(s.DB(), nil)
	if err := ks.Create(config.KeyConf{Name: "dev", Key: "vk-dev-111111", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	app := fakeApp(t, r, ks).withStore(s)

	tr, err := app.TestKey("dev")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Error != "" {
		t.Fatalf("test failed: %+v", tr)
	}
	if tr.Provider != "fake" || tr.Model != "m1" || tr.Content != "hi" {
		t.Fatalf("test result = %+v", tr)
	}

	// Ключ списывается: usage tokens выросли.
	k := ks.FindByName("dev")
	if k == nil || k.UsageTokens != 5 {
		t.Fatalf("key usage after TestKey = %+v", k)
	}
}

func TestExportLogsJSONAndCSV(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.DB().Exec(
		`INSERT INTO request_log(ts, rid, model, requested_model, provider, status, http_status, error, duration_ms)
		 VALUES('2026-08-01T10:00:00Z','rid-x','m-a','m-a','fake','ok',200,'',12)`); err != nil {
		t.Fatal(err)
	}
	app := fakeApp(t, fakeRouter(t, nil), nil).withStore(s)

	js, err := app.ExportLogs("request", "json")
	if err != nil {
		t.Fatal(err)
	}
	var filtered []map[string]any
	if err := json.Unmarshal([]byte(js), &filtered); err != nil {
		t.Fatalf("json export invalid: %v", err)
	}
	if len(filtered) != 1 || filtered[0]["rid"] != "rid-x" {
		t.Fatalf("json export = %+v", filtered)
	}

	csv, err := app.ExportLogs("request", "csv")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(csv, "rid,model,requested_model") || !strings.Contains(csv, "rid-x") {
		t.Fatalf("csv export = %q", csv)
	}
}

func TestClearLogs(t *testing.T) {
	s := openTestStore(t)
	for _, q := range []string{
		`INSERT INTO request_log(ts, rid, status) VALUES('2026-08-01T10:00:00Z','a','ok')`,
		`INSERT INTO proxies_log(ts, proxy, action, ok) VALUES('2026-08-01T10:00:00Z','socks://h', 'check', 1)`,
	} {
		if _, err := s.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	app := fakeApp(t, fakeRouter(t, nil), nil).withStore(s)

	if err := app.ClearLogs("request"); err != nil {
		t.Fatal(err)
	}
	if err := app.ClearLogs("proxies"); err != nil {
		t.Fatal(err)
	}
	rq, _ := app.Logs(10)
	if len(rq) != 0 {
		t.Fatalf("request logs after clear = %+v", rq)
	}
	px, _ := app.ProxiesLogs(10)
	if len(px) != 0 {
		t.Fatalf("proxies logs after clear = %+v", px)
	}
	if err := app.ClearLogs("bogus"); err == nil {
		t.Fatal("ClearLogs('bogus') must reject")
	}
}
