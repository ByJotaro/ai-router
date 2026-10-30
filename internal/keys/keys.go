// Package keys implements virtual API keys with token/request/USD quotas,
// model allowlists and usage accounting persisted to SQLite (with a one-shot
// import from the legacy state.json).
package keys

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/crypto"
)

// Sentinel errors mapped to OpenAI error codes by the gateway.
var (
	ErrUnknownKey = errors.New("unknown key")
	ErrRevoked    = errors.New("key revoked")
	ErrModel      = errors.New("model not allowed for key")
	ErrQuota      = errors.New("key quota exhausted")
)

// Key is a virtual key with live usage counters.
type Key struct {
	Name        string
	Key         string
	QuotaTokens int64
	QuotaReqs   int64
	BudgetUSD   int64 // USD cents
	NoLog       bool
	Models      []string
	Revoked     bool

	UsageTokens int64
	UsageReqs   int64
	UsageUSD    int64
}

// Store is a concurrency-safe virtual-key store backed by SQLite.
type Store struct {
	mu      sync.Mutex
	keys    map[string]*Key // by key value
	db      *sql.DB
	cipher  *crypto.Cipher
	priceFn PriceFunc // optional: model → USD cents per 1M tokens
}

// PriceFunc resolves pricing for cost accounting; returns (input, output
// USD per 1M tokens).
type PriceFunc func(model string) (inCents, outCents int64)

// SetPriceResolver installs a model-pricing lookup for USD cost accounting.
func (s *Store) SetPriceResolver(fn PriceFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.priceFn = fn
}

// New creates an in-memory store; without a DB, usage is not persisted.
func New(db *sql.DB, cipher *crypto.Cipher) *Store {
	if cipher == nil {
		cipher = crypto.New()
	}
	return &Store{keys: map[string]*Key{}, db: db, cipher: cipher}
}

// LegacyStatePath names the pre-v2 state.json (for import).
func LegacyStatePath() string { return filepath.Join(config.BaseDir(), "state.json") }

// Load imports usage from the legacy state.json if present and the DB is
// empty, then hydrates keys from SQLite.
func (s *Store) Load() error {
	if s.db != nil {
		s.migrateLegacy()
		return s.loadFromDB()
	}
	return nil
}

