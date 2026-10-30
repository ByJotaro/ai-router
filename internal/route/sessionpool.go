package route

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// sesPrefix trims a session id for logs: "ses_"+8 chars, never the full secret.
func sesPrefix(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Session pool: genuine TUI-minted x-opencode-session IDs harvested from the
// local opencode database.
//
// 2026-09-17: zen Console gates free-tier on server-known sessions ("free
// tier can only be used from within OpenCode"). MITM of a genuine TUI run
// vs ai-router showed the ONLY deciding difference is the session value:
// TUI-minted ses_* (27 chars, from opencode.db) passes on ANY egress/UA,
// while ai-router's generated ses_* (22 chars) and random 27-char IDs fail
// on EVERY egress/UA/TLS-stack (Go/curl/Python/Bun matrix). UA, project,
// request-id format, endpoint (chat/responses) are all irrelevant once the
// session is genuine (isolation matrix q8/q14).
//
// Sessions are reusable (same id served 3+ completions) and minted by every
// real `opencode run` (persisted to opencode.db within seconds), so the pool
// reads recent IDs straight from sqlite — no MITM, no login, no key needed.
// Retirement is streak-based, wired from route.go via
// noteSessionGate -> MarkGateHit(id, egress, false): the FIRST gate only arms
// the (session,egress) streak (the gate is egress- and often model-scoped; one
// id must not die for a sibling model's or egress' verdict), the SECOND
// consecutive gate ON THE SAME EGRESS without an intervening 200 burns the id.
// An egress-blind hit arms and burns NOTHING. A 200 anywhere clears the streak
// (MarkGateHit(id, egress, true)). The 403 path also parks the
// egress softly (markBadSoft), so one stale id costs one probe plus one
// minute of park on that egress — not just a probe.
const (
	// sessionMaxAge bounds harvested ids. A stale id self-ejects via the
	// gate streak (two consecutive gates) or by falling out of the 72h
	// window — so err generous: covers active-but-old TUI sessions
	// (2026-09-17: live session minted 09-11).
	sessionMaxAge = 72 * time.Hour
	// sessionRefresh bounds db re-reads (WAL sqlite open per call is cheap
	// but pointless faster than TUI mint cadence).
	sessionRefresh = 30 * time.Second
	// sessionFetchCap caps ids per refresh (db grows unbounded over months).
	// It used to be 40 — BELOW the pool's egress churn (live 2026-10-23:
	// 21 WARP rotates, 320 distinct egress IPs in 7 h): the harvest could
	// never be wider than 40 no matter how many genuine ids the db held, so
	// most ekeys had no id and the pool answered 6324 x 503 "no free session"
	// in 7 h while 33 egresses served fine. Minting more ids cannot help while
	// the READ is capped, so the cap must track the egress set, not the db.
	sessionFetchCap = 512
	// gateStreakTTL bounds a streak arm: a gate that matured 90s ago says
	// nothing about now (the egress/minute window has long reopened), so a
	// second gate must land inside the TTL to burn the id.
	gateStreakTTL = 90 * time.Second
	// id-level retirement: the egress-keyed fast path cannot retire a truly
	// dead account (a fresh /128 almost every request - 1,172 distinct /128
	// today), so 390 client-visible gate verdicts/day kept the id in rotation
	// for the process lifetime and every draw of it cost a full sweep.
	gateIdWindow         = 10 * time.Minute
	gateIdDistinctEgress = 3
	gateIdTotalGates     = 6
	// sessionFreshSpan bounds how far below the newest id an id may sit and
	// still be ELIGIBLE for pinning. It used to be 2h (stale pins were blamed
	// for gate-403s in the pre-2026-10-22 broken-shape era), but the gate
	// accepts any server-known ses_* once the body shape is right — measured
	// 2026-10-23: ids 2.0/2.1/2.2h old answered 429 (gate PASSED, quota closed)
	// through a live egress. With 20+ rotating egresses the 2h window starved
	// the reservation map and forced session sharing ("harvest smaller than the
	// egress set" at 02:17), so eligibility now spans the whole harvest and
	// freshness only DECIDES THE ORDER (alive is newest-first).
	sessionFreshSpan = sessionMaxAge
	// sessionStaleRescue is how many out-of-window ids may be used when the
	// harvest holds NO id inside sessionMaxAge. Live 2026-10-22: the pool held
	// one id 6 minutes old and the rest 116-183h old (beyond the 72h window),
	// so selection was EMPTY and the router fell back to a generated session —
	// the one shape the free-tier gate rejects on every egress ("can only be
	// used from within OpenCode"), which is the 403 the user saw. A genuine TUI
	// id, even old, is a better bet than a fabricated one: upstream has no
	// documented TTL, and retirement here is streak-based.
	sessionStaleRescue = 5
	// sessionDeadTTL bounds how long a burned id stays burned. The gate verdict
	// is time-scoped upstream: the same session that 403s twice passes again a
	// few minutes later — until now the only recovery was a process restart
	// (which clears the dead map). Without a TTL the pool rots to live_left=0
	// and every request falls back to a generated session, a guaranteed
	// gate-403: the "падает при малейшей нагрузке" symptom.
	sessionDeadTTL = 10 * time.Minute
	// sessionEgressSeenTTL bounds a pin whose egress the pool stopped handing
	// out. Requests are short (stream deadline <= 2 min), so 5 min of silence
	// from the POOL means nothing is in flight on that IP.
	sessionEgressSeenTTL = 5 * time.Minute
)

// SessionPool serves genuine TUI session ids. Selection is STICKY
// first-live (not round-robin): upstream prompt-cache keys on (session,
// content), so all anonymous traffic sharing one session shares cache
// (the 2026-09-09 per-request-randomness storm lesson). Rotation happens
// only on death (gate-403) or expiry. Zero value is usable (lazy default
// db path); tests override DBPath/Fetch.
type SessionPool struct {
	mu     sync.Mutex
	DBPath string
	MaxAge time.Duration
	Now    func() time.Time // test hook, nil = time.Now
	// DeadTTL bounds how long a burned id stays dead (0 = sessionDeadTTL).
	DeadTTL  time.Duration
	Fetch    func(dbPath string, maxAge time.Duration, now time.Time) []sessionEntry
	dead     map[string]time.Time
	lastOK   map[string]time.Time // id -> last 200 served with it (proven-live rank)
	gateHits map[string]time.Time // streakKey(session, egress) -> arm time; TTL-bounded
	// gateById is a per-id gate RING: egress slot -> timestamps inside
	// gateIdWindow. A dead ACCOUNT must be retirable although the fast path is
	// egress-keyed, but the count has to be of GATES, not of slots (an earlier
	// map overwrote each slot, so "6 gates" was unreachable and the log field
	// lied), and only while the id served no 2xx in the window - gates are
	// egress/window-correlated, so a live id collects three of them on a
	// three-group pool (review: false burn of ses_eb692d6c).
	gateById map[string]map[string][]time.Time
	pinned   map[string]string // egress key -> pinned session id
	// owner is the reverse binding (session id -> egress key): it is what makes
	// the rotation unique — two IPs must never carry one session id, otherwise
	// the identities are linked and a ban follows both. usedAt drives the LRU
	// fallback when the harvest holds fewer ids than live egresses.
	owner    map[string]string
	usedAt   map[string]time.Time
	pinnedAt map[string]time.Time // egress key -> last call that used its pin
	// seenAt is the POOL's view of liveness: the WARP pool stamps an egress
	// every time it hands it out (NoteEgressSeen). A pin whose egress the pool
	// has not touched for sessionEgressSeenTTL is dead weight — the IP no
	// longer exists in the rotation — and blocks a fresh ekey from getting an
	// identity. Live 2026-10-23: 40 ids, 33 pins, ~20 live egresses, 6324 503s
	// in 7 h ("no free session — egress left without identity").
	seenAt map[string]time.Time
	// lastFor is the id this egress used before its current pin. After a ban
	// the egress must not re-pin the SAME id (deterministic hash would), so
	// the previous id is skipped while another live one exists.
	lastFor map[string]string
	// maxPinned is the high-water mark of concurrent pins: the growth edge is
	// logged at INFO (bounded by the egress count), which is what makes the
	// live "one egress = one session" map observable without a debug build.
	maxPinned int
	// retiredAt remembers when an id lost its egress. A retired id is not handed
	// to a DIFFERENT egress for sessionReuseCooldown: the same session id seen
	// from two IPs minutes apart is exactly the correlation the free tier bans
	// on, while reuse after a long gap looks like a real client roaming.
	retiredAt map[string]time.Time
	cached    []sessionEntry
	loadedAt  time.Time
}

// sessionEntry is one harvested id with its source mtime (ms epoch, as the
// TUI stores time_updated) for expiry without re-querying.
type sessionEntry struct {
	id      string
	updated int64
}

func opencodeDBPath() string {
	// AI_ROUTER_SESSION_DB overrides the harvest source ("off" disables
	// the pool entirely — used by tests for deterministic fallback).
	if v := os.Getenv("AI_ROUTER_SESSION_DB"); v != "" {
		if v == "off" {
			return ""
		}
		return v
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		h = os.Getenv("USERPROFILE")
	}
	return filepath.Join(h, ".local", "share", "opencode", "opencode.db")
}

func (s *SessionPool) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *SessionPool) maxAge() time.Duration {
	if s.MaxAge > 0 {
		return s.MaxAge
	}
	return sessionMaxAge
}

