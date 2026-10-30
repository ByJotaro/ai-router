package gui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/route"
	"github.com/ByJotaro/ai-router/internal/store"
)

func TestAppComboCRUD(t *testing.T) {
	s := openTestStore(t)
	app := fakeApp(t, fakeRouter(t, nil), nil).withStore(s)

	id, err := app.ComboSave("duo", "", []map[string]any{
		{"provider": "openai", "model": "gpt-4o-mini"},
		{"provider": "deepseek", "model": "deepseek-chat"},
	})
	if err != nil {
		t.Fatalf("ComboSave: %v", err)
	}
	if id <= 0 {
		t.Fatalf("ComboSave id = %d, want > 0", id)
	}

	views, err := app.ComboList()
	if err != nil {
		t.Fatalf("ComboList: %v", err)
	}
	if len(views) != 1 || views[0].Name != "duo" || views[0].ID != id {
		t.Fatalf("ComboList = %+v", views)
	}
	if len(views[0].Steps) != 2 || views[0].Steps[0].Provider != "openai" || views[0].Steps[1].Model != "deepseek-chat" {
		t.Fatalf("steps = %+v", views[0].Steps)
	}

	if err := app.ComboDelete(id); err != nil {
		t.Fatalf("ComboDelete: %v", err)
	}
	views, err = app.ComboList()
	if err != nil {
		t.Fatalf("ComboList after delete: %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("combos after delete = %d, want 0", len(views))
	}
}
func TestAppComboDeleteNotFound(t *testing.T) {
	s := openTestStore(t)
	app := fakeApp(t, fakeRouter(t, nil), nil).withStore(s)
	if err := app.ComboDelete(999999); err == nil {
		t.Fatal("ComboDelete missing id: want error, got nil")
	}
}

func TestAppComboTestFallsToLiveUpstream(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer down.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer up.Close()

	r := route.New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: down.URL, APIKeys: []string{"k1"}},
		"b": {Name: "b", Type: "openai-compatible", BaseURL: up.URL, APIKeys: []string{"k2"}},
	})
	s := openTestStore(t)
	app := fakeApp(t, r, nil).withStore(s)

	id, err := app.ComboSave("duo", "", []map[string]any{
		{"provider": "a", "model": "m-a"},
		{"provider": "b", "model": "m-b"},
	})
	if err != nil {
		t.Fatalf("ComboSave: %v", err)
	}

	tr, err := app.ComboTest(id, nil, "")
	if err != nil {
		t.Fatalf("ComboTest: %v", err)
	}
	if tr.Error != "" {
		t.Fatalf("combo failed: %s", tr.Error)
	}
	if tr.Provider != "b" || tr.ComboStep != "2:b" || tr.ComboName != "duo" {
		t.Fatalf("combo result = %+v", tr)
	}
	if tr.Model != "m-b" {
		t.Fatalf("model = %q, want m-b", tr.Model)
	}
	if tr.Content != "hi" {
		t.Fatalf("content = %q, want hi", tr.Content)
	}

	// Журнал: последняя запись несёт combo_name/combo_step_id.
	rows, err := app.Logs(5)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no log rows after combo test")
	}
	last := rows[0]
	if last.ComboName != "duo" || last.ComboStepID != "2:b" {
		t.Fatalf("log combo fields = %q/%q, want duo/2:b", last.ComboName, last.ComboStepID)
	}
	if last.Provider != "b" || last.Status != "ok" {
		t.Fatalf("log row = %+v", last)
	}
}

func TestAppComboTestStepsDirect(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer up.Close()

	r := route.New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: up.URL, APIKeys: []string{"k1"}},
	})
	s := openTestStore(t)
	app := fakeApp(t, r, nil).withStore(s)

	// Без id: шаги передаются напрямую (несохранённый черновик).
	tr, err := app.ComboTest(0, []map[string]any{
		{"provider": "a", "model": "m-a"},
	}, "m-a")
	if err != nil {
		t.Fatalf("ComboTest: %v", err)
	}
	if tr.Error != "" || tr.Provider != "a" || tr.ComboStep != "1:a" {
		t.Fatalf("combo result = %+v", tr)
	}
}