// migrateLegacy imports usage counters from state.json when the keys table
// is empty. Callers must not hold s.mu.
func (s *Store) migrateLegacy() {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM keys`).Scan(&n); err != nil || n > 0 {
		return
	}
	data, err := os.ReadFile(LegacyStatePath())
	if err != nil {
		return
	}
	var p struct {
		Keys map[string]*Key `json:"keys"`
	}
	if json.Unmarshal(data, &p) != nil {
		return
	}
	for _, k := range p.Keys {
		if k == nil || k.Key == "" {
			continue
		}
		_, _ = s.db.Exec(
			`INSERT INTO keys(name, key_hash, value_enc, quota_tokens, quota_reqs, budget_usd, no_log, models, revoked, usage_tokens, usage_reqs, usage_usd, created_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?, datetime('now'))
			 ON CONFLICT(name) DO UPDATE SET usage_tokens=excluded.usage_tokens, usage_reqs=excluded.usage_reqs, usage_usd=excluded.usage_usd`,
			k.Name, hashOf(k.Key), s.cipher.Encrypt(k.Key), k.QuotaTokens, k.QuotaReqs, k.BudgetUSD,
			boolToInt(k.NoLog), jsonList(k.Models), boolToInt(k.Revoked), k.UsageTokens, k.UsageReqs, k.UsageUSD)
	}
}

// loadFromDB hydrates the in-memory key map from SQLite.
func (s *Store) loadFromDB() error {
	rows, err := s.db.Query(
		`SELECT name, key_hash, value_enc, quota_tokens, quota_reqs, budget_usd, no_log, models, revoked, usage_tokens, usage_reqs, usage_usd FROM keys`)
	if err != nil {
		return err
	}
	defer rows.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for rows.Next() {
		var name, kh, enc, modelsJSON string
		var quotaT, quotaR, budget, usageT, usageR, usageUSD int64
		var noLog, revoked int
		if err := rows.Scan(&name, &kh, &enc, &quotaT, &quotaR, &budget, &noLog, &modelsJSON, &revoked, &usageT, &usageR, &usageUSD); err != nil {
			return err
		}
		value := s.cipher.Decrypt(enc)
		if value == "" {
			continue // undecryptable key: skip
		}
		s.keys[value] = &Key{
			Name: name, Key: value,
			QuotaTokens: quotaT, QuotaReqs: quotaR, BudgetUSD: budget,
			NoLog: noLog == 1, Revoked: revoked == 1,
			Models:      parseList(modelsJSON),
			UsageTokens: usageT, UsageReqs: usageR, UsageUSD: usageUSD,
		}
	}
	return rows.Err()
}

// SetConfig replaces the key set from config (adds/updates keys, keeps usage).
func (s *Store) SetConfig(cfgs []config.KeyConf) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := map[string]*Key{}
	for _, c := range cfgs {
		if c.Key == "" {
			continue
		}
		if prev, ok := s.keys[c.Key]; ok {
			prev.Name = c.Name
			// Config is authoritative for what it DECLARES, never for the
			// runtime state it cannot express. KeyConf has no revoked field,
			// so clearing it un-revoked every key on boot (review 2026-10-17,
			// r13/r18 F1); quotas/budget changed from the GUI live in the
			// store, and zero in YAML means "не задано" → keep the live value
			// instead of rolling it back to stale config.
			if c.QuotaTokens != 0 {
				prev.QuotaTokens = c.QuotaTokens
			}
			if c.QuotaReqs != 0 {
				prev.QuotaReqs = c.QuotaReqs
			}
			if c.BudgetUSD != 0 {
				prev.BudgetUSD = c.BudgetUSD
			}
			prev.NoLog = c.NoLog
			prev.Models = c.Models
			next[c.Key] = prev
			continue
		}
		next[c.Key] = &Key{
			Name: c.Name, Key: c.Key,
			QuotaTokens: c.QuotaTokens, QuotaReqs: c.QuotaReqs, BudgetUSD: c.BudgetUSD,
			NoLog: c.NoLog, Models: c.Models,
		}
	}
	s.keys = next
	return s.persistAll()
}

// persistAll writes every key to SQLite (called with s.mu held).
func (s *Store) persistAll() error {
	if s.db == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM keys`); err != nil {
		tx.Rollback()
		return err
	}
	for _, k := range s.keys {
		enc := s.cipher.Encrypt(k.Key)
		if s.cipher.Enabled() && enc == "" {
			tx.Rollback()
			return fmt.Errorf("crypto: encrypt failed for key %q", k.Name)
		}
		if _, err := tx.Exec(
			`INSERT INTO keys(name, key_hash, value_enc, quota_tokens, quota_reqs, budget_usd, no_log, models, revoked, usage_tokens, usage_reqs, usage_usd, created_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?, datetime('now'))`,
			k.Name, hashOf(k.Key), enc, k.QuotaTokens, k.QuotaReqs, k.BudgetUSD,
			boolToInt(k.NoLog), jsonList(k.Models), boolToInt(k.Revoked), k.UsageTokens, k.UsageReqs, k.UsageUSD); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// persistOne writes a single key row (upsert) — used after usage updates.
func (s *Store) persistOne(k *Key) error {
	if s.db == nil {
		return nil
	}
	enc := s.cipher.Encrypt(k.Key)
	if s.cipher.Enabled() && enc == "" {
		return fmt.Errorf("crypto: encrypt failed for key %q", k.Name)
	}
	_, err := s.db.Exec(
		`INSERT INTO keys(name, key_hash, value_enc, quota_tokens, quota_reqs, budget_usd, no_log, models, revoked, usage_tokens, usage_reqs, usage_usd, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?, datetime('now'))
		 ON CONFLICT(name) DO UPDATE SET
		   quota_tokens=excluded.quota_tokens, quota_reqs=excluded.quota_reqs, budget_usd=excluded.budget_usd,
		   no_log=excluded.no_log, models=excluded.models, revoked=excluded.revoked,
		   usage_tokens=excluded.usage_tokens, usage_reqs=excluded.usage_reqs, usage_usd=excluded.usage_usd`,
		k.Name, hashOf(k.Key), enc, k.QuotaTokens, k.QuotaReqs, k.BudgetUSD,
		boolToInt(k.NoLog), jsonList(k.Models), boolToInt(k.Revoked), k.UsageTokens, k.UsageReqs, k.UsageUSD)
	return err
}

// Authorize validates a key value and model, returning the key or an error
// from the sentinel set. Per-model fair-share applies when the key has both
// quotas and an explicit model allowlist. For N allowed models the quota is
// split as floor(quota/N) with the first quota%%N models receiving +1
// (remainder distribution); an empty quota (0) means unlimited.
// perModelExceeded is evaluated without holding the mutex to avoid blocking
// on SQLite I/O.
func (s *Store) Authorize(value, model string) (*Key, error) {
	s.mu.Lock()
	k, ok := s.keys[value]
	if !ok {
		s.mu.Unlock()
		return nil, ErrUnknownKey
	}
	if k.Revoked {
		s.mu.Unlock()
		return nil, ErrRevoked
	}
	if len(k.Models) > 0 && !contains(k.Models, model) {
		s.mu.Unlock()
		return nil, ErrModel
	}
	if k.QuotaReqs > 0 && k.UsageReqs >= k.QuotaReqs {
		s.mu.Unlock()
		return nil, ErrQuota
	}
	if k.QuotaTokens > 0 && k.UsageTokens >= k.QuotaTokens {
		s.mu.Unlock()
		return nil, ErrQuota
	}
	if k.BudgetUSD > 0 && k.UsageUSD >= k.BudgetUSD {
		s.mu.Unlock()
		return nil, ErrQuota
	}
	n := len(k.Models)
	if n <= 1 {
		s.mu.Unlock()
		return k, nil
	}
	// Snapshot per-model quota data before unlocking; perModelExceeded does
	// DB I/O and must not be called with mu held.
	name := k.Name
	quotaReqs := k.QuotaReqs
	quotaTokens := k.QuotaTokens
	modelsCopy := append([]string(nil), k.Models...)
	s.mu.Unlock()
	tmp := &Key{Name: name, QuotaReqs: quotaReqs, QuotaTokens: quotaTokens, Models: modelsCopy}
	if s.perModelExceeded(tmp, model) {
		return nil, ErrQuota
	}
	s.mu.Lock()
	k2, ok := s.keys[value]
	s.mu.Unlock()
	if !ok {
		return nil, ErrUnknownKey
	}
	return k2, nil
}

// perModelExceeded checks the (key, model) usage against its fair share.
// Floor division is used: base = quota / N, remainder = quota %% N.
// The first remainder models (in order of k.Models) receive base+1, the rest
// receive base. This ensures N>1 quotas are enforced honestly and no tokens
// are lost to integer truncation. Must be called without s.mu held (does DB I/O).
func (s *Store) perModelExceeded(k *Key, model string) bool {
	if s.db == nil {
		return false
	}
	n := int64(len(k.Models))
	if n == 0 {
		return false
	}
	baseReqs := k.QuotaReqs / n
	baseTokens := k.QuotaTokens / n
	remReqs := k.QuotaReqs % n
	remTokens := k.QuotaTokens % n
	shareReqs := baseReqs
	shareTokens := baseTokens
	idx := int64(-1)
	for i, m := range k.Models {
		if m == model {
			idx = int64(i)
			break
		}
	}
	if idx >= 0 {
		if remReqs > 0 && idx < remReqs {
			shareReqs++
		}
		if remTokens > 0 && idx < remTokens {
			shareTokens++
		}
	}
	var useR, useT int64
	_ = s.db.QueryRow(`SELECT usage_reqs, usage_tokens FROM quota WHERE key=? AND model=?`, k.Name, model).Scan(&useR, &useT)
	if shareReqs > 0 && useR >= shareReqs {
		return true
	}
	if shareTokens > 0 && useT >= shareTokens {
		return true
	}
	return false
}

// RecordUsage charges a key after a completed request for model. costCents
// is resolved via the price resolver for the actual model (not the key's
// first allowlist entry). Per-model quota is recorded with the real token
// counts (not 1,1). Returns ErrQuota when a quota is crossed.
func (s *Store) RecordUsage(k *Key, model string, promptTokens, completionTokens int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k.UsageReqs++
	k.UsageTokens += promptTokens + completionTokens
	k.UsageUSD += s.costForLocked(model, promptTokens, completionTokens)
	err := s.persistOne(k)
	if err != nil {
		return err
	}
	if model != "" && s.db != nil {
		total := promptTokens + completionTokens
		_, _ = s.db.Exec(
			`INSERT INTO quota(key, model, quota_tokens, quota_reqs, usage_tokens, usage_reqs)
			 VALUES(?,?,?,?,?,1)
			 ON CONFLICT(key, model) DO UPDATE SET usage_reqs=usage_reqs+1, usage_tokens=usage_tokens+excluded.usage_tokens`,
			k.Name, model, k.QuotaTokens, k.QuotaReqs, total)
	}
	if k.QuotaReqs > 0 && k.UsageReqs > k.QuotaReqs {
		return ErrQuota
	}
	if k.QuotaTokens > 0 && k.UsageTokens > k.QuotaTokens {
		return ErrQuota
	}
	if k.BudgetUSD > 0 && k.UsageUSD > k.BudgetUSD {
		return ErrQuota
	}
	return nil
}

// CostCents computes the USD cost in cents for prompt/completion tokens on a
// model via the price resolver (0 when pricing is unavailable or unknown).
func (s *Store) CostCents(model string, prompt, completion int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.costForLocked(model, prompt, completion)
}

// costLocked computes USD cents for a request via the price resolver.
// Deprecated: use costForLocked(model,...). Kept for compatibility.
func (s *Store) costLocked(k *Key, prompt, completion int64) int64 {
	model := ""
	if len(k.Models) == 1 {
		model = k.Models[0]
	}
	return s.costForLocked(model, prompt, completion)
}

// costForLocked applies the price resolver to token counts. Assumes s.mu held.
// Integer arithmetic uses ceil division so small token counts (<6k) still
// produce a non-zero cost when pricing is non-zero, ensuring budget
// accounting actually deducts.
func (s *Store) costForLocked(model string, prompt, completion int64) int64 {
	if s.priceFn == nil {
		return 0
	}
	inC, outC := s.priceFn(model)
	if inC <= 0 && outC <= 0 {
		return 0
	}
	var cents int64
	if prompt > 0 && inC > 0 {
		cents += (prompt*inC + 1_000_000 - 1) / 1_000_000
	}
	if completion > 0 && outC > 0 {
		cents += (completion*outC + 1_000_000 - 1) / 1_000_000
	}
	if cents < 0 {
		return 0
	}
	return cents
}

// currentModel returns the single-allowlist model, if the key has exactly one.
func (s *Store) currentModel(k *Key) (string, bool) {
	if len(k.Models) == 1 {
		return k.Models[0], true
	}
	return "", false
}

// RecordUsageByKey looks up a key by value and charges usage for model.
func (s *Store) RecordUsageByKey(value, model string, promptTokens, completionTokens int64) error {
	s.mu.Lock()
	k, ok := s.keys[value]
	s.mu.Unlock()
	if !ok {
		return ErrUnknownKey
	}
	return s.RecordUsage(k, model, promptTokens, completionTokens)
}

// RecordUsageByKeyLegacy is a compatibility shim for callers that do not
// provide a model (uses empty model, cost will be 0 unless price resolver
// handles it). Prefer RecordUsageByKey with model.
func (s *Store) RecordUsageByKeyLegacy(value string, promptTokens, completionTokens int64) error {
	return s.RecordUsageByKey(value, "", promptTokens, completionTokens)
}

// Revoke marks a key revoked. It stays in state but no longer authorizes.
func (s *Store) Revoke(value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[value]
	if !ok {
		return ErrUnknownKey
	}
	k.Revoked = true
	return s.persistOne(k)
}

// Create adds a virtual key at runtime (hub action keys.create). Fails when
// the key value already exists.
func (s *Store) Create(c config.KeyConf) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Key == "" {
		return errors.New("key is required")
	}
	if _, ok := s.keys[c.Key]; ok {
		return errors.New("key already exists")
	}
	s.keys[c.Key] = &Key{
		Name: c.Name, Key: c.Key,
		QuotaTokens: c.QuotaTokens, QuotaReqs: c.QuotaReqs, BudgetUSD: c.BudgetUSD,
		NoLog: c.NoLog, Models: c.Models,
	}
	return s.persistAll()
}

