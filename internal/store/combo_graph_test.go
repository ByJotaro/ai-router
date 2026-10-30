package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestComboStepQuotaOnlySources(t *testing.T) {
	cases := []struct {
		name string
		step ComboStep
		want bool
	}{
		{"legacy flag", ComboStep{Provider: "a", QuotaOnlyFlag: true}, true},
		{"on-gate quota", ComboStep{Provider: "a", On: []string{"quota"}}, true},
		{"on-gate multi", ComboStep{Provider: "a", On: []string{"quota", "auth"}}, false},
		{"params flag", ComboStep{Provider: "a", Params: json.RawMessage(`{"quota_only":true}`)}, true},
		{"params false", ComboStep{Provider: "a", Params: json.RawMessage(`{"quota_only":false}`)}, false},
		{"plain step", ComboStep{Provider: "a"}, false},
		{"broken params", ComboStep{Provider: "a", Params: json.RawMessage(`{`)}, false},
	}
	for _, c := range cases {
		if got := c.step.QuotaOnly(); got != c.want {
			t.Errorf("%s: QuotaOnly() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestComboStepOnClasses(t *testing.T) {
	if got := (ComboStep{On: []string{"auth", "network"}}).OnClasses(); len(got) != 2 || got[0] != "auth" || got[1] != "network" {
		t.Fatalf("OnClasses = %v, want [auth network]", got)
	}
	if got := (ComboStep{QuotaOnlyFlag: true}).OnClasses(); len(got) != 1 || got[0] != "quota" {
		t.Fatalf("legacy quota-only OnClasses = %v, want [quota]", got)
	}
	if got := (ComboStep{Provider: "a"}).OnClasses(); got != nil {
		t.Fatalf("plain step OnClasses = %v, want nil", got)
	}
	if got := (ComboStep{On: []string{"*"}}).OnClasses(); len(got) != 1 || got[0] != "*" {
		t.Fatalf("wildcard OnClasses = %v, want [*]", got)
	}
}

func TestComboStepKinds(t *testing.T) {
	if !(ComboStep{Kind: "condition", On: []string{"quota"}}).IsCondition() {
		t.Error("condition kind not recognised")
	}
	if !(ComboStep{Kind: "finish"}).IsFinish() {
		t.Error("finish kind not recognised")
	}
	if !(ComboStep{Kind: "start"}).IsStart() {
		t.Error("start kind not recognised")
	}
	if (ComboStep{Kind: "step"}).IsCondition() || (ComboStep{Kind: "step"}).IsFinish() {
		t.Error("plain step must not be condition/finish")
	}
}

func TestIsGraph(t *testing.T) {
	legacy := []ComboStep{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m", QuotaOnlyFlag: true}}
	if IsGraph(legacy) {
		t.Fatal("legacy preset reported as graph")
	}
	graphCases := map[string][]ComboStep{
		"kind":     {{Provider: "a", Kind: "condition"}},
		"on":       {{Provider: "a", On: []string{"quota"}}},
		"to":       {{Provider: "a", To: "x"}},
		"stop":     {{Provider: "a", Stop: true}},
		"id":       {{ID: "x", Provider: "a"}},
		"any step": {{Provider: "a"}, {Provider: "b", Kind: "finish"}},
	}
	for name, steps := range graphCases {
		if !IsGraph(steps) {
			t.Errorf("%s: IsGraph = false, want true", name)
		}
	}
}

func TestUpdateComboPresetNoDuplicate(t *testing.T) {
	s := openTestStore(t)
	id, err := s.SaveComboPreset("duo", "m-a", []ComboStep{{Provider: "a", Model: "m-a"}})
	if err != nil {
		t.Fatalf("SaveComboPreset: %v", err)
	}
	graph := []ComboStep{
		{ID: "s0", Provider: "a", Model: "m-a"},
		{ID: "c1", Kind: "condition", On: []string{"quota"}, To: "s2"},
		{ID: "s2", Provider: "b", Model: "m-b"},
	}
	for i := range 3 {
		if err := s.UpdateComboPreset(id, "graph-preset", "m-a", graph); err != nil {
			t.Fatalf("UpdateComboPreset #%d: %v", i+1, err)
		}
	}
	presets, err := s.ComboPresets()
	if err != nil {
		t.Fatalf("ComboPresets: %v", err)
	}
	if len(presets) != 1 || presets[0].ID != id {
		t.Fatalf("presets = %+v, want single row id=%d", presets, id)
	}
	if presets[0].Name != "graph-preset" {
		t.Fatalf("name = %q, want graph-preset", presets[0].Name)
	}
	steps, err := presets[0].Steps()
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("steps = %+v, want 3 (steps_json refreshed)", steps)
	}
	if !steps[1].IsCondition() || len(steps[1].OnClasses()) != 1 || steps[1].To != "s2" {
		t.Fatalf("condition step = %+v, want on=[quota] to=s2", steps[1])
	}
}

func TestUpdateComboPresetNotFound(t *testing.T) {
	s := openTestStore(t)
	err := s.UpdateComboPreset(999999, "x", "m", []ComboStep{{Provider: "a", Model: "m"}})
	if err == nil {
		t.Fatal("UpdateComboPreset missing id: want error, got nil")
	}
	if !strings.Contains(err.Error(), "999999") {
		t.Fatalf("err %q: want the missing id in the message", err)
	}
}

func TestUpdateComboPresetNilSteps(t *testing.T) {
	s := openTestStore(t)
	id, err := s.SaveComboPreset("legacy", "m", []ComboStep{{Provider: "a", Model: "m"}})
	if err != nil {
		t.Fatalf("SaveComboPreset: %v", err)
	}
	if err := s.UpdateComboPreset(id, "legacy", "m", nil); err != nil {
		t.Fatalf("UpdateComboPreset nil: %v", err)
	}
	p, err := s.ComboPreset(id)
	if err != nil {
		t.Fatalf("ComboPreset: %v", err)
	}
	if strings.TrimSpace(p.StepsJSON) != "[]" {
		t.Fatalf("steps_json = %q, want []", p.StepsJSON)
	}
}
