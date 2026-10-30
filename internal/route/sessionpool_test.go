package route

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// mkSessionDB builds a temp opencode.db-shaped sqlite with given
// (id, updatedMs) rows for pool tests. rows go to `session`, v2rows to
// `session_v2` (nil = table absent, mimicking older TUI schemas).
func mkSessionDB(t *testing.T, rows [][2]any, v2rows [][2]any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE session (id TEXT, time_updated INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if v2rows != nil {
		if _, err := db.Exec(`CREATE TABLE session_v2 (id TEXT, time_updated INTEGER)`); err != nil {
			t.Fatal(err)
		}
	}
	for _, tbl := range []struct {
		rows [][2]any
		name string
	}{{rows, "session"}, {v2rows, "session_v2"}} {
		for _, r := range tbl.rows {
			if _, err := db.Exec(`INSERT INTO `+tbl.name+` (id, time_updated) VALUES (?, ?)`, r[0], r[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	return p
}

func TestSessionPoolStickyAndDead(t *testing.T) {
	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	p := mkSessionDB(t, nil, [][2]any{
		{"ses_new11111111111111111111111", ms(-time.Hour)},
		{"ses_new22222222222222222222222", ms(-2 * time.Hour)},
		{"ses_old99999999999999999999999", ms(-100 * time.Hour)},
		{"not-a-session", ms(-time.Hour)},
	})
	sp := &SessionPool{DBPath: p, Now: func() time.Time { return now }}
	a, b := sp.Next(), sp.Next()
	if a == "" || a != b {
		t.Fatalf("sticky pool must repeat newest live id, got %q vs %q", a, b)
	}
	if a != "ses_new11111111111111111111111" {
		t.Fatalf("want newest id first, got %q", a)
	}
	// Gate is egress-correlated: ONE gate burns nothing (the same id may
	// 200 on another egress the same minute). The SECOND consecutive gate
	// on the SAME egress (no 200 in between) burns it and rotates to the
	// next live id. Stale (72h) and non-ses_ rows are skipped without a
	// refresh.
	sp.MarkGateHit(a, "ip-a", false)
	if got := sp.Next(); got != a {
		t.Fatalf("single gate must not burn the id, got %q want %q", got, a)
	}
	sp.MarkGateHit(a, "", true) // a 200 elsewhere clears the streak
	sp.MarkGateHit(a, "ip-a", false)
	if got := sp.Next(); got != a {
		t.Fatalf("gate after a 200 is a new first hit, got %q want %q", got, a)
	}
	sp.MarkGateHit(a, "ip-a", false) // second consecutive gate on the same egress: burned
	if got := sp.Next(); got != "ses_new22222222222222222222222" {
		t.Fatalf("after two consecutive gates want second id, got %q", got)
	}
	sp.MarkGateHit("ses_new22222222222222222222222", "ip-b", false)
	sp.MarkGateHit("ses_new22222222222222222222222", "ip-b", false)
	// Exhaustion is NOT a dead end (2026-10-21): returning "" parked the router
	// on a generated session, which zen always gates — the pool then stayed dead
	// until a process restart cleared the dead map. The contract is now
	// rotation: the longest-burned id is handed out again as a half-open probe.
	got := sp.Next()
	if got == "" {
		t.Fatal("exhausted pool must rotate (resurrection probe), not return empty")
	}
	if got != "ses_new11111111111111111111111" && got != "ses_new22222222222222222222222" {
		t.Fatalf("resurrection must pick a harvested id, got %q", got)
	}
}
func TestSessionPoolNextForPinsPerEgress(t *testing.T) {
	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	p := mkSessionDB(t, nil, [][2]any{
		{"ses_new11111111111111111111111", ms(-time.Hour)},
		{"ses_new22222222222222222222222", ms(-2 * time.Hour)},
	})
	sp := &SessionPool{DBPath: p, Now: func() time.Time { return now }}
	a1, a2 := sp.NextFor("ip-a"), sp.NextFor("ip-a")
	if a1 == "" || a1 != a2 {
		t.Fatalf("NextFor must pin one id per egress, got %q vs %q", a1, a2)
	}
	sp.MarkGateHit(a1, "ip-a", false)
	sp.MarkGateHit(a1, "ip-a", false) // burned: egress must re-pin to the other live id
	if got := sp.NextFor("ip-a"); got == a1 || got == "" {
		t.Fatalf("burned pin must rotate, got %q", got)
	}
}

func TestSessionPoolMissingDBEmpty(t *testing.T) {
	sp := &SessionPool{DBPath: filepath.Join(t.TempDir(), "nope.db")}
	if got := sp.Next(); got != "" {
		t.Fatalf("missing db must yield empty pool, got %q", got)
	}
}

func TestSessionPoolRealDBHarvest(t *testing.T) {
	// Production shape check against the live TUI db when present: fresh
	// TUI-minted ids must be harvested (skipped when absent, e.g. CI).
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no homedir")
	}
	p := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	if _, err := os.Stat(p); err != nil {
		t.Skip("no TUI db on this machine")
	}
	sp := &SessionPool{DBPath: p}
	got := sp.Next()
	if got == "" {
		t.Skip("pool empty (no fresh TUI sessions right now)")
	}
	if len(got) < 10 || got[:4] != "ses_" {
		t.Fatalf("harvested id malformed: %q", got)
	}
	t.Logf("harvested %q", got)
}

func TestSessionPoolHarvestsV2AndMerges(t *testing.T) {
	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	// session holds the older row; session_v2 holds a NEWER row for the
	// same id plus two v2-only ids. Harvest must merge both tables, dedupe
	// per id with the newest time_updated, and keep newest-first order.
	p := mkSessionDB(t, [][2]any{
		{"ses_new11111111111111111111111", ms(-5 * time.Hour)},
	}, [][2]any{
		{"ses_new11111111111111111111111", ms(-time.Hour)},
		{"ses_v2only22222222222222222222", ms(-2 * time.Hour)},
		{"ses_v2only33333333333333333333", ms(-3 * time.Hour)},
		{"not-a-session", ms(-time.Hour)},
	})
	got := fetchSessionIDs(p, 72*time.Hour, now)
	var ids []string
	for _, e := range got {
		ids = append(ids, e.id)
	}
	// newest first: session+v2 merged for new1111 (-1h beats its -5h
	// session row), then the two v2-only ids.
	want := []string{"ses_new11111111111111111111111", "ses_v2only22222222222222222222", "ses_v2only33333333333333333333"}
	if len(ids) != len(want) {
		t.Fatalf("want %v, got %v", want, ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("order/dedup mismatch at %d: want %v, got %v", i, want, ids)
		}
	}
	// A v2-only DB (no session rows at all) must still harvest.
	p2 := mkSessionDB(t, nil, [][2]any{{"ses_v2only44444444444444444444", ms(-2 * time.Hour)}})
	if got := fetchSessionIDs(p2, 72*time.Hour, now); len(got) != 1 || got[0].id != "ses_v2only44444444444444444444" {
		t.Fatalf("v2-only db harvest failed: %+v", got)
	}
	// Legacy DB without session_v2 must still harvest from session.
	p3 := mkSessionDB(t, [][2]any{{"ses_legacy55555555555555555555", ms(-2 * time.Hour)}}, nil)
	if got := fetchSessionIDs(p3, 72*time.Hour, now); len(got) != 1 || got[0].id != "ses_legacy55555555555555555555" {
		t.Fatalf("legacy db harvest failed: %+v", got)
	}
}
