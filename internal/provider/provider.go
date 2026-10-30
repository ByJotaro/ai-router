// Package provider models upstream LLM providers parsed from config.
package provider

import (
	"fmt"
	"maps"
	"strings"

	"github.com/ByJotaro/ai-router/internal/config"
)

// Type values.
const (
	TypeOpenAICompatible = "openai-compatible"
	TypeOAuth            = "oauth"
)

// Provider is a resolved upstream endpoint adapter.
type Provider struct {
	Name      string
	Type      string
	BaseURL   string
	APIKeys   []string
	AuthMode  string
	UserAgent string
	// ResponsesAPI: upstream speaks the OpenAI Responses protocol; route
	// requests to /responses and translate both directions (chat in,
	// chat-SSE out).
	ResponsesAPI bool
	// ResponsesModels: per-model override when only some models of this
	// provider speak the Responses protocol.
	ResponsesModels []string

	Models    []string
	DisabledModels []string
	HiddenModels   []string
	QuotaMode      string
	Fallback  []string
	Egress    string
	Transport config.TransportConf
	Request   config.RequestConf
	TimeoutS  int

	Tier         string  // subscription|api|cheap|free ("" → default tier)
	Weight       float64 // routing weight (>1 preferred)
	CostPer1MIn  float64 // USD per 1M input tokens (cost strategy)
	CostPer1MOut float64
	KeySwap      string // round_robin|random|least_used|best_latency ("" → least-used)
	// ErrorPolicy — действие при ошибке: none (не повторять) | retry |
	// retry_proxy (ретрай со сменой прокси — transport берётся заново) |
	// retry_token (ретрай со сменой ключа). Ключ: 401|429|5xx|timeout|network|rate_limit.
	ErrorPolicy map[string]string
	// DisabledKeys — хэши вручную выключенных ключей (из ProviderConf).
	DisabledKeys []string
	// KeyProxies — hash->proxy, привязки прокси к ключам (из ProviderConf).
	KeyProxies map[string]string
	// DefaultKey — хэш приоритетного ключа (из ProviderConf).
	DefaultKey string
	// ModelReasoning — per-model reasoning override (из ProviderConf).
	ModelReasoning map[string]string
}

// ModelAllowed reports whether model is served by this provider. Empty
// Models means the provider serves any requested model.
func (p *Provider) ModelAllowed(model string) bool {
	for _, m := range p.DisabledModels {
		if m == model {
			return false
		}
	}
	if len(p.Models) == 0 {
		return true
	}
	for _, m := range p.Models {
		if m == model {
			return true
		}
	}
	return false
}
// Model aliasing (static): well-known short names rewrite to canonical model
// ids before candidate resolution. Hidden
// models never surface via aliases; retired ids are dropped entirely so a
// stale config/model list cannot revive traffic to a dead integration.
var modelAliasSeed = map[string]string{
	"deepseek-chat":     "deepseek-v4",
	"deepseek-reasoner": "deepseek-v4",
	"gpt-5.6-sol":       "gpt-5.6",
	"muse-spark-1.2":    "muse-spark-1.2-contributor-free",
	"muse-spark-1.3":    "muse-spark-1.3-contributor-free",
}

// hiddenModels never resolve via alias (internal/canary ids).
var hiddenModels = map[string]bool{}

// retiredModels are dead upstream ids: alias + direct requests both fail
// closed with ErrRetired so SyncModels/config cannot resurrect them.
var retiredModels = map[string]bool{
	"gemini-3-pro-preview": true,
}

// ErrRetired reports a request for a retired model id.
var ErrRetired = fmt.Errorf("model retired")

