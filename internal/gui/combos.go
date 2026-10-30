package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ByJotaro/ai-router/internal/events"
	"github.com/ByJotaro/ai-router/internal/store"
	"github.com/ByJotaro/ai-router/internal/stream"
)

// ComboView — пресет комбо-цепочки для UI (шаги — структурированные).
type ComboView struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Model     string            `json:"model"`
	Steps     []store.ComboStep `json:"steps"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
}

// ComboList возвращает все комбо-пресеты (новые сверху).
func (a *App) ComboList() ([]ComboView, error) {
	st, err := a.openStore()
	if err != nil {
		return nil, err
	}
	presets, err := st.ComboPresets()
	if err != nil {
		return nil, err
	}
	out := make([]ComboView, 0, len(presets))
	for _, p := range presets {
		v := ComboView{ID: p.ID, Name: p.Name, Model: p.Model, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
		if steps, err := store.ParseComboSteps(p.StepsJSON); err == nil {
			v.Steps = steps
		}
		out = append(out, v)
	}
	return out, nil
}

// ComboSave сохраняет пресет и возвращает его id. Граф (id/kind/on/to/stop/
// quota_only) проходит через strict-конвертер: прежний lossy-конвертер молча
// выбрасывал узлы графа и сохранял линейку (review 2026-10-17, r11/r22 F1).
func (a *App) ComboSave(name, model string, steps []map[string]any) (int64, error) {
	st, err := a.openStore()
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(name) == "" {
		return 0, fmt.Errorf("имя комбо не может быть пустым")
	}
	comboSteps, err := strictComboStepsFromMaps(steps)
	if err != nil {
		return 0, err
	}
	if err := a.validateComboSteps(comboSteps); err != nil {
		return 0, err
	}
	if strings.TrimSpace(model) == "" && len(comboSteps) > 0 {
		model = comboSteps[0].Model
	}
	return st.SaveComboPreset(strings.TrimSpace(name), model, comboSteps)
}

func (a *App) validateComboSteps(steps []store.ComboStep) error {
	if len(steps) == 0 {
		return fmt.Errorf("комбо не содержит шагов")
	}
	for i, step := range steps {
		// Provider обязателен только у узла, который реально делает попытку
		// вверх. start/finish/condition — структура графа, редактор шлёт их
		// с пустым provider: прежняя проверка отвергала любой граф
		// (review 2026-10-17, r11/r22 F1).
		if !comboStepExecutes(step) {
			continue
		}
		if strings.TrimSpace(step.Provider) == "" {
			return fmt.Errorf("шаг %d: provider обязателен", i+1)
		}
		// Validate against the live router only when the provider exists;
		// presets may reference providers added later.
		if a.router != nil {
			if p := a.router.Provider(step.Provider); p != nil {
				if step.Model != "" && !p.ModelAllowed(step.Model) {
					return fmt.Errorf("шаг %d: модель %q не обслуживается провайдером %q", i+1, step.Model, step.Provider)
				}
			}
		}
	}
	return validateComboGraph(steps)
}

// comboStepExecutes reports whether the node performs an upstream attempt:
// legacy steps (empty kind) and explicit "step" nodes always do, a "start"
// node only when it carries a provider. condition/finish never do.
func comboStepExecutes(s store.ComboStep) bool {
	switch s.Kind {
	case "condition", "finish":
		return false
	case "start":
		return strings.TrimSpace(s.Provider) != ""
	default:
		return true
	}
}

// validateComboGraph проверяет инварианты графа: дубли id, висячие to,
// условие без on, неизвестный kind, отсутствие исполняемых шагов.
// Исполнитель (callComboGraph) дополнительно защищён бюджетом посещений,
// но битый граф должен отсекаться на сохранении.
func validateComboGraph(steps []store.ComboStep) error {
	seen := map[string]bool{}
	// Normalized ids (implicit n<i+1> when empty) decide duplicates: an
	// explicit id equal to a later implicit one used to make byID ambiguous,
	// so a valid `to` could jump to the wrong node (review 2026-10-17, r11 F4).
	norm := make([]string, len(steps))
	for i, st := range steps {
		id := st.ID
		if id == "" {
			id = fmt.Sprintf("n%d", i+1)
		}
		norm[i] = id
		if seen[id] {
			return fmt.Errorf("шаг %d: дублирующийся id %q", i+1, id)
		}
		seen[id] = true
	}
	for i, st := range steps {
		switch st.Kind {
		case "", "start", "step", "finish":
		case "condition":
			if len(st.OnClasses()) == 0 {
				return fmt.Errorf("условие %q: on пуст — маршрутизировать не по чему", st.ID)
			}
		default:
			return fmt.Errorf("шаг %d: неизвестный kind %q", i+1, st.Kind)
		}
	}
	if !hasAnyStep(steps) {
		return fmt.Errorf("граф не содержит ни одного исполняемого шага")
	}
	for i, st := range steps {
		if !st.IsCondition() || st.To == "" {
			continue
		}
		ok := false
		for j := i + 1; j < len(steps); j++ {
			if norm[j] == st.To {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("условие %q: цель %q не найдена впереди по порядку", st.ID, st.To)
		}
	}
	return nil
}

func hasAnyStep(steps []store.ComboStep) bool {
	for _, st := range steps {
		if strings.TrimSpace(st.Provider) != "" {
			return true
		}
	}
	return false
}

// ComboUpdate обновляет пресет (upsert): редактор не должен плодить дубли
// при повторном сохранении — это была причина «SaveComboPreset только INSERT».
func (a *App) ComboUpdate(id int64, name, model string, steps []map[string]any) error {
	st, err := a.openStore()
	if err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("имя комбо не может быть пустым")
	}
	comboSteps, err := strictComboStepsFromMaps(steps)
	if err != nil {
		return err
	}
	if err := a.validateComboSteps(comboSteps); err != nil {
		return err
	}
	if strings.TrimSpace(model) == "" && len(comboSteps) > 0 {
		model = comboSteps[0].Model
	}
	return st.UpdateComboPreset(id, strings.TrimSpace(name), model, comboSteps)
}

// ComboDelete удаляет пресет по id.
func (a *App) ComboDelete(id int64) error {
	st, err := a.openStore()
	if err != nil {
		return err
	}
	return st.DeleteComboPreset(id)
}

// ComboTest выполняет живой вызов комбо через CallCombo: шаги перебираются по
// порядку, первый успешный выигрывает. id > 0 — пресет из БД (модель можно
// переопределить), иначе steps передаются напрямую (несохранённый черновик).
// Результат пишется в request_log/daily_usage с полями combo_name/combo_step_id.
func (a *App) ComboTest(id int64, steps []map[string]any, model string) (*TestResult, error) {
	st, err := a.openStore()
	if err != nil {
		return nil, err
	}
	comboName := ""
	var comboSteps []store.ComboStep
	if id > 0 {
		p, err := st.ComboPreset(id)
		if err != nil {
			return nil, fmt.Errorf("пресет %d: %w", id, err)
		}
		comboName = p.Name
		if model == "" {
			model = p.Model
		}
		comboSteps, err = store.ParseComboSteps(p.StepsJSON)
		if err != nil {
			return nil, fmt.Errorf("шаги пресета %q: %w", p.Name, err)
		}
	} else {
		comboSteps = comboStepsFromMaps(steps)
	}
	if len(comboSteps) == 0 {
		return nil, fmt.Errorf("комбо не содержит шагов")
	}
	if model == "" {
		model = comboSteps[0].Model
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"max_tokens": 16,
		"stream":     false,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	start := time.Now()
	res, err := a.router.CallCombo(ctx, comboName, model, comboSteps, body, false)
	tr := &TestResult{Model: model, ComboName: comboName, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		tr.Error = err.Error()
		a.recordCombo(events.RequestEvent{
			Model: model, RequestedModel: model, ComboName: comboName,
			DurationMS: tr.LatencyMS, Status: "error", Error: err.Error(),
		})
		return tr, nil
	}
	defer res.Resp.Body.Close()
	tr.Provider = res.Provider
	tr.Status = res.Status
	tr.ComboStep = res.ComboStepID
	if res.Model != "" {
		tr.Model = res.Model
	}
	data, _ := io.ReadAll(io.LimitReader(res.Resp.Body, 1<<20))
	if res.Status >= 400 {
		tr.Error = strings.TrimSpace(truncate(string(data), 200))
		a.recordCombo(events.RequestEvent{
			Model: tr.Model, RequestedModel: model, Provider: tr.Provider,
			ConnectionID: res.ConnectionID, ComboName: comboName, ComboStepID: res.ComboStepID,
			ComboPath:  res.ComboPath,
			DurationMS: tr.LatencyMS, Status: "error", HTTPStatus: res.Status, Error: tr.Error,
		})
		return tr, nil
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *stream.Usage `json:"usage"`
	}
	_ = json.Unmarshal(data, &out)
	if len(out.Choices) > 0 {
		tr.Content, _ = out.Choices[0].Message.Content.(string)
	}
	tr.Content = truncate(tr.Content, 200)
	if u, err := json.Marshal(out.Usage); err == nil {
		tr.Usage = string(u)
	}
	ev := events.RequestEvent{
		Model: tr.Model, RequestedModel: model, Provider: tr.Provider,
		ConnectionID: res.ConnectionID, ComboName: comboName, ComboStepID: res.ComboStepID,
		ComboPath:  res.ComboPath,
		DurationMS: tr.LatencyMS, Status: "ok", HTTPStatus: res.Status,
	}
	if out.Usage != nil {
		ev.Tokens = events.Tokens{
			Prompt: out.Usage.PromptTokens, Completion: out.Usage.CompletionTokens,
			CacheRead: out.Usage.CacheRead, CacheWrite: out.Usage.CacheWrite,
			Reasoning: out.Usage.Reasoning, Compressed: out.Usage.Compressed,
		}
	}
	a.recordCombo(ev)
	return tr, nil
}

// recordCombo пишет событие комбо-теста в request_log и daily_usage
// (best-effort): журнал — место, где видны combo_name/combo_step_id.
func (a *App) recordCombo(ev events.RequestEvent) {
	st, err := a.openStore()
	if err != nil {
		return
	}
	if a.ks != nil {
		ev.CostCents = a.ks.CostCents(ev.Model, ev.Tokens.Prompt, ev.Tokens.Completion)
	}
	_ = st.InsertRequestLog(ev, "COMBO", "/combo/test")
	_ = st.IncrDailyUsage(time.Now().UTC().Format("2006-01-02"),
		ev.Tokens.Prompt, ev.Tokens.Completion, ev.Tokens.CacheRead, ev.Tokens.CacheWrite,
		ev.Tokens.Reasoning, ev.Tokens.Compressed, ev.CostCents)
}

// comboStepFromMap конвертирует одну JS-карту редактора в шаг, сохраняя
// граф-поля (id/kind/on/to/stop/quota_only).
func comboStepFromMap(m map[string]any) store.ComboStep {
	var s store.ComboStep
	if v, ok := m["id"].(string); ok {
		s.ID = v
	}
	if v, ok := m["kind"].(string); ok {
		s.Kind = v
	}
	if v, ok := m["provider"].(string); ok {
		s.Provider = v
	}
	if v, ok := m["model"].(string); ok {
		s.Model = v
	}
	if v, ok := m["to"].(string); ok {
		s.To = v
	}
	if v, ok := m["stop"].(bool); ok {
		s.Stop = v
	}
	if v, ok := m["quota_only"].(bool); ok {
		s.QuotaOnlyFlag = v
	}
	if raw, ok := m["on"]; ok {
		if arr, ok := raw.([]any); ok {
			out := make([]string, 0, len(arr))
			for _, c := range arr {
				if cs, ok := c.(string); ok {
					out = append(out, cs)
				}
			}
			s.On = out
		}
	}
	if raw, ok := m["params"].(map[string]any); ok {
		if v, ok := raw["quota_only"].(bool); ok && v {
			s.QuotaOnlyFlag = true
		}
	}
	return s
}

// strictComboStepsFromMaps как comboStepsFromMaps, но отклоняет неизвестные
// ключи и невалидные типы: молчаливое глотание полей редактора теряло
// quota_only при сохранении.
func strictComboStepsFromMaps(maps []map[string]any) ([]store.ComboStep, error) {
	allowed := map[string]bool{"id": true, "kind": true, "provider": true, "model": true,
		"on": true, "to": true, "stop": true, "quota_only": true, "params": true}
	out := make([]store.ComboStep, 0, len(maps))
	for i, m := range maps {
		for k := range m {
			if !allowed[k] {
				return nil, fmt.Errorf("шаг %d: неизвестное поле %q", i+1, k)
			}
		}
		if _, ok := m["on"].(string); ok {
			return nil, fmt.Errorf("шаг %d: on должен быть массивом классов", i+1)
		}
		if raw, ok := m["params"]; ok {
			if _, ok := raw.(map[string]any); !ok {
				return nil, fmt.Errorf("шаг %d: params должен быть объектом", i+1)
			}
		}
		out = append(out, comboStepFromMap(m))
	}
	return out, nil
}

// comboStepsFromMaps конвертирует JS-карты ({provider, model, params}) в шаги.
func comboStepsFromMaps(steps []map[string]any) []store.ComboStep {
	out := make([]store.ComboStep, 0, len(steps))
	for _, m := range steps {
		s := store.ComboStep{Provider: asString(m["provider"]), Model: asString(m["model"])}
		if p, ok := m["params"]; ok {
			if b, err := json.Marshal(p); err == nil {
				s.Params = b
			}
		}
		out = append(out, s)
	}
	return out
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
