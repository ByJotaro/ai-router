package gui

import (
	"sort"
	"time"

	"github.com/ByJotaro/ai-router/internal/pool"
	"github.com/ByJotaro/ai-router/internal/provider"
)

// EndpointView is the safe runtime/catalog contract exposed to the GUI.
type EndpointView struct {
	Name            string            `json:"name"`
	Type            string            `json:"type"`
	BaseURL         string            `json:"base_url"`
	Egress          string            `json:"egress"`
	EgressMode      string            `json:"egress_mode"`
	TransportReady  bool              `json:"transport_ready"`
	Models          []string          `json:"models"`
	DisabledModels  []string          `json:"disabled_models,omitempty"`
	HiddenModels    []string          `json:"hidden_models,omitempty"`
	QuotaMode       string            `json:"quota_mode,omitempty"`
	Wildcard        bool              `json:"wildcard"`
	Catalog         bool              `json:"catalog"`
	AuthKind        string            `json:"auth_kind"`
	AuthHint        string            `json:"auth_hint"`
	Status          string            `json:"status"`
	Fallback        []string          `json:"fallback"`
	TimeoutS        int               `json:"timeout_s"`
	Tier            string            `json:"tier"`
	KeySwap         string            `json:"key_swap"`
	ErrorPolicy     map[string]string `json:"error_policy,omitempty"`
	AuthMode        string            `json:"auth_mode,omitempty"`
	UserAgent       string            `json:"user_agent,omitempty"`
	ResponsesModels []string          `json:"responses_models,omitempty"`
	Transport       map[string]any    `json:"transport,omitempty"`
	ModelReasoning  map[string]string `json:"model_reasoning,omitempty"`
	Keys            int               `json:"keys"`
	KeysHealthy     int               `json:"keys_healthy"`
	KeysBackoff     int               `json:"keys_backoff"`
	KeysPct         float64           `json:"keys_pct"`
	BackoffMaxS     int64             `json:"backoff_max_s"`
	LastErr         string            `json:"last_err"`
	Breaker         string            `json:"breaker"`
	BreakerUntilS   int64             `json:"breaker_until_s"`
	BreakerFailures int               `json:"breaker_failures"`
}

