// Package config loads ai-router's YAML configuration and locates its
// per-user state directory (%LOCALAPPDATA%\ai-router on Windows).
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is ai-router's YAML configuration.
type Config struct {
	Server      ServerConfig            `yaml:"server"`
	AdminToken  string                  `yaml:"admin_token"`
	Hub         HubConfig               `yaml:"hub"`
	Providers   map[string]ProviderConf `yaml:"providers"`
	ProvidersOrder []string             `yaml:"-"` // declaration order from YAML; empty -> sorted fallback
	Keys        []KeyConf               `yaml:"keys"`
	Resilience  ResilienceConfig        `yaml:"resilience"`
	Routing     RoutingConfig           `yaml:"routing"`
	Encryption  EncryptionConfig        `yaml:"encryption"`
	Telemetry   TelemetryConfig         `yaml:"telemetry"`
	Sanitize    SanitizeConfig          `yaml:"sanitize"`
	Idempotency IdempotencyConfig       `yaml:"idempotency"`
	Egress      EgressConfig            `yaml:"egress"`
	// Defaults — наследуемые провайдерами параметры (методика свапа ключей
	// и политика ошибок). Провайдер может переопределить их в своей секции.
	Defaults ProviderDefaults `yaml:"provider_defaults"`
	// Modules — зависимые модули для GUI-секции «Модули» (авто-старт, пути).
	Modules ModulesConfig `yaml:"modules"`
	// AutoStart registers this binary in HKCU Run when true so it starts
	// at Windows login (full gateway mode chosen by how it is launched).
	AutoStart bool `yaml:"autostart"`
}

// ModulesConfig describes the dependent modules omnigate can auto-start.
type ModulesConfig struct {
	Egress ModuleConf `yaml:"egress"`
	Warp   ModuleConf `yaml:"warp"`
	Hub    ModuleConf `yaml:"hub"`
}

// ModuleConf is one dependent module's launch settings.
type ModuleConf struct {
	Bin    string   `yaml:"bin,omitempty"`    // путь к исполняемому
	Args   []string `yaml:"args,omitempty"`   // аргументы (напр. -config ...)
	Health string   `yaml:"health,omitempty"` // http-проба
	Auto   bool     `yaml:"auto,omitempty"`   // авто-старт при включении
}

// ProviderDefaults — параметры, которые провайдеры наследуют по умолчанию.
type ProviderDefaults struct {
	KeySwap     string            `yaml:"key_swap"`
	ErrorPolicy map[string]string `yaml:"error_policy"`
}

// EgressConfig connects ai-router to the hub's egress proxy pool module
// (cmd/egress, default :8801). When Enabled, every outbound provider request
// is routed through a proxy lease acquired from the pool (per-request rotation
// gives every request a fresh channel); otherwise the per-provider static
// `egress` proxy URL (ProviderConf.Egress) is used as before.
type EgressConfig struct {
	Enabled  bool           `yaml:"enabled"`
	BaseURL  string         `yaml:"base_url"` // e.g. http://127.0.0.1:8801
	Token    string         `yaml:"token"`
	Strategy string         `yaml:"strategy"` // round_robin (default) | best_latency | sticky
	Rotation EgressRotation `yaml:"rotation"`
}

// EgressRotation tunes further lease rotation.
type EgressRotation struct {
	PerRequest  bool `yaml:"per_request"`  // each acquire returns a fresh channel
	RotateAfter int  `yaml:"rotate_after"` // rotate a channel after N uses (>0)
}

// ResilienceConfig tunes the three resilience layers: breaker, key cooldown
// and model lockout.
type ResilienceConfig struct {
	Breaker  BreakerConfig  `yaml:"breaker"`
	Cooldown CooldownConfig `yaml:"cooldown"`
	Lockout  LockoutConfig  `yaml:"lockout"`
}

// BreakerConfig is the per-provider circuit breaker.
type BreakerConfig struct {
	MaxFailures       int `yaml:"max_failures"`
	RecoverySeconds   int `yaml:"recovery_seconds"`
	HalfOpenSuccesses int `yaml:"half_open_successes"`
}

