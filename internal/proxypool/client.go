// Package proxypool is the ai-router client for the hub "egress" module
// (hub cmd/egress, default :8801). It issues Hub module actions over HTTP
// — POST /actions/{action}, Authorization: Bearer <token>, body
// {"params":{...}} — and decodes the {"ok":true,"result":{...}} envelope.
//
// When configured with a logger it writes best-effort proxy actions
// (acquire/release/rotate/connect) into proxies_log.
package proxypool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config drives the pool client. Strategy is one of
// round_robin (default) | best_latency | sticky. PerRequest makes every
// Acquire request a fresh channel (the egress pool never returns the
// channel it handed out last time for per_request acquires); RotateAfter > 0
// forces a channel rotation after that many uses.
type Config struct {
	BaseURL     string
	Token       string
	Strategy    string
	PerRequest  bool
	RotateAfter int

	// Enabled is a convenience mirror of the egress config section; it does
	// not gate the client itself (callers decide whether to construct it).
	Enabled bool

	HTTPClient *http.Client // optional; a 15s-timeout client is used by default
}

// Geo is the geo-location block attached to a proxy lease.
type Geo struct {
	Country string `json:"country"`
	City    string `json:"city"`
	ASN     string `json:"asn"`
}

// Proxy is a lease granted by proxies.acquire.
type Proxy struct {
	ID        string `json:"id"`
	LeaseID   string `json:"lease_id"`
	VirtualIP string `json:"virtual_ip"`
	TTL_S     int    `json:"ttl_s"`
	Socks     string `json:"socks"`
	HTTP      string `json:"http"`
	PublicIP  string `json:"public_ip"`
	LatencyMs int64  `json:"latency_ms"`
	Geo       Geo    `json:"geo"`
}

// Account is one warp-router account as reported by accounts.list.
type Account struct {
	ID        string `json:"id"`
	VirtualIP string `json:"virtual_ip"`
	Healthy   bool   `json:"healthy"`
	FailCount int    `json:"fail_count"`
	LatencyMs int64  `json:"latency_ms"`
	Country   string `json:"country"`
	City      string `json:"city"`
	ASN       string `json:"asn"`
}

// Server is one warp-router server from servers.list. Warp's shape varies, so
// parsing is best-effort and generous.
type Server struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Colo      string `json:"colo"`
	LatencyMs int64  `json:"latency_ms"`
	Country   string `json:"country"`
	City      string `json:"city"`
	ASN       string `json:"asn"`
}

// PoolStatus aggregates the egress "status" contract (the pool health block,
// the active account and the public IP) plus accounts.list and servers.list.
type PoolStatus struct {
	// Enabled is a GUI hint (not returned by the egress status action): it is
	// set by the wiring layer to report whether the pool is configured at all.
	Enabled          bool    `json:"enabled"`
	Total            int     `json:"total"`
	Alive            int     `json:"alive"`
	Dead             int     `json:"dead"`
	Connecting       int     `json:"connecting"`
	ConnectRate      float64 `json:"connect_rate"`
	Failures24h      int     `json:"failures_24h"`
	Degraded         bool    `json:"degraded"`
	AvgLatencyMs     int64   `json:"avg_latency_ms"`
	LastLatencyMs    int64   `json:"last_latency_ms"`
	LatencyUpdatedAt int64   `json:"latency_updated_at"`

	ActiveAccountID string `json:"active_account_id"`
	PublicIP        string `json:"public_ip"`

	Accounts []Account `json:"accounts"`
	Servers  []Server  `json:"servers"`
}

// LogFunc records one proxy action. It is called for status/connect/
// rotate/acquire/release; err is nil on success.
type LogFunc func(proxy, action string, ok bool, latencyMS int64, errText string)

// Client is a pooled egress RPC client.
type Client struct {
	cfg Config
	hc  *http.Client

	logMu sync.Mutex
	logf  LogFunc
}

// New builds a pool client. cfg.BaseURL may carry a trailing slash.
func New(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{cfg: cfg, hc: hc}
}

// SetLog installs the proxies_log sink (nil to disable bookkeeping).
func (c *Client) SetLog(fn LogFunc) {
	c.logMu.Lock()
	c.logf = fn
	c.logMu.Unlock()
}

