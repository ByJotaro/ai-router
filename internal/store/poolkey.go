package store

import (
	"time"
)

// PoolKeyRow is the persisted health state of one provider key.
type PoolKeyRow struct {
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

// SavePoolKey upserts one pool-key health row.
func (s *Store) SavePoolKey(r PoolKeyRow) error {
	_, err := s.db.Exec(
		`INSERT INTO pool_keys(provider, key_hash, value_enc, failures, disabled, disabled_until, backoff_level, rate_limited_until, last_err)
		 VALUES(?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(provider, key_hash) DO UPDATE SET
		   value_enc=excluded.value_enc, failures=excluded.failures, disabled=excluded.disabled,
		   disabled_until=excluded.disabled_until, backoff_level=excluded.backoff_level,
		   rate_limited_until=excluded.rate_limited_until, last_err=excluded.last_err`,
		r.Provider, r.KeyHash, r.ValueEnc, r.Failures, boolInt(r.Disabled), tsOrNil(r.DisabledUntil),
		r.BackoffLevel, tsOrNil(r.RateLimitedUntil), r.LastErr)
	return err
}

// LoadPoolKeys returns persisted health rows for one provider.
func (s *Store) LoadPoolKeys(provider string) ([]PoolKeyRow, error) {
	rows, err := s.db.Query(
		`SELECT key_hash, value_enc, failures, disabled, COALESCE(disabled_until,''), backoff_level, COALESCE(rate_limited_until,''), last_err
		 FROM pool_keys WHERE provider=?`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PoolKeyRow
	for rows.Next() {
		var r PoolKeyRow
		var dis, du, rlu string
		if err := rows.Scan(&r.KeyHash, &r.ValueEnc, &r.Failures, &dis, &du, &r.BackoffLevel, &rlu, &r.LastErr); err != nil {
			return nil, err
		}
		r.Provider = provider
		r.Disabled = dis == "1" || dis == "true"
		if t, err := time.Parse(time.RFC3339, rlu); err == nil {
			r.RateLimitedUntil = t
		}
		if t, err := time.Parse(time.RFC3339, du); err == nil {
			r.DisabledUntil = t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func tsOrNil(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