// CooldownConfig is the per-key exponential backoff.
type CooldownConfig struct {
	BaseSeconds       int     `yaml:"base_seconds"`
	MaxSeconds        int     `yaml:"max_seconds"`
	Factor            float64 `yaml:"factor"`
	RespectRetryAfter bool    `yaml:"respect_retry_after"`
}

// LockoutConfig disables a model across providers after repeated failures.
type LockoutConfig struct {
	MaxFailures    int `yaml:"max_failures"`
	WindowSeconds  int `yaml:"window_seconds"`
	LockoutSeconds int `yaml:"lockout_seconds"`
}

// RoutingConfig selects the provider-selection strategy and tier cascade.
type RoutingConfig struct {
	Strategy    string      `yaml:"strategy"` // priority|weighted|round-robin|p2c|cost|lkgp
	Tiers       []string    `yaml:"tiers"`    // cascade order, e.g. [subscription, api, cheap, free]
	DefaultTier string      `yaml:"default_tier"`
	Costs       CostsConfig `yaml:"costs"`
}

// CostsConfig enables cost-based accounting.
type CostsConfig struct {
	Enabled bool `yaml:"enabled"`
}

// EncryptionConfig points at the env var holding the at-rest key.
type EncryptionConfig struct {
	KeyEnv string `yaml:"key_env"`
}

// TelemetryConfig controls X-AiRouter-* response headers.
type TelemetryConfig struct {
	Headers bool `yaml:"headers"`
	Metrics bool `yaml:"metrics"`
}

// SanitizeConfig controls upstream header stripping and error sanitization.
type SanitizeConfig struct {
	StripHeaders []string `yaml:"strip_headers"`
	Errors       bool     `yaml:"errors"`
}

// IdempotencyConfig enables request dedup by X-Request-ID or body hash.
type IdempotencyConfig struct {
	Enabled    bool `yaml:"enabled"`
	TTLSeconds int  `yaml:"ttl_seconds"`
}

// ServerConfig is the OpenAI-compatible listener.
type ServerConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	LogLevel string `yaml:"log_level"`
}

// HubConfig configures registration as a hub module.
type HubConfig struct {
	URL      string `yaml:"url"`
	Module   string `yaml:"module"`
	Token    string `yaml:"token"`
	Addr     string `yaml:"addr"`
	Register bool   `yaml:"register"`
}

