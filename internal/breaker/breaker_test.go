package breaker

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/store"
)

func newTestBreaker(cfg Config) (*Breaker, func()) {
	b := New("prov", cfg, nil)
	return b, func() {}
}

func TestClosedAllows(t *testing.T) {
	b, _ := newTestBreaker(Config{MaxFailures: 3, Recovery: time.Minute, HalfOpenSuccesses: 2})
	if !b.Allow(time.Now()) {
		t.Fatal("closed circuit must allow")
	}
}

func TestOpenAfterFailures(t *testing.T) {
	b, _ := newTestBreaker(Config{MaxFailures: 3, Recovery: time.Minute, HalfOpenSuccesses: 2})
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !b.Allow(now) {
			t.Fatalf("request %d should pass while closing", i+1)
		}
		b.RecordFailure("boom")
	}
	if b.Allow(now) {
		t.Fatal("circuit must be open after max failures")
	}
	if b.Row().State != Open {
		t.Fatalf("state = %s, want open", b.Row().State)
	}
}

func TestLazyRecoveryHalfOpenThenClose(t *testing.T) {
	b, _ := newTestBreaker(Config{MaxFailures: 2, Recovery: 10 * time.Second, HalfOpenSuccesses: 2})
	now := time.Now()
	b.RecordFailure("e1")
	b.RecordFailure("e2")
	if b.Allow(now) {
		t.Fatal("must be open")
	}
	if !b.Allow(now.Add(11 * time.Second)) {
		t.Fatal("trial probe must be allowed after recovery")
	}
	if b.Row().State != HalfOpen {
		t.Fatalf("state = %s, want half_open", b.Row().State)
	}
	b.RecordSuccess()
	b.RecordSuccess()
	if b.Row().State != Closed {
		t.Fatalf("state = %s, want closed after half-open successes", b.Row().State)
	}
}

func TestFailureInHalfOpenReopens(t *testing.T) {
	b, _ := newTestBreaker(Config{MaxFailures: 2, Recovery: time.Minute, HalfOpenSuccesses: 2})
	b.RecordFailure("a")
	b.RecordFailure("b")
	b.Allow(time.Now().Add(61 * time.Second))
	b.RecordFailure("again")
	if b.Row().State != Open {
		t.Fatalf("state = %s, want open", b.Row().State)
	}
}

func TestPersistAndReload(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer s.Close()
	persist := func(r store.BreakerRow) error { return s.SaveBreaker(r) }
	b := New("p1", Config{MaxFailures: 2, Recovery: time.Minute, HalfOpenSuccesses: 2}, persist)
	b.RecordFailure("x")
	b.RecordFailure("y")
	row, err := s.LoadBreaker("p1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if row == nil || row.State != "open" || row.ConsecutiveFailures != 2 {
		t.Fatalf("persisted row = %+v", row)
	}
	b2 := New("p1", Config{MaxFailures: 2, Recovery: time.Minute, HalfOpenSuccesses: 2}, persist)
	b2.Load(row)
	if b2.Row().State != Open {
		t.Fatalf("reloaded state = %s, want open", b2.Row().State)
	}
	if b2.Allow(time.Now()) {
		t.Fatal("reloaded open circuit must reject before recovery elapses")
	}
}

func TestDisabledNeverTrips(t *testing.T) {
	b, _ := newTestBreaker(Config{MaxFailures: -1, Recovery: time.Minute, HalfOpenSuccesses: 2})
	for i := 0; i < 50; i++ {
		if !b.Allow(time.Now()) {
			t.Fatalf("disabled breaker blocked request %d", i+1)
		}
		b.RecordFailure("429 rate limited")
	}
	if b.Row().State != Closed {
		t.Fatalf("disabled breaker state = %s, want closed", b.Row().State)
	}
	b.RecordSuccess()
	if !b.Allow(time.Now()) {
		t.Fatal("disabled breaker must keep allowing after success")
	}
}

