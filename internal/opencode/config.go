package opencode

import (
	"encoding/json"
	"strings"
)

func NormalizeBaseURL(v string) string {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	v = strings.TrimSuffix(v, "/v1")
	return v + "/v1"
}

func MergeConfig(existing []byte, baseURL, apiKey string, models []string) ([]byte, error) {
	var doc map[string]any
	if len(strings.TrimSpace(string(existing))) > 0 && json.Unmarshal(existing, &doc) == nil {
	} else {
		doc = map[string]any{}
	}
	if doc["$schema"] == nil {
		doc["$schema"] = "https://opencode.ai/config.json"
	}
	providers, _ := doc["provider"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	mr := map[string]any{}
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m != "" {
			mr[m] = map[string]any{"name": m}
		}
	}
	if len(mr) == 0 {
		mr["auto"] = map[string]any{"name": "Auto-Combo"}
	}
	providers["ai-router"] = map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "ai-router", "options": map[string]any{"baseURL": NormalizeBaseURL(baseURL), "apiKey": func() string {
		if strings.TrimSpace(apiKey) != "" {
			return apiKey
		}
		return "sk_ai_router"
	}()}, "models": mr}
	doc["provider"] = providers
	return json.MarshalIndent(doc, "", "  ")
}