func (s *SessionPool) dbPath() string {
	if s.DBPath != "" {
		return s.DBPath
	}
	return opencodeDBPath()
}

// fetchSessionIDs reads recent session ids (newest first) from opencode.db.
// Harvests BOTH storage tables: the classic `session` rows and the newer
// `session_v2` projection (verified 2026-10-13: session_v2 ids such as
// ses_f43d8c03dffeQHFf0YpDeRMgGZ pass the zen free-tier gate identically to
// session rows — the pool previously ignored them, shrinking the candidate
// set to ~60% on a TUI-heavy box). Union is deduped per id with the newest
// time_updated winning. Read-only: never touches TUI state (WAL/shm intact).
func fetchSessionIDs(dbPath string, maxAge time.Duration, now time.Time) []sessionEntry {
	if dbPath == "" {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		slog.Debug("sessionpool: harvest open failed", "err", err)
		return nil
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tables := []string{}
	for _, t := range []string{"session", "session_v2"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, t).Scan(&n); err == nil && n > 0 {
			tables = append(tables, t)
		}
	}
	if len(tables) == 0 {
		slog.Debug("sessionpool: harvest failed", "err", "no session tables")
		return nil
	}
	union := "SELECT id, time_updated FROM " + tables[0]
	for _, t := range tables[1:] {
		union += " UNION ALL SELECT id, time_updated FROM " + t
	}
	rows, err := db.Query(`SELECT id, MAX(time_updated) AS updated FROM (` + union + `) GROUP BY id ORDER BY updated DESC LIMIT ` +
		strconv.Itoa(sessionFetchCap))
	if err != nil {
		slog.Debug("sessionpool: harvest query failed", "err", err)
		return nil
	}
	defer rows.Close()
	// No age filter here: the cache must also carry the newest OUT-OF-WINDOW
	// ids so aliveLocked can fall back to them (stale rescue) instead of
	// handing out a generated session — the one shape the free-tier gate always
	// 403s. Age filtering lives in aliveLocked, where maxAge is applied.
	_ = maxAge
	var out []sessionEntry
	for rows.Next() {
		var id string
		var updated int64
		if err := rows.Scan(&id, &updated); err != nil {
			continue
		}
		if !strings.HasPrefix(id, "ses_") {
			continue
		}
		// Shape filter (MITM 2026-10-24): the free gate validates the id
		// FORMAT. Current TUI (1.19.4) mints ses_+12hex+14alnum = 30 chars;
		// older 27/28-char ids draw the gate-403 on every egress, so they are
		// worse than a correctly-shaped synthetic fallback. Drop them at the
		// source instead of letting the burn machinery discover them.
		if len(id) != 30 {
			continue
		}
		out = append(out, sessionEntry{id: id, updated: updated})
	}
	slog.Debug("sessionpool: harvested", "n", len(out), "tables", len(tables))
	return out
}

