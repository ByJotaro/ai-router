package hubmod

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/keys"
	"github.com/ByJotaro/ai-router/internal/logring"
	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/route"
	"github.com/ByJotaro/ai-router/internal/sdk"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newTestModule(t *testing.T, h http.HandlerFunc) (*Module, string) {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, APIKeys: []string{"upkey"}, Models: []string{"m1"}},
	}
	r := route.New(provs)
	ks := keys.New(nil, nil)
	_ = ks.Load()
	_ = ks.SetConfig([]config.KeyConf{{Name: "vk", Key: "vkey", QuotaReqs: 10}})
	ring := logring.New(50)
	cfg := &config.Config{Hub: config.HubConfig{URL: "", Module: "ai-router", Token: "mt", Addr: "", Register: false}}
	m := New(cfg, r, ks, ring, "test")
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	go func() {
		_ = m.mux.Serve(addr, sdk.Options{Token: "mt", Version: "test", Capabilities: m.Capabilities()})
	}()
	time.Sleep(100 * time.Millisecond)
	return m, addr
}

func callAction(t *testing.T, addr, action string, params any) (*http.Response, []byte) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"params": params})
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/actions/"+action, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer mt")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, body
}

func TestChatCompleteAction(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-9", "object": "chat.completion", "model": "m1",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "hi from fake"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7},
		})
	}))
	defer up.Close()
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1"}},
	}
	r := route.New(provs)
	ks := keys.New(nil, nil)
	_ = ks.Load()
	cfg := &config.Config{Hub: config.HubConfig{Module: "ai-router", Token: "mt"}}
	m := New(cfg, r, ks, logring.New(10), "test")
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	go func() { _ = m.mux.Serve(addr, sdk.Options{Token: "mt", Version: "test"}) }()
	time.Sleep(100 * time.Millisecond)

	res, body := callAction(t, addr, "chat.complete", map[string]any{
		"model": "m1", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var out struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Error != nil {
		t.Fatalf("expected ok, got %s", body)
	}
	if out.Result["content"] != "hi from fake" {
		t.Fatalf("bad content: %v", out.Result)
	}
	if out.Result["provider"] != "p" {
		t.Fatalf("bad provider: %v", out.Result)
	}
}

func TestModelsListAction(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1", "m2"}},
	}
	r := route.New(provs)
	ks := keys.New(nil, nil)
	_ = ks.Load()
	cfg := &config.Config{Hub: config.HubConfig{Module: "ai-router", Token: "mt"}}
	m := New(cfg, r, ks, logring.New(10), "test")
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	go func() { _ = m.mux.Serve(addr, sdk.Options{Token: "mt", Version: "test"}) }()
	time.Sleep(100 * time.Millisecond)

	res, body := callAction(t, addr, "models.list", nil)
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			Models []string `json:"models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Result.Models) != 5 || out.Result.Models[0] != "auto" {
		t.Fatalf("want auto + 2 bare + 2 pinned models, got %s", body)
	}
	for _, want := range []string{"m1", "m2", "p/m1", "p/m2"} {
		found := false
		for _, id := range out.Result.Models {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing model %q in %v", want, out.Result.Models)
		}
	}
}

func TestUsageAction(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1"}},
	}
	r := route.New(provs)
	ks := keys.New(nil, nil)
	_ = ks.Load()
	_ = ks.SetConfig([]config.KeyConf{{Name: "vk", Key: "vkey", QuotaReqs: 10}})
	cfg := &config.Config{Hub: config.HubConfig{Module: "ai-router", Token: "mt"}}
	m := New(cfg, r, ks, logring.New(10), "test")
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	go func() { _ = m.mux.Serve(addr, sdk.Options{Token: "mt", Version: "test"}) }()
	time.Sleep(100 * time.Millisecond)

	_, body := callAction(t, addr, "usage", nil)
	if !strings.Contains(string(body), `"total_requests"`) {
		t.Fatalf("usage missing totals: %s", body)
	}
	if strings.Contains(string(body), "vkey") && strings.Contains(string(body), `"key":"`) {
		// masked key must not equal the raw value
		if strings.Contains(string(body), `"key":"vkey"`) {
			t.Fatalf("raw key leaked: %s", body)
		}
	}
}

func TestAuthRequired(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1"}},
	}
	r := route.New(provs)
	ks := keys.New(nil, nil)
	_ = ks.Load()
	cfg := &config.Config{Hub: config.HubConfig{Module: "ai-router", Token: "mt"}}
	m := New(cfg, r, ks, logring.New(10), "test")
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	go func() { _ = m.mux.Serve(addr, sdk.Options{Token: "mt", Version: "test"}) }()
	time.Sleep(100 * time.Millisecond)

	data, _ := json.Marshal(map[string]any{"params": map[string]any{}})
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/actions/models.list", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer wrong")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", res.StatusCode)
	}
}
