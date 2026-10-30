// Package breaker implements the per-provider circuit breaker: three states
// (closed/open/half_open), lazy recovery after a cooldown, and persistence of
// the state so a restart does not reset it.
package breaker

import (
	"sync"
	"time"

	"github.com/ByJotaro/ai-router/internal/store"
)

// State is the circuit state.
type State string

const (
	Closed   State = "closed"
	Open     State = "open"
	HalfOpen State = "half_open"
)

// Config tunes the breaker.
type Config struct {
	MaxFailures       int
	Recovery          time.Duration // how long the circuit stays open
	HalfOpenSuccesses int           // successes in half-open before closing
}

// Breaker is one provider's circuit. Safe for concurrent use.
type Breaker struct {
	mu                  sync.Mutex
	provider            string
	cfg                 Config
	state               State
	failures            int
	successes           int
	openedUntil         time.Time
	consecutiveFailures int
	lastError           string
	disabled            bool
	halfOpenProbe       bool // single-flight gate for half-open; transient, not persisted

	// Persist writes state after each transition (SQLite via store).
	persist func(store.BreakerRow) error
}

// Row exposes breaker state for persistence and status.
type Row struct {
	State               State
	OpenedUntil         time.Time
	Failures            int
	ConsecutiveFailures int
	LastError           string
}

// New creates a closed breaker. persist may be nil. MaxFailures < 0 builds a
// permanently disabled breaker: Allow always returns true and failures are
// swallowed, so a provider on the edge of upstream rate limits never trips a
// circuit (the model lockout and per-provider error policies still govern).
func New(provider string, cfg Config, persist func(store.BreakerRow) error) *Breaker {
	if cfg.MaxFailures < 0 {
		return &Breaker{provider: provider, cfg: cfg, state: Closed, persist: persist, disabled: true}
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = 5
	}
	if cfg.Recovery <= 0 {
		cfg.Recovery = 30 * time.Second
	}
	if cfg.HalfOpenSuccesses <= 0 {
		cfg.HalfOpenSuccesses = 2
	}
	return &Breaker{provider: provider, cfg: cfg, state: Closed, persist: persist}
}

// Load restores state from a persisted row.
func (b *Breaker) Load(r *store.BreakerRow) {
	if r == nil {
		return
	}
	if b.disabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = State(r.State)
	if b.state == "" {
		b.state = Closed
	}
	b.failures = r.Failures
	b.successes = r.Successes
	b.consecutiveFailures = r.ConsecutiveFailures
	b.lastError = r.LastError
	if t, err := time.Parse(time.RFC3339, r.OpenedUntil); err == nil && !t.IsZero() {
		b.openedUntil = t
	}
	b.halfOpenProbe = false
}

// Allow reports whether a request may proceed now. Open circuits grant a
// single trial probe after Recovery elapses (lazy recovery → half_open).
func (b *Breaker) Allow(now time.Time) bool {
	if b.disabled {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true
	case Open:
		if now.After(b.openedUntil) {
			b.state = HalfOpen
			b.successes = 0
			b.halfOpenProbe = true
			b.saveLocked()
			return true // trial request
		}
		return false
	case HalfOpen:
		if b.halfOpenProbe {
			return false
		}
		b.halfOpenProbe = true
		return true
	}
	return true
}

// RecordSuccess notes a successful request.
func (b *Breaker) RecordSuccess() {
	if b.disabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.lastError = ""
	b.halfOpenProbe = false
	switch b.state {
	case HalfOpen:
		b.successes++
		if b.successes >= b.cfg.HalfOpenSuccesses {
			b.state = Closed
			b.consecutiveFailures = 0
			b.openedUntil = time.Time{}
			b.saveLocked()
		}
	case Open:
		b.state = Closed
		b.consecutiveFailures = 0
		b.openedUntil = time.Time{}
		b.saveLocked()
	case Closed:
		b.consecutiveFailures = 0
	}
}

// RecordFailure notes a failed request.
func (b *Breaker) RecordFailure(errMsg string) {
	if b.disabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.consecutiveFailures++
	b.lastError = errMsg
	if b.state == HalfOpen || b.consecutiveFailures >= b.cfg.MaxFailures {
		b.halfOpenProbe = false
		b.openLocked()
	}
}

// openLocked trips the circuit. Called with b.mu held.
func (b *Breaker) openLocked() {
	b.state = Open
	b.openedUntil = time.Now().Add(b.cfg.Recovery)
	b.halfOpenProbe = false
	b.saveLocked()
}

// saveLocked persists current state. Called with b.mu held.
func (b *Breaker) saveLocked() {
	if b.persist == nil {
		return
	}
	_ = b.persist(b.rowLocked())
}

// Row returns a snapshot for status output.
func (b *Breaker) Row() Row {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.rowLocked()
	opened, _ := time.Parse(time.RFC3339, r.OpenedUntil)
	return Row{
		State:               State(r.State),
		OpenedUntil:         opened,
		Failures:            r.Failures,
		ConsecutiveFailures: r.ConsecutiveFailures,
		LastError:           r.LastError,
	}
}
// OpenedUntil returns when an open circuit re-opens for a trial probe
// (zero when closed/half-open/disabled): the router surfaces it as the
// outward 503 Retry-After instead of a bare 502 (wave review 2026-09-20).
func (b *Breaker) OpenedUntil() time.Time {
	if b.disabled {
		return time.Time{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != Open {
		return time.Time{}
	}
	return b.openedUntil
}

func (b *Breaker) rowLocked() store.BreakerRow {
	return store.BreakerRow{
		Provider:            b.provider,
		State:               string(b.state),
		Failures:            b.failures,
		Successes:           b.successes,
		OpenedUntil:         tsOrEmpty(b.openedUntil),
		ConsecutiveFailures: b.consecutiveFailures,
		LastError:           b.lastError,
	}
}

func tsOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
