package store

import (
	"database/sql"
	"time"
)

// IdempotencyPut stores a completed response for hash, expiring old rows.
func (s *Store) IdempotencyPut(hash, key string, status int, body []byte, ttl time.Duration) error {
	now := time.Now().UTC()
	expired := now.Add(-ttl).Format(time.RFC3339)
	return s.Tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM idempotency WHERE created_at < ?`, expired); err != nil {
			return err
		}
		_, err := tx.Exec(
			`INSERT INTO idempotency(hash, key, response, status, created_at) VALUES(?,?,?,?,?)
			 ON CONFLICT(hash) DO UPDATE SET response=excluded.response, status=excluded.status, created_at=excluded.created_at`,
			hash, key, string(body), status, now.Format(time.RFC3339))
		return err
	})
}

// IdempotencyGet returns a cached response for hash, if present and fresh.
func (s *Store) IdempotencyGet(hash string, ttl time.Duration) (int, []byte, bool) {
	var status int
	var resp string
	var created string
	err := s.db.QueryRow(`SELECT status, response, created_at FROM idempotency WHERE hash=?`, hash).Scan(&status, &resp, &created)
	if err != nil {
		return 0, nil, false
	}
	if t, err := time.Parse(time.RFC3339, created); err == nil && time.Since(t) > ttl {
		return 0, nil, false
	}
	return status, []byte(resp), true
}