// TransportConf selects an optional independent outbound transport.
type TransportConf struct {
	Mode        string `yaml:"mode,omitempty"` // direct | static_proxy | warp | hub | iprotate | pool
	APIURL      string `yaml:"api_url,omitempty"`
	// PoolAPI selects the warp-router pool contract: "" or "v1" (default)
	// or "v2" (lease TTL/heartbeat/batch/epoch on /api/v2/pool/*). v2 is
	// additive warp-side; the flag flips only this transport's dialect.
	PoolAPI string `yaml:"pool_api,omitempty"`
	ProxyURL    string `yaml:"proxy_url,omitempty"`
	Token       string `yaml:"token,omitempty"`
	PerRequest  bool   `yaml:"per_request,omitempty"`
	RotateAfter int    `yaml:"rotate_after,omitempty"`
	// DirectFallback — для mode: pool. Если пул (warp-router) недоступен,
	// не падать с ошибкой, а слать запросы напрямую с нашего IP.
	DirectFallback bool `yaml:"direct_fallback,omitempty"`
	// AllowHomeIPDirect — ВТОРОЙ ключ к DirectFallback. Даже при
	// direct_fallback: true прямое подключение с нашего IP разрешается
	// только если этот флаг выставлен явно. Причина (2026-10-19): пока
	// пул лежал, direct-лизы уходили с домашнего IP в opencode, апстрим
	// пометил его («detected abusive traffic from this IP»), и мы получили
	// 49 × 403 за сутки — все с proxy='' (= direct), ноль при выходе через
	// пул. Теперь прямой выход по умолчанию запрещён: честный 503 «pool has
	// no ready egress» вместо бана нашего адреса.
	AllowHomeIPDirect bool `yaml:"allow_home_ip_direct,omitempty"`
	// JitterTail — defense-in-depth (по умолчанию выключено): переписывать
	// кодовые точки пробелов в ХВОСТЕ тела запроса (внутри последнего
	// "content", вне кэшируемого префикса), чтобы одинаковые запросы не
	// уходили с разных выходов байт-в-байт.
	JitterTail bool `yaml:"jitter_tail,omitempty"`
	// MaxEgressInflight — мягкий предел одновременных запросов через один
	// egress пула (0 = выключено). Не блокирует: при выборе среди
	// кандидатов занятые сверх предела уступают свободным.
	MaxEgressInflight int `yaml:"max_egress_inflight,omitempty"`
	// WaitForLeaseS — сколько секунд запрос ЖДЁТ один egress, когда пул
	// выгорел, прежде чем отдать честный 503. Клиент ретраит ограниченное
	// число раз (OMP: 10), и мгновенный 503 сжигает ретрай, не давая
	// ротации (respawn воркеров 30-90с) ни шанса. 0 = старое поведение
	// (fail-fast), по умолчанию 30с.
	WaitForLeaseS int `yaml:"wait_for_lease_s,omitempty"`
	// IPRotate — параметры IP-ротации (mode: iprotate): при 429 сразу
	// отправляются Fanout параллельных запросов на разные IP хоста, ответившие
	// попадают в пул, следующий запрос идёт на любой IP из пула; если IP из
	// пула снова 429 — используются те, что отвечали без 429; если все 429 —
	// разведка повторяется до Rounds раундов.
	Fanout int `yaml:"fanout,omitempty"`
	Rounds int `yaml:"rounds,omitempty"`
	// CooldownS — сколько секунд IP, ответивший 429, не используется.
	CooldownS int `yaml:"cooldown_s,omitempty"`
	// StormCollapse — для mode: pool (nil = включено, как было). false —
	// провайдер исключён из storm-collapse: при шторме из коротких 429
	// (окна reopen за секунды, без часовых Retry-After) веер остаётся
	// полным (fanout×rounds) вместо 1×1. Кило keyless: баны — 5-секундные
	// парки, коллапс превращал reopening-пул в преждеверные 503.
	StormCollapse *bool `yaml:"storm_collapse,omitempty"`
	// ResolveTTLS — как часто перечитывать DNS хоста для пополнения пула IP.
	ResolveTTLS int `yaml:"resolve_ttl_s,omitempty"`
	// SeedIPs — стартовый список IP хоста (пул разведки), помимо DNS.
	SeedIPs []string `yaml:"seed_ips,omitempty"`
}

// RequestConf describes endpoint-level request capabilities.
type RequestConf struct {
	Protocol string `yaml:"protocol,omitempty"` // openai-chat
	Stream   string `yaml:"stream,omitempty"`   // passthrough
	Tools    string `yaml:"tools,omitempty"`    // passthrough
	Usage    string `yaml:"usage,omitempty"`    // passthrough
}