// Next returns the sticky live session id, or "" when the pool is empty
// (caller falls back to generated session). Sticky = cache affinity (see
// struct doc); rotation only on death/expiry.
func (s *SessionPool) Next() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.refreshLocked()
	alive, rescued := s.aliveRescueLocked(now)
	if rescued {
		// An in-window id that was burned may still be alive; a probe beats an
		// out-of-window id (the pre-rescue contract).
		if id := s.resurrectLocked(now); id != "" {
			return id
		}
	}
	if len(alive) > 0 {
		return alive[0].id
	}
	return s.resurrectLocked(now)
}

// NextFor returns the sticky live session for one egress key: the first
// call pins an id to the key, later calls return the same id while it is
// alive. A burned/expired id unpins and the egress re-pins to the next
// live id (rotation across the harvest pool, not per request).
func (s *SessionPool) NextFor(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.refreshLocked()
	// A live egress keeps its id until the id is BURNED. The harvest cache is
	// a freshness hint for NEW assignments, never a verdict on an id already in
	// use: an empty or short refresh used to run the no-alive/rescue branches
	// first and silently re-pin, i.e. put a new identity on the same IP for no
	// reason (live 2026-10-23 05:17:19 -> 05:18:26, no unpin event).
	if id := s.pinned[key]; id != "" {
		if t, dead := s.dead[id]; !dead || now.Sub(t) >= s.deadTTL() {
			if s.pinnedAt == nil {
				s.pinnedAt = map[string]time.Time{}
			}
			s.pinnedAt[key] = now
			if s.usedAt == nil {
				s.usedAt = map[string]time.Time{}
			}
			s.usedAt[id] = now
			return id
		}
		// Burned: drop the reservation, retire the id for the cooldown, and
		// let the egress re-pin to another live id below.
		delete(s.pinned, key)
		delete(s.pinnedAt, key)
		if s.owner[id] == key {
			delete(s.owner, id)
		}
		if s.lastFor == nil {
			s.lastFor = map[string]string{}
		}
		s.lastFor[key] = id
		if s.retiredAt == nil {
			s.retiredAt = map[string]time.Time{}
		}
		s.retiredAt[id] = now
	}
	alive, rescued := s.aliveRescueLocked(now)
	if rescued {
		if id := s.resurrectLocked(now); id != "" {
			if s.pinned == nil {
				s.pinned = map[string]string{}
			}
			s.pinned[key] = id
			return id
		}
	}
	if len(alive) == 0 {
		id := s.resurrectLocked(now)
		if id == "" {
			return ""
		}
		if s.pinned == nil {
			s.pinned = map[string]string{}
		}
		s.pinned[key] = id
		return id
	}
	if s.pinned == nil {
		s.pinned = map[string]string{}
	}
	if s.owner == nil {
		s.owner = map[string]string{}
	}
	if _, ok := s.pinned[key]; ok {
		// Unreachable: a kept pin returns at the top, a burned one is dropped
		// there. Kept as a guard so a future edit cannot silently re-pin.
		delete(s.pinned, key)
	}
	// Pins of silent egresses are NOT swept here: dropping one retires the id
	// for sessionReuseCooldown and leaves a live egress with no identity for
	// that whole window. Reclaim is demand-driven instead (reclaimSilentPin),
	// so a returning egress keeps its id and a newcomer takes over only what
	// nobody is using.
	// Unique assignment: two egresses must never carry the same session id,
	// because a shared id links the IPs and a ban on one identity follows the
	// other. Hashing alone collided, so the reservation map makes the link
	// impossible. When the harvest runs out the answer is REFUSAL ("" up to
	// acquireLease), never a shared or fabricated identity.
	width := freshWidthLocked(alive)
	if width == 0 {
		return ""
	}
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h = (h ^ uint32(key[i])) * 16777619
	}
	start := int(h % uint32(width))
	if rescued {
		// Every id is out of the max-age window (stale rescue): there is no
		// freshness signal to spread over, so take them newest-first instead
		// of hashing blindly into the middle of the rescue list.
		start = 0
	}
	prev := s.lastFor[key]
	retired := func(id string) bool {
		at, ok := s.retiredAt[id]
		return ok && now.Sub(at) < sessionReuseCooldown
	}
	// Order preference: ids inside the 2h freshness window first, then the
	// rest of the harvest. Both are gate-valid (measured 2026-10-23), but a
	// fresh id is the one the newest CLI traffic proved.
	order := make([]int, 0, width)
	for i := 0; i < width; i++ {
		if (start+i)%width < width && alive[(start+i)%width].updated >= now.Add(-2*time.Hour).UnixMilli() {
			order = append(order, (start+i)%width)
		}
	}
	for i := 0; i < width; i++ {
		idx := (start + i) % width
		if alive[idx].updated < now.Add(-2*time.Hour).UnixMilli() {
			order = append(order, idx)
		}
	}
	// Pass 1: unowned, not retired, not the egress' own previous id (a banned
	// identity must not get its old session back).
	tryFree := func() string {
		for _, idx := range order {
			cand := alive[idx].id
			if cand == prev || retired(cand) {
				continue
			}
			if _, taken := s.owner[cand]; taken {
				continue
			}
			return s.pinLocked(key, cand, len(alive), width, h)
		}
		return ""
	}
	if id := tryFree(); id != "" {
		return id
	}
	// (No pass 2 by design.) An id that was just released is NOT re-issued to
	// another egress: a session seen on two IPs minutes apart is one client
	// with two addresses. Once sessionReuseCooldown expires `retired()` turns
	// false and pass 1 picks the id up again.
	// Pass 3: no free id. NEVER hand the same id to a second LIVE egress: an
	// id seen from two IPs is one identity with two addresses — the exact
	// correlation the free tier bans on, and the thing the operator forbade.
	// What we may do is take back the pin of an egress that stopped calling
	// (warp rotated its /64 away): its IP is gone, so the id has no live peer.
	if id := s.reclaimSilentPinLocked(now); id != "" && id != prev {
		slog.Info("sessionpool: reclaimed pin of a silent egress",
			"ses", sesPrefix(id), "live", len(alive), "fresh", width)
		return s.pinLocked(key, id, len(alive), width, h)
	}
	// Same idea, but keyed on the POOL's liveness instead of traffic idleness:
	// a WARP slot that rotated away never calls again, so its pin would sit for
	// sessionPinIdle (20 min) and starve every new ekey meanwhile.
	if id := s.reclaimUnseenPinLocked(now); id != "" && id != prev {
		slog.Info("sessionpool: reclaimed pin of an unseen egress",
			"ses", sesPrefix(id), "live", len(alive), "fresh", width, "pinned", len(s.pinned))
		return s.pinLocked(key, id, len(alive), width, h)
	}
	// Supply exhausted: this egress gets NO identity. The caller must refuse
	// the egress instead of shipping a shared or fabricated session.
	slog.Warn("sessionpool: no free session — egress left without identity",
		"live", len(alive), "pinned", len(s.pinned))
	return ""
}

