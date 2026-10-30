package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateEmptyDB(t *testing.T) {
	s := openTestStore(t)
	for _, table := range []string{"providers", "pool_keys", "keys", "breakers", "quota", "usage", "models_catalog", "audit", "idempotency"} {
		var n int
		if err := s.DB().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n); err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %s missing", table)
		}
	}
}

func TestMigrateIdempotent(t *testing.T) {
	s := openTestStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestBreakerCRUD(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.DB().Exec(`INSERT INTO breakers(provider, state, failures, opened_until, updated_at) VALUES(?,?,?,?,?)`,
		"opencode-zen", "open", 3, now, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var state string
	var failures int
	if err := s.DB().QueryRow(`SELECT state, failures FROM breakers WHERE provider=?`, "opencode-zen").Scan(&state, &failures); err != nil {
		t.Fatalf("select: %v", err)
	}
	if state != "open" || failures != 3 {
		t.Errorf("got state=%q failures=%d, want open/3", state, failures)
	}
}

func TestDailyUsageRangeDateBoundedWindow(t *testing.T) {
	s := openTestStore(t)
	today := time.Now().UTC().Format("2006-01-02")
	old := time.Now().UTC().AddDate(0, 0, -60).Format("2006-01-02")
	if err := s.IncrDailyUsage(old, 10, 100, 0, 0, 0, 0, 0); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if err := s.IncrDailyUsage(today, 1, 5, 0, 0, 0, 0, 0); err != nil {
		t.Fatalf("insert today: %v", err)
	}
	rows, err := s.DailyUsageRange(30)
	if err != nil {
		t.Fatalf("DailyUsageRange: %v", err)
	}
	if len(rows) != 1 || rows[0].Day != today {
		t.Fatalf("window must exclude the 60-day-old row, got %+v", rows)
	}
	wide, err := s.DailyUsageRange(90)
	if err != nil {
		t.Fatalf("DailyUsageRange(90): %v", err)
	}
	if len(wide) != 2 {
		t.Fatalf("90-day window must include both rows, got %+v", wide)
	}
}

func TestPoolKeyUpsert(t *testing.T) {
	s := openTestStore(t)
	const q = `INSERT INTO pool_keys(provider, key_hash, value_enc, backoff_level) VALUES(?,?,?,?)
		ON CONFLICT(provider, key_hash) DO UPDATE SET backoff_level=excluded.backoff_level`
	if _, err := s.DB().Exec(q, "p", "h1", "enc:v1:x", 1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.DB().Exec(q, "p", "h1", "enc:v1:x", 2); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var level int
	if err := s.DB().QueryRow(`SELECT backoff_level FROM pool_keys WHERE provider=? AND key_hash=?`, "p", "h1").Scan(&level); err != nil {
		t.Fatalf("select: %v", err)
	}
	if level != 2 {
		t.Errorf("backoff_level = %d, want 2", level)
	}
}

func TestAudit(t *testing.T) {
	s := openTestStore(t)
	if err := s.Audit("admin", "keys.create", `{"name":"test"}`); err != nil {
		t.Fatalf("Audit: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
}

func TestTxRollback(t *testing.T) {
	s := openTestStore(t)
	boom := errors.New("forced")
	err := s.Tx(func(tx *sql.Tx) error {
		if _, e := tx.Exec(`INSERT INTO audit(actor, action) VALUES('a','b')`); e != nil {
			return e
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx error = %v, want %v", err, boom)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("audit rows after rollback = %d, want 0", n)
	}
}
