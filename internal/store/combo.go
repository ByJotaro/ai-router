package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ComboStep is one step of a combo chain. Legacy presets carry only
// provider+model (Kind empty ⇒ step). Graph presets add the routing model:
// kind start|step|condition|finish, condition routes by error class (on),
// jumps forward (to) or terminates the cascade (stop).
type ComboStep struct {
	ID       string `json:"id,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// On lists error classes the step/condition reacts to (quota, auth,
	// network, gate_terminal, pool_exhausted, "*" for any). For a step it
	// is the on-gate (quota-only legacy form); for a condition it is the
	// routing predicate.
	On []string `json:"on,omitempty"`
	// To is the id of the step a matching condition jumps to. Only forward
	// jumps are valid (a back edge is a cycle).
	To   string `json:"to,omitempty"`
	Stop bool   `json:"stop,omitempty"`
	// Params are kept for future phases and for legacy flag storage.
	Params json.RawMessage `json:"params,omitempty"`
	// QuotaOnlyFlag is the legacy quota-gate flag (Omni
	// fallbackOnlyOnQuotaExhaustion parity). Stored inside Params as
	// {"quota_only":true}; the accessor reads both the field and Params
	// so old presets keep working.
	QuotaOnlyFlag bool `json:"quota_only,omitempty"`
}

// QuotaOnly reports whether the step is quota-gated. A step with an on-gate
// of exactly [quota] is equivalent to the legacy quota_only flag.
func (s ComboStep) QuotaOnly() bool {
	if s.QuotaOnlyFlag {
		return true
	}
	if len(s.On) == 1 && s.On[0] == "quota" {
		return true
	}
	if len(s.Params) == 0 {
		return false
	}
	var p struct {
		QuotaOnly bool `json:"quota_only"`
	}
	if err := json.Unmarshal(s.Params, &p); err != nil {
		return false
	}
	return p.QuotaOnly
}

// OnClasses reports the classes this step/condition reacts to. A legacy
// quota-only step reports ["quota"] so the on-gate keeps its meaning.
func (s ComboStep) OnClasses() []string {
	if len(s.On) > 0 {
		return s.On
	}
	if s.QuotaOnly() {
		return []string{"quota"}
	}
	return nil
}

// IsCondition reports whether the step is a routing node.
func (s ComboStep) IsCondition() bool { return s.Kind == "condition" }

// IsFinish reports whether the step terminates the walk explicitly.
func (s ComboStep) IsFinish() bool { return s.Kind == "finish" }

// IsStart reports whether the step is the entry node (carries the model).
func (s ComboStep) IsStart() bool { return s.Kind == "start" }

// IsGraph reports whether any step carries graph fields; presets without
// them run the legacy linear path (CallCombo) unchanged.
func IsGraph(steps []ComboStep) bool {
	for _, s := range steps {
		if s.Kind != "" || len(s.On) > 0 || s.To != "" || s.Stop || s.ID != "" {
			return true
		}
	}
	return false
}

// ComboPreset is a stored combo chain (combo_presets row).
type ComboPreset struct {
	ID        int64
	Name      string
	Model     string
	StepsJSON string
	CreatedAt string
	UpdatedAt string
}

// Steps parses the preset's steps_json into structured steps.
func (p ComboPreset) Steps() ([]ComboStep, error) { return ParseComboSteps(p.StepsJSON) }

// ParseComboSteps decodes stored steps JSON; empty input yields an empty slice.
func ParseComboSteps(raw string) ([]ComboStep, error) {
	if strings.TrimSpace(raw) == "" {
		return []ComboStep{}, nil
	}
	var steps []ComboStep
	if err := json.Unmarshal([]byte(raw), &steps); err != nil {
		return nil, err
	}
	return steps, nil
}

const comboPresetSelect = `SELECT id, name, model, steps_json, created_at, updated_at FROM combo_presets`

func scanComboPreset(row interface{ Scan(...any) error }) (ComboPreset, error) {
	var p ComboPreset
	err := row.Scan(&p.ID, &p.Name, &p.Model, &p.StepsJSON, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// ComboPresets returns all combo presets, newest first.
func (s *Store) ComboPresets() ([]ComboPreset, error) {
	rows, err := s.db.Query(comboPresetSelect + ` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ComboPreset{}
	for rows.Next() {
		p, err := scanComboPreset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ComboPreset returns one preset by id.
func (s *Store) ComboPreset(id int64) (ComboPreset, error) {
	return scanComboPreset(s.db.QueryRow(comboPresetSelect+` WHERE id=?`, id))
}

// SaveComboPreset inserts a preset and returns its new id. Steps are stored
// as JSON in steps_json; a nil steps slice is stored as "[]".
func (s *Store) SaveComboPreset(name, model string, steps []ComboStep) (int64, error) {
	data := []byte("[]")
	if steps != nil {
		var err error
		data, err = json.Marshal(steps)
		if err != nil {
			return 0, err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		`INSERT INTO combo_presets(name, model, steps_json, created_at, updated_at) VALUES(?,?,?,?,?)`,
		name, model, string(data), now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateComboPreset updates an existing preset (upsert by id) so the editor
// never duplicates rows on repeated saves.
func (s *Store) UpdateComboPreset(id int64, name, model string, steps []ComboStep) error {
	data := []byte("[]")
	if steps != nil {
		var err error
		data, err = json.Marshal(steps)
		if err != nil {
			return err
		}
	}
	res, err := s.db.Exec(
		`UPDATE combo_presets SET name=?, model=?, steps_json=?, updated_at=? WHERE id=?`,
		name, model, string(data), time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("пресет %d не найден", id)
	}
	return nil
}

// DeleteComboPreset removes a preset by id.
// При вводе default_combo перед DELETE проверить ссылку (сейчас её нет — задел).
func (s *Store) DeleteComboPreset(id int64) error {
	res, err := s.db.Exec(`DELETE FROM combo_presets WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("пресет %d не найден", id)
	}
	return nil
}