// ProviderConf is one upstream endpoint adapter. Type is "openai-compatible".
type ProviderConf struct {
	Type      string        `yaml:"type"`
	BaseURL   string        `yaml:"base_url"`
	APIKeys   []string      `yaml:"api_keys"`
	AuthMode  string        `yaml:"auth_mode,omitempty"`  // bearer | none | public
	UserAgent string        `yaml:"user_agent,omitempty"` // кастомный User-Agent (обязателен для opencode.ai/zen/v1)
	// ResponsesAPI: upstream speaks the OpenAI Responses protocol
	// (POST /responses, "input" array, response.output_text.* SSE) instead
	// of chat/completions. The router converts chat requests and maps the
	// SSE back so every client keeps speaking plain chat.
	ResponsesAPI bool `yaml:"responses_api,omitempty"`
	// ResponsesModels: model ids on this provider that speak the Responses
	// protocol while the rest stay on chat/completions (zen: muse-spark vs
	// x-preview on the same keyless endpoint).
	ResponsesModels []string `yaml:"responses_models,omitempty"`

	Models    []string      `yaml:"models"`
	DisabledModels []string `yaml:"disabled_models,omitempty"`
	HiddenModels   []string `yaml:"hidden_models,omitempty"`
	QuotaMode      string   `yaml:"quota_mode,omitempty"` // auto|usd|tokens
	Fallback  []string      `yaml:"fallbacks"`
	Egress    string        `yaml:"egress,omitempty"` // static proxy URL (optional)
	Transport TransportConf `yaml:"transport,omitempty"`
	Request   RequestConf   `yaml:"request,omitempty"`
	TimeoutS  int           `yaml:"timeout_s,omitempty"`

	Tier         string  `yaml:"tier,omitempty"` // subscription|api|cheap|free
	Weight       float64 `yaml:"weight,omitempty"`
	CostPer1MIn  float64 `yaml:"cost_per_1m_in,omitempty"`
	CostPer1MOut float64 `yaml:"cost_per_1m_out,omitempty"`

	// KeySwap — как выбирать ключ из пула провайдера: round_robin (default),
	// random, least_used, best_latency.
	KeySwap string `yaml:"key_swap,omitempty"`
	// ErrorPolicy — что делать при конкретной ошибке: none (не повторять),
	// retry (повторить), retry_proxy (повторить со сменой прокси),
	// retry_token (повторить со сменой ключа). Ключ — код/тип ошибки
	// (401, 429, 5xx, timeout, network, rate_limit).
	ErrorPolicy map[string]string `yaml:"error_policy,omitempty"`
	// DisabledKeys — sha256-хэши ключей (pool KeyHash), выключенных вручную
	// через GUI. Хэши, не значения: переживают рестарт, применяются в route.Add.
	DisabledKeys []string `yaml:"disabled_keys,omitempty"`
	// KeyProxies — hash->proxy URL, привязка прокси к ключу из GUI.
	KeyProxies map[string]string `yaml:"key_proxies,omitempty"`
	// DefaultKey — хэш приоритетного ключа (один), выставляется звездой в GUI.
	DefaultKey string `yaml:"default_key,omitempty"`
	// ModelReasoning — per-model reasoning override: ""/auto = gateway
	// auto-detect (ensureReasoningMax), иначе explicit effort (xhigh/max/high/
	// low/custom) — gateway passthrough, enforcement в route.tryProvider.
	ModelReasoning map[string]string `yaml:"model_reasoning,omitempty"`
}

// KeyConf is a virtual key with optional quotas and model allowlist.
// Quota values of 0 mean unlimited. Empty Models means "all models".
type KeyConf struct {
	Name        string   `yaml:"name"`
	Key         string   `yaml:"key"`
	QuotaTokens int64    `yaml:"quota_tokens,omitempty"`
	QuotaReqs   int64    `yaml:"quota_reqs,omitempty"`
	Models      []string `yaml:"models,omitempty"`
	BudgetUSD   int64    `yaml:"budget_usd,omitempty"` // monthly spend cap, USD cents
	NoLog       bool     `yaml:"no_log,omitempty"`     // skip audit/usage logging
}

// UnmarshalYAML preserves declaration order of providers in ProvidersOrder.
// It decodes via an alias to keep Default()-seeded true bools for missing keys
// (Telemetry.Headers etc.) and extracts providers key order from the YAML node.
func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	type plain Config
	if err := node.Decode((*plain)(c)); err != nil {
		return err
	}
	cur := node
	if cur.Kind == yaml.DocumentNode && len(cur.Content) > 0 {
		cur = cur.Content[0]
	}
	if cur.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(cur.Content)-1; i += 2 {
		k := cur.Content[i]
		v := cur.Content[i+1]
		if k.Value == "providers" && v.Kind == yaml.MappingNode {
			order := make([]string, 0, len(v.Content)/2)
			for j := 0; j < len(v.Content); j += 2 {
				order = append(order, v.Content[j].Value)
			}
			c.ProvidersOrder = append([]string(nil), order...)
			break
		}
	}
	return nil
}

