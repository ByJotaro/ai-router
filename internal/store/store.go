// Package store is ai-router's SQLite persistence layer (WAL, busy_timeout)
// plus the schema for breakers, keys, quotas, usage, audit and idempotency.
// The design is SQLite-first: hot-path counters live in short transactions,
// lookups go by key hash, and raw SQL outside this package is avoided.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ByJotaro/ai-router/internal/events"
)

// Store wraps the SQLite handle with a fixed DSN and a migration step.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path. Pragmas are
// tuned for a single-writer gateway: WAL journal, a short busy timeout, and
// normal durability (fine for counters; fsync on commit is avoided).
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)", filepath.ToSlash(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	_ = os.Chmod(path+"-wal", 0o600)
	_ = os.Chmod(path+"-shm", 0o600)
	db.SetMaxOpenConns(1) // single writer: avoids SQLITE_BUSY on hot paths
	s := &Store{db: db}
	if err := s.Migrate(); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	_ = os.Chmod(path+"-wal", 0o600)
	_ = os.Chmod(path+"-shm", 0o600)
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB returns the raw handle for domain packages that own their queries.
func (s *Store) DB() *sql.DB { return s.db }

// Tx runs fn inside a transaction, committing on nil error.
func (s *Store) Tx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Migrate applies the schema, creating missing tables and indexes.
func (s *Store) Migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	if err := s.migrateRequestLogCost(); err != nil {
		return err
	}
	if err := s.migrateRequestLogDay(); err != nil {
		return err
	}
	if err := s.migrateRequestLogWaitedMS(); err != nil {
		return err
	}
	if err := s.migrateRequestLogComboPath(); err != nil {
		return err
	}
	return s.migrateModelsCatalogCents()
}

// migrateRequestLogCost adds the cost_usd column to pre-existing request_log
// tables (fresh databases get it from schema.go). Idempotent.
func (s *Store) migrateRequestLogCost() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('request_log') WHERE name='cost_usd'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE request_log ADD COLUMN cost_usd INTEGER NOT NULL DEFAULT 0`)
	return err
}

// migrateRequestLogComboPath adds the combo_path column to pre-existing
// request_log tables (fresh databases get it from schema.go). Idempotent.
func (s *Store) migrateRequestLogComboPath() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('request_log') WHERE name='combo_path'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE request_log ADD COLUMN combo_path TEXT NOT NULL DEFAULT ''`)
	return err
}

// migrateRequestLogWaitedMS adds the waited_ms column to pre-existing
// request_log tables: the pool records how long a request waited for an egress
// so "served after wait" is a journal category, not invisible latency.
// Idempotent.
func (s *Store) migrateRequestLogWaitedMS() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('request_log') WHERE name='waited_ms'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.db.Exec(`ALTER TABLE request_log ADD COLUMN waited_ms INTEGER NOT NULL DEFAULT 0`)
	return err
}

