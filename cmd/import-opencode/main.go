// Command import-opencode merges LLM providers and keys into ai-router's
// config.yaml from opencode's auth.json + opencode.jsonc. API-key providers
// become providers.<name>.api_keys[]; oauth providers are reported but not
// imported (they need an endpoint and models, which the config does not carry).
//
// Keys are NEVER printed: the report shows names, key lengths and counts only.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ByJotaro/ai-router/internal/config"
)

// knownBaseURLs maps provider ids to OpenAI-compatible endpoints.
var knownBaseURLs = map[string]string{
	"nvidia": "https://api.nvidia.com/v1", "openrouter": "https://openrouter.ai/api/v1",
	"gitlawb": "https://opengateway.gitlawb.com/v1", "fireworks": "https://api.fireworks.ai/inference/v1",
	"deepseek": "https://api.deepseek.com/v1", "opencode": "https://opencode.ai/inference/v1",
}

type importedProvider struct {
	baseURL string
	models  []string
	keys    map[string]bool // key -> present
	sources []string
}

func (p *importedProvider) addKey(k, source string) {
	k = strings.TrimSpace(k)
	if len(k) < 8 {
		return // stub/placeholder keys
	}
	if p.keys == nil {
		p.keys = map[string]bool{}
	}
	p.keys[k] = true
	p.addSource(source)
}

func (p *importedProvider) addSource(s string) {
	for _, v := range p.sources {
		if v == s {
			return
		}
	}
	p.sources = append(p.sources, s)
}

func (p *importedProvider) addModel(m string) {
	for _, v := range p.models {
		if v == m {
			return
		}
	}
	p.models = append(p.models, m)
}

func (p *importedProvider) keyList() []string {
	out := make([]string, 0, len(p.keys))
	for k := range p.keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func main() {
	cfgPath := flag.String("config", config.ConfigPath(), "config file path")
	flag.Parse()

	provs := map[string]*importedProvider{}
	oauth := map[string]int{} // providers seen with non-api credentials

	// --- (a) opencode ---
	authPath := filepath.Join(home(), ".local", "share", "opencode", "auth.json")
	if data, err := os.ReadFile(authPath); err == nil {
		var auth map[string]struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		}
		if err := json.Unmarshal(data, &auth); err == nil {
			for name, e := range auth {
				k := strings.TrimSpace(e.Key)
				if e.Type != "api" || len(k) < 8 {
					if e.Type != "" && e.Type != "api" {
						oauth[name]++
					}
					continue
				}
				p := provs[name]
				if p == nil {
					p = &importedProvider{}
					provs[name] = p
				}
				p.addKey(k, "opencode")
			}
		}
	}

	ocPath := filepath.Join(home(), ".config", "opencode", "opencode.jsonc")
	if data, err := os.ReadFile(ocPath); err == nil {
		cfg, err := parseOpenCodeConfig(stripJSONC(string(data)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: opencode.jsonc: %v\n", err)
		} else {
			for name, pc := range cfg.Provider {
				p := provs[name]
				if p == nil {
					p = &importedProvider{}
					provs[name] = p
				}
				if pc.Options.BaseURL != "" {
					p.baseURL = pc.Options.BaseURL
				}
				for m := range pc.Models {
					p.addModel(m)
				}
				if pc.Options.APIKey != "" {
					p.addKey(pc.Options.APIKey, "opencode")
				}
				p.addSource("opencode")
			}
		}
	}

	// Apply known base URLs for providers that lack one.
	for name, p := range provs {
		if p.baseURL == "" {
			p.baseURL = knownBaseURLs[name]
		}
	}

	// --- merge into config ---
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatalf("config: %v", err)
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.ProviderConf{}
	}
	if _, ok := provs["opencode"]; !ok {
		provs["opencode"] = &importedProvider{baseURL: knownBaseURLs["opencode"], models: []string{"auto"}, sources: []string{"catalog:no-auth"}}
	}
	names := make([]string, 0, len(provs))
	for name := range provs {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Println("== imported providers ==")
	for _, name := range names {
		p := provs[name]
		existing := cfg.Providers[name]
		existing.Type = "openai-compatible"
		if p.baseURL != "" {
			existing.BaseURL = p.baseURL
		}
		if len(p.models) > 0 {
			existing.Models = dedupe(append(existing.Models, p.models...))
		}
		existing.APIKeys = mergeKeys(existing.APIKeys, p.keyList())
		cfg.Providers[name] = existing

		base := p.baseURL
		if base == "" {
			base = "(заполните base_url)"
		}
		var lens []string
		for _, k := range p.keyList() {
			lens = append(lens, fmt.Sprintf("%d", len(k)))
		}
		fmt.Printf("  %-20s base_url=%s keys=%d [%s] models=%v sources=%s\n",
			name, base, len(p.keyList()), strings.Join(lens, ","), p.models, strings.Join(p.sources, "+"))
	}

	if err := config.Save(cfg, *cfgPath); err != nil {
		fatalf("save: %v", err)
	}
	fmt.Printf("== saved -> %s\n", *cfgPath)

	if len(oauth) > 0 {
		fmt.Println("== skipped (oauth, no endpoint/models in config) ==")
		for name := range oauth {
			fmt.Printf("  %-20s connections=%d\n", name, oauth[name])
		}
	}
}

// --- opencode.jsonc parsing ---

type openCodeCfg struct {
	Provider map[string]struct {
		Models map[string]struct {
			Name string `json:"name"`
		} `json:"models"`
		Options struct {
			BaseURL string `json:"baseURL"`
			APIKey  string `json:"apiKey"`
		} `json:"options"`
	} `json:"provider"`
}

func parseOpenCodeConfig(data string) (*openCodeCfg, error) {
	var c openCodeCfg
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// stripJSONC removes // line comments and trailing commas (JSONC subset).
func stripJSONC(s string) string {
	var out strings.Builder
	inStr := false
	prev := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr:
			out.WriteByte(c)
			if c == '"' && prev != '\\' {
				inStr = false
			}
		case c == '"':
			inStr = true
			out.WriteByte(c)
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
		case c == ',':
			// lookahead for closing bracket/brace -> drop comma
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue // trailing comma
			}
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
		prev = c
	}
	return out.String()
}

func mergeKeys(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range append(append([]string{}, a...), b...) {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

func dedupe(a []string) []string {
	return mergeKeys(nil, a)
}

func home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("USERPROFILE")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "import-opencode: "+format+"\n", args...)
	os.Exit(1)
}
