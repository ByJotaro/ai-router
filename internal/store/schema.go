package store

// schema is the full ai-router SQLite schema. Timestamps are RFC3339 text;
// money is integer cents; encrypted key values carry the enc:v1: prefix
// (see internal/crypto).
const schema = `
CREATE TABLE IF NOT EXISTS providers (
	name TEXT PRIMARY KEY,
	tier TEXT NOT NULL DEFAULT 'free',
	weight REAL NOT NULL DEFAULT 1,
	models TEXT NOT NULL DEFAULT '[]',
	fallbacks TEXT NOT NULL DEFAULT '[]',
	base_url TEXT NOT NULL DEFAULT '',
	egress TEXT NOT NULL DEFAULT '',
	timeout_s INTEGER NOT NULL DEFAULT 0,
	cost_per_1m_in REAL NOT NULL DEFAULT 0,
	cost_per_1m_out REAL NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS pool_keys (
	provider TEXT NOT NULL,
	key_hash TEXT NOT NULL,
	value_enc TEXT NOT NULL DEFAULT '',
	failures INTEGER NOT NULL DEFAULT 0,
	disabled INTEGER NOT NULL DEFAULT 0,
	disabled_until TEXT,
	backoff_level INTEGER NOT NULL DEFAULT 0,
	rate_limited_until TEXT,
	last_err TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (provider, key_hash)
);

CREATE TABLE IF NOT EXISTS keys (
	name TEXT PRIMARY KEY,
	key_hash TEXT NOT NULL UNIQUE,
	value_enc TEXT NOT NULL,
	quota_tokens INTEGER NOT NULL DEFAULT 0,
	quota_reqs INTEGER NOT NULL DEFAULT 0,
	budget_usd INTEGER NOT NULL DEFAULT 0,
	no_log INTEGER NOT NULL DEFAULT 0,
	models TEXT NOT NULL DEFAULT '[]',
	revoked INTEGER NOT NULL DEFAULT 0,
	usage_tokens INTEGER NOT NULL DEFAULT 0,
	usage_reqs INTEGER NOT NULL DEFAULT 0,
	usage_usd INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS breakers (
	provider TEXT PRIMARY KEY,
	state TEXT NOT NULL DEFAULT 'closed',
	failures INTEGER NOT NULL DEFAULT 0,
	successes INTEGER NOT NULL DEFAULT 0,
	opened_at TEXT,
	opened_until TEXT,
	consecutive_failures INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS quota (
	key TEXT NOT NULL,
	model TEXT NOT NULL,
	quota_tokens INTEGER NOT NULL DEFAULT 0,
	quota_reqs INTEGER NOT NULL DEFAULT 0,
	usage_tokens INTEGER NOT NULL DEFAULT 0,
	usage_reqs INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (key, model)
);

CREATE TABLE IF NOT EXISTS usage (
	ts TEXT NOT NULL,
	key TEXT NOT NULL,
	provider TEXT NOT NULL,
	model TEXT NOT NULL,
	tokens INTEGER NOT NULL DEFAULT 0,
	requests INTEGER NOT NULL DEFAULT 0,
	cost_usd INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	status INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage(ts);

CREATE TABLE IF NOT EXISTS models_catalog (
	model TEXT PRIMARY KEY,
	provider TEXT NOT NULL DEFAULT '',
	input_price_cents INTEGER NOT NULL DEFAULT 0,
	output_price_cents INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS audit (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL DEFAULT '',
	actor TEXT NOT NULL,
	action TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit(ts);

CREATE TABLE IF NOT EXISTS idempotency (
	hash TEXT PRIMARY KEY,
	key TEXT NOT NULL,
	response TEXT NOT NULL,
	status INTEGER NOT NULL DEFAULT 200,
	created_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS request_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL DEFAULT '',
	rid TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	requested_model TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	account TEXT NOT NULL DEFAULT '',
	connection_id TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read INTEGER NOT NULL DEFAULT 0,
	cache_write INTEGER NOT NULL DEFAULT 0,
	reasoning INTEGER NOT NULL DEFAULT 0,
	compressed INTEGER NOT NULL DEFAULT 0,
	combo_name TEXT NOT NULL DEFAULT '',
	combo_step_id TEXT NOT NULL DEFAULT '',
	combo_path TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	http_status INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	proxy TEXT NOT NULL DEFAULT '',
	waited_ms INTEGER NOT NULL DEFAULT 0,
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	cost_usd INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_request_log_ts ON request_log(ts);
CREATE INDEX IF NOT EXISTS idx_request_log_day ON request_log(substr(ts,1,10));

CREATE TABLE IF NOT EXISTS daily_usage (
	day TEXT PRIMARY KEY,
	requests INTEGER NOT NULL DEFAULT 0,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read INTEGER NOT NULL DEFAULT 0,
	cache_write INTEGER NOT NULL DEFAULT 0,
	reasoning INTEGER NOT NULL DEFAULT 0,
	compressed INTEGER NOT NULL DEFAULT 0,
	cost_usd INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS combo_presets (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	steps_json TEXT NOT NULL DEFAULT '[]',
	created_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS proxies_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL DEFAULT '',
	proxy TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL DEFAULT '',
	ok INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_proxies_log_ts ON proxies_log(ts);
`