// ResolveAlias rewrites a client model alias to its canonical id.
// Combo-prefixed names ("combo/...") bypass aliasing. Unknown names pass
// through unchanged; retired ids return ErrRetired.
func ResolveAlias(model string) (string, error) {
	if strings.HasPrefix(model, "combo/") {
		return model, nil
	}
	if retiredModels[model] {
		return "", ErrRetired
	}
	if hiddenModels[model] {
		return model, nil
	}
	if canon, ok := modelAliasSeed[model]; ok {
		if retiredModels[canon] {
			return "", ErrRetired
		}
		return canon, nil
	}
	return model, nil
}

// HasKeys reports whether the provider has at least one key. A provider
// without keys may still work if its upstream needs no auth (e.g. local
// gateways).
func (p *Provider) HasKeys() bool { return len(p.APIKeys) > 0 }

func sanitizeUserAgent(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\r' || r == '\n' {
			continue
		}
		if r < 0x20 || r == 0x7F {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 512 {
		out = out[:512]
		out = strings.TrimSpace(out)
	}
	return out
}

// Resolve converts a config entry into a Provider, validating it.
func Resolve(name string, c config.ProviderConf) (*Provider, error) {
	if c.Type == "" {
		c.Type = TypeOpenAICompatible
	}
	switch c.Type {
	case TypeOpenAICompatible, TypeOAuth:
	default:
		return nil, fmt.Errorf("provider %s: unknown type %q", name, c.Type)
	}
	switch c.AuthMode {
	case "", "none", "bearer", "public":
	default:
		return nil, fmt.Errorf("provider %s: unknown auth_mode %q", name, c.AuthMode)
	}
	sanitizedUA := sanitizeUserAgent(c.UserAgent)
	apiKeys := append([]string(nil), c.APIKeys...)
	models := append([]string(nil), c.Models...)
	disModels := append([]string(nil), c.DisabledModels...)
	hidModels := append([]string(nil), c.HiddenModels...)
	fallback := append([]string(nil), c.Fallback...)
	respModels := append([]string(nil), c.ResponsesModels...)
	var ep map[string]string
	if c.ErrorPolicy != nil {
		ep = make(map[string]string, len(c.ErrorPolicy))
		maps.Copy(ep, c.ErrorPolicy)
	}
	transport := c.Transport
	if len(c.Transport.SeedIPs) > 0 {
		transport.SeedIPs = append([]string(nil), c.Transport.SeedIPs...)
	}
	disKeys := append([]string(nil), c.DisabledKeys...)
	var mreas map[string]string
	if c.ModelReasoning != nil {
		mreas = make(map[string]string, len(c.ModelReasoning))
		for k, v := range c.ModelReasoning {
			mreas[k] = v
		}
	}
	var kprox map[string]string
	if c.KeyProxies != nil {
		kprox = make(map[string]string, len(c.KeyProxies))
		for k, v := range c.KeyProxies {
			kprox[k] = v
		}
	}
	p := &Provider{
		Name:            name,
		Type:            c.Type,
		BaseURL:         strings.TrimRight(c.BaseURL, "/"),
		APIKeys:         apiKeys,
		AuthMode:        c.AuthMode,
		UserAgent:       sanitizedUA,
		ResponsesAPI:    c.ResponsesAPI,
		ResponsesModels: respModels,
		Models:          models,
		DisabledModels:  disModels,
		HiddenModels:    hidModels,
		QuotaMode:       c.QuotaMode,
		Fallback:        fallback,
		Egress:          c.Egress,
		Transport:       transport,
		Request:         c.Request,
		TimeoutS:        c.TimeoutS,
		Tier:            c.Tier,
		Weight:          c.Weight,
		CostPer1MIn:     c.CostPer1MIn,
		CostPer1MOut:    c.CostPer1MOut,
		KeySwap:         c.KeySwap,
		ErrorPolicy:     ep,
		DisabledKeys:    disKeys,
		KeyProxies:      kprox,
		DefaultKey:      c.DefaultKey,
		ModelReasoning:  mreas,
	}
	if p.Tier == "" {
		p.Tier = "free"
	}
	if p.Weight <= 0 {
		p.Weight = 1
	}
	return p, nil
}
