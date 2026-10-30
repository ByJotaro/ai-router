package gui

import (
	"strconv"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/opencode"
	"gopkg.in/yaml.v3"
)

// SaveResult — ответ на операции с конфигом.
type SaveResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// GetConfig returns a redacted configuration map. Secrets are deliberately
// omitted from the Wails boundary; they can only be updated by dedicated
// write-only actions.
func (a *App) GetConfig() (map[string]any, error) {
	c, err := config.Load(a.cfgPath)
	if err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	redactConfigSecrets(m)
	return m, nil
}

// redactConfigSecrets strips values GetConfig must never hand to the
// renderer: admin/hub/egress/provider-transport tokens, provider API keys,
// and virtual-key secrets. SaveConfig restores whichever of these the GUI
// left unchanged (see the "blank means unchanged" comment there).
func redactConfigSecrets(m map[string]any) {
	delete(m, "admin_token")
	if hub, ok := m["hub"].(map[string]any); ok {
		delete(hub, "token")
	}
	if egress, ok := m["egress"].(map[string]any); ok {
		delete(egress, "token")
	}
	if ps, ok := m["providers"].(map[string]any); ok {
		for _, raw := range ps {
			if p, ok := raw.(map[string]any); ok {
				delete(p, "api_keys")
				if tr, ok := p["transport"].(map[string]any); ok {
					delete(tr, "token")
				}
			}
		}
	}
	if ks, ok := m["keys"].([]any); ok {
		for _, raw := range ks {
			if k, ok := raw.(map[string]any); ok {
				delete(k, "key")
			}
		}
	}
}

// OpenCodeConfigMerge returns a provider fragment merged
// with an existing OpenCode JSON document. It never returns or logs secrets.
func (a *App) OpenCodeConfigMerge(existing string, models []string) (string, error) {
	base := "http://" + a.cfg.Server.Host + ":" + strconv.Itoa(a.cfg.Server.Port)
	out, err := opencode.MergeConfig([]byte(existing), base, "", models)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// SaveConfig persists a redacted GUI edit but never claims that a separate
// daemon applied it. Secrets omitted by GetConfig are preserved from disk.
func (a *App) SaveConfig(cfg map[string]any) (*SaveResult, error) {
	if cfg == nil {
		return &SaveResult{OK: false, Message: "пустой конфиг"}, nil
	}
	current, err := config.Load(a.cfgPath)
	if err != nil {
		return &SaveResult{OK: false, Message: "чтение: " + err.Error()}, nil
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return &SaveResult{OK: false, Message: "yaml: " + err.Error()}, nil
	}
	var next config.Config
	if err := yaml.Unmarshal(data, &next); err != nil {
		return &SaveResult{OK: false, Message: "невалидный конфиг: " + err.Error()}, nil
	}
	// GetConfig redacts secrets, so an empty field here means "unchanged";
	// a non-empty value is a deliberate edit and must be written through.
	if next.AdminToken == "" {
		next.AdminToken = current.AdminToken
	}
	if next.Hub.Token == "" {
		next.Hub.Token = current.Hub.Token
	}
	if next.Egress.Token == "" {
		next.Egress.Token = current.Egress.Token
	}
	for name, old := range current.Providers {
		p, ok := next.Providers[name]
		if !ok {
			continue
		}
		if len(p.APIKeys) == 0 {
			p.APIKeys = old.APIKeys
		}
		if p.Transport.Token == "" {
			p.Transport.Token = old.Transport.Token
		}
		next.Providers[name] = p
	}
	oldKeyByName := make(map[string]string, len(current.Keys))
	for _, k := range current.Keys {
		oldKeyByName[k.Name] = k.Key
	}
	for i, k := range next.Keys {
		if k.Key == "" {
			next.Keys[i].Key = oldKeyByName[k.Name]
		}
	}
	if err := config.Save(&next, a.cfgPath); err != nil {
		return &SaveResult{OK: false, Message: "запись: " + err.Error()}, nil
	}
	return &SaveResult{OK: true, Message: "конфиг сохранён. Для применения перезапустите ai-router."}, nil
}

// ReloadConfig is intentionally truthful: this GUI does not own a daemon.
func (a *App) ReloadConfig() (*SaveResult, error) {
	return &SaveResult{OK: false, Message: "GUI не применяет конфиг к отдельному ai-router. Перезапустите daemon."}, nil
}
