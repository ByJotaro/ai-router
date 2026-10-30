package route

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/store"
)

// echoServer returns 200 echoing the received model in the response body.
func echoServer(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	got := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = req.Model
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestCallComboFallsThroughToNextStep(t *testing.T) {
	down := fakeUpstream(t, 500, "")
	defer down.Close()
	up := fakeUpstream(t, 200, `{"id":"ok","object":"chat.completion","choices":[]}`)
	defer up.Close()

	r := New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: down.URL, APIKeys: []string{"k1"}},
		"b": {Name: "b", Type: "openai-compatible", BaseURL: up.URL, APIKeys: []string{"k2"}},
	})
	steps := []store.ComboStep{
		{Provider: "a", Model: "m-a"},
		{Provider: "b", Model: "m-b"},
	}
	res, err := r.CallCombo(context.Background(), "my-combo", "m-a", steps, []byte(`{"model":"m-a"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Provider != "b" {
		t.Fatalf("want provider b, got %s", res.Provider)
	}
	if res.ComboName != "my-combo" || res.ComboStepID != "2:b" {
		t.Fatalf("combo fields: name=%q step=%q, want my-combo/2:b", res.ComboName, res.ComboStepID)
	}
	if res.Model != "m-b" {
		t.Fatalf("model = %q, want m-b", res.Model)
	}
	res.Resp.Body.Close()
}

func TestCallComboPatchesStepModel(t *testing.T) {
	srv, got := echoServer(t)
	r := New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: srv.URL, APIKeys: []string{"k1"}},
	})
	steps := []store.ComboStep{{Provider: "a", Model: "step-model-9"}}
	res, err := r.CallCombo(context.Background(), "c", "requested", steps, []byte(`{"model":"requested"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	res.Resp.Body.Close()
	if *got != "step-model-9" {
		t.Fatalf("upstream saw model %q, want step-model-9", *got)
	}
	if res.ComboStepID != "1:a" {
		t.Fatalf("combo step = %q, want 1:a", res.ComboStepID)
	}
	if res.Model != "step-model-9" {
		t.Fatalf("res.Model = %q, want step-model-9", res.Model)
	}
}

func TestCallComboUnknownProviderStep(t *testing.T) {
	r := New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", APIKeys: []string{"k1"}},
	})
	steps := []store.ComboStep{{Provider: "missing", Model: "m"}}
	_, err := r.CallCombo(context.Background(), "c", "m", steps, []byte(`{"model":"m"}`), false)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("want unknown-provider error, got %v", err)
	}
}

func TestCallComboAllStepsFail(t *testing.T) {
	down := fakeUpstream(t, 503, "")
	defer down.Close()
	r := New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: down.URL, APIKeys: []string{"k1"}},
		"b": {Name: "b", Type: "openai-compatible", BaseURL: down.URL, APIKeys: []string{"k2"}},
	})
	steps := []store.ComboStep{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}}
	_, err := r.CallCombo(context.Background(), "c", "m", steps, []byte(`{"model":"m"}`), false)
	if err == nil {
		t.Fatalf("want error, got nil")
	}
}