// NoteEgressSeen records the POOL's liveness signal for one egress key: the
// WARP pool stamps every egress it hands out. Pins are then freed by liveness
// (this) as well as by idleness (sessionPinIdle): a rotated-away WARP slot kept
// its pin for 20 min while its IP no longer existed, and with the harvest
// smaller than the churn every fresh ekey was refused (6324 503s / 7 h).
func (s *SessionPool) NoteEgressSeen(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenAt == nil {
		s.seenAt = map[string]time.Time{}
	}
	s.seenAt[key] = s.now()
}

// HasPin reports whether the egress already owns a session id. The pool uses
// it to PREFER a lease that can actually serve: an identity-less egress must
// be refused, so picking one is a wasted round trip.
func (s *SessionPool) HasPin(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.pinned[key]
	if id == "" {
		return false
	}
	// A burned id is not an identity any more; report it as absent so the
	// pool keeps looking instead of handing out a doomed egress.
	if t, dead := s.dead[id]; dead && s.now().Sub(t) < s.deadTTL() {
		return false
	}
	return true
}

// reusableNowLocked reports whether a reclaimed id may go to ANOTHER egress
// right now. Two guards: it must not be inside the ban cooldown, and it must
// not have been USED too recently — the silent reclaim was safe only because
// its 20-minute idle wait exceeds sessionReuseCooldown, while the unseen
// reclaim frees a pin after 5 minutes, so without the last-use check a rotated
// egress's id could appear on a second IP three times sooner than the
// uniqueness rule allows.
func (s *SessionPool) reusableNowLocked(id string, now time.Time) bool {
	if id == "" {
		return false
	}
	if at, ok := s.retiredAt[id]; ok && now.Sub(at) < sessionReuseCooldown {
		return false
	}
	if at, ok := s.usedAt[id]; ok && now.Sub(at) < sessionReuseCooldown {
		return false
	}
	return true
}

