package store

import (
	"database/sql"
	"time"
)

// BreakerRow is the persisted circuit-breaker state for one provider.
// OpenedUntil is RFC3339 text; empty means not open.
type BreakerRow struct {
	Provider            string
	State               string // closed|open|half_open
	Failures            int
	Successes           int
	OpenedUntil         string
	ConsecutiveFailures int
	LastError           string
}

// LoadBreaker fetches a breaker row, returning nil when absent.
func (s *Store) LoadBreaker(provider string) (*BreakerRow, error) {
	var r BreakerRow
	var opened sql.NullString
	err := s.db.QueryRow(
		`SELECT state, failures, successes, COALESCE(opened_until,''), consecutive_failures, last_error
		 FROM breakers WHERE provider=?`, provider).
		Scan(&r.State, &r.Failures, &r.Successes, &opened, &r.ConsecutiveFailures, &r.LastError)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	r.Provider = provider
	r.OpenedUntil = opened.String
	return &r, nil
}

// SaveBreaker upserts a breaker row.
func (s *Store) SaveBreaker(r BreakerRow) error {
	_, err := s.db.Exec(
		`INSERT INTO breakers(provider, state, failures, successes, opened_until, consecutive_failures, last_error, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(provider) DO UPDATE SET
		   state=excluded.state, failures=excluded.failures, successes=excluded.successes,
		   opened_until=excluded.opened_until, consecutive_failures=excluded.consecutive_failures,
		   last_error=excluded.last_error, updated_at=excluded.updated_at`,
		r.Provider, r.State, r.Failures, r.Successes, r.OpenedUntil, r.ConsecutiveFailures, r.LastError,
		time.Now().UTC().Format(time.RFC3339))
	return err
}
