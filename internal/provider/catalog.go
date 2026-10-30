package provider

import "strings"

// CatalogEntry describes a provider that may be shown before it is routable.
type CatalogEntry struct {
	ID             string
	Name           string
	AuthKind       string // no-auth | api-key | oauth | unknown
	BaseURL        string
	ModelDiscovery string
	AuthHint       string
}

var catalog = map[string]CatalogEntry{
	"opencode": {
		ID: "opencode", Name: "OpenCode Keyless", AuthKind: "no-auth",
		BaseURL: "https://opencode.ai/inference/openai/v1", ModelDiscovery: "catalog",
		AuthHint: "Без API key (keyless). Обязателен User-Agent opencode; используется IP-ротация.",
	},
	"opencode-go": {
		ID: "opencode-go", Name: "OpenCode Go", AuthKind: "api-key",
		ModelDiscovery: "remote", AuthHint: "API key или connection OpenCode Go.",
	},
	"deepseek": {
		ID: "deepseek", Name: "DeepSeek", AuthKind: "api-key",
		BaseURL: "https://api.deepseek.com/v1", ModelDiscovery: "remote",
		AuthHint: "Bearer API key.",
	},
}

func Catalog() []CatalogEntry {
	out := make([]CatalogEntry, 0, len(catalog))
	for _, v := range catalog {
		out = append(out, v)
	}
	return out
}

func CatalogEntryByID(id string) (CatalogEntry, bool) {
	v, ok := catalog[strings.TrimSpace(id)]
	return v, ok
}
