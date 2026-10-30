package route

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/store"
)

// hitUpstream serves a fixed status/body and counts the requests it saw.
func hitUpstream(t *testing.T, status int, body string, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const okBody = `{"id":"ok","object":"chat.completion","choices":[]}`

// graphRouter wires providers a (429 quota), au (401 auth) and c (200);
// hits counts upstream requests per provider name.
func graphRouter(t *testing.T, hits map[string]*int32) *Router {
	t.Helper()
	if hits == nil {
		hits = map[string]*int32{}
	}
	provs := map[string]*provider.Provider{}
	for name, status := range map[string]int{"a": 429, "au": 401, "b": 429, "c": 200} {
		if hits[name] == nil {
			hits[name] = new(int32)
		}
		provs[name] = &provider.Provider{
			Name:     name,
			Type:     "openai-compatible",
			BaseURL:  hitUpstream(t, status, okBody, hits[name]).URL,
			APIKeys:  []string{"k-" + name},
		}
	}
	return New(provs)
}

func comboGraphPath(res *Result) string {
	if res == nil {
		return ""
	}
	return strings.Join(res.ComboPath, " ")
}

func TestCallComboGraphConditionToJumpsToWinner(t *testing.T) {
	hits := map[string]*int32{}
	r := graphRouter(t, hits)
	steps := []store.ComboStep{
		{ID: "s0", Provider: "a", Model: "m-a"},
		{ID: "cond", Kind: "condition", On: []string{"quota"}, To: "s3"},
		{ID: "s2", Provider: "b", Model: "m-b"},
		{ID: "s3", Provider: "c", Model: "m-c"},
	}
	res, err := r.CallCombo(context.Background(), "graph", "m-a", steps, []byte(`{"model":"m-a"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	defer res.Resp.Body.Close()
	if res.Provider != "c" {
		t.Fatalf("provider = %q, want c", res.Provider)
	}
	if res.ComboName != "graph" || res.ComboStepID != "4:c" {
		t.Fatalf("combo fields: name=%q step=%q, want graph/4:c", res.ComboName, res.ComboStepID)
	}
	if res.Model != "m-c" {
		t.Fatalf("model = %q, want m-c", res.Model)
	}
	path := comboGraphPath(res)
	if !strings.Contains(path, "1:a:fail") {
		t.Fatalf("path %q: want first-step failure", path)
	}
	if !strings.Contains(path, "cond:to(") || !strings.Contains(path, ":win") {
		t.Fatalf("path %q: want cond:to( and :win", path)
	}
	if !strings.HasSuffix(path, "4:c:win") {
		t.Fatalf("path %q: want winner 4:c:win", path)
	}
	if n := atomic.LoadInt32(hits["b"]); n != 0 {
		t.Fatalf("provider b got %d requests, want 0 (jumped over)", n)
	}
}

func TestCallComboGraphConditionStopFails(t *testing.T) {
	hits := map[string]*int32{}
	r := graphRouter(t, hits)
	steps := []store.ComboStep{
		{ID: "s0", Provider: "au", Model: "m-au"},
		{ID: "cond", Kind: "condition", On: []string{"auth", "network"}, Stop: true},
		{ID: "s2", Provider: "c", Model: "m-c"},
	}
	res, err := r.CallCombo(context.Background(), "graph", "m-au", steps, []byte(`{"model":"m-au"}`), false)
	if err == nil {
		res.Resp.Body.Close()
		t.Fatal("want error on stop-condition, got success")
	}
	var cge *comboGraphError
	if !errors.As(err, &cge) {
		t.Fatalf("err %v: want comboGraphError carrying the walk path", err)
	}
	path := strings.Join(cge.Path(), " ")
	if !strings.Contains(path, "cond:stop") {
		t.Fatalf("path %q: want cond:stop", path)
	}
	if !strings.Contains(path, "tail:stop") {
		t.Fatalf("path %q: want tail:stop", path)
	}
	if n := atomic.LoadInt32(hits["c"]); n != 0 {
		t.Fatalf("provider c got %d requests, want 0 (cascade stopped)", n)
	}
}

func TestCallComboGraphConditionMissFallsThroughInOrder(t *testing.T) {
	hits := map[string]*int32{}
	r := graphRouter(t, hits)
	steps := []store.ComboStep{
		{ID: "s0", Provider: "au", Model: "m-au"},
		{ID: "cond", Kind: "condition", On: []string{"quota"}, To: "s3"},
		{ID: "s2", Provider: "c", Model: "m-c"},
	}
	res, err := r.CallCombo(context.Background(), "graph", "m-au", steps, []byte(`{"model":"m-au"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	defer res.Resp.Body.Close()
	if res.ComboStepID != "3:c" {
		t.Fatalf("step = %q, want 3:c (declaration order after miss)", res.ComboStepID)
	}
	path := comboGraphPath(res)
	if !strings.Contains(path, "cond:miss") {
		t.Fatalf("path %q: want cond:miss", path)
	}
	if strings.Contains(path, "cond:to(") {
		t.Fatalf("path %q: miss must not jump", path)
	}
}

func TestCallComboGraphQuotaOnlyStepSkippedOnAuth(t *testing.T) {
	hits := map[string]*int32{}
	r := graphRouter(t, hits)
	steps := []store.ComboStep{
		{ID: "s0", Provider: "au", Model: "m-au"},
		{ID: "s1", Provider: "b", Model: "m-b", On: []string{"quota"}},
		{ID: "s2", Provider: "c", Model: "m-c"},
	}
	res, err := r.CallCombo(context.Background(), "graph", "m-au", steps, []byte(`{"model":"m-au"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	defer res.Resp.Body.Close()
	if res.ComboStepID != "3:c" {
		t.Fatalf("step = %q, want 3:c", res.ComboStepID)
	}
	path := comboGraphPath(res)
	if !strings.Contains(path, "2:b:skip") {
		t.Fatalf("path %q: want 2:b:skip", path)
	}
	if n := atomic.LoadInt32(hits["b"]); n != 0 {
		t.Fatalf("quota-only step got %d requests, want 0", n)
	}
}

func TestCallComboGraphFinishEndsWalk(t *testing.T) {
	hits := map[string]*int32{}
	r := graphRouter(t, hits)
	steps := []store.ComboStep{
		{ID: "s0", Provider: "a", Model: "m-a"},
		{ID: "fin", Kind: "finish"},
		{ID: "s2", Provider: "c", Model: "m-c"},
	}
	res, err := r.CallCombo(context.Background(), "graph", "m-a", steps, []byte(`{"model":"m-a"}`), false)
	if err == nil {
		res.Resp.Body.Close()
		t.Fatal("want error (finish terminates the walk), got success")
	}
	if n := atomic.LoadInt32(hits["c"]); n != 0 {
		t.Fatalf("provider c got %d requests, want 0 (finish ended the walk)", n)
	}
	// finish surfaces the last failure of the walk, not a comboGraphError.
	var cge *comboGraphError
	if errors.As(err, &cge) {
		t.Fatalf("err %v: finish must not produce a stop-verdict error", err)
	}
	if !strings.Contains(err.Error(), "m-a") && !strings.Contains(err.Error(), "provider a") {
		t.Fatalf("err %q: want the failed step's error", err)
	}
}

func TestCallComboGraphBackEdgeCycleBounded(t *testing.T) {
	hits := map[string]*int32{}
	r := graphRouter(t, hits)
	steps := []store.ComboStep{
		{ID: "s0", Provider: "a", Model: "m-a"},
		{ID: "loop", Kind: "condition", On: []string{"quota"}, To: "s0"},
	}
	res, err := r.CallCombo(context.Background(), "graph", "m-a", steps, []byte(`{"model":"m-a"}`), false)
	if err == nil {
		res.Resp.Body.Close()
		t.Fatal("want cycle error, got success")
	}
	if !strings.Contains(err.Error(), "граф зациклен") {
		t.Fatalf("err %q: want «граф зациклен»", err)
	}
	// Бюджет 8N+8 посещений: узел s0 должен быть пройден многократно, но не
	// бесконечно (иначе запрос завис бы).
	n := int(atomic.LoadInt32(hits["a"]))
	if n < 4 || n > 8*len(steps)+8 {
		t.Fatalf("step s0 visited %d times, want within the 8N+8 budget", n)
	}
}

func TestCallComboLegacyPresetUnchanged(t *testing.T) {
	hits := map[string]*int32{}
	r := graphRouter(t, hits)
	// Легаси-пресет: только provider/model + флаг quota_only.
	steps := []store.ComboStep{
		{Provider: "au", Model: "m-au"},
		{Provider: "b", Model: "m-b", QuotaOnlyFlag: true},
		{Provider: "c", Model: "m-c"},
	}
	res, err := r.CallCombo(context.Background(), "legacy", "m-au", steps, []byte(`{"model":"m-au"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	defer res.Resp.Body.Close()
	if res.ComboStepID != "3:c" {
		t.Fatalf("step = %q, want 3:c (quota-only skipped on auth)", res.ComboStepID)
	}
	if res.ComboName != "legacy" {
		t.Fatalf("name = %q, want legacy", res.ComboName)
	}
	if len(res.ComboPath) != 0 {
		t.Fatalf("legacy path = %v, want empty", res.ComboPath)
	}
	if n := atomic.LoadInt32(hits["b"]); n != 0 {
		t.Fatalf("quota-only step got %d requests, want 0", n)
	}
}

func TestCallComboGraphUnknownNodeIDFallsBackToDeclarationOrder(t *testing.T) {
	// To ссылается на несуществующий id: условие матчится, но прыжка нет —
	// каскад идёт по порядку объявления (dangling-метка в пути).
	hits := map[string]*int32{}
	r := graphRouter(t, hits)
	steps := []store.ComboStep{
		{ID: "s0", Provider: "a", Model: "m-a"},
		{ID: "cond", Kind: "condition", On: []string{"quota"}, To: "nope"},
		{ID: "s2", Provider: "c", Model: "m-c"},
	}
	res, err := r.CallCombo(context.Background(), "graph", "m-a", steps, []byte(`{"model":"m-a"}`), false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	defer res.Resp.Body.Close()
	if res.ComboStepID != "3:c" {
		t.Fatalf("step = %q, want 3:c", res.ComboStepID)
	}
	if path := comboGraphPath(res); !strings.Contains(path, "cond:match") {
		t.Fatalf("path %q: want cond:match", path)
	}
}
