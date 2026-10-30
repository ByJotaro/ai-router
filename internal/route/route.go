// Package route routes model requests to provider key-pools with fallback
// chains, tier cascades, circuit breakers, model lockout, per-provider
// timeouts and optional egress proxies.
package route

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

import (
	"github.com/ByJotaro/ai-router/internal/breaker"
	"github.com/ByJotaro/ai-router/internal/buntransport"
	"github.com/ByJotaro/ai-router/internal/crypto"
	"github.com/ByJotaro/ai-router/internal/lockout"
	"github.com/ByJotaro/ai-router/internal/pool"
	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/proxypool"
	"github.com/ByJotaro/ai-router/internal/routing"
	"github.com/ByJotaro/ai-router/internal/store"
)

// EventFunc receives route events (used by hubmod to emit ai.fallback).
type EventFunc func(typ string, payload any)

// Router resolves models to providers and executes requests.
type Router struct {
	mu             sync.RWMutex
	providers      map[string]*provider.Provider
	providersOrder []string // declaration order from Config.ProvidersOrder; empty -> sorted fallback
	pools          map[string]*pool.Pool
	clients        map[string]*http.Client
	breakers       map[string]*breaker.Breaker
	strategy       routing.Strategy
	tiers          []string
	lock           *lockout.Tracker
	// lastIdentGen is the identity generation the current per-model failure
	// verdicts were recorded against (see syncIdentityGen).
	lastIdentGen atomic.Uint64
	poolCfg      pool.Config
	breakerCfg   breaker.Config
	persist      func(store.BreakerRow) error
	poolPersist  func(pool.KeyState) error
	encrypt      func(string) string
	decrypt      func(string) string
	OnEvent      EventFunc

	// transport is an optional independent outbound transport capability.
	// nil means direct/static-provider transport.
	transport TransportProvider
}

// Transport exposes the outbound transport for lifecycle ops (graceful
// shutdown flushes StateFlusher transports synchronously).
func (r *Router) Transport() TransportProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.transport
}

// Options carries resilience/routing tuning from config.
type Options struct {
	Strategy       string
	Tiers          []string
	Breaker        breaker.Config
	Cooldown       pool.Config
	Lockout        lockout.Config
	Persist        func(store.BreakerRow) error
	PoolPersist    func(pool.KeyState) error
	Encrypt        func(string) string
	Decrypt        func(string) string
	Transport      TransportProvider
	ProvidersOrder []string
}

// Option mutates Options.
type Option func(*Options)

// WithStrategy selects the routing strategy name.
func WithStrategy(name string) Option { return func(o *Options) { o.Strategy = name } }

// WithTiers sets the tier cascade order.
func WithTiers(t []string) Option { return func(o *Options) { o.Tiers = t } }

// WithBreaker sets circuit-breaker tuning.
func WithBreaker(cfg breaker.Config) Option { return func(o *Options) { o.Breaker = cfg } }

// WithCooldown sets key-pool backoff tuning.
func WithCooldown(cfg pool.Config) Option { return func(o *Options) { o.Cooldown = cfg } }

// WithLockout sets model-lockout tuning.
func WithLockout(cfg lockout.Config) Option { return func(o *Options) { o.Lockout = cfg } }

// WithPersist sets the breaker persistence callback.
func WithPersist(f func(store.BreakerRow) error) Option {
	return func(o *Options) { o.Persist = f }
}

// WithPoolPersist sets the key-pool health persistence callback.
func WithPoolPersist(f func(pool.KeyState) error) Option {
	return func(o *Options) { o.PoolPersist = f }
}

// WithEncrypt sets the at-rest key encryption func for pool persistence.
func WithEncrypt(f func(string) string) Option { return func(o *Options) { o.Encrypt = f } }

// WithDecrypt sets the at-rest key decryption func for config/DB values.
func WithDecrypt(f func(string) string) Option { return func(o *Options) { o.Decrypt = f } }

// WithProvidersOrder sets the declaration order captured from YAML
// (Config.ProvidersOrder). It drives deterministic priority when multiple
// providers share a tier.
func WithProvidersOrder(order []string) Option {
	return func(o *Options) { o.ProvidersOrder = append([]string(nil), order...) }
}

// WithTransport attaches an optional outbound transport capability. When nil,
// the router uses direct or provider-static proxy transport.
func WithTransport(tp TransportProvider) Option { return func(o *Options) { o.Transport = tp } }

// New creates a router from resolved providers.
func New(provs map[string]*provider.Provider, opts ...Option) *Router {
	o := Options{}
	for _, f := range opts {
		f(&o)
	}
	r := &Router{
		providers: map[string]*provider.Provider{},
		// Empty: Add() appends in call order below. Pre-seeding from
		// o.ProvidersOrder here duplicated every entry (map starts empty,
		// exists==false for all) and randomized Priority (last-write-wins).
		providersOrder: nil,
		pools:          map[string]*pool.Pool{},
		clients:        map[string]*http.Client{},
		breakers:       map[string]*breaker.Breaker{},
		strategy:       routing.For(o.Strategy),
		lock:           lockout.New(o.Lockout),
		poolCfg:        o.Cooldown,
		breakerCfg:     o.Breaker,
		persist:        o.Persist,
		poolPersist:    o.PoolPersist,
		encrypt:        o.Encrypt,
		decrypt:        o.Decrypt,
		transport:      o.Transport,
	}
	if r.poolCfg.DisableAfter <= 0 {
		r.poolCfg.DisableAfter = 3
	}
	if r.breakerCfg.MaxFailures == 0 {
		r.breakerCfg = breaker.Config{MaxFailures: 5, Recovery: 30 * time.Second, HalfOpenSuccesses: 2}
	}
	if len(o.Tiers) > 0 {
		r.tiers = o.Tiers
	} else {
		r.tiers = []string{"subscription", "api", "cheap", "free"}
	}
	// Registration honors YAML declaration order (o.ProvidersOrder): Add()
	// appends in call order, so iterate names — never the map (random).
	// providersOrder starts empty above, so no duplication is possible.
	if len(o.ProvidersOrder) > 0 {
		for _, n := range o.ProvidersOrder {
			if p, ok := provs[n]; ok {
				r.Add(p)
			}
		}
		// Runtime-added providers not in the recorded order (tests).
		for n, p := range provs {
			if _, exists := r.providers[n]; !exists {
				r.Add(p)
			}
		}
	} else if len(provs) > 0 {
		names := make([]string, 0, len(provs))
		for n := range provs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			r.Add(provs[n])
		}
	}
	return r
}

// Add registers (or replaces) a provider and its pool.
func (r *Router) Add(p *provider.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[p.Name]; !exists {
		r.providersOrder = append(r.providersOrder, p.Name)
	}
	r.providers[p.Name] = p
	cfg := r.poolCfg
	cfg.Persist = r.poolPersist
	cfg.Encrypt = r.encrypt
	switch p.KeySwap {
	case "round-robin", "round_robin":
		cfg.Mode = "round-robin"
	case "random":
		cfg.Mode = "random"
	default: // least_used / пусто
		cfg.Mode = "least-used"
	}
	keys := p.APIKeys
	if r.decrypt != nil {
		keys = make([]string, len(p.APIKeys))
		for i, v := range p.APIKeys {
			keys[i] = v
			if strings.HasPrefix(v, "enc:v1:") {
				keys[i] = r.decrypt(v)
			}
		}
	}
	pl := pool.New(p.Name, keys, cfg)
	// A config edit must not resurrect a burned key: carry the live health of
	// the keys that survive the edit (same hash) into the rebuilt pool
	// (review 2026-10-17, r20 F1 — every GUI provider save used to clear 429
	// cooldowns and backoff, so the next request re-hit the hot budget).
	if prev := r.pools[p.Name]; prev != nil {
		carried := 0
		for hash, st := range prev.ExportHealth() {
			if k := pl.KeyByHash(hash); k != nil {
				pl.LoadState(k, st)
				carried++
			}
		}
		if carried > 0 {
			slog.Debug("router: provider key health carried across config edit", "provider", p.Name, "keys", carried)
		}
	}
	// Ручные ручки из конфига (persist GUI-тумблеров): применяются по KeyHash.
	if len(p.DisabledKeys) > 0 || len(p.KeyProxies) > 0 || p.DefaultKey != "" {
		dis := map[string]bool{}
		for _, h := range p.DisabledKeys {
			dis[h] = true
		}
		for _, kv := range pl.SnapshotKeys() {
			if dis[kv.Hash] {
				_ = pl.SetEnabled(kv.Hash, false)
			}
			if px, ok := p.KeyProxies[kv.Hash]; ok {
				_ = pl.SetProxy(kv.Hash, px)
			}
		}
		if p.DefaultKey != "" {
			_ = pl.SetDefault(p.DefaultKey)
		}
	}
	r.pools[p.Name] = pl
	r.clients[p.Name] = newClient(p)
	// Breaker state (failures / open / half-open) is provider-scoped, not
	// config-scoped: reusing the live instance keeps a provider that is
	// mid-recovery from being reset to Closed by a GUI save (review
	// 2026-10-17, r20 F1).
	if prev := r.breakers[p.Name]; prev != nil {
		r.breakers[p.Name] = prev
	} else {
		r.breakers[p.Name] = breaker.New(p.Name, r.breakerCfg, r.persist)
	}
}