func TestAppComboTestAllStepsFail(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer down.Close()
	r := route.New(map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: down.URL, APIKeys: []string{"k1"}},
	})
	s := openTestStore(t)
	app := fakeApp(t, r, nil).withStore(s)

	tr, err := app.ComboTest(0, []map[string]any{{"provider": "a", "model": "m"}}, "")
	if err != nil {
		t.Fatalf("ComboTest: %v", err)
	}
	if tr.Error == "" {
		t.Fatalf("want error, got %+v", tr)
	}
	rows, _ := app.Logs(5)
	if len(rows) == 0 || rows[0].Status != "error" {
		t.Fatalf("error row missing: %+v", rows)
	}
}

// TestComboSavePersistsGraph: the canvas editor sends a graph (start/step/
// condition/finish with id/kind/on/to/stop). ComboSave must accept it and
// keep every graph field — it used to reject the default graph ("provider
// обязателен") and, when it did save, silently stored a plain linear chain
// (review 2026-10-17, r11/r22 F1).
func TestComboSavePersistsGraph(t *testing.T) {
	s := openTestStore(t)
	app := fakeApp(t, fakeRouter(t, nil), nil).withStore(s)

	steps := []map[string]any{
		{"id": "n1", "kind": "start", "provider": "", "model": "gpt-4o-mini"},
		{"id": "n2", "kind": "step", "provider": "openai", "model": "gpt-4o-mini"},
		{"id": "n3", "kind": "step", "provider": "deepseek", "model": "deepseek-chat", "quota_only": true},
		{"id": "n4", "kind": "condition", "on": []any{"auth", "network"}, "to": "n5"},
		{"id": "n5", "kind": "step", "provider": "openai", "model": "gpt-4o"},
		{"id": "n6", "kind": "finish", "provider": ""},
	}
	id, err := app.ComboSave("graph", "", steps)
	if err != nil {
		t.Fatalf("ComboSave graph: %v", err)
	}
	views, err := app.ComboList()
	if err != nil {
		t.Fatalf("ComboList: %v", err)
	}
	if len(views) != 1 || views[0].ID != id {
		t.Fatalf("views = %+v", views)
	}
	got := views[0].Steps
	if len(got) != len(steps) {
		t.Fatalf("steps saved = %d, want %d (%+v)", len(got), len(steps), got)
	}
	if got[1].Kind != "step" || got[3].Kind != "condition" {
		t.Fatalf("kinds lost: %+v", got)
	}
	if got[3].To != "n5" || len(got[3].On) != 2 {
		t.Fatalf("condition routing lost: %+v", got[3])
	}
	if !got[2].QuotaOnly() {
		t.Fatalf("quota_only lost: %+v", got[2])
	}
	if !store.IsGraph(got) {
		t.Fatalf("saved preset is not a graph: %+v", got)
	}
}

// TestComboSaveRejectsBadGraph: graph invariants still gate the save —
// a dangling `to`, a duplicate (normalized) id or a provider-less step must
// be rejected before anything hits the DB.
func TestComboSaveRejectsBadGraph(t *testing.T) {
	s := openTestStore(t)
	app := fakeApp(t, fakeRouter(t, nil), nil).withStore(s)

	cases := map[string][]map[string]any{
		"dangling to": {
			{"id": "n1", "kind": "step", "provider": "openai", "model": "gpt-4o"},
			{"id": "n2", "kind": "condition", "on": []any{"auth"}, "to": "n9"},
		},
		"duplicate id": {
			{"id": "n2", "kind": "step", "provider": "openai", "model": "gpt-4o"},
			{"id": "n2", "kind": "step", "provider": "deepseek", "model": "deepseek-chat"},
		},
		"step without provider": {
			{"id": "n1", "kind": "step", "provider": "", "model": "gpt-4o"},
		},
		"condition without on": {
			{"id": "n1", "kind": "step", "provider": "openai", "model": "gpt-4o"},
			{"id": "n2", "kind": "condition", "to": "n1"},
		},
	}
	for name, steps := range cases {
		if _, err := app.ComboSave("bad-"+name, "", steps); err == nil {
			t.Fatalf("%s: ComboSave accepted a broken graph", name)
		}
	}
}
