package pool

import (
	"testing"
	"time"
)

func TestLeastUsedPicksLowestUsage(t *testing.T) {
	p := New("p", []string{"a", "b", "c"}, Config{})
	if p.Len() != 3 {
		t.Fatalf("want 3 keys, got %d", p.Len())
	}
	k1, _ := p.Pick()
	k2, _ := p.Pick()
	if k1 == k2 {
		t.Fatal("least-used should pick different keys while usage is equal")
	}
	// Use k1 once more; then pick should prefer k2.
	if _, err := p.Pick(); err != nil {
		t.Fatal(err)
	}
	// All keys have usage 1 now except first pick used twice.
	p.Report(k1, 200, "")
	p.Report(k2, 200, "")
}

func TestDisableAfterRepeatedFailures(t *testing.T) {
	p := New("p", []string{"a", "b"}, Config{DisableAfter: 2, Cooldown: time.Hour})
	k1, _ := p.Pick()
	p.Report(k1, 500, "boom")
	p.Report(k1, 500, "boom")
	if !k1.Disabled {
		t.Fatal("key should be disabled after 2 failures")
	}
	k2, err := p.Pick()
	if err != nil {
		t.Fatalf("pick after disable: %v", err)
	}
	if k2 == k1 {
		t.Fatal("should have picked the other key")
	}
}

func TestRotationOn401(t *testing.T) {
	p := New("p", []string{"a", "b", "c"}, Config{Cooldown: time.Minute})
	k1, _ := p.Pick()
	p.Report(k1, 401, "invalid api key")
	if !k1.Disabled {
		t.Fatal("401 should disable the key immediately")
	}
	k2, err := p.Next(k1)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if k2 == k1 {
		t.Fatal("next must differ from failed key")
	}
}

func TestSingleKeyNeverFullyDisabled(t *testing.T) {
	p := New("p", []string{"only"}, Config{DisableAfter: 1})
	k, err := p.Pick()
	if err != nil {
		t.Fatal(err)
	}
	p.Report(k, 401, "bad")
	if k.Disabled {
		t.Fatal("single key must not be disabled, it retries after cooldown")
	}
}

func TestAllDisabledReturnsError(t *testing.T) {
	p := New("p", []string{"a", "b"}, Config{Cooldown: time.Hour})
	k1, _ := p.Pick()
	p.Report(k1, 429, "rate limited")
	k2, _ := p.Next(k1)
	p.Report(k2, 429, "rate limited")
	if _, err := p.Pick(); err != ErrNoKeys {
		t.Fatalf("want ErrNoKeys, got %v", err)
	}
}

func TestExpiredDisableReenabled(t *testing.T) {
	p := New("p", []string{"a", "b"}, Config{DisableAfter: 2, Cooldown: time.Millisecond})
	k1, _ := p.Pick()
	p.Report(k1, 500, "x")
	p.Report(k1, 500, "x")
	if !k1.Disabled {
		t.Fatal("expected disabled")
	}
	// Drain the other key so both have equal usage.
	k2, err := p.Pick()
	if err != nil {
		t.Fatal(err)
	}
	if k2 == k1 {
		t.Fatal("expected other key while k1 disabled")
	}
	time.Sleep(2 * time.Millisecond)
	k, err := p.Pick()
	if err != nil {
		t.Fatalf("pick after cooldown: %v", err)
	}
	if k != k1 {
		t.Fatal("expected re-enabled key to be picked")
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	base := 10 * time.Millisecond
	p := New("p", []string{"a", "b"}, Config{
		Cooldown:      time.Minute,
		BackoffBase:   base,
		BackoffMax:    10 * base,
		BackoffFactor: 2.0,
	})
	k1, _ := p.Pick()
	p.Report(k1, 429, "")
	lvl1 := k1.BackoffLevel
	d1 := k1.RateLimitedUntil.Sub(time.Now())
	if lvl1 != 1 || d1 < 0 || d1 > base*13/10 {
		t.Fatalf("level=%d d=%v, want level 1 within %v", lvl1, d1, base)
	}
	p.Report(k1, 429, "")
	if k1.BackoffLevel != 2 {
		t.Fatalf("level = %d, want 2", k1.BackoffLevel)
	}
	// Fire enough times to hit the cap.
	for i := 0; i < 12; i++ {
		p.Report(k1, 429, "")
	}
	d := k1.RateLimitedUntil.Sub(time.Now())
	if d > 10*base+base {
		t.Fatalf("backoff not capped: %v", d)
	}
}

func TestRetryAfterRespected(t *testing.T) {
	p := New("p", []string{"a", "b"}, Config{RespectRetryAfter: true, Cooldown: time.Minute})
	k, _ := p.Pick()
	p.Report(k, 429, "7")
	d := k.RateLimitedUntil.Sub(time.Now())
	if d < 6*time.Second || d > 8*time.Second {
		t.Fatalf("Retry-After not respected: %v", d)
	}
}

func TestRateLimitedKeySkippedWhenAlternative(t *testing.T) {
	p := New("p", []string{"a", "b"}, Config{Cooldown: time.Minute})
	k1, _ := p.Pick()
	p.Report(k1, 429, "")
	k2, err := p.Pick()
	if err != nil {
		t.Fatal(err)
	}
	if k2 == k1 {
		t.Fatal("rate-limited key must be skipped while an alternative exists")
	}
}

func TestSingleKeyDegradesWhenRateLimited(t *testing.T) {
	p := New("p", []string{"only"}, Config{Cooldown: time.Minute})
	k1, _ := p.Pick()
	p.Report(k1, 429, "")
	// A single key is never disabled but gets a rate-limit window; Pick must
	// still return it (degraded mode) rather than failing.
	k, err := p.Pick()
	if err != nil {
		t.Fatalf("degraded pick: %v", err)
	}
	if k != k1 {
		t.Fatal("expected the only key")
	}
}

func TestSuccessResetsBackoff(t *testing.T) {
	p := New("p", []string{"a"}, Config{Cooldown: time.Minute})
	k, _ := p.Pick()
	p.Report(k, 429, "")
	if k.BackoffLevel == 0 {
		t.Fatal("expected backoff after 429")
	}
	p.Report(k, 200, "")
	if k.BackoffLevel != 0 || !k.RateLimitedUntil.IsZero() {
		t.Fatal("success must reset backoff state")
	}
}

func TestDefaultKeyPickedFirst(t *testing.T) {
	p := New("p", []string{"a", "b", "c"}, Config{Mode: "least-used"})
	if err := p.SetDefault(p.keys[1].KeyHash()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		k, err := p.Pick()
		if err != nil {
			t.Fatal(err)
		}
		if k != p.keys[1] {
			t.Fatalf("pick %d: expected default key, got %s", i, maskKey(k.Value))
		}
	}
}

func TestSetProxyOnKey(t *testing.T) {
	p := New("p", []string{"a", "b"}, Config{})
	if err := p.SetProxy(p.keys[0].KeyHash(), "http://127.0.0.1:8118"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetProxy("deadbeef", "x"); err == nil {
		t.Fatal("expected error for unknown hash")
	}
	views := p.SnapshotKeys()
	if len(views) != 2 || views[0].Proxy != "http://127.0.0.1:8118" || views[0].Default {
		t.Fatalf("unexpected snapshot: %+v", views[0])
	}
}
