package gui

import (
	"strings"
	"testing"
)

func TestValidateComboGraphCases(t *testing.T) {
	cases := []struct {
		name    string
		steps   []map[string]any
		wantSub string
	}{
		{
			name: "dup id",
			steps: []map[string]any{
				{"id": "s0", "provider": "fake", "model": "m1"},
				{"id": "s0", "provider": "fake", "model": "m2"},
			},
			wantSub: "дублирующийся id",
		},
		{
			name: "dangling to",
			steps: []map[string]any{
				{"id": "c1", "kind": "condition", "provider": "fake", "on": []any{"quota"}, "to": "nope"},
				{"id": "s1", "provider": "fake", "model": "m1"},
			},
			wantSub: "не найдена",
		},
		{
			name: "to backward",
			steps: []map[string]any{
				{"id": "s0", "provider": "fake", "model": "m1"},
				{"id": "c1", "kind": "condition", "provider": "fake", "on": []any{"quota"}, "to": "s0"},
			},
			wantSub: "впереди по порядку",
		},
		{
			name: "condition without on",
			steps: []map[string]any{
				{"id": "c1", "kind": "condition", "provider": "fake"},
				{"id": "s1", "provider": "fake", "model": "m1"},
			},
			wantSub: "on пуст",
		},
		{
			name: "unknown kind",
			steps: []map[string]any{
				{"id": "x", "kind": "wat", "provider": "fake", "model": "m1"},
				{"id": "s1", "provider": "fake", "model": "m1"},
			},
			wantSub: "неизвестный kind",
		},
		{
			name: "no executable steps",
			steps: []map[string]any{
				{"id": "c1", "kind": "condition", "on": []any{"quota"}},
			},
			wantSub: "ни одного исполняемого шага",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			steps, err := strictComboStepsFromMaps(c.steps)
			if err != nil {
				t.Fatalf("strictComboStepsFromMaps: %v", err)
			}
			err = validateComboGraph(steps)
			if err == nil {
				t.Fatalf("validateComboGraph: want error containing %q, got nil", c.wantSub)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("validateComboGraph err = %q, want substring %q", err, c.wantSub)
			}
		})
	}
}

func TestValidateComboGraphValid(t *testing.T) {
	steps, err := strictComboStepsFromMaps([]map[string]any{
		{"id": "s0", "kind": "start", "provider": "fake", "model": "m1"},
		{"id": "c1", "kind": "condition", "provider": "fake", "on": []any{"quota"}, "to": "s2"},
		{"id": "s2", "kind": "step", "provider": "fake", "model": "m2"},
		{"id": "f1", "kind": "finish", "provider": "fake"},
	})
	if err != nil {
		t.Fatalf("strictComboStepsFromMaps: %v", err)
	}
	if err := validateComboGraph(steps); err != nil {
		t.Fatalf("validateComboGraph: %v", err)
	}
}

func TestStrictComboStepsFromMapsUnknownField(t *testing.T) {
	_, err := strictComboStepsFromMaps([]map[string]any{
		{"provider": "fake", "model": "m1", "provdier": "typo"},
	})
	if err == nil {
		t.Fatal("strictComboStepsFromMaps: want error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "неизвестное поле") || !strings.Contains(err.Error(), "provdier") {
		t.Fatalf("err = %q, want unknown-field error naming «provdier»", err)
	}
	// on строкой и params не-объектом тоже ошибка (типы редактора).
	if _, err := strictComboStepsFromMaps([]map[string]any{{"provider": "fake", "on": "quota"}}); err == nil {
		t.Fatal("on as string: want error, got nil")
	}
	if _, err := strictComboStepsFromMaps([]map[string]any{{"provider": "fake", "params": "x"}}); err == nil {
		t.Fatal("params as string: want error, got nil")
	}
}

func TestStrictComboStepsPreservesGraphFields(t *testing.T) {
	steps, err := strictComboStepsFromMaps([]map[string]any{
		{"id": "s0", "kind": "step", "provider": "fake", "model": "m1", "on": []any{"quota", "auth"}, "to": "s2", "stop": true},
		{"id": "s2", "provider": "fake", "model": "m2", "quota_only": true},
	})
	if err != nil {
		t.Fatalf("strictComboStepsFromMaps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("steps = %+v, want 2", steps)
	}
	s0 := steps[0]
	if s0.ID != "s0" || s0.Kind != "step" || s0.To != "s2" || !s0.Stop {
		t.Fatalf("step 0 = %+v, want id/kind/to/stop preserved", s0)
	}
	if got := s0.OnClasses(); len(got) != 2 || got[0] != "quota" || got[1] != "auth" {
		t.Fatalf("step 0 on = %v, want [quota auth]", got)
	}
	if !steps[1].QuotaOnly() {
		t.Fatalf("step 1 = %+v, want quota_only preserved", steps[1])
	}
	// params.quota_only тоже флаг (легаси-хранение внутри params).
	pSteps, err := strictComboStepsFromMaps([]map[string]any{
		{"provider": "fake", "model": "m1", "params": map[string]any{"quota_only": true}},
	})
	if err != nil {
		t.Fatalf("strictComboStepsFromMaps params: %v", err)
	}
	if !pSteps[0].QuotaOnly() {
		t.Fatalf("params.quota_only lost: %+v", pSteps[0])
	}
}

func TestAppComboUpdateNoDuplicates(t *testing.T) {
	s := openTestStore(t)
	app := fakeApp(t, fakeRouter(t, nil), nil).withStore(s)

	id, err := app.ComboSave("duo", "", []map[string]any{
		{"provider": "fake", "model": "m1"},
		{"provider": "fake", "model": "m2"},
	})
	if err != nil {
		t.Fatalf("ComboSave: %v", err)
	}
	graph := []map[string]any{
		{"id": "s0", "kind": "start", "provider": "fake", "model": "m1"},
		{"id": "c1", "kind": "condition", "provider": "fake", "on": []any{"quota"}, "to": "s2"},
		{"id": "s2", "kind": "step", "provider": "fake", "model": "m2"},
	}
	// Повторные сохранения редактора не плодят дубли строк.
	for range 3 {
		if err := app.ComboUpdate(id, "graph", "m1", graph); err != nil {
			t.Fatalf("ComboUpdate: %v", err)
		}
	}
	views, err := app.ComboList()
	if err != nil {
		t.Fatalf("ComboList: %v", err)
	}
	if len(views) != 1 || views[0].ID != id {
		t.Fatalf("ComboList = %+v, want single preset id=%d", views, id)
	}
	if views[0].Name != "graph" {
		t.Fatalf("name = %q, want graph", views[0].Name)
	}
	steps := views[0].Steps
	if len(steps) != 3 {
		t.Fatalf("steps = %+v, want 3", steps)
	}
	if !steps[1].IsCondition() || len(steps[1].OnClasses()) != 1 || steps[1].To != "s2" {
		t.Fatalf("condition step = %+v, want on=[quota] to=s2", steps[1])
	}
	if steps[2].Kind != "step" || steps[2].Model != "m2" {
		t.Fatalf("step 2 = %+v", steps[2])
	}
	// Несуществующий id — ошибка, а не молчаливый no-op.
	if err := app.ComboUpdate(999999, "ghost", "m1", graph); err == nil {
		t.Fatal("ComboUpdate missing id: want error, got nil")
	}
}