// comboPathJSON сериализует трек обхода графа в JSON-массив для колонки
// combo_path ("" для легаси-линейных комбо и не-комбо запросов).
func comboPathJSON(path []string) string {
	if len(path) == 0 {
		return ""
	}
	b, err := json.Marshal(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseComboPath разбирает колонку combo_path обратно в трек обхода.
func ParseComboPath(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil
	}
	return out
}

// migrateRequestLogDay adds the expression index on the day part of ts
// for pre-existing request_log tables (fresh databases get it from schema.go).
// Idempotent (IF NOT EXISTS): cheap one-time DDL.
func (s *Store) migrateRequestLogDay() error {
	_, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_request_log_day ON request_log(substr(ts,1,10))`)
	return err
}

// migrateModelsCatalogCents migrates legacy REAL price columns to integer cents.
func (s *Store) migrateModelsCatalogCents() error {
	var hasReal, hasCents int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('models_catalog') WHERE name='input_price'`).Scan(&hasReal)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('models_catalog') WHERE name='input_price_cents'`).Scan(&hasCents)
	if hasReal > 0 && hasCents == 0 {
		if _, err := s.db.Exec(`ALTER TABLE models_catalog ADD COLUMN input_price_cents INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		if _, err := s.db.Exec(`ALTER TABLE models_catalog ADD COLUMN output_price_cents INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		_, _ = s.db.Exec(`UPDATE models_catalog SET input_price_cents = CAST(input_price*100 AS INTEGER), output_price_cents = CAST(output_price*100 AS INTEGER)`)
	}
	if hasCents > 0 && hasReal > 0 {
		_, _ = s.db.Exec(`UPDATE models_catalog SET input_price_cents = CAST(input_price*100 AS INTEGER) WHERE input_price_cents=0 AND input_price!=0`)
		_, _ = s.db.Exec(`UPDATE models_catalog SET output_price_cents = CAST(output_price*100 AS INTEGER) WHERE output_price_cents=0 AND output_price!=0`)
	}
	return nil
}

// DeleteStaleModels removes catalog entries for models no longer configured.
func (s *Store) DeleteStaleModels(keep map[string]struct{}) error {
	if len(keep) == 0 {
		return nil
	}
	rows, err := s.db.Query(`SELECT model FROM models_catalog`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var toDel []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			continue
		}
		if _, ok := keep[m]; !ok {
			toDel = append(toDel, m)
		}
		if err := rows.Err(); err != nil {
			break
		}
	}
	rows.Close()
	for _, m := range toDel {
		_, _ = s.db.Exec(`DELETE FROM models_catalog WHERE model=?`, m)
	}
	return nil
}

// GetModelCost returns integer cents per 1M tokens for a model, 0 if unknown.
func (s *Store) GetModelCost(model string) (inCents, outCents int64) {
	if s.db == nil {
		return 0, 0
	}
	_ = s.db.QueryRow(`SELECT input_price_cents, output_price_cents FROM models_catalog WHERE model=?`, model).Scan(&inCents, &outCents)
	return inCents, outCents
}

// ResetDailyUsage clears the daily_usage rollup (for manual budget month roll).
func (s *Store) ResetDailyUsage() error {
	_, err := s.db.Exec(`DELETE FROM daily_usage`)
	return err
}

// Audit appends a row to the audit trail.
func (s *Store) Audit(actor, action, detail string) error {
	_, err := s.db.Exec(
		"INSERT INTO audit(ts, actor, action, detail) VALUES(?,?,?,?)",
		time.Now().UTC().Format(time.RFC3339), actor, action, detail)
	return err
}

// UpsertModel records a model and its per-1M-token prices in the catalog.
func (s *Store) UpsertModel(model, provider string, inCentsPer1M, outCentsPer1M int64) error {
	_, err := s.db.Exec(
		`INSERT INTO models_catalog(model, provider, input_price_cents, output_price_cents, updated_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(model) DO UPDATE SET
		   provider=excluded.provider, input_price_cents=excluded.input_price_cents,
		   output_price_cents=excluded.output_price_cents, updated_at=excluded.updated_at`,
		model, provider, inCentsPer1M, outCentsPer1M,
		time.Now().UTC().Format(time.RFC3339))
	return err
}

// IncrDailyUsage folds one request into the per-day rollup. costCents is the
// USD spend in cents (0 when pricing is unavailable).
func (s *Store) IncrDailyUsage(day string, prompt, completion, cacheRead, cacheWrite, reasoning, compressed, costCents int64) error {
	_, err := s.db.Exec(
		`INSERT INTO daily_usage(day, requests, prompt_tokens, completion_tokens, cache_read, cache_write, reasoning, compressed, cost_usd)
		 VALUES(?,1,?,?,?,?,?,?,?)
		 ON CONFLICT(day) DO UPDATE SET
		   requests=requests+1,
		   prompt_tokens=prompt_tokens+excluded.prompt_tokens,
		   completion_tokens=completion_tokens+excluded.completion_tokens,
		   cache_read=cache_read+excluded.cache_read,
		   cache_write=cache_write+excluded.cache_write,
		   reasoning=reasoning+excluded.reasoning,
		   compressed=compressed+excluded.compressed,
		   cost_usd=cost_usd+excluded.cost_usd`,
		day, prompt, completion, cacheRead, cacheWrite, reasoning, compressed, costCents)
	return err
}

// InsertRequestLog appends one completed gateway request to request_log.
// e.CostCents (USD cents) lands in the cost_usd column.
func (s *Store) InsertRequestLog(e events.RequestEvent, method, path string) error {
	_, err := s.db.Exec(
		`INSERT INTO request_log(ts, rid, model, requested_model, provider, account, connection_id,
		   duration_ms, prompt_tokens, completion_tokens, cache_read, cache_write, reasoning, compressed,
		   combo_name, combo_step_id, combo_path, status, http_status, error, proxy, waited_ms, method, path, cost_usd)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339), e.RID, e.Model, e.RequestedModel, e.Provider, e.Account, e.ConnectionID,
		e.DurationMS, e.Tokens.Prompt, e.Tokens.Completion, e.Tokens.CacheRead, e.Tokens.CacheWrite,
		e.Tokens.Reasoning, e.Tokens.Compressed, e.ComboName, e.ComboStepID, comboPathJSON(e.ComboPath), e.Status, e.HTTPStatus,
		e.Error, e.Proxy, e.WaitedMS, method, path, e.CostCents)
	return err
}

