// Package hubmod integrates ai-router with the hub: it registers the module,
// serves hub actions (chat.complete, models.list, keys.list, usage, test)
// over the SDK ActionMux and publishes ai.request / ai.fallback events.
package hubmod

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/gateway"
	"github.com/ByJotaro/ai-router/internal/keys"
	"github.com/ByJotaro/ai-router/internal/logring"
	"github.com/ByJotaro/ai-router/internal/route"
	"github.com/ByJotaro/ai-router/internal/sdk"
)

// Module is the hub-facing side of ai-router.
type Module struct {
	cfg        config.HubConfig
	client     *sdk.Client
	router     *route.Router
	keys       *keys.Store
	ring       *logring.Ring
	version    string
	mux        *sdk.ActionMux
	httpServer *http.Server
}

// New creates a hub module. Call Start to register and serve.
func New(cfg *config.Config, r *route.Router, ks *keys.Store, ring *logring.Ring, version string) *Module {
	m := &Module{
		cfg:     cfg.Hub,
		router:  r,
		keys:    ks,
		ring:    ring,
		version: version,
		mux:     sdk.NewActionMux(),
	}
	if cfg.Hub.URL != "" {
		m.client = sdk.NewClient(cfg.Hub.URL, cfg.Hub.Module, cfg.Hub.Token)
	}
	r.OnEvent = m.emit
	m.mux.Handle("chat.complete", m.actionChatComplete)
	m.mux.Handle("models.list", m.actionModelsList)
	m.mux.Handle("models.sync", m.actionModelsList)
	m.mux.Handle("keys.list", m.actionKeysList)
	m.mux.Handle("keys.create", m.actionKeyCreate)
	m.mux.Handle("keys.revoke", m.actionKeyRevoke)
	m.mux.Handle("quota.set", m.actionQuotaSet)
	m.mux.Handle("usage", m.actionUsage)
	m.mux.Handle("usage.reset", m.actionUsageReset)
	m.mux.Handle("test", m.actionTest)
	m.mux.Handle("routes.status", m.actionRoutesStatus)
	return m
}

// Capabilities reports the module's actions to the hub.
func (m *Module) Capabilities() []string {
	return []string{
		"chat.complete", "models.list", "models.sync",
		"keys.list", "keys.create", "keys.revoke", "quota.set",
		"usage", "usage.reset", "test", "routes.status",
	}
}

// Start registers with the hub (best-effort: a pre-declared module or an
// unavailable hub must not prevent serving) and serves /health and /actions
// on cfg.Hub.Addr.
func (m *Module) Start() error {
	if m.client != nil && m.cfg.Register {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		baseURL := "http://" + m.cfg.Addr
		if err := m.client.Register(ctx, baseURL, m.version, m.Capabilities()); err != nil {
			slog.Warn("hub register failed (module still serves actions)", "err", err)
		}
	}
	addr := m.cfg.Addr
	go func() {
		err := m.mux.Serve(addr, sdk.Options{
			Token:        m.cfg.Token,
			Version:      m.version,
			Capabilities: m.Capabilities(),
			Client:       m.client,
		})
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("hub module server stopped", "addr", addr, "err", err)
		}
	}()
	return nil
}

// WireGateway hooks gateway request events so they become hub events.
func (m *Module) WireGateway(g *gateway.Server) {
	g.SetOnRequest(m.emit)
}

func (m *Module) emit(typ string, payload any) {
	if m.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.client.EmitEvent(ctx, typ, payload); err != nil {
		slog.Debug("hub event failed", "type", typ, "err", err)
	}
}

// --- actions ---

