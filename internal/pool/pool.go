// Package pool manages provider API-key pools: least-used selection, health
// tracking, temporary disable, rotation on 401/429 and exponential backoff
// with Retry-After support.
package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNoKeys is returned when a pool has no usable key.
var ErrNoKeys = errors.New("no usable key in pool")

// Key is one upstream credential with runtime health state.
type Key struct {
	Value            string
	Usage            int64 // number of requests routed to this key
	Failures         int
	Disabled         bool
	DisabledAt       time.Time
	LastErr          string
	BackoffLevel     int       // exponential backoff stage
	RateLimitedUntil time.Time // do not use before this instant
	Default          bool      // preferred key: picked first while healthy
	Proxy            string    // proxy URL pinned to this key ("" = провайдерский)
}

// KeyHash returns the stable lookup key for persistence.
func (k *Key) KeyHash() string {
	sum := sha256.Sum256([]byte(k.Value))
	return hex.EncodeToString(sum[:])
}

// KeyState is the persisted subset of a key's health (see pool_keys table).
type KeyState struct {
	Provider         string
	KeyHash          string
	ValueEnc         string
	Failures         int
	Disabled         bool
	DisabledUntil    time.Time
	BackoffLevel     int
	RateLimitedUntil time.Time
	LastErr          string
}

// Config tunes pool behaviour.
type Config struct {
	Mode              string
	Cooldown          time.Duration // disable duration after repeated failures
	DisableAfter      int           // consecutive failures before disable
	BackoffBase       time.Duration // exponential backoff base
	BackoffMax        time.Duration // backoff cap
	BackoffFactor     float64
	RespectRetryAfter bool
	// Persist persists key state (may be nil). Encrypt transforms the value
	// for at-rest storage (may be nil).
	Persist func(KeyState) error
	Encrypt func(string) string
}

// Pool is a concurrency-safe key pool for one provider.
type Pool struct {
	mu           sync.Mutex
	name         string
	keys         []*Key
	rr           int // round-robin cursor
	cooldown     time.Duration
	disableAfter int
	mode         string
	cfg          Config
}

// New creates a pool from key values.
func New(name string, values []string, cfg Config) *Pool {
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 60 * time.Second
	}
	if cfg.DisableAfter <= 0 {
		cfg.DisableAfter = 3
	}
	if cfg.Mode == "" {
		cfg.Mode = "least-used"
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = 60 * time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = time.Hour
	}
	if cfg.BackoffFactor == 0 {
		cfg.BackoffFactor = 2.0
	}
	p := &Pool{
		name:         name,
		cooldown:     cfg.Cooldown,
		disableAfter: cfg.DisableAfter,
		mode:         cfg.Mode,
		cfg:          cfg,
	}
	for _, v := range values {
		p.keys = append(p.keys, &Key{Value: v})
	}
	return p
}

// Len returns the number of keys in the pool.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.keys)
}

// SetEnabled включает/выключает ключ по его хэшу (KeyHash). Выключенные
// ключи пропускаются при Pick до повторного включения.
func (p *Pool) SetEnabled(hash string, enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.keys {
		if k.KeyHash() == hash {
			if enabled {
				k.Disabled = false
				k.Failures = 0
				k.DisabledAt = time.Time{}
			} else {
				k.Disabled = true
				k.DisabledAt = time.Now().Add(24 * time.Hour * 365 * 10) // практический «вручную»
			}
			return nil
		}
	}
	return fmt.Errorf("ключ не найден в пуле")
}

// SetDefault помечает ключ как используемый по дефолту (приоритетный).
// Дефолтным может быть только один ключ пула.
func (p *Pool) SetDefault(hash string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	found := false
	for _, k := range p.keys {
		if k.KeyHash() == hash {
			k.Default = true
			found = true
		} else {
			k.Default = false
		}
	}
	if !found {
		return fmt.Errorf("ключ не найден в пуле")
	}
	return nil
}

// SetProxy привязывает прокси-URL к конкретному ключу ("" — снять).
func (p *Pool) SetProxy(hash, proxy string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.keys {
		if k.KeyHash() == hash {
			k.Proxy = proxy
			return nil
		}
	}
	return fmt.Errorf("ключ не найден в пуле")
}

// KeyView is one key's snapshot for UI/audit (masked, no secrets).
type KeyView struct {
	Hash        string
	Masked      string
	Enabled     bool
	BackoffSecs int64
	Failures    int
	Default     bool
	Proxy       string
}

// SnapshotKeys returns per-key UI state (masked). Assumes callers read-only.
func (p *Pool) SnapshotKeys() []KeyView {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]KeyView, 0, len(p.keys))
	for _, k := range p.keys {
		kv := KeyView{
			Hash:     k.KeyHash(),
			Masked:   maskKey(k.Value),
			Enabled:  !k.Disabled,
			Failures: k.Failures,
			Default:  k.Default,
			Proxy:    k.Proxy,
		}
		if now.Before(k.RateLimitedUntil) {
			kv.BackoffSecs = int64(k.RateLimitedUntil.Sub(now) / time.Second)
		} else if !k.DisabledAt.IsZero() && now.Before(k.DisabledAt) && k.Disabled {
			kv.BackoffSecs = int64(k.DisabledAt.Sub(now) / time.Second)
		}
		out = append(out, kv)
	}
	return out
}