func (c *Client) scribe(proxy, action string, ok bool, ms int64, err error) {
	c.logMu.Lock()
	fn := c.logf
	c.logMu.Unlock()
	if fn == nil {
		return
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	fn(proxy, action, ok, ms, errText)
}

func (c *Client) url(action string) string {
	return strings.TrimRight(c.cfg.BaseURL, "/") + "/actions/" + action
}

// call performs one hub action. params nil → empty object.
func (c *Client) call(ctx context.Context, action string, params any, out any) error {
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(action), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("egress %s: %w", action, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("egress %s: %w", action, err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("egress %s: read: %w", action, err)
	}
	snip := strings.TrimSpace(string(bodyBytes))
	if len(snip) > 160 {
		snip = snip[:160] + "\u2026"
	}
	// HTTP-ошибка раньше decode: иначе 404-text/plain маскируется под "decode".
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if snip == "" {
			snip = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("egress %s: http %d: %s", action, resp.StatusCode, snip)
	}
	// Конверт только если в теле есть ключ "ok": иначе сырой результат
	// без обёртки (берём напрямую) либо мусор.
	var top map[string]json.RawMessage
	hasEnvelope := json.Unmarshal(bodyBytes, &top) == nil && top != nil && rawHas(top, "ok")
	if !hasEnvelope {
		if out != nil && json.Unmarshal(bodyBytes, out) == nil {
			return nil
		}
		return fmt.Errorf("egress %s: bad envelope (body %.120s)", action, snip)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(bodyBytes, &env); err != nil {
		return fmt.Errorf("egress %s: bad envelope: %v (body %.120s)", action, err, snip)
	}
	if !env.OK {
		msg := envelopeErrText(env.Error)
		if msg == "" {
			msg = "action " + action + " failed"
		}
		return fmt.Errorf("egress %s: %s", action, msg)
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("egress %s: result: %w", action, err)
		}
	}
	return nil
}

// rawHas проверяет наличие ключа в уже разобранном объекте.
func rawHas(m map[string]json.RawMessage, key string) bool {
	_, ok := m[key]
	return ok
}

// envelopeErrText терпит обе формы error: строку и объект {code,message}.
func envelopeErrText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var obj struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.Message != "" {
			return obj.Message
		}
		return obj.Code
	}
	return strings.TrimSpace(string(raw))
}

// op wraps call + bookkeeping: it measures the round-trip, logs the action and
// returns both the call error and the result.
func (c *Client) op(ctx context.Context, action, logAction, proxy string, params any, out any) error {
	start := time.Now()
	err := c.call(ctx, action, params, out)
	c.scribe(proxy, logAction, err == nil, time.Since(start).Milliseconds(), err)
	return err
}

// ConfiguredStrategy returns the strategy that will be sent on Acquire.
func (c *Client) ConfiguredStrategy() string { return c.cfg.Strategy }

// ConfiguredPerRequest reports whether Acquire uses per-request rotation.
func (c *Client) ConfiguredPerRequest() bool { return c.cfg.PerRequest }

func (c *Client) acquireParams() map[string]any {
	p := map[string]any{}
	if c.cfg.Strategy != "" {
		p["strategy"] = c.cfg.Strategy
	}
	if c.cfg.PerRequest || c.cfg.RotateAfter > 0 {
		p["rotation"] = map[string]any{
			"per_request":  c.cfg.PerRequest,
			"rotate_after": c.cfg.RotateAfter,
		}
	}
	return p
}

// Status resolves the pool health, the active account/raw public IP, and the
// account/list and server/list snapshots. It returns the (possibly partial)
// status together with the underlying action error so callers can show the
// "egress off / unreachable" state.
func (c *Client) Status(ctx context.Context) (*PoolStatus, error) {
	ps := &PoolStatus{Accounts: []Account{}, Servers: []Server{}}

	var st map[string]any
	err := c.call(ctx, "status", nil, &st)
	if err == nil {
		ps.ActiveAccountID = asString(st["active_account_id"])
		ps.PublicIP = asString(st["public_ip"])
		if pool, ok := st["pool"].(map[string]any); ok {
			ps.Total = asInt(pool["total"])
			ps.Alive = asInt(pool["alive"])
			ps.Dead = asInt(pool["dead"])
			ps.Connecting = asInt(pool["connecting"])
			ps.ConnectRate = asFloat(pool["connect_rate"])
			ps.Failures24h = asInt(pool["failures_24h"])
			ps.Degraded = asBool(pool["degraded"])
			ps.AvgLatencyMs = asInt64(pool["avg_latency_ms"])
			ps.LastLatencyMs = asInt64(pool["last_latency_ms"])
			ps.LatencyUpdatedAt = asInt64(pool["latency_updated_at"])
		}
	}

	var acct struct {
		Accounts []accountRaw `json:"accounts"`
	}
	if err2 := c.call(ctx, "accounts.list", nil, &acct); err2 == nil {
		for _, a := range acct.Accounts {
			ps.Accounts = append(ps.Accounts, a.toAccount())
		}
	}

	ps.Servers = c.servers(ctx)
	return ps, err
}