// reclaimUnseenPinLocked takes back the pin of an egress the POOL has not
// handed out for sessionEgressSeenTTL. Unlike the idle rule this needs no
// traffic on the id — it uses the pool's own liveness signal, so a WARP slot
// that rotated away frees its id within minutes instead of 20.
func (s *SessionPool) reclaimUnseenPinLocked(now time.Time) string {
	victim, victimAt := "", time.Time{}
	for key := range s.pinned {
		seen, ok := s.seenAt[key]
		if !ok {
			continue // no pool signal (tests, direct mode): keep the pin
		}
		if now.Sub(seen) < sessionEgressSeenTTL {
			continue
		}
		if !s.reusableNowLocked(s.pinned[key], now) {
			continue // hand the id out only after the cooldown-from-last-use
		}
		if victim == "" || seen.Before(victimAt) {
			victim, victimAt = key, seen
		}
	}
	if victim == "" {
		return ""
	}
	id := s.pinned[victim]
	delete(s.pinned, victim)
	delete(s.pinnedAt, victim)
	delete(s.seenAt, victim)
	if s.owner[id] == victim {
		delete(s.owner, id)
	}
	if s.lastFor == nil {
		s.lastFor = map[string]string{}
	}
	s.lastFor[victim] = id
	return id
}

// reclaimSilentPinLocked frees the reservation of the egress that has waited
// longest without a call (idle >= sessionPinIdle) and returns its id.
func (s *SessionPool) reclaimSilentPinLocked(now time.Time) string {
	victim, victimAt := "", time.Time{}
	for key, at := range s.pinnedAt {
		if now.Sub(at) < sessionPinIdle {
			continue
		}
		if !s.reusableNowLocked(s.pinned[key], now) {
			continue
		}
		if victim == "" || at.Before(victimAt) {
			victim, victimAt = key, at
		}
	}
	if victim == "" {
		return ""
	}
	id := s.pinned[victim]
	delete(s.pinned, victim)
	delete(s.pinnedAt, victim)
	if s.owner[id] == victim {
		delete(s.owner, id)
	}
	delete(s.lastFor, victim)
	return id
}

func (s *SessionPool) pinLocked(key, id string, live, fresh int, h uint32) string {
	if cur, ok := s.pinned[key]; ok && cur != id {
		if s.lastFor == nil {
			s.lastFor = map[string]string{}
		}
		s.lastFor[key] = cur
	}
	s.pinned[key] = id
	s.owner[id] = key
	if s.pinnedAt == nil {
		s.pinnedAt = map[string]time.Time{}
	}
	nowPin := s.now()
	s.pinnedAt[key] = nowPin
	if s.seenAt == nil {
		s.seenAt = map[string]time.Time{}
	}
	s.seenAt[key] = nowPin
	if s.usedAt == nil {
		s.usedAt = map[string]time.Time{}
	}
	s.usedAt[id] = s.now()
	if len(s.pinned) > s.maxPinned {
		s.maxPinned = len(s.pinned)
		slog.Info("sessionpool: egress pinned", "ekey", key, "ses", sesPrefix(id),
			"pinned", len(s.pinned), "live", live, "fresh", fresh)
	} else {
		slog.Debug("sessionpool: egress pinned", "ekey_h8", fmt.Sprintf("%08x", h),
			"ses", sesPrefix(id), "live", live, "fresh", fresh)
	}
	return id
}