// Pick returns the best healthy key, or ErrNoKeys when all are disabled.
// Rate-limited keys are skipped while alternatives exist; if every healthy
// key is rate-limited, the least-restricted one is used (never full lockout).
func (p *Pool) Pick() (*Key, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// Re-enable expired disables.
	for _, k := range p.keys {
		if k.Disabled && now.After(k.DisabledAt) {
			k.Disabled = false
			k.Failures = 0
		}
	}
	healthy := make([]*Key, 0, len(p.keys))
	for _, k := range p.keys {
		if !k.Disabled {
			healthy = append(healthy, k)
		}
	}
	if len(healthy) == 0 {
		return nil, ErrNoKeys
	}
	// Ключ «по дефолту» (если включён и не в rate-limit) — приоритетный.
	if def := p.findDefault(healthy, now); def != nil {
		def.Usage++
		return def, nil
	}
	var best *Key
	switch p.mode {
	case "round-robin":
		best = p.pickRoundRobin(healthy, now)
	case "random":
		best = healthy[rand.Intn(len(healthy))]
	default: // least-used
		best = p.pickLeastUsed(healthy, now)
	}
	if best == nil {
		return nil, ErrNoKeys
	}
	best.Usage++
	return best, nil
}

// findDefault возвращает включённый дефолтный ключ вне rate-limit окна,
// если такой есть. Assumes p.mu held.
func (p *Pool) findDefault(keys []*Key, now time.Time) *Key {
	for _, k := range keys {
		if k.Default && !now.Before(k.RateLimitedUntil) {
			return k
		}
	}
	return nil
}

// pickLeastUsed prefers keys whose rate-limit window has expired; among the
// rate-limited ones it picks the earliest expiry. Assumes p.mu held.
func (p *Pool) pickLeastUsed(keys []*Key, now time.Time) *Key {
	var free, limited *Key
	for _, k := range keys {
		if now.Before(k.RateLimitedUntil) {
			if limited == nil || k.RateLimitedUntil.Before(limited.RateLimitedUntil) {
				limited = k
			}
			continue
		}
		if free == nil || k.Usage < free.Usage {
			free = k
		}
	}
	if free != nil {
		return free
	}
	return limited // degraded: everything rate-limited, use the soonest-free
}

// pickRoundRobin prefers the next non-rate-limited key. Assumes p.mu held.
func (p *Pool) pickRoundRobin(keys []*Key, now time.Time) *Key {
	for i := 0; i < len(p.keys); i++ {
		cand := p.keys[(p.rr+i)%len(p.keys)]
		if cand.Disabled {
			continue
		}
		p.rr = (p.rr + i + 1) % len(p.keys)
		return cand
	}
	// All disabled — degraded: re-enable the one disabled soonest.
	var best *Key
	for _, k := range p.keys {
		if best == nil || k.DisabledAt.Before(best.DisabledAt) {
			best = k
		}
	}
	if best != nil {
		best.Disabled = false
		best.Failures = 0
	}
	return best
}

// Report records the outcome of a request routed through k. retryAfter is the
// upstream Retry-After header value ("" when absent).
func (p *Pool) Report(k *Key, status int, retryAfter string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case status >= 200 && status < 300:
		k.Failures = 0
		k.LastErr = ""
		k.BackoffLevel = 0
		k.RateLimitedUntil = time.Time{}
		p.persistLocked(k)
	case status == 401 || status == 429:
		// Auth/rate-limit: rotate away and apply exponential backoff.
		k.Failures++
		k.LastErr = httpStatusText(status)
		k.BackoffLevel++
		d := p.backoffLocked(k, retryAfter)
		k.RateLimitedUntil = time.Now().Add(d)
		p.persistLocked(k)
		p.disableLocked(k, status)
	case status >= 500:
		k.Failures++
		k.LastErr = httpStatusText(status)
		if k.Failures >= p.disableAfter {
			p.disableLocked(k, status)
		}
		p.persistLocked(k)
	default:
		// 4xx (bad request, model not found, etc.) is a request problem,
		// not a key problem — do not penalise the key.
	}
}