// DailyUsageRow is one day of the daily_usage rollup. CostUSD is in USD cents.
// PromptTokens/CompletionTokens mirror Prompt/Completion for the GUI bindings.
type DailyUsageRow struct {
	Day        string
	Requests   int64
	Prompt     int64
	Completion int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
	Compressed int64
	CostUSD    int64

	PromptTokens     int64
	CompletionTokens int64
}

// DailyUsageRange returns the last N calendar days of the daily rollup,
// oldest first. days <= 0 defaults to 30. The window is date-bounded (UTC):
// row count may be less than days when some days had no traffic.
func (s *Store) DailyUsageRange(days int) ([]DailyUsageRow, error) {
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	rows, err := s.db.Query(
		`SELECT day, requests, prompt_tokens, completion_tokens, cache_read, cache_write, reasoning, compressed, cost_usd
         FROM daily_usage WHERE day >= ? ORDER BY day`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DailyUsageRow, 0, days)
	for rows.Next() {
		var r DailyUsageRow
		if err := rows.Scan(&r.Day, &r.Requests, &r.Prompt, &r.Completion, &r.CacheRead,
			&r.CacheWrite, &r.Reasoning, &r.Compressed, &r.CostUSD); err != nil {
			return nil, err
		}
		r.PromptTokens, r.CompletionTokens = r.Prompt, r.Completion
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ActiveDays returns the number of days with at least one request.
func (s *Store) ActiveDays() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM daily_usage WHERE requests > 0`).Scan(&n)
	return n, err
}

// RequestLogRow is one stored gateway request (request_log table).
type RequestLogRow struct {
	ID               int64
	Ts               string
	RID              string
	Model            string
	RequestedModel   string
	Provider         string
	Account          string
	ConnectionID     string
	DurationMS       int64
	PromptTokens     int64
	CompletionTokens int64
	CacheRead        int64
	CacheWrite       int64
	Reasoning        int64
	Compressed       int64
	ComboName        string
	ComboStepID      string
	ComboPath        string
	Status           string
	HTTPStatus       int
	Error            string
	Proxy            string
	Method           string
	Path             string
	// WaitedMS is the pool wait-for-egress time for this request (0 when it
	// was served immediately). Journal category "served after wait".
	WaitedMS int64
}

const requestLogSelect = `SELECT id, ts, rid, model, requested_model, provider, account, connection_id,
	duration_ms, prompt_tokens, completion_tokens, cache_read, cache_write, reasoning, compressed,
	combo_name, combo_step_id, combo_path, status, http_status, error, proxy, waited_ms, method, path
	FROM request_log`

func scanRequestLog(row interface{ Scan(...any) error }) (RequestLogRow, error) {
	var r RequestLogRow
	err := row.Scan(&r.ID, &r.Ts, &r.RID, &r.Model, &r.RequestedModel, &r.Provider, &r.Account, &r.ConnectionID,
		&r.DurationMS, &r.PromptTokens, &r.CompletionTokens, &r.CacheRead, &r.CacheWrite, &r.Reasoning, &r.Compressed,
		&r.ComboName, &r.ComboStepID, &r.ComboPath, &r.Status, &r.HTTPStatus, &r.Error, &r.Proxy, &r.WaitedMS, &r.Method, &r.Path)
	return r, err
}

func (s *Store) queryRequestLogs(q string, args ...any) ([]RequestLogRow, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RequestLogRow{}
	for rows.Next() {
		r, err := scanRequestLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// requestLogListSelect — лёгкий селект списка без тяжёлых TEXT (error, proxy):
// авторефреш гоняет 500 строк каждые 25с, полные тексты нужны только инспектору.
const requestLogListSelect = `SELECT id, ts, rid, model, requested_model, provider, account, connection_id,
	duration_ms, prompt_tokens, completion_tokens, cache_read, cache_write, reasoning, compressed,
	combo_name, combo_step_id, combo_path, status, http_status, waited_ms, method, path
	FROM request_log`

func scanRequestLogList(row interface{ Scan(...any) error }) (RequestLogRow, error) {
	var r RequestLogRow
	err := row.Scan(&r.ID, &r.Ts, &r.RID, &r.Model, &r.RequestedModel, &r.Provider, &r.Account, &r.ConnectionID,
		&r.DurationMS, &r.PromptTokens, &r.CompletionTokens, &r.CacheRead, &r.CacheWrite, &r.Reasoning, &r.Compressed,
		&r.ComboName, &r.ComboStepID, &r.ComboPath, &r.Status, &r.HTTPStatus, &r.WaitedMS, &r.Method, &r.Path)
	return r, err
}

func (s *Store) queryRequestLogList(q string, args ...any) ([]RequestLogRow, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RequestLogRow{}
	for rows.Next() {
		r, err := scanRequestLogList(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RequestLogs returns the most recent request_log rows, newest first (light list, no error/proxy texts).
func (s *Store) RequestLogs(limit int) ([]RequestLogRow, error) {
	limit = clampLimit(limit)
	return s.queryRequestLogList(requestLogListSelect+" ORDER BY id DESC LIMIT ?", limit)
}

// LogByID returns one full request_log row (all columns incl. error/proxy) for the log inspector.
func (s *Store) LogByID(id int64) (RequestLogRow, error) {
	return scanRequestLog(s.db.QueryRow(requestLogSelect+" WHERE id = ?", id))
}

// RequestLogsByQuery searches request_log by model/provider/rid (LIKE); error/status —
// только при query >= 3 символов (поиск по самому жирному TEXT на каждый ввод).
func (s *Store) RequestLogsByQuery(query string, limit int) ([]RequestLogRow, error) {
	limit = clampLimit(limit)
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	pat := "%" + esc.Replace(query) + "%"
	noCase := " COLLATE NOCASE"
	if len([]rune(query)) >= 3 {
		return s.queryRequestLogs(requestLogSelect+
			` WHERE model LIKE ? ESCAPE '\' `+noCase+` OR requested_model LIKE ? ESCAPE '\' `+noCase+` OR provider LIKE ? ESCAPE '\' `+noCase+` OR rid LIKE ? ESCAPE '\' `+noCase+` OR error LIKE ? ESCAPE '\' `+noCase+` OR status LIKE ? ESCAPE '\' `+noCase+` OR CAST(http_status AS TEXT) LIKE ? ESCAPE '\' `+noCase+`
			  ORDER BY id DESC LIMIT ?`,
			pat, pat, pat, pat, pat, pat, pat, limit)
	}
	return s.queryRequestLogList(requestLogListSelect+
		` WHERE model LIKE ? ESCAPE '\' COLLATE NOCASE OR requested_model LIKE ? ESCAPE '\' COLLATE NOCASE OR provider LIKE ? ESCAPE '\' COLLATE NOCASE OR rid LIKE ? ESCAPE '\' COLLATE NOCASE
		  ORDER BY id DESC LIMIT ?`,
		pat, pat, pat, pat, limit)
}

// RequestLogStats returns the total and error rows among the most recent
// `limit` request_log rows (newest first). Used by the risk center for the
// "errors > 5%" alert.
func (s *Store) RequestLogStats(limit int) (total, errors int64, err error) {
	limit = clampLimit(limit)
	var errs int64
	// Один проход, одно окно: раньше второй запрос брал LIMIT из строк
	// только с status='error' за всю историю — при >=limit ошибках rate всегда 100%.
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0) FROM (SELECT status FROM request_log ORDER BY id DESC LIMIT ?)`, limit).Scan(&total, &errs); err != nil {
		return 0, 0, err
	}
	return total, errs, nil
}