// RestorePoolState restores key health (backoff, disable, rate-limit) from
// the store at startup so a restart does not reset cooldowns.
func (r *Router) RestorePoolState(load func(provider string) ([]store.PoolKeyRow, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, pl := range r.pools {
		rows, err := load(name)
		if err != nil {
			continue
		}
		for _, row := range rows {
			k := pl.KeyByHash(row.KeyHash)
			if k == nil {
				continue
			}
			pl.LoadState(k, pool.KeyState{
				Failures:         row.Failures,
				Disabled:         row.Disabled,
				DisabledUntil:    row.DisabledUntil,
				BackoffLevel:     row.BackoffLevel,
				RateLimitedUntil: row.RateLimitedUntil,
				LastErr:          row.LastErr,
			})
		}
	}
}

// ReloadBreakers restores breaker state from the store (called at startup).
func (r *Router) ReloadBreakers(load func(provider string) (*store.BreakerRow, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.breakers {
		row, err := load(name)
		if err != nil || row == nil {
			continue
		}
		r.breakers[name].Load(row)
	}
}

// BreakerState returns per-provider circuit state for status/CLI.
func (r *Router) BreakerState() map[string]breaker.Row {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]breaker.Row{}
	for name, b := range r.breakers {
		out[name] = b.Row()
	}
	return out
}

// LockoutState returns per-model lockout state.
func (r *Router) LockoutState() map[string]lockout.State { return r.lock.Snapshot() }

// PoolDiag exposes per-egress pool accounting (admin diagnostics): window
// success counts, learned budget, ban/park state and the global burn latch.
func (r *Router) PoolDiag() map[string]any {
	if d, ok := r.transport.(interface{ PoolDiag() map[string]any }); ok {
		return d.PoolDiag()
	}
	return nil
}

// ForcePoolRotate asks the egress pool for a fresh identity generation
// (empty provider = every pooled provider). True when a rotate was dispatched.
func (r *Router) ForcePoolRotate(provider string) bool {
	if d, ok := r.transport.(interface{ ForceRotate(string) bool }); ok {
		return d.ForceRotate(provider)
	}
	return false
}

// syncIdentityGen drops per-model lockout state when the egress pool was
// re-created: a storm locks a model out within a second (10 failures / 5 min),
// and that lock would otherwise keep answering 429 for the whole 15-minute
// window AFTER a rotate already minted fresh identities. "The pool healed but
// the client still dies" is exactly this stale-verdict path (live 2026-10-22:
// 429 "model locked out (repeated failures)" on a pool that had just rotated).
// recordModelFailure feeds the per-model lockout ONLY with verdicts about the
// model. A transport failure (no egress, pool saturated, pool api restarting)
// is a verdict about the pool: during a pool-wide outage every request fails,
// so counting them locked a healthy model out for the whole 15-minute window
// and the recovered pool then answered 429 instead of serving (stand
// 2026-10-22, 1000 rpm: 277/1000 requests in the recovered phase).
func (r *Router) recordModelFailure(model string, err error) {
	if err == nil || errors.Is(err, ErrTransportUnavailable) || isPoolExhaustedErr(err) {
		return
	}
	// 429/503 are capacity verdicts, not verdicts about the model: the pool
	// already bans the egress for the Retry-After window. Feeding them here
	// locked the model for 15 minutes on a spent quota (live 2026-10-22: 2687
	// `model locked out` in 30 minutes while the pool was rotating).
	var use *UpstreamStatusError
	if errors.As(err, &use) && (use.Status == http.StatusTooManyRequests || use.Status == http.StatusServiceUnavailable) {
		return
	}
	r.lock.RecordFailure(model, err.Error(), time.Now())
}

func (r *Router) syncIdentityGen() {
	gen, ok := r.transport.(interface{ IdentityGen() uint64 })
	if !ok {
		return
	}
	cur := gen.IdentityGen()
	prev := r.lastIdentGen.Load()
	if cur == prev {
		return
	}
	if !r.lastIdentGen.CompareAndSwap(prev, cur) {
		return
	}
	if prev == 0 {
		return // first sighting: nothing recorded against an older generation
	}
	r.lock.ResetAll()
	if r.OnEvent != nil {
		r.OnEvent("ai.lockout-reset", map[string]any{"generation": cur, "previous": prev})
	}
}

// Providers returns the registered provider names.
func (r *Router) Providers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.providers))
	for n := range r.providers {
		names = append(names, n)
	}
	return names
}

// Provider returns the resolved provider, or nil when unknown.
func (r *Router) Provider(name string) *provider.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[name]
}

// ProviderEffort returns the GUI-configured per-model reasoning effort for
// model ("" or "auto" = no override, fall back to family auto-max).
// It strips an optional "provider/model" pin like tryProvider, then falls
// back to the first provider (declaration order) serving the model.
// Aliases resolve to canonical ids before lookup (router.Call rewrites them).
func (r *Router) ProviderEffort(model string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Same ai-router/ strip as Call (cap 3): effort must not diverge on
	// double-prefixed ids while routing succeeds.
	for i := 0; i < 3; i++ {
		rest, ok := strings.CutPrefix(model, "ai-router/")
		if !ok || rest == "" {
			break
		}
		model = rest
	}
	if prov, real, ok := strings.Cut(model, "/"); ok && prov != "" && real != "" {
		if p, exists := r.providers[prov]; exists {
			return p.ModelReasoning[real]
		}
		model = real
	}
	if canon, err := provider.ResolveAlias(model); err == nil && canon != model {
		model = canon
	}
	for _, name := range r.providersOrder {
		p := r.providers[name]
		if p == nil {
			continue
		}
		if eff, ok := p.ModelReasoning[model]; ok && eff != "" {
			return eff
		}
	}
	return ""
}

// Pool returns the provider's key pool, or nil when unknown.
func (r *Router) Pool(name string) *pool.Pool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pools[name]
}

// Models returns all models advertised by providers, deduplicated.
// Models returns the complete advertised catalog: explicit provider models,
// catalog wildcard markers, the stable auto route, and the pinned
// "provider/model" aliases for each provider's explicit models.
func (r *Router) Models() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{"auto": true}
	out := []string{"auto"}
	for _, p := range r.providers {
		if len(p.Models) == 0 {
			continue
		}
		skip := map[string]bool{}
		for _, m := range p.DisabledModels {
			skip[m] = true
		}
		for _, m := range p.HiddenModels {
			skip[m] = true
		}
		for _, m := range p.Models {
			if m == "" {
				continue
			}
			if skip[m] {
				continue
			}
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
			pinned := p.Name + "/" + m
			if !seen[pinned] {
				seen[pinned] = true
				out = append(out, pinned)
			}
		}
	}
	sort.Strings(out[1:])
	return out
}

// preferPublishers moves the providers that list the composite id verbatim
// ahead of wildcard/loose matches, keeping everything else as fallback. Live
// case (review 2026-10-17): kilocode publishes "openai/gpt-5"; without this
// the cascade could hand the request to a wildcard provider that cannot even
// serve it (fireworks → "pool: no transport for provider") while the paid
// provider "openai" claimed the id under the pin syntax. Only the ordering
// changes: cascade order, tiers and strategy still decide inside each group.
func (r *Router) preferPublishers(cands []*provider.Provider, model string) []*provider.Provider {
	if !strings.Contains(model, "/") || len(cands) < 2 {
		return cands
	}
	published := func(p *provider.Provider) bool {
		return p != nil && len(p.Models) > 0 && p.ModelAllowed(model)
	}
	n := 0
	for _, p := range cands {
		if published(p) {
			n++
		}
	}
	if n == 0 || n == len(cands) {
		return cands
	}
	out := make([]*provider.Provider, 0, len(cands))
	for _, p := range cands {
		if published(p) {
			out = append(out, p)
		}
	}
	for _, p := range cands {
		if !published(p) {
			out = append(out, p)
		}
	}
	return out
}

// modelServedWhole reports whether some provider publishes the full model id
// verbatim (explicit model list only — wildcard providers are skipped on
// purpose: "anything goes" would swallow every provider/model pin, and the
// pin syntax is exactly what a wildcard cannot legitimize).
func (r *Router) modelServedWhole(model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.providers {
		if p == nil || len(p.Models) == 0 {
			continue
		}
		if p.ModelAllowed(model) {
			return true
		}
	}
	return false
}

