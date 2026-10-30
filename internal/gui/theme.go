package gui

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// userPrefsTheme читает user_prefs.theme из конфиг-файла; "" — тема не задана.
// Конфиг редактируем как карту (как GetConfig/SaveConfig), чтобы не трогать
// типизированную структуру ядра — лишние ключи Load игнорирует.
// Legacy mist маппится на nebula (ember с 2026-09-26 — полноценная тема).
func (a *App) userPrefsTheme() (string, error) {
	data, err := os.ReadFile(a.cfgPath)
	if err != nil {
		return "", err
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return "", err
	}
	up, _ := m["user_prefs"].(map[string]any)
	t, _ := up["theme"].(string)
	if t == "mist" {
		return "nebula", nil
	}
	return t, nil
}

// SetTheme сохраняет тему интерфейса (ember|abyss|nebula|mono|gray; legacy mist маппится на nebula).
func (a *App) SetTheme(theme string) error {
	if theme == "mist" {
		theme = "nebula"
	}
	switch theme {
	case "ember", "abyss", "nebula", "mono", "gray":
	default:
		return fmt.Errorf("неизвестная тема: %q", theme)
	}
	data, err := os.ReadFile(a.cfgPath)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return err
	}
	if m == nil {
		m = map[string]any{}
	}
	up, _ := m["user_prefs"].(map[string]any)
	if up == nil {
		up = map[string]any{}
	}
	up["theme"] = theme
	m["user_prefs"] = up
	out, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.cfgPath, out); err != nil {
		return err
	}
	a.log.Printf("theme set: %s", theme)
	return nil
}

// GetTheme возвращает сохранённую тему интерфейса (user_prefs/theme), "" если не задана.
func (a *App) GetTheme() (string, error) {
	return a.userPrefsTheme()
}