// releaseOwnerLocked drops a dead id from the reservation map so the egress
// that held it re-pins to a different live id instead of a burned one.
func (s *SessionPool) releaseOwnerLocked(id string) {
	if id == "" {
		return
	}
	if key, ok := s.owner[id]; ok {
		delete(s.owner, id)
		if s.pinned[key] == id {
			delete(s.pinned, key)
		}
	}
	delete(s.usedAt, id)
}

// ReleaseEgress drops the reservation held by one egress key: it is called
// when the IP identity itself is burned (hours-class 429) so the egress never
// comes back carrying the session of a banned identity. The freed id stays in
// the pool for OTHER egresses (a session is not blamed for its IP's quota).
func (s *SessionPool) ReleaseEgress(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.pinned[key]
	if !ok {
		return
	}
	delete(s.pinned, key)
	delete(s.pinnedAt, key)
	if s.owner[id] == key {
		delete(s.owner, id)
	}
	if s.lastFor == nil {
		s.lastFor = map[string]string{}
	}
	s.lastFor[key] = id // never hand this identity's id back to this egress
	if s.retiredAt == nil {
		s.retiredAt = map[string]time.Time{}
	}
	s.retiredAt[id] = s.now()
	slog.Info("sessionpool: egress banned, session unpinned",
		"ekey", key, "ses", sesPrefix(id), "live", len(s.owner))
}

// sessionPinIdle is how long an egress pin survives without a call. Egress
// keys are proxy+public-IP, so a warp rotation silently orphans the old key;
// its reservation would keep a live id out of circulation for good.
const sessionPinIdle = 20 * time.Minute

// sessionReuseCooldown keeps a retired session id away from OTHER egresses for
// this long: an id that two IPs share within minutes is one identity with two
// addresses (the correlation the free tier bans on); reuse after a long gap is
// indistinguishable from a genuine client changing networks.
// 10 min, not 30: the measured bad reuse that started this work was 3-5 s
// (live_20261023_0158: the same id on two banned IPs), while a churning pool
// bans ~1 egress per 5 s under load — a 30 min cooldown retired the whole
// harvest inside minutes and made pass 2 hand retired ids out on every other
// pin, i.e. the quarantine became cosmetic log noise.
const sessionReuseCooldown = 10 * time.Minute

// refreshLocked re-harvests the db when the cache is stale and returns now.
func (s *SessionPool) refreshLocked() time.Time {
	now := s.now()
	s.pruneLocked(now)
	if s.cached == nil || now.Sub(s.loadedAt) > sessionRefresh {
		fetch := s.Fetch
		if fetch == nil {
			fetch = fetchSessionIDs
		}
		s.cached = fetch(s.dbPath(), s.maxAge(), now)
		s.loadedAt = now
	}
	return now
}

// pruneLocked drops state that has outlived its meaning: retiredAt entries
// past sessionPinIdle (well beyond sessionReuseCooldown, after which the id is
// free to be issued again) and dead entries whose TTL expired (they count as
// alive everywhere else already). Without this both maps grow with every ban
// and every storm for the whole process lifetime — a rotation that runs for
// weeks would carry thousands of dead keys.
func (s *SessionPool) pruneLocked(now time.Time) {
	for id, at := range s.retiredAt {
		if now.Sub(at) >= sessionPinIdle {
			delete(s.retiredAt, id)
		}
	}
	ttl := s.deadTTL()
	for id, at := range s.dead {
		if now.Sub(at) >= ttl {
			delete(s.dead, id)
		}
	}
}

// deadTTL is how long a burned id stays burned (tests/ops override DeadTTL).
func (s *SessionPool) deadTTL() time.Duration {
	if s.DeadTTL > 0 {
		return s.DeadTTL
	}
	return sessionDeadTTL
}

// aliveLocked lists the harvested ids that are neither expired nor currently
// burned. The cache itself is NEVER truncated: burned ids must stay reachable
// for resurrection and for the TTL re-check.
func (s *SessionPool) aliveLocked(now time.Time) []sessionEntry {
	alive, _ := s.aliveRescueLocked(now)
	return alive
}

