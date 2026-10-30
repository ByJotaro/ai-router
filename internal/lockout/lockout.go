// Package lockout implements per-model lockout: when a model fails repeatedly
// across providers within a window it is taken out of service for a while, so
// a dead model stops eating requests everywhere.
package lockout

import (
	"errors"
	"sync"
	"time"
)

// ErrLocked is returned when the model is temporarily unavailable.
var ErrLocked = errors.New("model locked out (repeated failures)")

// Config tunes the tracker.
type Config struct {
	MaxFailures int           // failures inside Window before locking
	Window      time.Duration // sliding window
	Lockout     time.Duration // how long the model stays locked
	// ProbeInterval is the half-open probe spacing: while a model is locked one
	// request every interval is let through so a recovered model unlocks
	// instead of answering 429 for the whole Lockout window. The measured
	// failure (stand 2026-10-22, 1000 rpm): after the burn window the pool
	// served 200s again but the model stayed locked for the full 15 minutes —
	// 994 of 1000 requests answered `model locked out` in 2 ms.
	ProbeInterval time.Duration
}

type entry struct {
	failures    []time.Time
	lockedUntil time.Time
	lastProbe   time.Time
	lastErr     string
}

// Tracker keeps per-model failure state. Safe for concurrent use.
type Tracker struct {
	mu       sync.Mutex
	cfg      Config
	models   map[string]*entry
	disabled bool
}

// New creates an empty tracker.
func New(cfg Config) *Tracker {
	if cfg.MaxFailures < 0 {
		return &Tracker{cfg: cfg, models: map[string]*entry{}, disabled: true}
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = 10
	}
	if cfg.Window <= 0 {
		cfg.Window = 5 * time.Minute
	}
	if cfg.Lockout <= 0 {
		cfg.Lockout = 15 * time.Minute
	}
	if cfg.ProbeInterval <= 0 {
		// 5s: measured on the stand (2026-10-22) — with a 20s interval a
		// recovered pool still answered 390 lockout 429s (~23s of a 1000 rpm
		// stream) before the first probe proved the model was alive. A dead
		// model costs one probe per interval, so 5s is nearly free.
		cfg.ProbeInterval = 5 * time.Second
	}
	return &Tracker{cfg: cfg, models: map[string]*entry{}}
}

// Allowed reports whether requests for model may proceed now.
func (t *Tracker) Allowed(model string, now time.Time) bool {
	if t.disabled {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.models[model]
	if !ok {
		return true
	}
	t.pruneLocked(e, now)
	if now.Before(e.lockedUntil) {
		// Half-open probe: let one request through every ProbeInterval. A
		// success calls Reset and lifts the lock; a failure re-arms it (see
		// RecordFailure), so a dead model still costs one attempt per interval.
		if now.Sub(e.lastProbe) >= t.cfg.ProbeInterval {
			e.lastProbe = now
			return true
		}
		return false
	}
	if !e.lockedUntil.IsZero() {
		// Lock expired: clear state.
		delete(t.models, model)
	}
	return true
}

// ResetAll clears every per-model failure record and lock. Callers use it when
// the world the verdicts described no longer exists — e.g. the egress pool was
// re-created with fresh identities (a 1000 rpm storm locks a model in under a
// second; without this reset the lock outlives the outage it recorded and the
// healed pool still answers 429 for the whole lockout window).
func (t *Tracker) ResetAll() {
	if t.disabled {
		return
	}
	t.mu.Lock()
	t.models = map[string]*entry{}
	t.mu.Unlock()
}

// RecordFailure notes a failed request for model.
func (t *Tracker) RecordFailure(model, errMsg string, now time.Time) {
	if t.disabled {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.models[model]
	if !ok {
		e = &entry{}
		t.models[model] = e
	}
	t.pruneLocked(e, now)
	e.failures = append(e.failures, now)
	e.lastErr = errMsg
	// Keep only failures inside the window.
	cutoff := now.Add(-t.cfg.Window)
	kept := e.failures[:0]
	for _, f := range e.failures {
		if f.After(cutoff) {
			kept = append(kept, f)
		}
	}
	e.failures = kept
	if !e.lockedUntil.IsZero() && now.Before(e.lockedUntil) {
		// Failure of a half-open probe (or of a request that slipped through):
		// the model is still dead — extend the lock instead of letting it
		// expire while the storm continues.
		e.lockedUntil = now.Add(t.cfg.Lockout)
		return
	}
	if len(e.failures) >= t.cfg.MaxFailures {
		e.lockedUntil = now.Add(t.cfg.Lockout)
		e.lastProbe = now // first half-open probe waits one interval
		e.failures = nil
	}
}

// Reset clears failure state for model (called after a successful request).
func (t *Tracker) Reset(model string) {
	if t.disabled {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.models, model)
}

// State is a snapshot for status output.
type State struct {
	LockedUntil time.Time
	Failures    int
	LastErr     string
}

// Snapshot returns per-model lockout state.
func (t *Tracker) Snapshot() map[string]State {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]State{}
	for m, e := range t.models {
		if !e.lockedUntil.IsZero() || len(e.failures) > 0 {
			out[m] = State{LockedUntil: e.lockedUntil, Failures: len(e.failures), LastErr: e.lastErr}
		}
	}
	return out
}

// pruneLocked drops stale failures outside the window. Assumes t.mu held.
func (t *Tracker) pruneLocked(e *entry, now time.Time) {
	cutoff := now.Add(-t.cfg.Window)
	kept := e.failures[:0]
	for _, f := range e.failures {
		if f.After(cutoff) {
			kept = append(kept, f)
		}
	}
	e.failures = kept
}