func (m *Module) actionChatComplete(ctx context.Context, params json.RawMessage, _ *sdk.Job) (any, error) {
	var in struct {
		Model       string          `json:"model"`
		Messages    json.RawMessage `json:"messages"`
		MaxTokens   int             `json:"max_tokens,omitempty"`
		Temperature *float64        `json:"temperature,omitempty"`
	}
	if err := json.Unmarshal(params, &in); err != nil {
		return nil, &sdk.Error{Code: "bad_request", Message: "params: " + err.Error()}
	}
	if in.Model == "" {
		return nil, &sdk.Error{Code: "bad_request", Message: "model is required"}
	}
	body := map[string]any{
		"model":    in.Model,
		"messages": messagesOrEmpty(in.Messages),
		"stream":   false,
	}
	if in.MaxTokens > 0 {
		body["max_tokens"] = in.MaxTokens
	}
	if in.Temperature != nil {
		body["temperature"] = *in.Temperature
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	res, err := m.router.Call(ctx, in.Model, data, false)
	if err != nil {
		return nil, &sdk.Error{Code: "upstream_error", Message: err.Error()}
	}
	defer res.Resp.Body.Close()
	out := parseCompletion(res.Resp.Body)
	out["provider"] = res.Provider
	out["latency_ms"] = time.Since(start).Milliseconds()
	return out, nil
}

func (m *Module) actionModelsList(context.Context, json.RawMessage, *sdk.Job) (any, error) {
	return map[string]any{"models": m.router.Models()}, nil
}

func (m *Module) actionKeysList(context.Context, json.RawMessage, *sdk.Job) (any, error) {
	return map[string]any{"keys": m.keys.List()}, nil
}

// actionKeyCreate provisions a virtual key with quotas at runtime so other
// hub modules can mint scoped credentials for the endpoint.
func (m *Module) actionKeyCreate(_ context.Context, params json.RawMessage, _ *sdk.Job) (any, error) {
	var in config.KeyConf
	if err := json.Unmarshal(params, &in); err != nil {
		return nil, &sdk.Error{Code: "bad_request", Message: "params: " + err.Error()}
	}
	if err := m.keys.Create(in); err != nil {
		return nil, &sdk.Error{Code: "invalid_request", Message: err.Error()}
	}
	return map[string]any{"ok": true, "name": in.Name}, nil
}

// actionKeyRevoke disables a virtual key by its value.
func (m *Module) actionKeyRevoke(_ context.Context, params json.RawMessage, _ *sdk.Job) (any, error) {
	var in struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(params, &in); err != nil || in.Key == "" {
		return nil, &sdk.Error{Code: "bad_request", Message: "key is required"}
	}
	if err := m.keys.Revoke(in.Key); err != nil {
		return nil, &sdk.Error{Code: "invalid_request", Message: err.Error()}
	}
	return map[string]any{"ok": true}, nil
}

// actionQuotaSet updates quotas on an existing key by name.
func (m *Module) actionQuotaSet(_ context.Context, params json.RawMessage, _ *sdk.Job) (any, error) {
	var in struct {
		Name        string `json:"name"`
		QuotaTokens int64  `json:"quota_tokens"`
		QuotaReqs   int64  `json:"quota_reqs"`
		BudgetUSD   int64  `json:"budget_usd"`
	}
	if err := json.Unmarshal(params, &in); err != nil || in.Name == "" {
		return nil, &sdk.Error{Code: "bad_request", Message: "name is required"}
	}
	if err := m.keys.SetQuota(in.Name, in.QuotaTokens, in.QuotaReqs, in.BudgetUSD); err != nil {
		return nil, &sdk.Error{Code: "invalid_request", Message: err.Error()}
	}
	return map[string]any{"ok": true, "name": in.Name}, nil
}

// actionRoutesStatus exposes breaker and lockout state to the hub GUI.
func (m *Module) actionRoutesStatus(context.Context, json.RawMessage, *sdk.Job) (any, error) {
	return map[string]any{
		"breakers": m.router.BreakerState(),
		"lockout":  m.router.LockoutState(),
	}, nil
}

func (m *Module) actionUsage(context.Context, json.RawMessage, *sdk.Job) (any, error) {
	views := m.keys.List()
	var totalTokens, totalReqs int64
	for _, v := range views {
		totalTokens += v.UsageTokens
		totalReqs += v.UsageReqs
	}
	return map[string]any{
		"keys": views, "total_tokens": totalTokens, "total_requests": totalReqs,
	}, nil
}

func (m *Module) actionUsageReset(_ context.Context, params json.RawMessage, _ *sdk.Job) (any, error) {
	var in struct {
		Name string `json:"name,omitempty"`
	}
	_ = json.Unmarshal(params, &in)
	if in.Name != "" {
		if err := m.keys.ResetUsage(in.Name); err != nil {
			return nil, &sdk.Error{Code: "invalid_request", Message: err.Error()}
		}
		return map[string]any{"ok": true, "name": in.Name, "reset": "key"}, nil
	}
	// No name -> reset all (monthly roll / manual wipe) - also clears daily_usage
	if err := m.keys.ResetAllUsage(); err != nil {
		return nil, &sdk.Error{Code: "internal_error", Message: err.Error()}
	}
	return map[string]any{"ok": true, "reset": "all"}, nil
}

// actionTest probes the first available model with a tiny completion so the
// hub GUI can run a one-click health check on the module.
func (m *Module) actionTest(ctx context.Context, params json.RawMessage, _ *sdk.Job) (any, error) {
	var in struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(params, &in)
	model := in.Model
	if model == "" {
		models := m.router.Models()
		if len(models) == 0 {
			return nil, &sdk.Error{Code: "no_models", Message: "no models configured"}
		}
		model = models[0]
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"max_tokens": 8,
		"stream":     false,
	})
	start := time.Now()
	res, err := m.router.Call(ctx, model, body, false)
	if err != nil {
		return map[string]any{"ok": false, "model": model, "error": err.Error(), "latency_ms": time.Since(start).Milliseconds()}, nil
	}
	defer res.Resp.Body.Close()
	out := parseCompletion(res.Resp.Body)
	out["ok"] = res.Status < 400
	out["model"] = model
	out["provider"] = res.Provider
	out["latency_ms"] = time.Since(start).Milliseconds()
	return out, nil
}

// parseCompletion decodes an upstream chat completion into a compact map.
func parseCompletion(body io.Reader) map[string]any {
	out := map[string]any{}
	dec := json.NewDecoder(body)
	var raw map[string]any
	if dec.Decode(&raw) != nil {
		return out
	}
	if id, ok := raw["id"]; ok {
		out["id"] = id
	}
	if model, ok := raw["model"]; ok {
		out["model"] = model
	}
	if choices, ok := raw["choices"].([]any); ok && len(choices) > 0 {
		first, _ := choices[0].(map[string]any)
		if msg, ok := first["message"].(map[string]any); ok {
			if c, ok := msg["content"]; ok {
				out["content"] = c
			}
			if tc, ok := msg["tool_calls"]; ok {
				out["tool_calls"] = tc
			}
		}
		if fr, ok := first["finish_reason"]; ok {
			out["finish_reason"] = fr
		}
	}
	if usage, ok := raw["usage"]; ok {
		out["usage"] = usage
	}
	return out
}

func messagesOrEmpty(raw json.RawMessage) any {
	if len(raw) == 0 {
		return []any{}
	}
	var msgs []any
	if err := json.Unmarshal(raw, &msgs); err != nil || len(msgs) == 0 {
		return []any{}
	}
	return msgs
}