// SetQuota updates quotas on an existing key by name (hub action quota.set).
func (s *Store) SetQuota(name string, tokens, reqs, budgetUSD int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.findByNameLocked(name)
	if k == nil {
		return ErrUnknownKey
	}
	k.QuotaTokens = tokens
	k.QuotaReqs = reqs
	k.BudgetUSD = budgetUSD
	return s.persistOne(k)
}

// ResetUsage clears usage counters for a single key and its per-model quota rows.
// Used for monthly budget roll and manual reset API.
func (s *Store) ResetUsage(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.findByNameLocked(name)
	if k == nil {
		return ErrUnknownKey
	}
	k.UsageTokens = 0
	k.UsageReqs = 0
	k.UsageUSD = 0
	if err := s.persistOne(k); err != nil {
		return err
	}
	if s.db != nil {
		_, _ = s.db.Exec(`DELETE FROM quota WHERE key=?`, k.Name)
	}
	return nil
}

// ResetAllUsage clears usage for all keys and wipes quota and daily rollup.
func (s *Store) ResetAllUsage() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		k.UsageTokens = 0
		k.UsageReqs = 0
		k.UsageUSD = 0
	}
	if err := s.persistAll(); err != nil {
		return err
	}
	if s.db != nil {
		_, _ = s.db.Exec(`DELETE FROM quota`)
		_, _ = s.db.Exec(`DELETE FROM daily_usage`)
	}
	return nil
}

