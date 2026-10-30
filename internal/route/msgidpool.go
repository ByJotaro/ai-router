package route

import (
	"database/sql"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// msgID is one harvested real request id with the session it belonged to.
// The inference lane's free-tier gate (MITM bisect 2026-10-14) checks the
// x-opencode-request value the same way it checks the session: only ids the
// server has seen from a genuine TUI run pass — minted/random msg_* values
// get the FreeTier 403 even with a real session, correct headers, lane and
// TLS (matrix K1=200 vs K2..K5=403, byte-identical bodies).
type msgID struct {
	id      string
	session string
	created int64
}

// MsgIDPool serves genuine TUI-minted x-opencode-request ids harvested from
// the local opencode database (message rows). Sticky first-live like the
// session pool; rotates on DB refresh (newest TUI traffic first).
type MsgIDPool struct {
	mu       sync.Mutex
	DBPath   string
	MaxAge   time.Duration
	Now      func() time.Time // test hook, nil = time.Now
	Fetch    func(dbPath string, maxAge time.Duration, now time.Time) []msgID
	cached   []msgID
	loadedAt time.Time
	// served rotates the ids handed to one session; usedBy records which
	// session received an id, so an id is never shared between sessions (a
	// shared request id links two IPs exactly like a shared session id).
	served map[string]int
	usedBy map[string]string
}

var (
	msgIDPoolOnce sync.Once
	msgIDPoolInst *MsgIDPool
)

func getMsgIDPool() *MsgIDPool {
	msgIDPoolOnce.Do(func() {
		msgIDPoolInst = &MsgIDPool{}
	})
	return msgIDPoolInst
}

func (m *MsgIDPool) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *MsgIDPool) maxAge() time.Duration {
	if m.MaxAge > 0 {
		return m.MaxAge
	}
	return sessionMaxAge
}

func (m *MsgIDPool) dbPath() string {
	if m.DBPath != "" {
		return m.DBPath
	}
	if v := os.Getenv("AI_ROUTER_SESSION_DB"); v == "off" {
		return ""
	}
	return opencodeDBPath()
}

// fetchMsgIDs harvests recent message ids from the TUI database. Message ids
// are the real x-opencode-request values the TUI stamped server-side.
func fetchMsgIDs(dbPath string, maxAge time.Duration, now time.Time) []msgID {
	if dbPath == "" {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		slog.Debug("msgidpool: harvest open failed", "err", err)
		return nil
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='message'`).Scan(&n); err != nil || n == 0 {
		slog.Debug("msgidpool: harvest failed", "err", "no message table")
		return nil
	}
	rows, err := db.Query(`SELECT id, session_id, time_created FROM message ORDER BY time_created DESC LIMIT ` +
		strconv.Itoa(sessionFetchCap))
	if err != nil {
		slog.Debug("msgidpool: harvest query failed", "err", err)
		return nil
	}
	defer rows.Close()
	cutoff := now.Add(-maxAge).UnixMilli()
	var out []msgID
	for rows.Next() {
		var id, ses string
		var created int64
		if err := rows.Scan(&id, &ses, &created); err != nil {
			continue
		}
		if !strings.HasPrefix(id, "msg_") || created < cutoff {
			continue
		}
		out = append(out, msgID{id: id, session: ses, created: created})
	}
	slog.Debug("msgidpool: harvested", "live", len(out))
	return out
}

// NextFor returns the harvested request id paired with ses when known, else
// the newest harvested id (any server-seen id passes), or "" when the pool
// is empty (caller falls back to a generated id).
func (m *MsgIDPool) NextFor(ses string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if m.cached == nil || now.Sub(m.loadedAt) > sessionRefresh {
		fetch := m.Fetch
		if fetch == nil {
			fetch = fetchMsgIDs
		}
		m.cached = fetch(m.dbPath(), m.maxAge(), now)
		m.loadedAt = now
	}
	cutoff := now.Add(-m.maxAge()).UnixMilli()
	kept := m.cached[:0]
	for _, e := range m.cached {
		if e.created >= cutoff {
			kept = append(kept, e)
		}
	}
	m.cached = kept
	if len(m.cached) == 0 {
		return ""
	}
	if ses == "" {
		return ""
	}
	// Only ids that the TUI minted INSIDE this session: a request id from a
	// sibling session would link two egresses. The old fallback to
	// cached[0] handed the SAME newest id to every session — the strongest
	// possible cross-IP signature.
	if m.served == nil {
		m.served = map[string]int{}
	}
	if m.usedBy == nil {
		m.usedBy = map[string]string{}
	}
	mine := make([]string, 0, 4)
	for _, e := range m.cached {
		if e.session == ses {
			mine = append(mine, e.id)
		}
	}
	if len(mine) == 0 {
		return ""
	}
	start := m.served[ses] % len(mine)
	for i := 0; i < len(mine); i++ {
		cand := mine[(start+i)%len(mine)]
		if owner, ok := m.usedBy[cand]; ok && owner != ses {
			continue
		}
		m.served[ses] = start + i + 1
		m.usedBy[cand] = ses
		return cand
	}
	// Every paired id is taken by another session: an empty header is honest
	// (the lane accepts a request without x-opencode-request — measured
	// 2026-10-23), sharing one is not.
	return ""
}