// ClearRequestLogs empties request_log (best-effort retention purge).
func (s *Store) ClearRequestLogs() error {
	_, err := s.db.Exec(`DELETE FROM request_log`)
	return err
}

// ClearProxiesLogs empties proxies_log.
func (s *Store) ClearProxiesLogs() error {
	_, err := s.db.Exec(`DELETE FROM proxies_log`)
	return err
}

// exportCap — окно экспорта: вся таблица (135k+ строк с текстами error)
// через Wails-мост = десятки МБ + блокировка единственного коннекта GUI.
const exportCap = 20000

// RequestLogsAll returns the last exportCap request_log rows in ascending id
// order (capped window, not the whole table).
func (s *Store) RequestLogsAll() ([]RequestLogRow, error) {
	return s.queryRequestLogs(requestLogSelect+" WHERE id > (SELECT COALESCE(MAX(id),0)-? FROM request_log) ORDER BY id", exportCap)
}

// ProxiesLogsAll returns the last exportCap proxies_log rows (same window as requests).
func (s *Store) ProxiesLogsAll() ([]ProxiesLogRow, error) {
	rows, err := s.db.Query(`SELECT id, ts, proxy, action, ok, latency_ms, error FROM proxies_log WHERE id > (SELECT COALESCE(MAX(id),0)-? FROM proxies_log) ORDER BY id`, exportCap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProxiesLogRow{}
	for rows.Next() {
		var r ProxiesLogRow
		var ok int
		if err := rows.Scan(&r.ID, &r.Ts, &r.Proxy, &r.Action, &ok, &r.LatencyMS, &r.Error); err != nil {
			return nil, err
		}
		r.OK = ok != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// clampLimit bounds a user-supplied row limit to a sane window.
func clampLimit(limit int) int {
	if limit <= 0 {
		return 200
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

// UsageByModelRow — агрегат request_log по модели за окно.
type UsageByModelRow struct {
	Model      string
	Provider   string
	Requests   int64
	Prompt     int64
	Completion int64
	Total      int64
	Errors     int64
	LastUsed   string
	CostUSD    int64
}

// UsageByModel агрегирует request_log по модели (новые сверху по запросам).
func (s *Store) UsageByModel(days int) ([]UsageByModelRow, error) {
	cutoff := ""
	if days > 0 {
		cutoff = time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	}
	q := `SELECT model, provider,
		COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(completion_tokens+cache_read+cache_write+(prompt_tokens-CASE WHEN cache_read>prompt_tokens THEN prompt_tokens ELSE cache_read END)),0),
		COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0),
		COALESCE(MAX(ts),''), COALESCE(SUM(cost_usd),0)
		FROM request_log`
	var rows *sql.Rows
	var err error
	if cutoff != "" {
		// Sargable: ts — RFC3339, сравнение ts >= 'YYYY-MM-DD' эквивалентно
		// substr(ts,1,10) >= 'YYYY-MM-DD', но идёт range SEARCH по idx_request_log_ts.
		rows, err = s.db.Query(q+` WHERE ts >= ? GROUP BY model, provider ORDER BY COUNT(*) DESC LIMIT 50`, cutoff)
	} else {
		rows, err = s.db.Query(q + ` GROUP BY model, provider ORDER BY COUNT(*) DESC LIMIT 50`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageByModelRow{}
	for rows.Next() {
		var r UsageByModelRow
		if err := rows.Scan(&r.Model, &r.Provider, &r.Requests, &r.Prompt, &r.Completion,
			&r.Total, &r.Errors, &r.LastUsed, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageByProviderRow — агрегат request_log по провайдеру за окно.
type UsageByProviderRow struct {
	Provider string
	Requests int64
	Prompt   int64
	Complete int64
	Total    int64
	Errors   int64
	CostUSD  int64
}

// UsageByProvider агрегирует request_log по провайдеру.
func (s *Store) UsageByProvider(days int) ([]UsageByProviderRow, error) {
	cutoff := ""
	if days > 0 {
		cutoff = time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	}
	q := `SELECT provider,
		COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(completion_tokens+cache_read+cache_write+(prompt_tokens-CASE WHEN cache_read>prompt_tokens THEN prompt_tokens ELSE cache_read END)),0),
		COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(cost_usd),0)
		FROM request_log`
	var rows *sql.Rows
	var err error
	// Sargable: ts — RFC3339, ts >= 'YYYY-MM-DD' эквивалентно cutoff по дню,
	// но идёт range SEARCH по idx_request_log_ts вместо full scan.
	if cutoff != "" {
		rows, err = s.db.Query(q+` WHERE ts >= ? GROUP BY provider ORDER BY COUNT(*) DESC`, cutoff)
	} else {
		rows, err = s.db.Query(q + ` GROUP BY provider ORDER BY COUNT(*) DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageByProviderRow{}
	for rows.Next() {
		var r UsageByProviderRow
		if err := rows.Scan(&r.Provider, &r.Requests, &r.Prompt, &r.Complete,
			&r.Total, &r.Errors, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageStreak считает подряд идущие дни с трафиком назад от сегодня (UTC).
func (s *Store) UsageStreak() (int, error) {
	rows, err := s.db.Query(`SELECT day FROM daily_usage WHERE requests > 0 ORDER BY day DESC LIMIT 400`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return 0, err
		}
		have[d] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	streak := 0
	cur := time.Now().UTC()
	// streak идёт от сегодня назад; если сегодня пусто — старт со вчера.
	if !have[cur.Format("2006-01-02")] {
		cur = cur.AddDate(0, 0, -1)
	}
	for {
		if !have[cur.Format("2006-01-02")] {
			break
		}
		streak++
		cur = cur.AddDate(0, 0, -1)
	}
	return streak, nil
}

// ActivityRow — один день активности для heatmap (все дни с трафиком).
type ActivityRow struct {
	Day   string
	Total int64
}

// ActivityMap возвращает map день->токены за окно (heatmap-источник).
func (s *Store) ActivityMap(days int) (map[string]int64, error) {
	cutoff := ""
	if days > 0 {
		cutoff = time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	}
	q := `SELECT substr(ts,1,10) AS day, COALESCE(SUM(completion_tokens+cache_read+cache_write+(prompt_tokens-CASE WHEN cache_read>prompt_tokens THEN prompt_tokens ELSE cache_read END)),0)
		FROM request_log`
	var rows *sql.Rows
	var err error
	if cutoff != "" {
		rows, err = s.db.Query(q+` WHERE ts >= ? GROUP BY day ORDER BY day`, cutoff)
	} else {
		rows, err = s.db.Query(q + ` GROUP BY day ORDER BY day`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var d string
		var t int64
		if err := rows.Scan(&d, &t); err != nil {
			return nil, err
		}
		out[d] = t
	}
	return out, rows.Err()
}

// DailyTrendRow — один день тренда (токены + cost).
type DailyTrendRow struct {
	Day        string
	Requests   int64
	Prompt     int64
	Completion int64
	Total      int64
	CostUSD    int64
}

// DailyTrend возвращает ряд по дням из daily_usage (есть cost из роллапа).
func (s *Store) DailyTrend(days int) ([]DailyTrendRow, error) {
	if days <= 0 {
		days = 90
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	rows, err := s.db.Query(
		`SELECT day, requests, prompt_tokens, completion_tokens,
			completion_tokens+cache_read+cache_write+(prompt_tokens-CASE WHEN cache_read>prompt_tokens THEN prompt_tokens ELSE cache_read END), cost_usd
		 FROM daily_usage WHERE day >= ? ORDER BY day`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DailyTrendRow{}
	for rows.Next() {
		var r DailyTrendRow
		if err := rows.Scan(&r.Day, &r.Requests, &r.Prompt, &r.Completion, &r.Total, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WeeklyRow — агрегат по дню недели (0=вс..6=сб).
type WeeklyRow struct {
	DayOfWeek int64
	Days      int64
	Requests  int64
	Total     int64
}

// WeeklyPattern группирует request_log по дню недели.
func (s *Store) WeeklyPattern(days int) ([]WeeklyRow, error) {
	cutoff := ""
	if days > 0 {
		cutoff = time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	}
	q := `SELECT CAST(strftime('%w', ts) AS INTEGER), COUNT(DISTINCT substr(ts,1,10)),
		COUNT(*), COALESCE(SUM(completion_tokens+cache_read+cache_write+(prompt_tokens-CASE WHEN cache_read>prompt_tokens THEN prompt_tokens ELSE cache_read END)),0)
		FROM request_log`
	var rows *sql.Rows
	var err error
	if cutoff != "" {
		rows, err = s.db.Query(q+` WHERE ts >= ? GROUP BY strftime('%w', ts) ORDER BY 1`, cutoff)
	} else {
		rows, err = s.db.Query(q + ` GROUP BY strftime('%w', ts) ORDER BY 1`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WeeklyRow{}
	for rows.Next() {
		var r WeeklyRow
		if err := rows.Scan(&r.DayOfWeek, &r.Days, &r.Requests, &r.Total); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertProxiesLog appends one egress-proxy event row. Best-effort callers
// (routing proxy bookkeeping) ignore the error.
func (s *Store) InsertProxiesLog(proxy, action string, ok bool, latencyMS int64, errText string) error {
	okInt := 0
	if ok {
		okInt = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO proxies_log(ts, proxy, action, ok, latency_ms, error) VALUES(?,?,?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339), proxy, action, okInt, latencyMS, errText)
	if err != nil {
		return err
	}
	// Retention: лента пула не хранит древность — старше 30д режем, иначе
	// месячные bursts висят "live" вечно.
	_, _ = s.db.Exec(`DELETE FROM proxies_log WHERE ts < ?`,
		time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339))
	return nil
}

// ProxiesLogRow is one egress-proxy event (proxies_log table).
type ProxiesLogRow struct {
	ID        int64
	Ts        string
	Proxy     string
	Action    string
	OK        bool
	LatencyMS int64
	Error     string
}

// ProxiesLogs returns the most recent proxies_log rows, newest first.
func (s *Store) ProxiesLogs(limit int) ([]ProxiesLogRow, error) {
	limit = clampLimit(limit)
	rows, err := s.db.Query(
		`SELECT id, ts, proxy, action, ok, latency_ms, error FROM proxies_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProxiesLogRow{}
	for rows.Next() {
		var r ProxiesLogRow
		var ok int
		if err := rows.Scan(&r.ID, &r.Ts, &r.Proxy, &r.Action, &ok, &r.LatencyMS, &r.Error); err != nil {
			return nil, err
		}
		r.OK = ok != 0
		out = append(out, r)
	}
	return out, rows.Err()
}