// Default returns a config with sane defaults; AdminToken is blank until
// Ensure is run.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Host:     "127.0.0.1",
			Port:     9001,
			LogLevel: "info",
		},
		Hub: HubConfig{
			URL:      "http://127.0.0.1:8700",
			Module:   "ai-router",
			Addr:     "127.0.0.1:9002",
			Register: true,
		},
		Providers: map[string]ProviderConf{},
		Resilience: ResilienceConfig{
			Breaker:  BreakerConfig{MaxFailures: 5, RecoverySeconds: 30, HalfOpenSuccesses: 2},
			Cooldown: CooldownConfig{BaseSeconds: 60, MaxSeconds: 3600, Factor: 2.0, RespectRetryAfter: true},
			Lockout:  LockoutConfig{MaxFailures: 10, WindowSeconds: 300, LockoutSeconds: 900},
		},
		Routing: RoutingConfig{
			Strategy:    "priority",
			Tiers:       []string{"subscription", "api", "cheap", "free"},
			DefaultTier: "free",
			Costs:       CostsConfig{Enabled: false},
		},
		Encryption:  EncryptionConfig{KeyEnv: "STORAGE_ENCRYPTION_KEY"},
		Telemetry:   TelemetryConfig{Headers: true, Metrics: true},
		Sanitize:    SanitizeConfig{Errors: true},
		Idempotency: IdempotencyConfig{Enabled: false, TTLSeconds: 5},
		Defaults: ProviderDefaults{
			KeySwap: "round_robin",
			ErrorPolicy: map[string]string{
				"401":        "retry_token",
				"403":        "none",
				"408":        "retry_proxy",
				"429":        "retry",
				"524":        "retry_proxy",
				"5xx":        "retry",
				"timeout":    "retry_proxy",
				"network":    "retry_proxy",
				"rate_limit": "retry",
			},
		},
		Modules: ModulesConfig{
			Egress: ModuleConf{Auto: true},
			Warp:   ModuleConf{Auto: true},
			Hub:    ModuleConf{Auto: true},
		},
	}
}

// applyDefaults fills zero-valued fields with defaults.
// Load/Read now start from Default() and overlay YAML, so bool true
// defaults (Telemetry.Headers/Metrics, Hub.Register, Sanitize.Errors,
// Cooldown.RespectRetryAfter) are already correct and are not forced here
// — this keeps explicit `false` disableable. This helper only fills
// sentinel-0 ints/strings that YAML left zero.
func (c *Config) applyDefaults() {
	d := Default()
	if c.Server.Host == "" {
		c.Server.Host = d.Server.Host
	}
	if c.Server.Port == 0 {
		c.Server.Port = d.Server.Port
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = d.Server.LogLevel
	}
	if c.Hub.URL == "" {
		c.Hub.URL = d.Hub.URL
	}
	if c.Hub.Module == "" {
		c.Hub.Module = d.Hub.Module
	}
	if c.Hub.Addr == "" {
		c.Hub.Addr = d.Hub.Addr
	}
	if c.Providers == nil {
		c.Providers = map[string]ProviderConf{}
	}
	if c.Defaults.KeySwap == "" {
		c.Defaults.KeySwap = d.Defaults.KeySwap
	}
	if c.Defaults.ErrorPolicy == nil {
		c.Defaults.ErrorPolicy = map[string]string{}
		for k, v := range d.Defaults.ErrorPolicy {
			c.Defaults.ErrorPolicy[k] = v
		}
	}
	if c.Resilience.Breaker.MaxFailures == 0 {
		c.Resilience.Breaker.MaxFailures = d.Resilience.Breaker.MaxFailures
	}
	if c.Resilience.Breaker.RecoverySeconds == 0 {
		c.Resilience.Breaker.RecoverySeconds = d.Resilience.Breaker.RecoverySeconds
	}
	if c.Resilience.Breaker.HalfOpenSuccesses == 0 {
		c.Resilience.Breaker.HalfOpenSuccesses = d.Resilience.Breaker.HalfOpenSuccesses
	}
	if c.Resilience.Cooldown.BaseSeconds == 0 {
		c.Resilience.Cooldown.BaseSeconds = d.Resilience.Cooldown.BaseSeconds
	}
	if c.Resilience.Cooldown.MaxSeconds == 0 {
		c.Resilience.Cooldown.MaxSeconds = d.Resilience.Cooldown.MaxSeconds
	}
	if c.Resilience.Cooldown.Factor == 0 {
		c.Resilience.Cooldown.Factor = d.Resilience.Cooldown.Factor
	}
	if c.Resilience.Lockout.MaxFailures == 0 {
		c.Resilience.Lockout.MaxFailures = d.Resilience.Lockout.MaxFailures
	}
	if c.Resilience.Lockout.WindowSeconds == 0 {
		c.Resilience.Lockout.WindowSeconds = d.Resilience.Lockout.WindowSeconds
	}
	if c.Resilience.Lockout.LockoutSeconds == 0 {
		c.Resilience.Lockout.LockoutSeconds = d.Resilience.Lockout.LockoutSeconds
	}
	if c.Routing.Strategy == "" {
		c.Routing.Strategy = d.Routing.Strategy
	}
	if len(c.Routing.Tiers) == 0 {
		c.Routing.Tiers = d.Routing.Tiers
	}
	if c.Routing.DefaultTier == "" {
		c.Routing.DefaultTier = d.Routing.DefaultTier
	}
	if c.Encryption.KeyEnv == "" {
		c.Encryption.KeyEnv = d.Encryption.KeyEnv
	}
	if c.Idempotency.TTLSeconds == 0 {
		c.Idempotency.TTLSeconds = d.Idempotency.TTLSeconds
	}
}