// resolve returns the ordered candidate providers for a model: providers
// that allow it, followed by their fallback chains (cycle-protected), then
// reordered by tier cascade and the configured strategy. Without a strategy
// the order is deterministic (sorted name, like the pre-v2 router).
func (r *Router) resolve(model string) []*provider.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var provs []*provider.Provider
	seen := map[string]bool{}
	var walk func(name string, depth int)
	walk = func(name string, depth int) {
		if depth > 5 || seen[name] {
			return
		}
		seen[name] = true
		p, ok := r.providers[name]
		if !ok || !p.ModelAllowed(model) {
			return
		}
		if p.BaseURL == "" {
			// A provider without a base URL cannot serve any request
			// (unless reachable via fallback later); skip it so it does not
			// shadow real providers in the priority order.
			return
		}
		provs = append(provs, p)
		for _, fb := range p.Fallback {
			walk(fb, depth+1)
		}
	}
	// Declaration order: config ProvidersOrder (captured from YAML)
	// preserves the user's file order; fallback to sorted names when no
	// order was recorded (e.g. providers injected in tests).
	var names []string
	if len(r.providersOrder) > 0 {
		names = append([]string(nil), r.providersOrder...)
		// include any runtime-added providers (tests) not in order list
		for name := range r.providers {
			found := false
			for _, n := range names {
				if n == name {
					found = true
					break
				}
			}
			if !found {
				names = append(names, name)
			}
		}
	} else {
		names = make([]string, 0, len(r.providers))
		for name := range r.providers {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	order := map[string]int{}
	for i, n := range names {
		order[n] = i
	}
	for _, name := range names {
		walk(name, 0)
	}
	if len(provs) <= 1 {
		return provs
	}
	cands := make([]routing.Candidate, 0, len(provs))
	for _, p := range provs {
		cands = append(cands, routing.Candidate{
			Name:       p.Name,
			Tier:       p.Tier,
			Weight:     p.Weight,
			CostIn:     p.CostPer1MIn,
			CostOut:    p.CostPer1MOut,
			Keys:       len(p.APIKeys),
			Priority:   order[p.Name],
			IsWildcard: len(p.Models) == 0,
		})
	}
	cands = routing.CascadeChoose(cands, r.tiers, r.strategy)
	out := make([]*provider.Provider, 0, len(cands))
	for _, c := range cands {
		out = append(out, r.providers[c.Name])
	}
	return out
}

// Result is a completed upstream response. Resp is always non-nil on success
// and owns the response body; callers must close Resp.Body. Body is retained
// for compatibility but is not populated by the router.
type Result struct {
	Provider string
	Status   int
	Header   http.Header
	Body     []byte
	Resp     *http.Response
	// ConnectionID is the provider key id (pool key hash) that served the
	// request; empty when the provider has no key pool.
	ConnectionID string
	// Model is the model actually sent upstream (the requested one, or the
	// step's model for combo calls).
	Model string
	// ComboPath is the graph walk trace (visited/skipped/winner labels);
	// scalar ComboStepID keeps the legacy "N:provider" winner contract.
	ComboPath []string
	// ComboName/ComboStepID are set for combo calls ("N:provider"); empty for
	// ordinary Call invocations.
	ComboName   string
	ComboStepID string
	// Proxy is the egress proxy used for this request (pool channel id when
	// the pool is enabled, else the provider's static egress URL). Empty when
	// no proxy was applied.
	Proxy string
	// WaitedMS is the raw acquire wait: it includes ordinary lease latency
	// (tens of ms), so only values ≥ ~1s mean "served after a real wait".
	// before serving it (0 = immediate). Journaled as the "served after
	// wait" category so retried-but-successful requests stay visible instead
	// of hiding inside latency.
	WaitedMS int64
}

// ErrNoCandidates is returned when no provider serves the model.
var ErrNoCandidates = errors.New("no provider serves model")

// UpstreamStatusError carries the last upstream HTTP status when every
// attempt failed. Gateways surface it as the honest status (e.g. 429)
// instead of collapsing everything into a generic 502. RetryAfter is kept
// for internal plumbing but NEVER set on pool/opencode paths (swap mode
// 2026-09-11: outward RA blocks client swapping).
type UpstreamStatusError struct {
	Status     int
	Err        error
	RetryAfter string
}

// isPoolExhaustedErr: 503-хвосты пула (бюджет выгорел, провайдер жив).
// Брейкер их не считает: иначе волна pool-503 открывает circuit и наружу
// идут 502-окна на 30с вместо честных 503 (волна 2026-09-20, R4).
func isPoolExhaustedErr(err error) bool {
	if err == nil {
		return false
	}
	var use *UpstreamStatusError
	if errors.As(err, &use) && use.Status == http.StatusServiceUnavailable {
		msg := strings.ToLower(use.Err.Error())
		// Wave review 2026-09-20 (review_2 F2): все pool-503 хвосты —
		// иначе global/budget-groups/pool-hot/api-restarting открывают
		// circuit на 30с и наружу идут 503-окна вместо ротации.
		for _, s := range []string{"no ready egress", "budget exhausted", "budget burned", "pool saturated", "no cold lease", "pool cycled", "budget groups hot", "pool hot", "pool api restarting"} {
			if strings.Contains(msg, s) {
				return true
			}
		}
	}
	return false
}

// FailureClass groups upstream failures for quota-gated fallback (Omni
// fallbackOnlyOnQuotaExhaustion parity): auth/policy errors must not burn a
// quota-only step, network/timeout may advance.
type FailureClass string

const (
	FailAuth    FailureClass = "auth"    // 401, 403
	FailQuota   FailureClass = "quota"   // 429, 5xx, 408, 524
	FailNetwork FailureClass = "network" // transport errors
)

// ClassifyStatus maps an upstream HTTP status to its failure class.
func ClassifyStatus(status int) FailureClass {
	switch status {
	case 401, 403, 404:
		return FailAuth
	case 429, 408, 524:
		return FailQuota
	default:
		if status >= 500 {
			return FailQuota
		}
		return FailNetwork
	}
}

// FailureClassOf unwraps a combo step error to its class: UpstreamStatusError
// carries the real status, anything else is network.
func FailureClassOf(err error) FailureClass {
	var use *UpstreamStatusError
	if errors.As(err, &use) {
		return ClassifyStatus(use.Status)
	}
	return FailNetwork
}

func (e *UpstreamStatusError) Error() string { return e.Err.Error() }
func (e *UpstreamStatusError) Unwrap() error { return e.Err }

// Call executes a chat completion against the first working candidate.
// stream=true returns resp.Body open for SSE passthrough; the caller owns
// closing it. On transport errors and 401/429 the request is retried with
// other keys and fallback providers; on 5xx the next provider in the chain
// is tried.
func (r *Router) Call(ctx context.Context, model string, body []byte, stream bool) (*Result, error) {
	// Alias rewrite before anything else (Omni modelAliasResolver parity):
	// short names map to canonical ids, combo/ names bypass, retired ids
	// fail closed. The body model is rewritten too so upstream sees canon.
	if canon, err := provider.ResolveAlias(model); err != nil {
		return nil, err
	} else if canon != model {
		model = canon
		body = withModel(body, canon)
	}
	// Optional "provider/model" syntax pins routing to a single provider,
	// bypassing the candidate cascade and strategy. The provider suffix is
	// stripped from the model before it is sent upstream.
	// "ai-router/" is the local gateway name, not a provider: strip ALL
	// leading ai-router/ segments (cap 3), then re-pin the remainder once —
	// single-strip left "opencode/X" matching wildcards only (502/404).
	for i := 0; i < 3; i++ {
		if rest, ok := strings.CutPrefix(model, "ai-router/"); ok && rest != "" {
			model = rest
			body = withModel(body, rest)
			continue
		}
		break
	}
	// Gate body-shape backfill MUST run before the pin branch too: the zen
	// free gate parses the body (1+ tools) regardless of how the provider was
	// selected, and pinned "opencode/model-free" calls used to skip the
	// read+bash backfill and ate a guaranteed gate-403 (MITM matrix
	// 2026-10-24: tools-present body + 30-char ses_* passes, tools-less body
	// 403s on every egress).
	body = withAgentTools(body)
	// Whole-id precedence: a vendor-prefixed id that some provider publishes
	// verbatim must never be hijacked by the pin syntax when another provider
	// happens to own the first segment. Live case (review 2026-10-17):
	// kilocode publishes "openai/gpt-5" (config.yaml:541) while provider
	// "openai" (paid api.openai.com keys) allows "gpt-5" — the published
	// free id silently rode the paid upstream. Pins keep working for
	// composites nobody lists, which is the usual "provider/model" addressing.
	if prov, real, ok := strings.Cut(model, "/"); ok && !r.modelServedWhole(model) {
		if prov == "" || real == "" {
			return nil, ErrNoCandidates // leading/trailing-slash: honest 404, never wildcard
		}
		r.mu.RLock()
		p, exists := r.providers[prov]
		r.mu.RUnlock()
		// Static config fact first: unknown provider or disallowed model
		// is terminal 404 regardless of lockout state (lockout-first
		// masked typos as retryable 429). Lockout gates only routable ids.
		if !exists || !p.ModelAllowed(real) {
			return nil, ErrNoCandidates
		}
		r.syncIdentityGen()
		if !r.lock.Allowed(real, time.Now()) {
			return nil, lockout.ErrLocked
		}
		res, err := r.tryProvider(ctx, p, withModel(body, real), stream)
		if err == nil {
			r.lock.Reset(real)
			return res, nil
		}
		r.recordModelFailure(real, err)
		return nil, err
	}
	// zen free-tier gate требует read+bash tools в теле (MITM 2026-10-06):
	// без них mimo/ling/nemotron/laguna/deepseek отвечают 403
	// "used from within OpenCode". Клиентские tools не трогаем.
	body = withAgentTools(body)
	candidates := r.resolve(model)
	candidates = r.preferPublishers(candidates, model)

	if len(candidates) == 0 {
		return nil, ErrNoCandidates
	}
	r.syncIdentityGen()
	if !r.lock.Allowed(model, time.Now()) {
		return nil, lockout.ErrLocked
	}
	var lastErr error
	seenProv := map[string]bool{}
	for _, p := range candidates {
		if seenProv[p.Name] {
			continue
		}
		seenProv[p.Name] = true
		res, err := r.tryProvider(ctx, p, body, stream)
		if err == nil {
			r.lock.Reset(model)
			return res, nil
		}
		// Terminal account-gate 403 (FreeTier/billing, gateTerminalHeader
		// decided inside tryProvider): the account verdict is decided for
		// THIS request — continuing to wildcards only masks it as a
		// misleading wildcard 404 (2026-09-18: bare spark opencode gate-403
		// then opencode-zen gate-403, client saw fireworks Not Found).
		// Surface the honest gate now, never the last-candidate error.
		var use *UpstreamStatusError
		if errors.As(err, &use) && use.Status == http.StatusForbidden && isGateTerminalErr(use) {
			r.lock.RecordFailure(model, err.Error(), time.Now())
			return nil, err
		}
		lastErr = err
		// Отмена клиента: каскад стоп, lockout не кормим — это не вердикт модели.
		// Два случая различаются принципиально: клиентский ctx мёртв → отдаём
		// ctx.Err(); отмена пришла изнутри (пул гасит свой attemptCtx, клиент
		// жив) → отдаём саму ошибку. Прежний общий `return nil, ctx.Err()`
		// возвращал (nil, nil) и вызывающие падали на разыменовании результата
		// (review 2026-10-17, r06 F1).
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(lastErr, context.Canceled) {
		return nil, lastErr
	}
	r.recordModelFailure(model, lastErr)
	if !r.lock.Allowed(model, time.Now()) && r.OnEvent != nil {
		r.OnEvent("ai.lockout", map[string]any{"model": model})
	}
	return nil, lastErr
}

// CallCombo executes a chat completion through an explicit combo chain: steps
// are tried in order, each step naming a provider + model. A step that fails
// (transport error, circuit open, 401/429/5xx after key rotation) moves to the
// next; the first success wins. The step's model replaces the body's model so
// the upstream sees the pinned model. Result carries ComboName and ComboStepID
// ("N:provider", 1-based) for the caller's event/journal.
func (r *Router) CallCombo(ctx context.Context, name, model string, steps []store.ComboStep, body []byte, stream bool) (*Result, error) {
	if !r.lock.Allowed(model, time.Now()) {
		return nil, lockout.ErrLocked
	}
	if store.IsGraph(steps) {
		return r.callComboGraph(ctx, name, model, steps, body, stream)
	}
	order := comboOrder(steps)
	var lastErr error
	for _, si := range order {
		step := steps[si]
		// Quota-gated fallback (Omni fallbackOnlyOnQuotaExhaustion): a
		// quota-only step is skipped unless the previous failure was
		// quota-class (429/5xx). Auth/policy failures must not burn it.
		if step.QuotaOnly() && lastErr != nil && FailureClassOf(lastErr) != FailQuota {
			continue
		}
		p := r.Provider(step.Provider)
		if p == nil {
			lastErr = fmt.Errorf("combo step %d: unknown provider %s", si+1, step.Provider)
			continue
		}
		stepModel := step.Model
		if stepModel == "" {
			stepModel = model
		}
		stepBody := body
		if stepModel != "" {
			stepBody = withModel(body, stepModel)
		}
		res, err := r.tryProvider(ctx, p, stepBody, stream)
		if err == nil {
			res.ComboName = name
			res.ComboStepID = fmt.Sprintf("%d:%s", si+1, step.Provider)
			r.lock.Reset(model)
			return res, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("combo %q: no steps", name)
	}
	r.lock.RecordFailure(model, lastErr.Error(), time.Now())
	if !r.lock.Allowed(model, time.Now()) && r.OnEvent != nil {
		r.OnEvent("ai.lockout", map[string]any{"model": model})
	}
	return nil, lastErr
}

// comboClassNames перечисляет классы ошибки шага, видимые CallCombo:
// quota (429/5xx/408/524), auth (401/403/404), network (транспорт),
// gate_terminal (403 аккаунт/биллинг), pool_exhausted (пул/брейкер).
// "*" в условии матчит любой класс.
func comboClassNames(err error) []string {
	var use *UpstreamStatusError
	if errors.As(err, &use) && isGateTerminalErr(use) {
		return []string{"gate_terminal", "auth"}
	}
	if isPoolExhaustedErr(err) || strings.Contains(err.Error(), "circuit open") {
		return []string{"pool_exhausted", "quota"}
	}
	return []string{string(FailureClassOf(err))}
}

func comboClassMatch(on []string, err error) bool {
	if len(on) == 0 {
		return false
	}
	for _, want := range on {
		w := strings.ToLower(strings.TrimSpace(want))
		if w == "" || w == "*" || w == "any" {
			return true
		}
		for _, got := range comboClassNames(err) {
			if w == got {
				return true
			}
		}
	}
	return false
}

// indexOfOrder возвращает позицию индекса si в порядке order (или -1).
func indexOfOrder(order []int, si int) int {
	for pos, v := range order {
		if v == si {
			return pos
		}
	}
	return -1
}

// comboGraphError несёт путь каскада вместе с ошибкой наружу (журнал/GUI).
type comboGraphError struct {
	path []string
	err  error
}

func (e *comboGraphError) Error() string  { return e.err.Error() }
func (e *comboGraphError) Unwrap() error  { return e.err }
func (e *comboGraphError) Path() []string { return e.path }

// callComboGraph исполняет пресет с блоками условий: шаги идут в порядке
// объявления, условие-узел маршрутизирует каскад по классу ошибки
// предыдущего шага (to — целевой узел, stop — оборвать каскад), finish
// завершает обход. Прыжки ограничены счётом посещений: битый граф с
// циклом не может подвесить запрос. Пресеты без graph-полей идут по
// исходному линейному пути (CallCombo), поведение там не меняется.
func (r *Router) callComboGraph(ctx context.Context, name, model string, steps []store.ComboStep, body []byte, stream bool) (*Result, error) {
	if !r.lock.Allowed(model, time.Now()) {
		return nil, lockout.ErrLocked
	}
	byID := map[string]int{}
	for i, st := range steps {
		id := st.ID
		if id == "" {
			id = fmt.Sprintf("n%d", i+1)
		}
		byID[id] = i
	}
	order := comboOrder(steps)
	var lastErr error
	path := []string{}
	visited := 0
	maxVisits := 8*len(steps) + 8
	pos := 0
	for pos < len(order) {
		if visited > maxVisits {
			lastErr = fmt.Errorf("combo %q: граф зациклен (%d посещений)", name, visited)
			break
		}
		visited++
		si := order[pos]
		step := steps[si]
		label := fmt.Sprintf("%d:%s", si+1, step.Provider)
		if step.Provider == "" && step.ID != "" {
			label = fmt.Sprintf("%d:%s", si+1, step.ID)
		}
		switch {
		case step.IsFinish():
			// Явный терминат каскада: обход закончен, вердикт — последняя
			// ошибка (как если бы шаги кончились естественно).
			path = append(path, label+":finish")
			pos = len(order)
			continue
		case step.IsCondition():
			if lastErr == nil {
				path = append(path, label+":cond:skip(no-failure)")
				pos++
				continue
			}
			if comboClassMatch(step.OnClasses(), lastErr) {
				if step.Stop {
					path = append(path, label+":cond:stop")
					r.lock.RecordFailure(model, lastErr.Error(), time.Now())
					if !r.lock.Allowed(model, time.Now()) && r.OnEvent != nil {
						r.OnEvent("ai.lockout", map[string]any{"model": model})
					}
					return nil, &comboGraphError{path: append(path, "tail:stop"), err: lastErr}
				}
				target := -1
				if step.To != "" {
					if ti, ok := byID[step.To]; ok {
						target = ti
					} else {
						path = append(path, label+":cond:dangling("+step.To+")")
					}
				}
				if target >= 0 {
					path = append(path, label+":cond:to("+step.To+")")
					pos = indexOfOrder(order, target)
					continue
				}
				path = append(path, label+":cond:match")
				pos++
				continue
			}
			path = append(path, label+":cond:miss")
			pos++
			continue
		}
		// start-узел без провайдера — заголовок графа (несёт модель по
		// умолчанию), а не попытка вверх: раньше он уходил в
		// "unknown provider" и жёг посещение (review 2026-10-17, r11/r22 F1).
		if step.IsStart() && step.Provider == "" {
			path = append(path, label+":start")
			pos++
			continue
		}
		// quota/on-гейт шага: входить только при совпавшем классе.
		if lastErr != nil && len(step.OnClasses()) > 0 && !comboClassMatch(step.OnClasses(), lastErr) {
			path = append(path, label+":skip")
			pos++
			continue
		}
		p := r.Provider(step.Provider)
		if p == nil {
			lastErr = fmt.Errorf("combo step %d: unknown provider %s", si+1, step.Provider)
			path = append(path, label+":unknown-provider")
			pos++
			continue
		}
		stepModel := step.Model
		if stepModel == "" {
			stepModel = model
		}
		stepBody := body
		if stepModel != "" {
			stepBody = withModel(body, stepModel)
		}
		res, err := r.tryProvider(ctx, p, stepBody, stream)
		if err == nil {
			res.ComboName = name
			res.ComboStepID = label
			res.ComboPath = append(path, label+":win")
			r.lock.Reset(model)
			return res, nil
		}
		lastErr = err
		path = append(path, label+":fail")
		pos++
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("combo %q: no steps", name)
	}
	r.lock.RecordFailure(model, lastErr.Error(), time.Now())
	if !r.lock.Allowed(model, time.Now()) && r.OnEvent != nil {
		r.OnEvent("ai.lockout", map[string]any{"model": model})
	}
	return nil, fmt.Errorf("combo %q: %w", name, lastErr)
}

// comboOrder переставляет индексы шагов по методу свапа (params.swap первого
// шага): priority — как есть; round_robin — сдвиг на 1 (по кругу);
// random — перемешивание; least_used — как есть (устойчивый порядок).
func comboOrder(steps []store.ComboStep) []int {
	n := len(steps)
	if n == 0 {
		return []int{}
	}
	if n == 1 {
		return []int{0}
	}
	swap := ""
	if len(steps) > 0 && len(steps[0].Params) > 0 {
		var p struct {
			Swap string `json:"swap"`
		}
		if err := json.Unmarshal(steps[0].Params, &p); err == nil {
			swap = p.Swap
		}
	}
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	switch swap {
	case "round_robin":
		first := out[0]
		copy(out, out[1:])
		out[n-1] = first
	case "random":
		for i := n - 1; i > 0; i-- {
			j := rand.IntN(i + 1)
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// gateExcerpt renders a short human reason from an account-gate 403 body:
// prefers the upstream error.message, falls back to trimmed raw text.
func gateExcerpt(b []byte) string {
	s := string(b)
	if i := strings.Index(s, `"message":"`); i >= 0 {
		rest := s[i+11:]
		if j := strings.Index(rest, `"`); j > 0 && j < 300 {
			return "account gate: " + rest[:j]
		}
	}
	t := strings.TrimSpace(strings.ToValidUTF8(s, ""))
	if len(t) > 160 {
		t = t[:160]
	}
	if t == "" {
		return "account gate"
	}
	return "account gate: " + t
}

// gateTerminalErr is the UpstreamStatusError marker for a TERMINAL account
// gate (FreeTier/billing, gateTerminalHeader decided inside tryProvider):
// Call must stop the candidate cascade on it instead of masking it with a
// later wildcard 404. Plain 403s (region/non-region swaps, non-pool legs)
// keep cascading — only this marker stops.
const gateTerminalMarker = "gate-terminal"

// sessionGateID returns the opencode session the served attempt actually
// carried (pool-stamped or inbound). "" for detached responses and for
// upstreams the pool never stamps (non-opencode), which is a no-op for
// the session pool.
func sessionGateID(res *http.Response) string {
	if res == nil || res.Request == nil {
		return ""
	}
	return res.Request.Header.Get("x-opencode-session")
}

// sessionGateEgressHeader is stamped by PoolTransport on gate-terminal
// responses so the session streak can key on (session, egress).
const sessionGateEgressHeader = "X-Pool-Egress"

// sessionGateEgress returns the egress that produced the gate verdict:
// PoolTransport stamps X-Pool-Egress (ipKey) on gate-terminal responses so
// the session streak can key on (session, egress) — a gate is egress-
// correlated, so two gates on different egresses must not burn the id.
// "" when the verdict came from a path without egress identity.
func sessionGateEgress(res *http.Response) string {
	if res == nil {
		return ""
	}
	return res.Header.Get(sessionGateEgressHeader)
}

// noteSessionGate records one TERMINAL account-gate 403 against the session
// that served the attempt: MarkGateHit(id, egress, false) — arm only, never burn.
// The gate is egress-correlated (2026-09-18: same session+minute, one egress
// 200 while others 403) and sometimes model-scoped rather than session-scoped,
// so the first 403 only arms the streak; the id dies on the second consecutive
// gate ON THE SAME EGRESS, and any 200 in between clears the streak (see
// MarkGateHit(id, egress, true) on the success path below). A gate verdict
// without an egress cannot burn the id at all.
func noteSessionGate(pool *SessionPool, res *http.Response) {
	if pool == nil {
		return
	}
	pool.MarkGateHit(sessionGateID(res), sessionGateEgress(res), false)
}

// isGateTerminalErr reports whether err is a terminal gate verdict: 403
// whose chain carries the gate-terminal marker. O(1) string check, no body
// peek — the body was consumed inside tryProvider.
func isGateTerminalErr(err *UpstreamStatusError) bool {
	if err == nil || err.Status != http.StatusForbidden || err.Err == nil {
		return false
	}
	return strings.Contains(err.Err.Error(), gateTerminalMarker)
}

// policyKey maps an HTTP status to the config error-policy key.
func policyKey(status int) string {
	switch {
	case status == 401:
		return "401"
	case status == 403:
		return "403"
	case status == 408:
		return "408"
	case status == 429:
		return "429"
	case status == 524:
		return "524"
	case status >= 500:
		return "5xx"
	default:
		return ""
	}
}

// tryProvider attempts a provider, rotating keys on 401/429 and tripping the
// circuit breaker on repeated failure.
func (r *Router) tryProvider(ctx context.Context, p *provider.Provider, body []byte, stream bool) (*Result, error) {
	model := modelOf(body)
	responsesModel := false
	for _, m := range p.ResponsesModels {
		if m == model {
			responsesModel = true
			break
		}
	}
	r.mu.RLock()
	pl := r.pools[p.Name]
	cl := r.clients[p.Name]
	br := r.breakers[p.Name]
	r.mu.RUnlock()

	// The client may address a model by its pinned "provider/model" alias,
	// but upstream only knows the bare id: zen now 401s prefixed names
	// ("Model opencode/x-preview-f-free is not supported"). Rewrite the
	// body's model to the canonical prefix-stripped name before sending.
	if pre := p.Name + "/"; strings.HasPrefix(model, pre) {
		body = withModel(body, strings.TrimPrefix(model, pre))
		model = strings.TrimPrefix(model, pre)
	}
	// Free-tier shape injection lives HERE, not at the call sites: every path
	// into an upstream (plain call, fallback list, combo step, GUI health,
	// hubmod) ends in tryProvider, so this is the only place that cannot miss
	// one — the combo/health paths used to skip it and stamp egresses with a
	// 403 the body itself caused. Gated by provider: a "-free"-looking id on a
	// non-opencode upstream never gets a foreign prompt or tool schema.
	if isOpencodeUpstream(p) {
		slog.Default().Info("DEBUG: isOpencodeUpstream=true, injecting canonical", "provider", p.Name, "baseURL", p.BaseURL)
		body = withCanonicalSystem(withAgentTools(body))
		// Every gated body leaves shaped as far as the stream flag goes: the
		// Responses lane converts and forces stream:true, and the chat lane
		// forces it too (see doRequest/chat_destream.go — a non-stream body is
		// answered 429 FreeUsageLimitError by the gate). Count that on the
		// wire shape instead of suppressing the verdict, so a real account-gate
		// 403 on a non-stream caller still blames/swaps the egress. A body the
		// shaping could not fix (invalid JSON) stays a false verdict.
		shapeStream := stream || p.ResponsesAPI || isOpencodeUpstream(p)
		ctx = withGateShape(ctx, gatedShapeComplete(body, shapeStream))
	}
	if !br.Allow(time.Now()) {
		err := fmt.Errorf("provider %s: circuit open", p.Name)
		r.emitFallback(p, model, 503)
		// Wave review 2026-09-20 (P4/R4 + review_2 F4): circuit open —
		// переход, статус 503 (MODEL_CAPACITY, очередь ретраит), но БЕЗ
		// outward Retry-After: gateway форвардит RA в хедер (860-862),
		// OMP встанет в backoff до 30с и стопит свапы (swap mode).
		// Секунды до trial — в текст для триажа, не в хедер.
		if until := br.OpenedUntil(); until.After(time.Now()) {
			err = fmt.Errorf("provider %s: circuit open, retry in %ds", p.Name, int(time.Until(until).Seconds())+1)
		}
		return nil, &UpstreamStatusError{Status: http.StatusServiceUnavailable, Err: err}
	}

	attempts := 1
	if len(p.APIKeys) > 0 {
		attempts = len(p.APIKeys)
	}
	if r.transport != nil {
		// Пул сам ходит fanout×rounds внутри одного RoundTrip: внешние
		// попытки не добавляют egress, только жгут окна повторными
		// запросами (каждый 429 снаружи = +10 прогревов внутри).
		// Self-rotating: всегда 1 внешняя попытка — inner уже ротирует
		// ключи (401) и egress (429/5xx); N внешних попыток = N полных
		// свипов по тем же /48-группам и N×120s против gateway 180s.
		if _, ok := r.transport.(interface{ SelfRotating() bool }); ok {
			attempts = 1
		} else if attempts < 4 {
			attempts = 4
		}
	} else if attempts < 4 {
		// No transport: direct client. Retry transient upstream failures.
		attempts = 4
	}
	var lastErr error
	lastStatus := 0
	// lastRetryAfter удалён: outward RA стёрт (swap mode 2026-09-11).
	// Внутренний key-backoff живёт через pl.Report выше.
	seenProxy := map[string]bool{}
	// regionSwaps bounds outer 403-region swaps (muse-spark not served in
	// some egress regions): each swap extends the attempt budget by one so
	// the next iteration acquires a FRESH lease (seenProxy skips repeats).
	// Direct chat-403 regions already swap inside poolRetryRT; this outer
	// loop covers ONLY the /responses-mapped 403 (inner saw 200-SSE/JSON
	// with region failure, mapping happens in doRequest after the pool).
	// A synthetic inner verdict (regionExhaustedHeader) is already the
	// product of a full 5x2 sweep — never re-swap it (60 warmups).
	const maxRegionSwaps = 3
	regionSwaps := 0
	// sessionSwaps bounds in-request session rotation on a free-tier 429: the
	// quota is bound to (session, egress), so a spent id is a verdict about the
	// PINNED SESSION, not about the lane. Retire the id and retry the SAME
	// request with a fresh harvested one (2026-10-27 fake-429 stand: without
	// this, only the NEXT client request rotated, so a burned id cost one
	// visible 429 per request and a drained pool "always 429").
	const maxSessionSwaps = 2
	sessionSwaps := 0
	for i := 0; i < attempts; i++ {
		var key *pool.Key
		var err error
		if len(p.APIKeys) > 0 {
			key, err = pl.Pick()
			if err != nil {
				lastErr = fmt.Errorf("provider %s: %w", p.Name, err)
				// Retries exhausted every usable key (e.g. repeated 429
				// cooldowns): report the real upstream status, not the
				// internal "no usable key" plumbing detail.
				if lastStatus != 0 {
					return nil, &UpstreamStatusError{Status: lastStatus, Err: fmt.Errorf("%s (pool exhausted): %w", http.StatusText(lastStatus), lastErr)}
				}
				break
			}
		}
		var lease *Lease
		if r.transport != nil {
			acqCtx := ctx
			cancelAcq := context.CancelFunc(nil)
			if dl, ok := ctx.Deadline(); ok {
				if left := time.Until(dl); left <= 0 {
					lastErr = fmt.Errorf("provider %s transport: shared budget exhausted before swap %d", p.Name, regionSwaps)
					break
				} else if left < poolTryDeadline {
					acqCtx, cancelAcq = context.WithTimeout(ctx, left)
				}
			}
			lease, err = r.transport.Acquire(acqCtx, RequestContext{Provider: p.Name, Model: model, Attempt: i, Stream: stream}, p)
			if cancelAcq != nil {
				cancelAcq()
			}
			if err != nil {
				// Отмена клиента: без свапа/продолжения — наружу ctx err.
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					if lease != nil {
						_ = lease.Release(context.Background())
					}
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					return nil, err
				}
				lastErr = fmt.Errorf("provider %s transport: %w", p.Name, err)
				continue
			}
			// direct-fallback is the resilience path, not a pool egress:
			// it may repeat across attempts without being a rotation bug.
			if lease != nil && lease.Proxy != "" && lease.Proxy != "direct-fallback" && seenProxy[lease.Proxy] {
				_ = lease.Release(ctx)
				lastErr = fmt.Errorf("provider %s transport: repeated proxy lease %q", p.Name, lease.Proxy)
				continue
			}
			if lease != nil && lease.Proxy != "" {
				seenProxy[lease.Proxy] = true
			}
		}
		res, status, proxyID, err := r.doRequestWithLease(ctx, cl, p, key, body, lease)
		if err != nil {
			// Отмена клиента: лизу вернуть, попытки стоп — без свапа дальше.
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				if lease != nil {
					_ = lease.Release(context.Background())
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, err
			}
			if key != nil {
				pl.Report(key, 0, "")
			}
			if lease != nil {
				_ = lease.Release(ctx)
			}
			lastErr = fmt.Errorf("provider %s: %w", p.Name, err)
			// Stable transport-level failures (TLS cert mismatch, connection
			// refused, DNS) will not heal between retries milliseconds
			// apart: stop burning this provider's attempt budget and move
			// to the next candidate right away.
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "tls:") || strings.Contains(msg, "x509") ||
				strings.Contains(msg, "certificate") || strings.Contains(msg, "connection refused") {
				break
			}
			policy := "network"
			if errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "timeout") {
				policy = "timeout"
			}
			act := p.ErrorPolicy[policy]
			if act == "none" || (act == "retry_proxy" && r.transport == nil) {
				break
			}
			continue
		}
		// 404 from a wildcard/keyless upstream ("Model not found, not
		// deployed") must cascade to the next candidate (e.g. the keyed
		// opencode-zen), not reach the client as a terminal answer.
		if status == 401 || status == 403 || status == 404 || status == 408 || status == 429 || status == 524 || status >= 500 {
			lastStatus = status
			// 403 region_error on a pool lease: the MODEL is not served in
			// this egress region (muse-spark), another egress may serve
			// it — swap to a fresh lease instead of failing terminally.
			// Decided inner sweeps (synthetic region verdict) and non-region
			// 403 stay honest terminal below. Body is closed on every error
			// path anyway, so peeking needs no restore. No cooldown/ban:
			// region support is per (model, region), the egress stays
			// usable for other models. Fallback event fires once per swap
			// decision (region single emit below), not per attempt.
			isPoolLease := r.transport != nil && lease != nil &&
				lease.Proxy != "" && lease.Proxy != "direct-fallback"
			// Inner-decided terminal account gate (FreeTier/billing): surface
			// honestly NOW — no swap, no breaker trip, no key backoff burn.
			// Retrying cannot help (account-scoped); the message carries the
			// real upstream reason instead of a "saturated" lie (2026-09-17:
			// spark answered eternal 503 while upstream said FreeTier-403).
			if status == http.StatusForbidden && isPoolLease &&
				res.Header.Get(gateTerminalHeader) != "" {
				peek, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
				res.Body.Close()
				r.emitFallback(p, model, status)
				// Streak, не burn: гейт бывает МОДЕЛЬНЫМ (12× свип mimo —
				// вердикт модели, не сессии), и он egress-скоррелирован,
				// поэтому сжигать общий свежий SES с первого гейта нельзя.
				// Первая 403 только заряжает streak в SessionPool; вторая
				// подряд (без 200 между) — id вылетает из пула и егресс
				// пере-пинится на живой (иначе мёртвый id остаётся
				// pinned-per-egress до 72h expiry).
				noteSessionGate(getSessionPool(), res)
				lastErr = fmt.Errorf("provider %s: Forbidden (%s) [%s]", p.Name, gateExcerpt(peek), gateTerminalMarker)
				return nil, &UpstreamStatusError{Status: status, Err: lastErr}
			}
			// 403 на pool-лизе: свап на свежий лиз внутри провайдера
			// (2026-09-16: клиент 403 не видит вообще). Region-body —
			// классический свап без бана; non-region — тоже свап (лимит
			// тот же maxRegionSwaps), потому что inner-swеep уже забанил
			// egress мягко на минуту, а решение "модель мертва везде"
			// принимает только decided-sweep (regionExhaustedHeader).
			// Исключение: decided inner verdict (synthetic, с хедером) —
			// это продукт полного 5x2 свипа, ре-свапать его = жечь до 60
			// прогревов на заведомо решённый запрос.
			if status == http.StatusForbidden && isPoolLease && regionSwaps < maxRegionSwaps &&
				res.Header.Get(regionExhaustedHeader) == "" {
				// Responses-mapped 403 (inner saw 200-SSE/JSON, mapping happened
				// in doRequest after the pool): peek for an account gate BEFORE
				// swapping — a gate verdict must not burn up to maxRegionSwaps
				// full sweeps (2026-09-17 spark: 4 sweeps per request).
				peek, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
				if isAccountGateBody(peek) {
					res.Body.Close()
					r.emitFallback(p, model, status)
					// Та же streak-метка, что и на gateTerminalHeader-пути
					// выше: peeked-гейт тоже терминальный вердикт аккаунта.
					noteSessionGate(getSessionPool(), res)
					lastErr = fmt.Errorf("provider %s: Forbidden (%s) [%s]", p.Name, gateExcerpt(peek), gateTerminalMarker)
					return nil, &UpstreamStatusError{Status: status, Err: lastErr}
				}
				res.Body.Close()
				lastErr = fmt.Errorf("provider %s: %s", p.Name, http.StatusText(status))
				r.emitFallback(p, model, status)
				regionSwaps++
				attempts++
				continue
			}
			// Key backoff AFTER region classification: Report(403) is a
			// no-op today (non-401/429/5xx branch), but a region swap must
			// never burn key backoff. Внутренний RA — только для key-pool
			// backoff (наружу не отдаём, иначе OMP/Omni встанет в backoff
			// и свап остановится — swap mode 2026-09-11).
			retryAfter := res.Header.Get("Retry-After")
			if key != nil {
				pl.Report(key, status, retryAfter)
			}
			res.Body.Close()
			lastErr = fmt.Errorf("provider %s: %s", p.Name, http.StatusText(status))
			if status == http.StatusTooManyRequests && isOpencodeUpstream(p) && sessionSwaps < maxSessionSwaps {
				// MarkDead unpins the id; the extra attempt re-picks a live one.
				if sid := sessionGateID(res); sid != "" {
					getSessionPool().MarkDead(sid)
					slog.Warn("session swap on 429 (in-request)", "provider", p.Name, "ses", sesPrefix(sid), "swap", sessionSwaps+1)
					sessionSwaps++
					attempts++
					continue
				}
			}
			// Only a result the caller will actually see counts as a fallback
			// transition; a swapped attempt is retried on the SAME provider.
			r.emitFallback(p, model, status)
			act := p.ErrorPolicy[policyKey(status)]
			if act == "none" {
				// 403/404 are policy rejections, not infra failures: the
				// provider is healthy, the request is simply not allowed
				// (region) or not deployed (wildcard 404) — never trip.
				if status != http.StatusForbidden && status != http.StatusNotFound {
					r.recordBreakerFailure(p, model, lastErr.Error())
				}
				return nil, &UpstreamStatusError{Status: status, Err: lastErr}
			}
			if status >= 500 && act == "" {
				r.recordBreakerFailure(p, model, lastErr.Error())
				return nil, &UpstreamStatusError{Status: status, Err: lastErr}
			}
			continue
		}
		// Gate-streak clear: a 200 proves the request session alive on some
		// egress (gate is egress-correlated), so any pending first-hit
		// streak for it must not burn the id on a later unrelated gate.
		if res != nil && res.Request != nil && isOpencodeUpstream(p) {
			getSessionPool().MarkGateHit(sessionGateID(res), sessionGateEgress(res), true)
		}
		br.RecordSuccess()
		conn := ""
		if key != nil {
			conn = key.KeyHash()
		}
		waited := int64(0)
		if lease != nil {
			waited = lease.WaitedMs
		}
		// The transport may know better than the lease: the direct home-IP leg
		// serves without a lane, and request_log.proxy must say so.
		if src := res.Header.Get(egressSourceHeader); src != "" {
			proxyID = src
		}
		return &Result{Provider: p.Name, Status: status, Header: res.Header, Resp: res, ConnectionID: conn, Model: model, Proxy: proxyID, WaitedMS: waited}, nil
	}
	// NO home-IP responses-fallback: the old path dialed opencode.ai direct
	// (newClient, no proxy) and exposed the home IP, which the upstream keys
	// with (account, session) into one identity — a burned home window then
	// drags the whole pool down (observed 2026-09-10: fresh main IP fixed
	// the same pool instantly). Pool verdict stands as-is; when the pool is
	// dead the honest 429/transport error reaches the client instead of a
	// home-IP retry. (Stub: block intentionally removed, not gated.)
	_ = responsesModel
	// Policy rejections (403 region, 404 wildcard) never trip the breaker —
	// same rule as the per-attempt path above (model not served, not down).
	// Pool-exhaustion 503 (no ready egress / budget exhausted / saturated /
	// no cold lease / pool cycled) тоже не трипает: это не падение провайдера,
	// а выгоревший бюджет — иначе после 5x503 breaker открывается на 30с и
	// наружу идут 502-окна вместо 503 (волна 2026-09-20, R4).
	if lastErr != nil && lastStatus != http.StatusForbidden && lastStatus != http.StatusNotFound && !isPoolExhaustedErr(lastErr) {
		// Отмена клиента — не вердикт провайдера: брейкер не кормим, наружу ctx err.
		if ctx.Err() == nil && !errors.Is(lastErr, context.Canceled) {
			r.recordBreakerFailure(p, model, lastErr.Error())
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(lastErr, context.Canceled) {
		return nil, lastErr
	}
	if lastStatus != 0 {
		return nil, &UpstreamStatusError{Status: lastStatus, Err: lastErr}
	}
	return nil, lastErr
}

// recordBreakerFailure records a provider failure and emits an event when
// the circuit trips open.
func (r *Router) recordBreakerFailure(p *provider.Provider, model, errMsg string) {
	br := r.breakers[p.Name]
	br.RecordFailure(errMsg)
	if br.Row().State == breaker.Open && r.OnEvent != nil {
		r.OnEvent("ai.breaker_open", map[string]any{
			"provider": p.Name,
			"model":    model,
			"error":    errMsg,
		})
	}
}

// doRequest performs the raw upstream POST. AuthMode=none omits Authorization;
// AuthMode=public always sends Bearer public. ResponsesAPI providers get the
// chat body converted to the /responses protocol and the SSE mapped back.
func (r *Router) doRequest(ctx context.Context, cl *http.Client, p *provider.Provider, key *pool.Key, body []byte, egressKey string, proxy string) (*http.Response, int, error) {
	// Defense-in-depth (transport.jitter_tail, по умолчанию выключено):
	// мутируем кодовые точки пробелов в хвосте тела, чтобы одинаковые
	// запросы не уходили с разных выходов байт-в-байт. Префикс (system,
	// tools, история) остаётся неизменным — prompt-cache апстрима жив.
	body = maybeJitterTail(p, body)
	model := modelOf(body)
	responsesModel := false
	for _, m := range p.ResponsesModels {
		if m == model {
			responsesModel = true
			break
		}
	}
	// The client's intent, captured before any gate shaping: the opencode
	// free tier answers 429 FreeUsageLimitError to `"stream": false` (see
	// chat_destream.go), so the chat lane forces stream:true upstream and
	// de-streams for non-stream callers.
	clientWantsStream := chatWantsStream(body)
	if p.ResponsesAPI || responsesModel {
		wantsStream := clientWantsStream
		converted, err := chatToResponses(body, isOpencodeUpstream(p))
		if err != nil {
			return nil, 0, err
		}
		apiKey := ""
		if p.AuthMode != "none" && key != nil {
			apiKey = key.Value
		}
		// Пин 1.18.99 и здесь (волна 50): primary шёл сырым, direct жёг ~5/IP.
		ua := p.UserAgent
		if pinned := leaseUA(p, "", ua); pinned != "" {
			ua = pinned
		}
		res, err := responsesUpstream(cl, p.BaseURL, ua, apiKey, converted, ctx, p, directHomeIPKey)
		if err != nil {
			return nil, 0, err
		}
		if res.StatusCode == 200 {
			res, err = wrapResponsesStream(res, modelOf(body), wantsStream)
			if err != nil {
				return nil, 0, err
			}
		}
		return res, res.StatusCode, nil
	}
	// The zen gate reads `"stream": false` as a non-OpenCode fingerprint and
	// answers 429 FreeUsageLimitError even on a clean egress. Ship stream:true
	// and fold the SSE back into one completion for non-stream callers.
	gatedChat := isOpencodeUpstream(p)
	if gatedChat && !clientWantsStream {
		body = forceChatStreamTrue(body)
	}
	if gatedChat {
		body = normalizeOpencodeToolChoice(body)
	}
	u := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	crypto.StripUpstreamHeaders(req.Header, crypto.DefaultStripHeaders)
	req.Header.Set("Content-Type", "application/json")
	// Per-request header diversity: without this every egress sends an identical
	// header set (Accept: */*, Go's default UA for non-gated lanes, keep-alive),
	// which is a cross-IP link signal. opencode-gated lanes keep their pinned
	// CLI UA (leaseUA) — only cosmetic/accept headers vary there.
	applyFingerprintDiversity(req, p)
	// Пин 1.18.99 и здесь (волна 50): pool пере-пинает клон, direct шёл сырым.
	ua := p.UserAgent
	if pinned := leaseUA(p, "", ua); pinned != "" {
		ua = pinned
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	switch {
	case p.AuthMode == "public":
		req.Header.Set("Authorization", "Bearer public")
	case p.AuthMode != "none" && key != nil:
		req.Header.Set("Authorization", "Bearer "+key.Value)
	}
	// zen Console gates free tier on the OpenCode session headers, not just
	// UA (2026-09: 400 MissingSessionID without x-opencode-session). This lane
	// has ONE physical egress (home IP or a single lease client), so it stamps
	// that egress's own stable id — never a process-wide default, which would
	// put the same session on every IP (cross-egress link).
	// egressKey "" = the caller is a pool lease: its RoundTrip stamps the
	// lease's own session; stamping a second (home) id here would reserve a
	// pool id for an egress that never sent it.
	setOpencodeHeadersAt(req, ctx, p, egressKey)
	var res *http.Response
	if isOpencodeUpstream(p) && buntransport.Enabled() && proxy != "" {
		res, err = buntransport.Do(ctx, cl, req, proxy)
	} else {
		res, err = cl.Do(req)
	}
	if err != nil {
		return nil, 0, err
	}
	// Two-protocol providers (zen): some models answer chat/completions,
	// others only the Responses protocol. On a non-retryable upstream
	// rejection (4xx) or a 5xx that smells like "wrong endpoint" (500 on a
	// 200-capable route), retry ONCE via /responses with a converted body.
	if p.ResponsesAPI && res.StatusCode != 200 && res.StatusCode != 401 && res.StatusCode != 429 {
		drainClose(res)
		fallbackStream := clientWantsStream
		if converted, cerr := chatToResponses(body, isOpencodeUpstream(p)); cerr == nil {
			apiKey := ""
			if p.AuthMode != "none" && key != nil {
				apiKey = key.Value
			}
			fbUA2 := p.UserAgent
			if pinned := leaseUA(p, "", fbUA2); pinned != "" {
				fbUA2 = pinned
			}
			if r2, e2 := responsesUpstream(cl, p.BaseURL, fbUA2, apiKey, converted, ctx, p, directHomeIPKey); e2 == nil {
				if r2.StatusCode == 200 && strings.HasPrefix(r2.Header.Get("Content-Type"), "text/event-stream") {
					r2, e2 = wrapResponsesStream(r2, modelOf(body), fallbackStream)
					if e2 != nil {
						return nil, 0, e2
					}
				}
				return r2, r2.StatusCode, nil
			}
		}
	}
	// Non-stream caller on the gated lane: the upstream body is SSE, fold it
	// into one chat.completion now. Body wrapping (lease release) is preserved:
	// the wrapper delegates Close to the upstream body and closes it exactly
	// once, on first Read.
	if gatedChat && !clientWantsStream && res.StatusCode == http.StatusOK &&
		strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		res.Body = newChatDestreamBody(res.Body, model)
		res.Header.Set("Content-Type", "application/json")
		res.Header.Del("Content-Length")
	}
	// Rotate the harvested session on an upstream 429: a pinned id that keeps
	// receiving 429s is banned; unpinning lets the next request re-pin to a
	// different live id instead of reusing the same banned identity forever.
	if res != nil && res.StatusCode == http.StatusTooManyRequests && isOpencodeUpstream(p) {
		if sid := sessionGateID(res); sid != "" {
			getSessionPool().MarkDead(sid)
		}
	}
	return res, res.StatusCode, nil
}

// doRequestWithLease performs an upstream request using an optional lease.
// The caller owns lease lifecycle; stream callers release after body close.
func (r *Router) doRequestWithLease(ctx context.Context, cl *http.Client, p *provider.Provider, key *pool.Key, body []byte, lease *Lease) (*http.Response, int, string, error) {
	proxyID := p.Egress
	requestClient := cl
	if lease != nil {
		requestClient = lease.Client
		proxyID = lease.Proxy
	}
	// The pool lane (a real lease) leaves the session to the lease's RoundTrip;
	// the home leg and the sync direct-fallback pin their own single egress.
	ek := directHomeIPKey
	if lease != nil && lease.Proxy != "" && lease.Proxy != "direct-fallback" {
		ek = ""
	}
	res, status, err := r.doRequest(ctx, requestClient, p, key, body, ek, proxyID)
	if err != nil {
		return nil, status, proxyID, err
	}
	if lease != nil && res.Body != nil {
		res.Body = &leaseBody{ReadCloser: res.Body, lease: lease, ctx: ctx}
	}
	return res, status, proxyID, nil
}

type leaseBody struct {
	io.ReadCloser
	lease *Lease
	ctx   context.Context
}

func (b *leaseBody) Close() error {
	err := b.ReadCloser.Close()
	// Release with an independent context: the caller's ctx is normally
	// already cancelled by the time a stream's body is closed (client
	// timeout, abort), which would make Release a no-op and leak the lease.
	if e := b.lease.Release(context.Background()); err == nil {
		err = e
	}
	return err
}

// doRequestPooled is retained only for source compatibility with older tests.
// New code uses the generic transport capability.
func (r *Router) doRequestPooled(ctx context.Context, cl *http.Client, p *provider.Provider, key *pool.Key, body []byte) (*http.Response, int, string, error) {
	return r.doRequestWithLease(ctx, cl, p, key, body, nil)
}

// proxiedThrough clones base's transport with an HTTP proxy pointing at the
// lease's HTTP endpoint. The original client is never mutated, so routing stays
// safe under concurrency.
func proxiedThrough(base *http.Client, lease *proxypool.Proxy) (*http.Client, error) {
	baseTr := http.DefaultTransport
	if base != nil && base.Transport != nil {
		baseTr = base.Transport
	}
	tr, ok := baseTr.(*http.Transport)
	if !ok {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		tr = tr.Clone()
	}
	proxyURL, err := url.Parse(proxyAddrScheme(lease.HTTP))
	if err != nil {
		return nil, err
	}
	tr.Proxy = http.ProxyURL(proxyURL)
	tr.DialTLSContext = utlsDialTLS
	out := &http.Client{Transport: tr}
	if base != nil {
		out.Timeout = base.Timeout
	}
	return out, nil
}

// proxyAddrScheme prefixes "http://" to a bare host:port proxy address from
// the egress lease (e.g. "127.0.0.1:8118").
func proxyAddrScheme(addr string) string {
	if strings.Contains(addr, "://") || addr == "" {
		return addr
	}
	return "http://" + addr
}

func (r *Router) emitFallback(p *provider.Provider, model string, status int) {
	if r.OnEvent == nil {
		return
	}
	r.OnEvent("ai.fallback", map[string]any{
		"provider": p.Name,
		"model":    model,
		"status":   status,
	})
}

func modelOf(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Model
}

// agentGateModels: zen Console гейтит free tier по tools-блоку в теле —
// требует tool с name=read И tool с name=bash (MITM-бисекция 2026-10-06:
// переименованный rx/bx=403, mini read+bash 498 байт=200, read/bash по
// одному=403, без tools=403). Без них вердикт "used from within OpenCode".
// Клиентские tools не трогаем никогда; докидываем mini-stub только когда
// tools нет вообще.
// 2026-10-17 (MITM-бисекция glm-5.3/deepseek-v4.1 через живой CLI): тот же
// гейт, плюс обязателен stream:true в теле. Список руками не ведём — гейтятся
// ВСЕ id с суффиксом "-free" (см. условие в withAgentTools); map оставлен
// для исторической справки и нестандартных имён.
var agentGateModels = map[string]bool{
	"mimo-v2.6-flash-free":        true,
	"mimo-v2.5-free":              true,
	"ling-3.0-flash-fin-free":     true,
	"nemotron-3-ultra-free":       true,
	"nemotron-3.5-lightning-free": true,
	"laguna-s-2.1-free":           true,
	"deepseek-v4-flash-free":      true,
	// muse-contributor: тот же zen free-gate — без read+bash вердикт
	// 403 [gate-terminal] «used from within OpenCode» (2026-10-12:
	// muse-spark-1.3 = 24/24 запросов 403, модель мертва).
	"muse-spark-1.3-contributor-free": true,
	"muse-spark-1.2-contributor-free": true,
}

// withAgentTools ensures the zen free-tier gate pair (read+bash) is present
// in chat bodies for gated models, or returns the original body when the
// model is not gated or body is not JSON. Client tools are preserved;
// missing gate names are backfilled (same forced pair as chatToolsToResponses
// on the responses path). Empty/absent tools get the mini stub.
func withAgentTools(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	mv, _ := m["model"].(string)
	base := mv
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	lb := strings.ToLower(base)
	// 2026-10-17: гейт шире ручного списка — любой "-free" id (glm-5.3,
	// deepseek-v4.1, будущие) проходит один и тот же zen free-gate.
	if !agentGateModels[lb] && !strings.HasSuffix(lb, "-free") {
		return body
	}
	var arr []any
	if t, ok := m["tools"]; ok && t != nil {
		a, ok := t.([]any)
		if !ok {
			return body
		}
		arr = a
	}
	have := map[string]bool{}
	for _, e := range arr {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := em["function"].(map[string]any); ok && fn != nil {
			if n, _ := fn["name"].(string); n != "" {
				have[n] = true
				continue
			}
		}
		if n, _ := em["name"].(string); n != "" {
			have[n] = true
		}
	}
	// Gate minimum is TWO tools (2026-10-23 bisect on /inference: full
	// system + 1 tool = 403, + 2 tools = 200, arbitrary definitions accepted).
	// Only read+bash are backfilled: the previous "append all 11 canonical
	// tools" made every gated request carry 22 KB of extra schema and gave the
	// model tools the client never declared. Client tools stay first;
	// stub-shaped copies of the two gate names are dropped so the canonical
	// definitions take their place.
	canonicalNames := map[string]bool{}
	for _, c := range gateToolNames {
		canonicalNames[c] = true
	}
	kept := arr[:0]
	for _, e := range arr {
		em, ok := e.(map[string]any)
		if !ok {
			kept = append(kept, e)
			continue
		}
		var fn map[string]any
		if f, ok := em["function"].(map[string]any); ok {
			fn = f
		} else {
			fn = em // already-flat responses shape
		}
		n, _ := fn["name"].(string)
		if canonicalNames[n] && isStubTool(fn) {
			delete(have, n) // drop the stub: the canonical definition is appended below
			continue
		}
		kept = append(kept, e)
	}
	arr = kept
	for _, c := range canonicalOpencodeTools {
		if !canonicalNames[c["name"].(string)] || have[c["name"].(string)] {
			continue
		}
		have[c["name"].(string)] = true
		arr = append(arr, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": c["name"], "description": c["description"], "parameters": c["parameters"],
				"strict": c["strict"],
			},
		})
	}
	m["tools"] = arr
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// withModel returns a copy of body with the model field replaced, or the
// original body when it is not valid JSON.
func withModel(body []byte, model string) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["model"] = model
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// WithReasoning returns a copy of body with explicit reasoning effort,
// unless the client already set any reasoning param (never override).
// Exported for the gateway hook: GUI override wins over family auto-max,
// client-explicit wins over both.
func WithReasoning(body []byte, effort string) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	for _, k := range []string{"reasoning", "reasoning_effort", "reasoning_level", "thinking", "reasoning_config", "thinking_level"} {
		if _, ok := m[k]; ok {
			return body
		}
	}
	// zen Console validates reasoning_effort per model family: muse-spark
	// accepts [minimal, low, medium, high, xhigh] — "max" earns a 400
	// (live 2026-09-10). "max" from the GUI is an alias for the family's
	// top level: xhigh for muse/mimo, max stays for x-preview.
	outEffort := effort
	if strings.EqualFold(strings.TrimSpace(effort), "max") {
		model := ""
		if mv, ok := m["model"]; ok {
			if ms, ok := mv.(string); ok {
				model = strings.ToLower(ms)
			}
		}
		if strings.Contains(model, "muse-spark") || strings.Contains(model, "mimo") {
			outEffort = "xhigh"
		}
	}
	m["reasoning_effort"] = outEffort
	m["reasoning"] = map[string]any{"effort": outEffort, "budget_tokens": 32768}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func newClient(p *provider.Provider) *http.Client {
	timeout := p.TimeoutS
	if timeout <= 0 {
		timeout = 300
	}
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 15 * time.Second,
		}).DialContext,
		// Randomized TLS fingerprint (JA3) per request: Go's default handshake
		// is identical for every Go program and a cross-IP link signal.
		DialTLSContext:    utlsDialTLS,
		ForceAttemptHTTP2: false,
	}
	if p.Egress != "" {
		if u, err := url.Parse(p.Egress); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   time.Duration(timeout) * time.Second,
	}
}
