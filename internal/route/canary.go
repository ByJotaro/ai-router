package route

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// canaryState is the per-provider canary budget.
//
// Degradation is judged on a SLIDING window of the last N samples, not on a
// lifetime counter: a soak that runs for hours will see the occasional
// handshake flake, and a terminal total would retire the canary on noise
// instead of on a real regression. A success pushes the window forward, so a
// recovered stand re-enters service without a process restart.
//
// MaxRequests is an absolute sample cap (0 or negative = unlimited, which is
// what a long soak uses).
type canaryState struct {
	tried  int
	off    bool
	offAt  time.Time
	window []bool // true = served, false = failed; at most conf.Window entries
}

// canaryVerdict classifies one canary attempt:
//
//	2xx            -> ok
//	429, 5xx, dial/timeout -> failed (the stand's own health)
//	other 4xx      -> neutral: the REQUEST was bad (health probes without a
//	                  model, malformed bodies), not the stand. It still falls
//	                  through to live, but it must not count against the stand,
//	                  or a GUI probe every minute would retire a healthy canary.
func canaryVerdict(status int) (ok, failed bool) {
	switch {
	case status == 0: // dial/timeout: the stand never answered
		return false, true
	case status >= 200 && status < 300:
		return true, false
	case status == http.StatusTooManyRequests || status >= 500:
		return false, true
	default:
		return false, false
	}
}

// canaryTry sends the (already shaped) request body to the provider's canary
// endpoint and returns its response on 2xx. Anything else — dial error,
// timeout, 429, 5xx — records a failure and reports false so the caller
// proceeds with the live path. The body is transport-agnostic: the canary
// endpoint is a full ai-router instance and re-shapes idempotently.
func (r *Router) canaryTry(ctx context.Context, p *provider.Provider, body []byte, model string, _ bool) (*Result, bool) {
	c := p.Canary
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return nil, false
	}
	maxReq := c.MaxRequests // <= 0: unlimited (long soak)
	maxFail := c.MaxFailures
	if maxFail <= 0 {
		maxFail = 3
	}
	winSize := c.Window
	if winSize <= 0 {
		winSize = 20
	}

	r.canaryMu.Lock()
	st := r.canaryStat[p.Name]
	if st == nil {
		st = &canaryState{}
		r.canaryStat[p.Name] = st
	}
	// A retired canary re-arms after CooldownS (0 = terminal for the process):
	// a burst of failures pauses the sample, it does not end the rollout, and a
	// long soak keeps testing a pool that is still burning in.
	if st.off && c.CooldownS > 0 && time.Since(st.offAt) >= time.Duration(c.CooldownS)*time.Second {
		st.off = false
		st.window = nil
		slog.Info("canary re-armed after cooldown", "provider", p.Name, "cooldown_s", c.CooldownS)
	}
	if st.off || (maxReq > 0 && st.tried >= maxReq) {
		st.off = true
		r.canaryMu.Unlock()
		return nil, false
	}
	st.tried++
	r.canaryMu.Unlock()

	record := func(ok, failed bool) (disabled bool, windowFail, windowN, tried int) {
		if !ok && !failed {
			return false, 0, 0, 0 // neutral: no sample
		}
		r.canaryMu.Lock()
		st.window = append(st.window, ok)
		if len(st.window) > winSize {
			st.window = st.window[len(st.window)-winSize:]
		}
		fails := 0
		for _, v := range st.window {
			if !v {
				fails++
			}
		}
		windowN = len(st.window)
		windowFail = fails
		tried = st.tried
		if fails >= maxFail {
			st.off = true
			st.offAt = time.Now()
		}
		disabled = st.off
		r.canaryMu.Unlock()
		return
	}
	fail := func(reason string, status int) (*Result, bool) {
		ok, failed := canaryVerdict(status)
		disabled, windowFail, windowN, tried := record(ok, failed)
		slog.Warn("canary failed - serving live", "provider", p.Name, "status", status,
			"tried", tried, "window_fail", windowFail, "window", windowN,
			"disabled", disabled, "reason", reason)
		return nil, false
	}

	timeout := time.Duration(c.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	// Pin the model to THIS provider: the stand is a full router, so a bare id
	// could resolve to another provider with different (or broken) wiring
	// (measured 2026-11-19: a bare deepseek id resolved to `fireworks`, which
	// has no transport, and the stand answered 502 while the live router served
	// the same request from opencode). The pin keeps the sample comparable.
	pinnedBody := withModel(body, p.Name+"/"+model)
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(pinnedBody))
	if err != nil {
		cancel()
		return fail(err.Error(), 0)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	client := r.canaryHTTP
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		// A caller that hung up is not a stand failure: the request never
		// reached a verdict about the test pool.
		if ctx.Err() != nil {
			slog.Debug("canary canceled by caller - serving live", "provider", p.Name, "reason", err.Error())
			return nil, false
		}
		return fail(err.Error(), 0)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		cancel()
		// A neutral verdict (bad request) must not move the window.
		if ok, failed := canaryVerdict(resp.StatusCode); !ok && !failed {
			slog.Debug("canary rejected request - serving live", "provider", p.Name, "status", resp.StatusCode,
				"reason", strings.TrimSpace(string(raw)))
			return nil, false
		}
		return fail(strings.TrimSpace(string(raw)), resp.StatusCode)
	}
	// Keep the context alive until the caller consumes the body: the gateway
	// streams it (stream.Passthrough) or buffers it for non-stream clients.
	resp.Body = &canaryBody{rc: resp.Body, cancel: cancel}
	ok, failed := canaryVerdict(resp.StatusCode)
	_, windowFail, windowN, tried := record(ok, failed)
	slog.Info("canary served", "provider", p.Name, "status", resp.StatusCode,
		"tried", tried, "window_fail", windowFail, "window", windowN)
	return &Result{
		Provider: p.Name + "~canary",
		Status:   resp.StatusCode,
		Header:   resp.Header,
		Resp:     resp,
		Model:    model,
	}, true
}

// canaryBody cancels the request context when the response body is closed,
// which is exactly when the caller is done streaming.
type canaryBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *canaryBody) Read(p []byte) (int, error) { return b.rc.Read(p) }

func (b *canaryBody) Close() error {
	err := b.rc.Close()
	b.once.Do(b.cancel)
	return err
}