// Addr returns host:port for the OpenAI endpoint.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}

// LocalAppData returns %LOCALAPPDATA% or the user config dir as fallback.
func LocalAppData() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return v
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "."
	}
	return dir
}

// BaseDir is the per-user ai-router directory: %LOCALAPPDATA%\ai-router.
func BaseDir() string { return filepath.Join(LocalAppData(), "ai-router") }

// ConfigPath is the config file location.
func ConfigPath() string { return filepath.Join(BaseDir(), "config.yaml") }

// StatePath is the state file location.
func StatePath() string { return filepath.Join(BaseDir(), "state.json") }

// PoolStatePathFor returns the pool egress state file (429 class-bans,
// Retry-After deadlines, ip_ok/ctrl probes) for a server port and an optional
// provider qualifier:
//
//	PoolStatePathFor(0, "")            -> pool_state.json
//	PoolStatePathFor(9003, "")         -> pool_state_9003.json
//	PoolStatePathFor(0, "kilocode")    -> pool_state_kilocode.json
//	PoolStatePathFor(9001, "kilocode") -> pool_state_kilocode__9001.json
//
// The port is a discriminator, not decoration: two instances that share BaseDir
// (same LOCALAPPDATA) but listen on different ports — LIVE :9001 next to a
// :9003 smoke stand — used to collapse onto one pool_state.json and overwrote
// each other's 8h per-IP bans last-wins, so a bench stand wiped LIVE's burned
// windows and LIVE re-stormed them from scratch. There is no :9001 exemption
// on purpose: that exemption was the collision. Port 0 keeps the
// pre-discriminator spelling for callers without a server port (libraries,
// tests, and the legacy-read fallback in route.loadPoolState). The provider
// qualifier keeps per-provider ban/heat state from parking another provider's
// egress (2026-10-12).
func PoolStatePathFor(port int, provider string) string {
	stem := "pool_state"
	if provider != "" {
		stem += "_" + provider
	}
	if port > 0 {
		stem += fmt.Sprintf("__%d", port)
	}
	return filepath.Join(BaseDir(), stem+".json")
}

// DBPath is the SQLite state database location.
func DBPath() string { return filepath.Join(BaseDir(), "state.db") }

// Load reads the config from path, generating a fresh file with a random
// admin token on first run.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		c := Default()
		if err := yaml.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
		c.applyDefaults()
		return c, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	c := Default()
	c.AdminToken = randHex(32) // 64 hex chars
	if err := Save(c, path); err != nil {
		return nil, err
	}
	return c, nil
}

// Read loads the config without creating it on first run (for CLI use).
func Read(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := Default()
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	c.applyDefaults()
	return c, nil
}
// Save writes the config atomically, creating parent directories.
func Save(c *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable at startup
	}
	return hex.EncodeToString(b)
}

// writeFileAtomic writes data to path via a temp file + rename.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
