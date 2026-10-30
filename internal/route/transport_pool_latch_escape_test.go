package route

import (
	"testing"
	"time"
)

// TestGlobalLatchEscapesOnDistinctBudget pins Wave 2026-10-21: shared-budget
// latch must be escapable by two 200s from DIFFERENT /48 groups inside
// globalProbationWindow (1 minute was too tight under 1x1 load), while a vote
// collected BEFORE the current arm must not clear the fresh latch (that would
// saw clear→burn→re-arm every RA cycle).
func TestGlobalLatchEscapesOnDistinctBudget(t *testing.T) {
	const ipA = "2a09:bac1:47a0:170::1"
	const ipB = "2a09:bac1:47c0:170::1"
	if v48(ipA) == v48(ipB) {
		t.Fatalf("test fixture broken: %q and %q collapse into one /48", ipA, ipB)
	}

	pt := &PoolTransport{}
	// Один hours-class RA — это ещё не общий бюджет (нужна пара).
	if pt.noteHoursRA(ipA, 3600) && pt.globalActive() {
		t.Fatalf("single RA observation must not arm the global latch")
	}
	pt.noteHoursRA(ipB, 3600)
	if !pt.globalActive() {
		t.Fatalf("two clustered hours-class RAs from distinct egresses must arm the latch")
	}

	// Голос, набранный в этом окне.
	pt.markGoodKey(ipA)
	if !pt.globalActive() {
		t.Fatalf("one 200 must not clear the latch (same-budget proof required)")
	}

	// Латч истёк по RA и взвёлся заново — голос прошлого окна испорчен.
	pt.muMu.Lock()
	pt.globalBurnUntil = time.Now().Add(-time.Second)
	pt.muMu.Unlock()
	pt.noteHoursRA(ipA, 3600)
	pt.noteHoursRA(ipB, 3600)
	if !pt.globalActive() {
		t.Fatalf("latch did not re-arm after expiry")
	}

	// Первый 200 после пере-арма приходит из ДРУГОЙ /48, чем стоптанный голос:
	// без сброса голоса при арме он мгновенно снимает свежий латч (пила).
	pt.markGoodKey(ipB)
	if !pt.globalActive() {
		t.Fatalf("stale vote cleared a fresh latch: saw clear->burn->re-arm")
	}
	// Второй 200 из отличной /48 внутри окна — доказательство переоткрытия.
	pt.markGoodKey(ipA)
	if pt.globalActive() {
		t.Fatalf("two 200s from distinct /48 inside the window must clear the latch")
	}
}