// backoffLocked computes the cooldown duration: upstream Retry-After when
// present and respected, else base*factor^level capped at max, ±20% jitter.
// Assumes p.mu held.
func (p *Pool) backoffLocked(k *Key, retryAfter string) time.Duration {
	if p.cfg.RespectRetryAfter && retryAfter != "" {
		// RFC 7231 §7.1.3: delay-seconds or HTTP-date. Date form:
		// "Sun, 06 Nov 1994 08:49:37 GMT".
		if secs, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && secs > 0 {
			if secs > int(p.cfg.BackoffMax.Seconds()) {
				secs = int(p.cfg.BackoffMax.Seconds())
			}
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(strings.TrimSpace(retryAfter)); err == nil {
			cd := time.Until(t)
			if cd <= 0 {
				return 0
			}
			if cd > p.cfg.BackoffMax {
				cd = p.cfg.BackoffMax
			}
			return cd
		}
	}
	d := float64(p.cfg.BackoffBase) * math.Pow(p.cfg.BackoffFactor, float64(k.BackoffLevel-1))
	jitter := 0.8 + 0.4*rand.Float64()
	d *= jitter
	if d > float64(p.cfg.BackoffMax) {
		d = float64(p.cfg.BackoffMax)
	}
	return time.Duration(d)
}

// disableLocked disables k for the configured cooldown. Called with p.mu held.
func (p *Pool) disableLocked(k *Key, status int) {
	if len(p.keys) <= 1 {
		return // single key: never fully disable, let it retry after cooldown
	}
	k.Disabled = true
	k.DisabledAt = time.Now().Add(p.cooldown)
	_ = status
}

// DisableKey disables a specific key (by value) for the cooldown.
func (p *Pool) DisableKey(value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.keys {
		if k.Value == value {
			k.Disabled = true
			k.DisabledAt = time.Now().Add(p.cooldown)
			p.persistLocked(k)
			return
		}
	}
}

// Next returns a healthy key different from prev, or nil if none exists.
// Used for rotation after a 401/429.
func (p *Pool) Next(prev *Key) (*Key, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, k := range p.keys {
		if k.Disabled && now.After(k.DisabledAt) {
			k.Disabled = false
			k.Failures = 0
		}
	}
	for _, k := range p.keys {
		if k == prev || k.Disabled {
			continue
		}
		k.Usage++
		return k, nil
	}
	return nil, ErrNoKeys
}

// KeyByHash returns the key with the given persisted hash, or nil.
func (p *Pool) KeyByHash(hash string) *Key {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.keys {
		if k.KeyHash() == hash {
			return k
		}
	}
	return nil
}

// LoadState restores persisted health for a key (from pool_keys).
func (p *Pool) LoadState(k *Key, st KeyState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k.Failures = st.Failures
	k.Disabled = st.Disabled
	k.DisabledAt = st.DisabledUntil
	k.BackoffLevel = st.BackoffLevel
	k.RateLimitedUntil = st.RateLimitedUntil
	k.LastErr = st.LastErr
}

// ExportHealth snapshots per-key health by hash so a pool rebuilt for the
// same provider (config edit from the GUI) inherits live cooldowns instead of
// starting clean: every provider save used to wipe 429 bans/backoff
// (review 2026-10-17, r20 F1). Values never leave the pool — hash + health
// only.
func (p *Pool) ExportHealth() map[string]KeyState {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]KeyState, len(p.keys))
	for _, k := range p.keys {
		h := k.KeyHash()
		out[h] = KeyState{
			Provider:         p.name,
			KeyHash:          h,
			Failures:         k.Failures,
			Disabled:         k.Disabled,
			DisabledUntil:    k.DisabledAt,
			BackoffLevel:     k.BackoffLevel,
			RateLimitedUntil: k.RateLimitedUntil,
			LastErr:          k.LastErr,
		}
	}
	return out
}

// Snapshot returns a read-only view of pool state (for status/CLI).
func (p *Pool) Snapshot() []Key {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Key, len(p.keys))
	for i, k := range p.keys {
		k2 := *k
		k2.Value = maskKey(k.Value)
		out[i] = k2
	}
	return out
}

// persistLocked writes key health via the configured callback. The value is
// encrypted at rest only when an Encrypt func is set. Assumes p.mu held.
func (p *Pool) persistLocked(k *Key) {
	if p.cfg.Persist == nil {
		return
	}
	enc := ""
	if p.cfg.Encrypt != nil {
		enc = p.cfg.Encrypt(k.Value)
	}
	_ = p.cfg.Persist(KeyState{
		Provider:         p.name,
		KeyHash:          k.KeyHash(),
		ValueEnc:         enc,
		Failures:         k.Failures,
		Disabled:         k.Disabled,
		DisabledUntil:    k.DisabledAt,
		BackoffLevel:     k.BackoffLevel,
		RateLimitedUntil: k.RateLimitedUntil,
		LastErr:          k.LastErr,
	})
}

// maskKey keeps only a short prefix for display; never leaks full keys.
func maskKey(v string) string {
	if len(v) <= 8 {
		return "****"
	}
	return v[:4] + "…" + v[len(v)-4:]
}

func httpStatusText(status int) string {
	switch status {
	case 401:
		return "unauthorized"
	case 429:
		return "rate limited"
	case 500:
		return "server error"
	case 502:
		return "bad gateway"
	case 503:
		return "service unavailable"
	default:
		return "upstream error"
	}
}
