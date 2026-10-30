package lockout

import (
	"testing"
	"time"
)

func TestAllowedWhenClean(t *testing.T) {
	tr := New(Config{MaxFailures: 3, Window: time.Minute, Lockout: time.Minute})
	if !tr.Allowed("m1", time.Now()) {
		t.Fatal("clean model must be allowed")
	}
}

func TestDisabledNeverLocks(t *testing.T) {
	tr := New(Config{MaxFailures: -1, Window: time.Minute, Lockout: time.Minute})
	now := time.Now()
	for i := 0; i < 50; i++ {
		tr.RecordFailure("m1", "boom", now)
		if !tr.Allowed("m1", now) {
			t.Fatalf("disabled tracker must never lock (failure %d)", i)
		}
	}
	tr.Reset("m1")
}

func TestLockAfterMaxFailures(t *testing.T) {
	tr := New(Config{MaxFailures: 3, Window: time.Minute, Lockout: time.Minute})
	now := time.Now()
	for i := 0; i < 3; i++ {
		tr.RecordFailure("m1", "boom", now)
	}
	if tr.Allowed("m1", now) {
		t.Fatal("model must be locked after max failures")
	}
}

func TestLockExpires(t *testing.T) {
	tr := New(Config{MaxFailures: 2, Window: time.Minute, Lockout: time.Minute})
	now := time.Now()
	tr.RecordFailure("m1", "a", now)
	tr.RecordFailure("m1", "b", now)
	// While locked the only way out is the half-open probe, so the assertion is
	// "no more than one attempt per ProbeInterval", not "nothing at all".
	if tr.Allowed("m1", now.Add(2*time.Second)) {
		t.Fatal("must still be locked inside the probe interval")
	}
	if !tr.Allowed("m1", now.Add(6*time.Second)) {
		t.Fatal("half-open probe must be let through after the interval")
	}
	if tr.Allowed("m1", now.Add(7*time.Second)) {
		t.Fatal("second request inside the same probe interval must be refused")
	}
	if !tr.Allowed("m1", now.Add(61*time.Minute)) {
		t.Fatal("lock must expire")
	}
}

// TestLockedModelProbesUntilSuccess pins the measured stand failure of
// 2026-10-22: after a burn window the pool served 200s again, yet the model
// stayed locked for the full 15-minute window because nothing could prove the
// model had recovered — 994 of 1000 requests answered "model locked out" in
// 2 ms. A locked model now gets one probe per interval; a successful request
// resets the tracker.
func TestLockedModelProbesUntilSuccess(t *testing.T) {
	tr := New(Config{MaxFailures: 2, Window: time.Minute, Lockout: 15 * time.Minute})
	now := time.Now()
	tr.RecordFailure("m1", "a", now)
	tr.RecordFailure("m1", "b", now)
	if tr.Allowed("m1", now.Add(time.Second)) {
		t.Fatal("precondition: locked right after the failures")
	}
	probeAt := now.Add(6 * time.Second)
	if !tr.Allowed("m1", probeAt) {
		t.Fatal("probe must be allowed after ProbeInterval")
	}
	tr.Reset("m1") // probe succeeded
	if !tr.Allowed("m1", probeAt.Add(time.Millisecond)) {
		t.Fatal("successful probe must clear the lock")
	}
}

// TestFailedProbeExtendsLock: a probe that fails means the model is still
// dead — the lock must be re-armed from the probe, otherwise a storm simply
// waits out the original deadline and floods a dead model.
func TestFailedProbeExtendsLock(t *testing.T) {
	tr := New(Config{MaxFailures: 2, Window: time.Minute, Lockout: time.Minute})
	now := time.Now()
	tr.RecordFailure("m1", "a", now)
	tr.RecordFailure("m1", "b", now)
	probeAt := now.Add(6 * time.Second)
	if !tr.Allowed("m1", probeAt) {
		t.Fatal("probe must be allowed")
	}
	tr.RecordFailure("m1", "still dead", probeAt)
	if tr.Allowed("m1", probeAt.Add(2*time.Second)) {
		t.Fatal("failed probe must keep the model locked")
	}
	if !tr.Allowed("m1", probeAt.Add(2*time.Minute)) {
		t.Fatal("lock must still expire after the extended window")
	}
}

func TestResetOnSuccess(t *testing.T) {
	tr := New(Config{MaxFailures: 2, Window: time.Minute, Lockout: time.Minute})
	now := time.Now()
	tr.RecordFailure("m1", "a", now)
	tr.RecordFailure("m1", "b", now)
	tr.Reset("m1")
	if !tr.Allowed("m1", now) {
		t.Fatal("reset must clear the lock")
	}
}

func TestWindowSlides(t *testing.T) {
	tr := New(Config{MaxFailures: 3, Window: time.Minute, Lockout: time.Minute})
	now := time.Now()
	tr.RecordFailure("m1", "a", now)
	tr.RecordFailure("m1", "b", now)
	// Old failures fall out of the window → still allowed.
	tr.RecordFailure("m1", "c", now.Add(2*time.Minute))
	tr.RecordFailure("m1", "d", now.Add(2*time.Minute))
	if !tr.Allowed("m1", now.Add(2*time.Minute)) {
		t.Fatal("failures outside window must not trigger lock")
	}
}

func TestSnapshot(t *testing.T) {
	tr := New(Config{MaxFailures: 2, Window: time.Minute, Lockout: time.Minute})
	now := time.Now()
	tr.RecordFailure("m1", "e", now)
	s := tr.Snapshot()
	if len(s) == 0 || s["m1"].Failures != 1 {
		t.Fatalf("snapshot = %+v", s)
	}
}