// Connect activates a server (warp /api/v1/connect).
func (c *Client) Connect(ctx context.Context, server string) error {
	params := map[string]any{}
	if server != "" {
		params["server"] = server
	}
	return c.op(ctx, "connect", "connect", server, params, nil)
}

// Disconnect deactivates the active server/account.
func (c *Client) Disconnect(ctx context.Context) error {
	return c.op(ctx, "disconnect", "disconnect", "", nil, nil)
}

// Rotate rotates the active account; mode ("" for default) is passed along.
func (c *Client) Rotate(ctx context.Context, mode string) error {
	params := map[string]any{}
	if mode != "" {
		params["mode"] = mode
	}
	return c.op(ctx, "rotate", "rotate", "", params, nil)
}

// Acquire leases one channel. Per-request rotation and rotate-after are taken
// from the client config. The acquired proxy is logged on success/failure.
func (c *Client) Acquire(ctx context.Context) (*Proxy, error) {
	var pr Proxy
	start := time.Now()
	err := c.call(ctx, "proxies.acquire", c.acquireParams(), &pr)
	logProxy := ""
	if err == nil {
		logProxy = pr.ID
	} else if pr.LeaseID != "" {
		logProxy = pr.LeaseID
	}
	c.scribe(logProxy, "acquire", err == nil, time.Since(start).Milliseconds(), err)
	if err != nil {
		return nil, err
	}
	return &pr, nil
}

// Release returns a lease to the pool (best-effort; errors are surfaced).
func (c *Client) Release(ctx context.Context, leaseID string) error {
	return c.op(ctx, "proxies.release", "release", leaseID, map[string]any{"lease_id": leaseID}, nil)
}

// ReleaseAcquired releases the lease behind proxy (nil-safe).
func (c *Client) ReleaseAcquired(ctx context.Context, pr *Proxy) error {
	if pr == nil {
		return nil
	}
	return c.Release(ctx, pr.LeaseID)
}

// Heartbeat extends a lease's TTL (long sessions).
func (c *Client) Heartbeat(ctx context.Context, leaseID string) error {
	return c.call(ctx, "proxies.heartbeat", map[string]any{"lease_id": leaseID}, nil)
}

func (c *Client) servers(ctx context.Context) []Server {
	var raw json.RawMessage
	if err := c.call(ctx, "servers.list", nil, &raw); err != nil {
		return nil
	}
	return parseServers(raw)
}

func parseServers(raw []byte) []Server {
	var arr []Server
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	var obj struct {
		Servers []Server `json:"servers"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Servers
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		out := make([]Server, 0, len(m))
		for _, v := range m {
			if b, err := json.Marshal(v); err == nil {
				var s Server
				if json.Unmarshal(b, &s) == nil && s.Name != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
}

type accountRaw struct {
	ID        string         `json:"id"`
	VirtualIP string         `json:"virtual_ip"`
	Healthy   *bool          `json:"healthy"`
	FailCount int            `json:"fail_count"`
	LatencyMs int64          `json:"latency_ms"`
	Geo       map[string]any `json:"geo"`
}

func (a accountRaw) toAccount() Account {
	out := Account{
		ID: a.ID, VirtualIP: a.VirtualIP, FailCount: a.FailCount, LatencyMs: a.LatencyMs,
	}
	if a.Healthy != nil {
		out.Healthy = *a.Healthy
	}
	if a.Geo != nil {
		out.Country = asString(a.Geo["country"])
		out.City = asString(a.Geo["city"])
		out.ASN = asString(a.Geo["asn"])
	}
	return out
}

// --- tolerant prett(scalar) helpers for map[string]any decoding ---

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case float64:
		return b != 0
	default:
		return false
	}
}

func asFloat(v any) float64 {
	switch f := v.(type) {
	case float64:
		return f
	case float32:
		return float64(f)
	case int:
		return float64(f)
	case int64:
		return float64(f)
	default:
		return 0
	}
}

func asInt(v any) int { return int(asInt64(v)) }

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}