// aliveRescueLocked is aliveLocked plus the rescued flag: true when the result
// came from the out-of-window fallback rather than from ids inside maxAge.
// Callers prefer a resurrection probe of an in-window id in that case — a
// burned id that might still be alive is a better first bet than one that fell
// out of the harvest window entirely.
func (s *SessionPool) aliveRescueLocked(now time.Time) ([]sessionEntry, bool) {
	cutoff := now.Add(-s.maxAge()).UnixMilli()
	out := make([]sessionEntry, 0, len(s.cached))
	for _, e := range s.cached {
		// Proven-live ranks like a fresh mint: an id that served a 200 today
		// beats a TUI row that merely has a newer mtime — and it must be
		// ranked BEFORE the age filter, otherwise a long-lived session with an
		// old mtime is dropped even though it is demonstrably working.
		if t, ok := s.lastOK[e.id]; ok && t.UnixMilli() > e.updated {
			e.updated = t.UnixMilli()
		}
		if e.updated < cutoff {
			continue
		}
		if t, dead := s.dead[e.id]; dead && now.Sub(t) < s.deadTTL() {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].updated > out[j].updated })
	if len(out) > 0 {
		return out, false
	}
	// Stale rescue: nothing inside the window. Fall back to the newest genuine
	// ids instead of handing the caller "" (generated session = guaranteed
	// gate-403). Bounded so an ancient pool cannot masquerade as healthy.
	stale := make([]sessionEntry, 0, len(s.cached))
	for _, e := range s.cached {
		if t, dead := s.dead[e.id]; dead && now.Sub(t) < s.deadTTL() {
			continue
		}
		stale = append(stale, e)
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].updated > stale[j].updated })
	if len(stale) > sessionStaleRescue {
		stale = stale[:sessionStaleRescue]
	}
	if len(stale) > 0 {
		slog.Warn("sessionpool: no fresh ids, using stale-rescue",
			"n", len(stale), "newest_age", now.Sub(time.UnixMilli(stale[0].updated)).Round(time.Minute).String())
	}
	return stale, len(stale) > 0
}

// freshWidthLocked is how many of the (already freshest-first) ids are
// eligible for egress pinning: everything ranked within sessionFreshSpan of
// the newest, at least one.
func freshWidthLocked(alive []sessionEntry) int {
	if len(alive) <= 1 {
		return len(alive)
	}
	cut := alive[0].updated - sessionFreshSpan.Milliseconds()
	n := 1
	for n < len(alive) && alive[n].updated >= cut {
		n++
	}
	return n
}

// resurrectLocked is the restart-equivalent: when every harvested id is burned
// it clears the death of the longest-burned id and hands it out as a half-open
// probe. A 200 revives it (MarkGateHit ok), a gate re-burns it with a fresh
// timestamp — so rotation keeps walking the set instead of parking the router
// on a generated session (guaranteed gate-403) until the next restart.
func (s *SessionPool) resurrectLocked(now time.Time) string {
	cutoff := now.Add(-s.maxAge()).UnixMilli()
	best, bestAt := "", time.Time{}
	for _, e := range s.cached {
		if e.updated < cutoff {
			continue
		}
		t, dead := s.dead[e.id]
		if !dead {
			continue
		}
		if best == "" || t.Before(bestAt) {
			best, bestAt = e.id, t
		}
	}
	if best == "" {
		return ""
	}
	delete(s.dead, best)
	slog.Warn("sessionpool: resurrection probe", "ses", sesPrefix(best), "burned_for", now.Sub(bestAt).Round(time.Second).String())
	return best
}

