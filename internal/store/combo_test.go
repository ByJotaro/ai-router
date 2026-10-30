package store

import "testing"

func TestComboPresetCRUD(t *testing.T) {
	s := openTestStore(t)

	id, err := s.SaveComboPreset("fast", "gpt-4o-mini", []ComboStep{
		{Provider: "openai", Model: "gpt-4o-mini"},
		{Provider: "deepseek", Model: "deepseek-chat"},
	})
	if err != nil {
		t.Fatalf("SaveComboPreset: %v", err)
	}
	if id <= 0 {
		t.Fatalf("SaveComboPreset id = %d, want > 0", id)
	}

	presets, err := s.ComboPresets()
	if err != nil {
		t.Fatalf("ComboPresets: %v", err)
	}
	if len(presets) != 1 || presets[0].Name != "fast" {
		t.Fatalf("ComboPresets = %+v", presets)
	}

	p, err := s.ComboPreset(id)
	if err != nil {
		t.Fatalf("ComboPreset: %v", err)
	}
	if p.Model != "gpt-4o-mini" || p.UpdatedAt == "" {
		t.Fatalf("ComboPreset = %+v", p)
	}
	steps, err := p.Steps()
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 2 || steps[0].Provider != "openai" || steps[1].Model != "deepseek-chat" {
		t.Fatalf("steps = %+v", steps)
	}

	if err := s.DeleteComboPreset(id); err != nil {
		t.Fatalf("DeleteComboPreset: %v", err)
	}
	presets, err = s.ComboPresets()
	if err != nil {
		t.Fatalf("ComboPresets after delete: %v", err)
	}
	if len(presets) != 0 {
		t.Fatalf("presets after delete = %d, want 0", len(presets))
	}
}
func TestDeleteComboPresetNotFound(t *testing.T) {
	s := openTestStore(t)
	if err := s.DeleteComboPreset(999999); err == nil {
		t.Fatal("DeleteComboPreset missing id: want error, got nil")
	}
}

func TestSaveComboPresetNilSteps(t *testing.T) {
	s := openTestStore(t)
	id, err := s.SaveComboPreset("empty", "", nil)
	if err != nil {
		t.Fatalf("SaveComboPreset: %v", err)
	}
	p, err := s.ComboPreset(id)
	if err != nil {
		t.Fatalf("ComboPreset: %v", err)
	}
	steps, err := p.Steps()
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %+v, want empty", steps)
	}
}

func TestParseComboStepsEmpty(t *testing.T) {
	steps, err := ParseComboSteps("")
	if err != nil {
		t.Fatalf("ParseComboSteps(\"\"): %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %+v, want empty", steps)
	}
}