func TestHalfOpenSingleFlightStorm(t *testing.T) {
	b, _ := newTestBreaker(Config{MaxFailures: 2, Recovery: 10 * time.Second, HalfOpenSuccesses: 2})
	b.RecordFailure("e1")
	b.RecordFailure("e2")
	// open
	now := time.Now()
	if b.Allow(now) {
		t.Fatal("open should block")
	}
	// after recovery, only one probe allowed
	probeTime := now.Add(11 * time.Second)
	if !b.Allow(probeTime) {
		t.Fatal("first probe after recovery must be allowed")
	}
	if b.Row().State != HalfOpen {
		t.Fatalf("state %s", b.Row().State)
	}
	// concurrent storm: only probe holder allowed, others blocked
	var wg sync.WaitGroup
	blocked := 0
	allowed := 0
	var mu sync.Mutex
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Allow(probeTime) {
				mu.Lock()
				allowed++
				mu.Unlock()
			} else {
				mu.Lock()
				blocked++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 0 {
		t.Fatalf("storm allowed %d, want 0 (probe already taken)", allowed)
	}
	if blocked != 20 {
		t.Fatalf("storm blocked %d, want 20", blocked)
	}
	// after success, probe reset: next half_open probe allowed? Need half_open -> still half_open with 1 success left
	b.RecordSuccess()
	// One more success should close
	if !b.Allow(probeTime) {
		// In half_open after one success, halfOpenProbe was reset, so next Allow should be allowed
		t.Fatal("after 1 success in half_open, next Allow should be allowed")
	}
	b.RecordSuccess()
	if b.Row().State != Closed {
		t.Fatalf("after 2 successes should be closed, got %s", b.Row().State)
	}
}

func TestHalfOpenProbeResetOnFailure(t *testing.T) {
	b, _ := newTestBreaker(Config{MaxFailures: 2, Recovery: 30 * time.Second, HalfOpenSuccesses: 2})
	b.RecordFailure("a")
	b.RecordFailure("b")
	// Capture openedUntil from row to compute correct times
	row := b.Row()
	openUntil := row.OpenedUntil
	if openUntil.IsZero() {
		t.Fatal("openedUntil zero")
	}
	probeTime := openUntil.Add(1 * time.Second)
	if !b.Allow(probeTime) {
		t.Fatal("should transition to half_open")
	}
	// second Allow in half_open should be blocked (probe already taken)
	if b.Allow(probeTime) {
		t.Fatal("second half_open Allow should be blocked")
	}
	// failure in half_open reopens and resets probe
	b.RecordFailure("half fail")
	if b.Row().State != Open {
		t.Fatalf("want open got %s", b.Row().State)
	}
	// Allow before new recovery should still block (1s after reopen)
	newOpenUntil := b.Row().OpenedUntil
	if b.Allow(newOpenUntil.Add(-1 * time.Second)) {
		t.Fatal("open should block before recovery")
	}
	// After recovery, probe allowed again
	if !b.Allow(newOpenUntil.Add(1 * time.Second)) {
		t.Fatal("after recovery should allow probe again")
	}
}

func TestLoadNilAndDisabledNoop(t *testing.T) {
	b, _ := newTestBreaker(Config{MaxFailures: 2, Recovery: time.Minute, HalfOpenSuccesses: 2})
	b.Load(nil)
	if b.Row().State != Closed {
		t.Fatal("nil load should keep closed")
	}
	bd, _ := newTestBreaker(Config{MaxFailures: -1, Recovery: time.Minute, HalfOpenSuccesses: 2})
	bd.RecordFailure("x")
	row := &store.BreakerRow{Provider: "prov", State: "open", Failures: 10}
	bd.Load(row)
	if bd.Row().State != Closed {
		t.Fatalf("disabled Load should be noop, got %s", bd.Row().State)
	}
}

func TestDefaultConfig(t *testing.T) {
	b, _ := newTestBreaker(Config{})
	if b.cfg.MaxFailures != 5 || b.cfg.Recovery != 30*time.Second || b.cfg.HalfOpenSuccesses != 2 {
		t.Fatalf("defaults %+v", b.cfg)
	}
}

func TestRecordSuccessInClosedResetsConsecutive(t *testing.T) {
	b, _ := newTestBreaker(Config{MaxFailures: 3, Recovery: time.Minute, HalfOpenSuccesses: 2})
	b.RecordFailure("a")
	b.RecordSuccess()
	if b.Row().ConsecutiveFailures != 0 {
		t.Fatalf("consecutive %d", b.Row().ConsecutiveFailures)
	}
}

func TestRecordSuccessInOpenCloses(t *testing.T) {
	b, _ := newTestBreaker(Config{MaxFailures: 1, Recovery: time.Minute, HalfOpenSuccesses: 2})
	b.RecordFailure("a")
	if b.Row().State != Open {
		t.Fatalf("want open %s", b.Row().State)
	}
	b.RecordSuccess()
	if b.Row().State != Closed {
		t.Fatalf("success in open should close, got %s", b.Row().State)
	}
}

func TestRowOpenedUntilParsing(t *testing.T) {
	b, _ := newTestBreaker(Config{MaxFailures: 1, Recovery: time.Minute, HalfOpenSuccesses: 2})
	b.RecordFailure("a")
	row := b.Row()
	if row.OpenedUntil.IsZero() {
		t.Fatal("OpenedUntil should be set when open")
	}
}

func TestConcurrentAllowAndRecord(t *testing.T) {
	b, _ := newTestBreaker(Config{MaxFailures: 100, Recovery: time.Minute, HalfOpenSuccesses: 2})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = b.Allow(time.Now()) }()
		go func() { defer wg.Done(); b.RecordFailure("c") }()
	}
	wg.Wait()
}