// MarkGateHit records one gate attempt for the (session, egress) pair: the
// gate is egress-correlated (2026-09-18 live: same session+minute, one
// egress 200 while others 403) and often model-scoped, so the streak must
// key on BOTH — two gates on DIFFERENT egresses prove nothing about the id.
// ok clears every armed streak for that id AND revives it in `dead` (a 200
// anywhere proves the id alive; in-process burn is not a death sentence).
// !ok arms the (session,egress) streak or — on the second consecutive gate
// on the SAME egress inside gateStreakTTL — burns the id.
func (s *SessionPool) MarkGateHit(id, ekey string, ok bool) {
	if id == "" {
		return
	}
	s.mu.Lock()
	if s.gateHits == nil {
		s.gateHits = map[string]time.Time{}
	}
	if ok {
		for k := range s.gateHits {
			if k == id || strings.HasPrefix(k, id+"\x1f") {
				delete(s.gateHits, k)
			}
		}
		delete(s.gateById, id)
		delete(s.dead, id)
		if s.lastOK == nil {
			s.lastOK = map[string]time.Time{}
		}
		s.lastOK[id] = s.now()
		s.mu.Unlock()
		return
	}
	now := s.now()
	if s.gateById == nil {
		s.gateById = map[string]map[string][]time.Time{}
	}
	perEgress := s.gateById[id]
	if perEgress == nil {
		perEgress = map[string][]time.Time{}
		s.gateById[id] = perEgress
	}
	slot := ekey
	if slot == "" {
		slot = "\x00noegress"
	}
	perEgress[slot] = append(perEgress[slot], now)
	distinct, total := 0, 0
	for k, ring := range perEgress {
		keep := ring[:0]
		for _, at := range ring {
			if now.Sub(at) <= gateIdWindow {
				keep = append(keep, at)
			}
		}
		if len(keep) == 0 {
			delete(perEgress, k)
			continue
		}
		perEgress[k] = keep
		total += len(keep)
		if k != "\x00noegress" {
			distinct++
		}
	}
	recent2xx := false
	if at, ok := s.lastOK[id]; ok && now.Sub(at) <= gateIdWindow {
		recent2xx = true
	}
	if (distinct >= gateIdDistinctEgress || total >= gateIdTotalGates) && !recent2xx {
		delete(s.gateById, id)
		for k := range s.gateHits {
			if k == id || strings.HasPrefix(k, id+"\x1f") {
				delete(s.gateHits, k)
			}
		}
		if s.dead == nil {
			s.dead = map[string]time.Time{}
		}
		s.dead[id] = now
		s.loadedAt = time.Time{}
		s.releaseOwnerLocked(id)
		n := len(s.aliveLocked(now))
		slog.Warn("sessionpool: session burned (id-level gate count)",
			"ses", sesPrefix(id), "distinct_egress", distinct, "gates", total, "live_left", n)
		s.mu.Unlock()
		return
	}
	if ekey == "" {
		// Egress-blind gate hit: a synthetic verdict that did not carry its
		// egress (or a legacy caller) says nothing about the id — the gate is
		// a (session x egress x minute) window, proven live by one egress
		// answering 200 while another answered 403 in the same second.
		// Keying the streak on the bare id made TWO such hits inside
		// gateStreakTTL burn a healthy harvest (2026-10-22 17:54), after
		// which every router-minted request was a guaranteed 403. Arm nothing,
		// burn nothing.
		slog.Debug("sessionpool: gate without egress, not burning", "ses", sesPrefix(id))
		s.mu.Unlock()
		return
	}
	key := streakKey(id, ekey)
	if armedAt, armed := s.gateHits[key]; armed && now.Sub(armedAt) < gateStreakTTL {
		delete(s.gateHits, key)
		if s.dead == nil {
			s.dead = map[string]time.Time{}
		}
		s.dead[id] = now
		s.loadedAt = time.Time{}
		s.releaseOwnerLocked(id)
		n := len(s.aliveLocked(now))
		slog.Warn("sessionpool: session burned", "ses", sesPrefix(id), "ekey", ekey != "", "live_left", n)
		s.mu.Unlock()
		return
	}
	s.gateHits[key] = now
	slog.Debug("sessionpool: gate streak armed", "ses", sesPrefix(id), "ekey", ekey != "")
	s.mu.Unlock()
}

// MarkDead immediately retires a harvested session id (e.g. on an upstream
// 429) so the next request re-pins to a different live id instead of
// reusing the same banned identity.
func (s *SessionPool) MarkDead(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead == nil {
		s.dead = map[string]time.Time{}
	}
	s.dead[id] = s.now()
	s.loadedAt = time.Time{}
	s.releaseOwnerLocked(id)
	for k, v := range s.pinned {
		if v == id {
			delete(s.pinned, k)
			delete(s.pinnedAt, k)
		}
	}
	slog.Warn("sessionpool: session marked dead (rate limit)", "ses", sesPrefix(id))
}

// streakKey names the per-(session,egress) streak. Empty egress keeps the
// bare id so callers without egress identity (legacy path) stay single-key.
func streakKey(id, ekey string) string {
	if ekey == "" {
		return id
	}
	return id + "\x1f" + ekey
}

// LiveCount reports non-dead, non-expired cached ids (observability).
func (s *SessionPool) LiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.aliveLocked(s.now()))
}

// sessionPoolSingleton is the process-wide pool used by setOpencodeHeaders
// (lazily created on first use; tests build their own SessionPool).
var (
	sessionPoolMu sync.Mutex
	sessionPool   *SessionPool
	// sessionPoolSeed, when set (tests only), initialises every lazily
	// created pool. Production leaves it nil and the pool harvests
	// opencode.db; on a clean machine there is no such db, so a test binary
	// would otherwise see an empty supply and fail every lease that needs a
	// session identity.
	sessionPoolSeed func(*SessionPool)
)

func getSessionPool() *SessionPool {
	sessionPoolMu.Lock()
	defer sessionPoolMu.Unlock()
	if sessionPool == nil {
		sessionPool = &SessionPool{dead: map[string]time.Time{}}
		if sessionPoolSeed != nil {
			sessionPoolSeed(sessionPool)
		}
		// Ops lever: how long a burned session stays burned. 0/absent keeps
		// sessionDeadTTL; a bad value is ignored (never fatal at boot).
		if v := os.Getenv("AI_ROUTER_SESSION_DEAD_TTL"); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				sessionPool.DeadTTL = d
			}
		}
	}
	return sessionPool
}

// resetSessionPool drops the process singleton (tests only — isolates the
// harvest source between cases).
func resetSessionPool() {
	sessionPoolMu.Lock()
	sessionPool = nil
	sessionPoolMu.Unlock()
}
