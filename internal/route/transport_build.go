package route

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/proxypool"
)

// BuildTransportOption resolves the single outbound transport implied by a
// config.Config and returns the matching Option, or nil when the config asks
// for direct connections everywhere. Every server entrypoint (cmd/ai-router,
// cmd/omnigate-gui) MUST call this so egress/pool/warp routing behaves
// identically regardless of which binary is serving traffic; skipping it
// silently turns a configured proxy/pool requirement into a direct-connect
// fail-open.
//
// Precedence mirrors the historical cmd/ai-router wiring: hub egress pool
// first (cfg.Egress), then a provider-declared "pool" transport, then a
// provider-declared "warp" transport. Only the first provider found using
// "pool" or "warp" is used, matching prior single-transport behavior.
func BuildTransportOption(cfg *config.Config, hubPool *proxypool.Client) Option {
	switch {
	case cfg.Egress.Enabled && cfg.Egress.BaseURL != "" && hubPool != nil:
		return WithTransport(HubTransport{Pool: hubPool})
	default:
		if pt := poolTransport(cfg); pt != nil {
			return WithTransport(pt)
		}
		if tp := directWarpTransport(cfg); tp != nil {
			return WithTransport(tp)
		}
		if ip := ipRotateTransport(cfg); ip != nil {
			return WithTransport(ip)
		}
	}
	return nil
}
func cfgURL(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

// poolGroup мультиплексирует per-provider PoolTransport'ы за одним
// TransportProvider. Каждый pool-провайдер получает СВОЙ транспорт: свой
// ban/heat/storm state, свой state-файл и свои fanout/rounds/token —
// один общий транспорт (и сидинг настроек от «первого в map-order»,
// то есть случайного) делал шторм одного провайдера парковкой egress для
// всех остальных (2026-10-12: кило-503 24% при opencode 0.8% в те же минуты).
type poolGroup struct {
	byProv map[string]TransportProvider
}

func (g *poolGroup) Name() string { return "pool" }

// IdentityGen sums the recycles of every pooled provider: any rotate changes
// the value, which is all the Router needs to invalidate per-model verdicts.
func (g *poolGroup) IdentityGen() uint64 {
	var sum uint64
	for _, t := range g.byProv {
		if pg, ok := t.(interface{ IdentityGen() uint64 }); ok {
			sum += pg.IdentityGen()
		}
	}
	return sum
}

// SelfRotating сохраняет внешний контракт: ротация лизов внутри пула.
func (g *poolGroup) SelfRotating() bool { return true }

func (g *poolGroup) FlushState() {
	for _, pt := range g.byProv {
		if sf, ok := pt.(StateFlusher); ok {
			sf.FlushState()
		}
	}
}

func (g *poolGroup) Acquire(ctx context.Context, req RequestContext, prov *provider.Provider) (*Lease, error) {
	if pt, ok := g.byProv[req.Provider]; ok && pt != nil {
		return pt.Acquire(ctx, req, prov)
	}
	return nil, fmt.Errorf("pool: no transport for provider %q", req.Provider)
}

// directProviderTransport serves a provider with transport.mode: direct —
// plain host HTTP with no proxy. Used for local endpoints (e.g. the
// opencode-cli bridge on 127.0.0.1) that must never be dialed through the
// egress pool.
//
// One http.Client is built per transport (see newDirectProviderTransport) and
// shared by every lease: the client owns the idle-connection pool, so the old
// per-Acquire &http.Client{} leaked a fresh pool — and its sockets — per
// request, and carried no Timeout at all, so a wedged local bridge parked the
// caller's request slot forever.
type directProviderTransport struct {
	client *http.Client
}

func (t *directProviderTransport) Name() string { return "direct" }

func (t *directProviderTransport) Acquire(_ context.Context, _ RequestContext, _ *provider.Provider) (*Lease, error) {
	return &Lease{Client: t.client, Proxy: ""}, nil
}

// directTimeoutS is the per-request budget for a direct endpoint whose provider
// declares no timeout_s; it mirrors newClient (route.go) so both request paths
// behave identically. 300s, not 60s: the canonical direct endpoint is the local
// opencode-cli bridge, which shells `opencode run` under its own 240s internal
// cap — a shorter client timeout would abort the request before the bridge
// could answer, turning a slow-but-successful model turn into a transport error
// and discarding the CLI's real output. The live config asks for exactly this
// (opencode-cli timeout_s: 300).
const directTimeoutS = 300

func newDirectProviderTransport(timeoutS int) *directProviderTransport {
	if timeoutS <= 0 {
		timeoutS = directTimeoutS
	}
	return &directProviderTransport{client: &http.Client{
		Timeout: time.Duration(timeoutS) * time.Second,
		// Transport/Proxy left nil-default on purpose: "direct" means plain
		// HTTP from this host, never the egress pool or a warp tunnel.
	}}
}

// loadPoolState restores persisted ban/heat/storm state into pt. The
// port-qualified file (pool_state_<name>__<port>.json) is the only file ever
// written; when it does not exist yet — first boot after the port discriminator
// was introduced — the pre-discriminator pool_state_<name>.json is read once,
// so a promoted binary does not forget /48 windows it had already parked and
// re-storm them from scratch. The legacy file is never migrated, rewritten, or
// deleted: the fallback is read-only, and as soon as the primary exists (or is
// saved) the legacy copy is ignored. Restore is best-effort — a missing or
// corrupt file means a cold start, exactly like PoolTransport.loadState.
func loadPoolState(pt *PoolTransport, legacy string) {
	primary := pt.StateFile
	if stateFileExists(primary) {
		pt.StateFile = primary
		pt.loadState()
		return
	}
	if legacy != primary && stateFileExists(legacy) {
		pt.StateFile = legacy
		pt.loadState()
	}
	pt.StateFile = primary
}

// stateFileExists reports whether path is a regular file. A directory or an
// unreadable path counts as absent — state restore is best-effort, never fatal.
func stateFileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// leaseWaitFromConfig resolves the wait-for-one-egress budget: explicit YAML
// wins, AI_ROUTER_POOL_WAIT_S overrides it for stands, and the default keeps
// a burned pool from answering 503 in two milliseconds (clients give up after
// a handful of retries while a rotate needs 30-90s to respawn workers).
func leaseWaitFromConfig(secs int) time.Duration {
	if v := strings.TrimSpace(os.Getenv("AI_ROUTER_POOL_WAIT_S")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	if secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 20 * time.Second
}

func poolTransport(cfg *config.Config) TransportProvider {
	group := &poolGroup{byProv: map[string]TransportProvider{}}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	// Детерминированный порядок: раньше настройки сидились от «первого в
	// map-order» (Go map — случайный на каждый бут).
	sort.Strings(names)
	for _, name := range names {
		p := cfg.Providers[name]
		if p.Transport.Mode == "direct" {
			// Explicit direct: plain HTTP from this host, never the egress
			// pool. Without this the group errors "no transport for
			// provider" for every non-pool provider once any provider
			// declares pool mode (2026-10-13: local bridges unreachable).
			group.byProv[name] = newDirectProviderTransport(p.TimeoutS)
			continue
		}
		if p.Transport.Mode != "pool" {
			continue
		}
		pt := &PoolTransport{
			// pool_api: v2 — lease TTL/heartbeat/batch/epoch dialect.
			// Default ("" / "v1") keeps the v1 contract byte-for-byte.
			PoolV2:         p.Transport.PoolAPI == "v2",
			DirectFallback: p.Transport.DirectFallback,
			// Второй ключ: без него direct_fallback игнорируется (домашний
			// IP не уходит в апстрим, честный 503 вместо 403-бана).
			AllowHomeIPDirect: p.Transport.AllowHomeIPDirect,
			MaxEgressInflight: p.Transport.MaxEgressInflight,
			WaitForLease:      leaseWaitFromConfig(p.Transport.WaitForLeaseS),
			Fanout:            p.Transport.Fanout,
			Rounds:            p.Transport.Rounds,
			// storm_collapse: false = провайдер вне storm-collapse (kilo
			// keyless: баны 5s — коллапс 1x1 давал преждеверные 503).
			NoStormCollapse: p.Transport.StormCollapse != nil && !*p.Transport.StormCollapse,
			Providers:       map[string]bool{name: true},
			ProviderKeys:    map[string][]string{name: p.APIKeys},
			Keys:            p.APIKeys,
			APIURL:          cfgURL(p.Transport.APIURL, "http://127.0.0.1:8787"),
			Token:           p.Transport.Token,
			// Свой state-файл, ещё и с портом в имени: ban/heat/storm
			// соседнего провайдера не паркует наш egress — окна разные
			// ((IP, UA)-гейт), парки были общими; а два инстанса с общим
			// LOCALAPPDATA на разных портах (LIVE :9001 и стенд :9003) не
			// перезаписывают 8h-баны друг другу last-wins. Пишется только
			// новое имя; легаси pool_state_<name>.json читается один раз
			// как fallback (loadPoolState).
			StateFile: config.PoolStatePathFor(cfg.Server.Port, name),
		}
		loadPoolState(pt, config.PoolStatePathFor(0, name))
		group.byProv[name] = pt
		pt.startRotatePolicy()
	}
	if len(group.byProv) == 0 {
		return nil
	}
	return group
}

func directWarpTransport(cfg *config.Config) *WarpTransport {
	for _, p := range cfg.Providers {
		if p.Transport.Mode != "warp" {
			continue
		}
		return &WarpTransport{
			APIURL:     cfgURL(p.Transport.APIURL, "http://127.0.0.1:8787"),
			Token:      p.Transport.Token,
			ProxyURL:   cfgURL(p.Transport.ProxyURL, "http://127.0.0.1:8118"),
			PerRequest: p.Transport.PerRequest,
		}
	}
	return nil
}

func ipRotateTransport(cfg *config.Config) *IPRotateTransport {
	for _, p := range cfg.Providers {
		if p.Transport.Mode != "iprotate" {
			continue
		}
		if strings.TrimSpace(p.BaseURL) == "" {
			continue
		}
		host := p.Transport.APIURL
		port := ""
		if host == "" {
			u, err := url.Parse(p.BaseURL)
			if err != nil {
				continue
			}
			host = u.Hostname()
			port = u.Port()
			if port == "" {
				if u.Scheme == "http" {
					port = "80"
				} else {
					port = "443"
				}
			}
		}
		return &IPRotateTransport{
			Host:       host,
			Port:       port,
			Fanout:     p.Transport.Fanout,
			Rounds:     p.Transport.Rounds,
			Cooldown:   time.Duration(p.Transport.CooldownS) * time.Second,
			ResolveTTL: time.Duration(p.Transport.ResolveTTLS) * time.Second,
			SeedIPs:    p.Transport.SeedIPs,
		}
	}
	return nil
}