func (a *App) Endpoints() ([]EndpointView, error) {
	now := time.Now()
	byName := map[string]EndpointView{}
	for _, name := range a.router.Providers() {
		ev := EndpointView{Name: name, Status: "connected"}
		// A config provider with no keys, no keyless auth and no pool
		// traffic is effectively unconfigured: group it into the
		// collapsed "Не настроено" section instead of the active grid,
		// so the tab shows what the user actually uses on top.
		unconfigured := false
		if p := a.router.Provider(name); p != nil {
			ev.Type, ev.BaseURL, ev.Models = p.Type, p.BaseURL, p.Models
			ev.DisabledModels, ev.HiddenModels, ev.QuotaMode = p.DisabledModels, p.HiddenModels, p.QuotaMode
			ev.Wildcard, ev.Fallback, ev.Egress, ev.TimeoutS = len(p.Models) == 0, p.Fallback, p.Egress, p.TimeoutS
			ev.Tier, ev.KeySwap, ev.ErrorPolicy, ev.Keys = p.Tier, p.KeySwap, p.ErrorPolicy, len(p.APIKeys)
			ev.AuthMode, ev.UserAgent, ev.ResponsesModels = p.AuthMode, p.UserAgent, p.ResponsesModels
			ev.ModelReasoning = p.ModelReasoning
			ev.Transport = map[string]any{"mode": p.Transport.Mode, "fanout": p.Transport.Fanout, "rounds": p.Transport.Rounds, "cooldown_s": p.Transport.CooldownS, "api_url": p.Transport.APIURL, "direct_fallback": p.Transport.DirectFallback, "allow_home_ip_direct": p.Transport.AllowHomeIPDirect, "jitter_tail": p.Transport.JitterTail}
			if p.AuthMode == "none" {
				ev.AuthKind = "no-auth"
			} else if len(p.APIKeys) > 0 {
				ev.AuthKind = "api-key"
			}
		}
		if ce, ok := provider.CatalogEntryByID(name); ok {
			ev.AuthKind, ev.AuthHint = ce.AuthKind, ce.AuthHint
			if ev.BaseURL == "" {
				ev.BaseURL = ce.BaseURL
			}
		}
		if pl := a.router.Pool(name); pl != nil {
			ev.KeysHealthy, ev.KeysBackoff, ev.BackoffMaxS = poolHealth(pl.Snapshot(), now)
		}
		if ev.Keys > 0 {
			ev.KeysPct = float64(ev.KeysHealthy) / float64(ev.Keys)
		} else {
			ev.KeysPct = -1
		}
		if br, ok := a.router.BreakerState()[name]; ok {
			ev.Breaker, ev.BreakerFailures, ev.LastErr = string(br.State), br.Failures, br.LastError
			if !br.OpenedUntil.IsZero() {
				ev.BreakerUntilS = int64(br.OpenedUntil.Sub(now).Seconds())
			}
		}
		// api-key providers with zero keys AND providers without a usable
		// base URL (config stubs that can never serve a request) belong to
		// the collapsed "Не настроено" block, not the active grid.
		if ev.AuthKind == "" || ev.AuthKind == "api-key" {
			unconfigured = ev.Keys == 0 || ev.BaseURL == ""
		}
		if unconfigured {
			ev.Catalog, ev.Status, ev.KeysPct = true, "unconfigured", -1
		}
		byName[name] = ev
	}
	for _, ce := range provider.Catalog() {
		if _, ok := byName[ce.ID]; ok {
			continue
		}
		byName[ce.ID] = EndpointView{Name: ce.ID, Type: "catalog", BaseURL: ce.BaseURL, Models: nil, Wildcard: true, Catalog: true, AuthKind: ce.AuthKind, AuthHint: ce.AuthHint, Status: "unconfigured", KeysPct: -1}
	}
	out := make([]EndpointView, 0, len(byName))
	for _, ev := range byName {
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return a.decorateEgress(out), nil
}

// decorateEgress resolves the per-provider egress mode from the global pool
// config + the provider's static egress URL.
// decorateEgress resolves the effective transport mode and readiness.
func (a *App) decorateEgress(eps []EndpointView) []EndpointView {
	poolByName := map[string]string{}
	if a.cfg != nil {
		for name, pc := range a.cfg.Providers {
			switch pc.Transport.Mode {
			case "pool", "warp", "iprotate":
				poolByName[name] = pc.Transport.Mode
			}
		}
	}
	hubOn := a.cfg != nil && a.cfg.Egress.Enabled && a.cfg.Egress.BaseURL != ""
	for i := range eps {
		if m, ok := poolByName[eps[i].Name]; ok {
			eps[i].EgressMode = m
			eps[i].TransportReady = true
			continue
		}
		switch {
		case hubOn:
			eps[i].EgressMode = "pool"
			eps[i].TransportReady = a.proxies != nil
		case eps[i].Egress != "":
			eps[i].EgressMode = "static"
			eps[i].TransportReady = true
		default:
			eps[i].EgressMode = "off"
			eps[i].TransportReady = true
		}
	}
	return eps
}

// poolHealth сворачивает снапшот пула: здоровые, в бэкоффе/отключённые,
// максимальный остаток backoff/rate-limit (секунды).
func poolHealth(snap []pool.Key, now time.Time) (healthy, backoff int, maxBackoffS int64) {
	for _, k := range snap {
		if k.Disabled {
			backoff++
			continue
		}
		if now.Before(k.RateLimitedUntil) {
			backoff++
			rem := int64(k.RateLimitedUntil.Sub(now).Seconds())
			if rem > maxBackoffS {
				maxBackoffS = rem
			}
			continue
		}
		healthy++
	}
	return
}

// BreakerView — строка таблицы брейкеров.
type BreakerView struct {
	Provider            string `json:"provider"`
	State               string `json:"state"`
	OpenedUntilS        int64  `json:"opened_until_s"`
	Failures            int    `json:"failures"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastError           string `json:"last_error"`
}

func (a *App) Breakers() ([]BreakerView, error) {
	now := time.Now()
	out := make([]BreakerView, 0, len(a.router.BreakerState()))
	for name, r := range a.router.BreakerState() {
		bv := BreakerView{
			Provider:            name,
			State:               string(r.State),
			Failures:            r.Failures,
			ConsecutiveFailures: r.ConsecutiveFailures,
			LastError:           r.LastError,
		}
		if !r.OpenedUntil.IsZero() {
			bv.OpenedUntilS = int64(r.OpenedUntil.Sub(now).Seconds())
		}
		out = append(out, bv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out, nil
}

// LockoutView — строка таблицы lockout моделей.
type LockoutView struct {
	Model    string `json:"model"`
	Locked   bool   `json:"locked"`
	UntilS   int64  `json:"until_s"`
	Failures int    `json:"failures"`
	LastErr  string `json:"last_err"`
}

func (a *App) Lockouts() ([]LockoutView, error) {
	now := time.Now()
	out := make([]LockoutView, 0, len(a.router.LockoutState()))
	for model, st := range a.router.LockoutState() {
		lv := LockoutView{Model: model, Locked: !st.LockedUntil.IsZero(), Failures: st.Failures, LastErr: st.LastErr}
		if lv.Locked {
			lv.UntilS = int64(st.LockedUntil.Sub(now).Seconds())
			if lv.UntilS < 0 {
				lv.UntilS = 0
			}
		}
		out = append(out, lv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}