// MonthlyBudgetUsed returns the cost (cents) consumed in the current calendar
// month derived from daily_usage. When store is nil, it falls back to 0.
func (s *Store) MonthlyBudgetUsed() int64 {
	if s.db == nil {
		return 0
	}
	now := timeNow()
	year, month, _ := now.Date()
	monthStart := fmt.Sprintf("%04d-%02d-01", year, int(month))
	var sum sql.NullInt64
	_ = s.db.QueryRow(`SELECT SUM(cost_usd) FROM daily_usage WHERE day >= ?`, monthStart).Scan(&sum)
	if sum.Valid {
		return sum.Int64
	}
	return 0
}

var timeNow = func() time.Time { return time.Now() }

// FindByName returns a key by name, or nil.
func (s *Store) FindByName(name string) *Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findByNameLocked(name)
}

// NameOf returns the name of the virtual key with the given value, or ""
// when the value is not a known key. Used to attribute requests in logs.
func (s *Store) NameOf(value string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.keys[value]; ok {
		return k.Name
	}
	return ""
}

func (s *Store) findByNameLocked(name string) *Key {
	for _, k := range s.keys {
		if k.Name == name {
			return k
		}
	}
	return nil
}

// List returns masked snapshots of all keys for status/CLI output. Keys are
// never exposed; only a short prefix/suffix.
func (s *Store) List() []View {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]View, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, View{
			Name: k.Name, Key: mask(k.Key),
			QuotaTokens: k.QuotaTokens, QuotaReqs: k.QuotaReqs, BudgetUSD: k.BudgetUSD,
			Models: k.Models, Revoked: k.Revoked, NoLog: k.NoLog,
			UsageTokens: k.UsageTokens, UsageReqs: k.UsageReqs, UsageUSD: k.UsageUSD,
		})
	}
	return out
}

// View is a key snapshot safe for display.
type View struct {
	Name        string   `json:"name"`
	Key         string   `json:"key"` // masked
	QuotaTokens int64    `json:"quota_tokens"`
	QuotaReqs   int64    `json:"quota_reqs"`
	BudgetUSD   int64    `json:"budget_usd"`
	Models      []string `json:"models,omitempty"`
	Revoked     bool     `json:"revoked"`
	NoLog       bool     `json:"no_log"`
	UsageTokens int64    `json:"usage_tokens"`
	UsageReqs   int64    `json:"usage_reqs"`
	UsageUSD    int64    `json:"usage_usd"`
}

func hashOf(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func jsonList(ss []string) string {
	b, _ := json.Marshal(ss)
	return string(b)
}

func parseList(s string) []string {
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func mask(v string) string {
	if len(v) <= 8 {
		return "****"
	}
	return v[:6] + "…" + v[len(v)-4:]
}
