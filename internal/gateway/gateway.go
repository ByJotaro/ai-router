// Package gateway implements the OpenAI-compatible HTTP server:
// POST /v1/chat/completions (streaming + non-streaming, tools passthrough),
// POST /v1/responses + POST /responses (OpenAI Responses API over the same
// router: responsesInToChat converts to chat, the result maps back to a
// Response object / Responses SSE), GET /v1/models, OpenAI-format errors.
package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ByJotaro/ai-router/internal/crypto"
	"github.com/ByJotaro/ai-router/internal/events"
	"github.com/ByJotaro/ai-router/internal/keys"
	"github.com/ByJotaro/ai-router/internal/lockout"
	"github.com/ByJotaro/ai-router/internal/logring"
	"github.com/ByJotaro/ai-router/internal/route"
	"github.com/ByJotaro/ai-router/internal/store"
	"github.com/ByJotaro/ai-router/internal/stream"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// EventFunc receives request events (used by hubmod to emit ai.request).
type EventFunc func(typ string, payload any)

// Server is the OpenAI-compatible gateway.
type Server struct {
	router     *route.Router
	keys       *keys.Store
	adminToken string
	ring       *logring.Ring
	onRequest  EventFunc
	db         *sql.DB
	store      *store.Store
	telemetry  bool
	idemOn     bool
	idemTTL    time.Duration
	sanitize   bool
	// recQ decouples request accounting (hub event + SQLite) from the
	// response path: record() can take seconds (hub POST up to 5s, SQLite
	// serialized) and must never delay or hold a finished response.
	recQ chan recItem
}

// recItem pairs a request event with the gateway path that produced it
// (/v1/chat/completions vs /v1/responses) for request_log accounting.
type recItem struct {
	ev   events.RequestEvent
	path string
}

// enqueueRecord hands a request event to the background recorder. When the
// queue is full the event is dropped — accounting is best-effort, responses
// are not.
func (s *Server) enqueueRecord(ev events.RequestEvent, path string) {
	if s.recQ == nil {
		s.record(ev, path)
		return
	}
	select {
	case s.recQ <- recItem{ev: ev, path: path}:
	default:
		if s.ring != nil {
			s.ring.Add("warn", "openai request accounting queue full, dropping event")
		}
	}
}

func (s *Server) recWorker() {
	for it := range s.recQ {
		s.record(it.ev, it.path)
	}
}

// Option mutates Server configuration.
type Option func(*Server)

// WithTelemetry enables X-AiRouter-* response headers.
func WithTelemetry(on bool) Option { return func(s *Server) { s.telemetry = on } }

// WithIdempotency enables request dedup by X-Request-ID/body hash (non-stream).
func WithIdempotency(on bool, ttl time.Duration, db *sql.DB) Option {
	return func(s *Server) { s.idemOn, s.idemTTL, s.db = on, ttl, db }
}

// WithStore enables request_log/daily_usage persistence for chat requests.
func WithStore(st *store.Store) Option { return func(s *Server) { s.store = st } }

// WithSanitizeErrors masks upstream internals in error responses.
func WithSanitizeErrors(on bool) Option { return func(s *Server) { s.sanitize = on } }

// New creates the gateway. adminToken authenticates with full access.
func New(r *route.Router, ks *keys.Store, adminToken string, ring *logring.Ring, opts ...Option) *Server {
	s := &Server{
		router:     r,
		keys:       ks,
		adminToken: adminToken,
		ring:       ring,
		recQ:       make(chan recItem, 256),
	}
	for _, f := range opts {
		f(s)
	}
	go s.recWorker()
	return s
}

// SetOnRequest registers a callback for completed chat requests.
func (s *Server) SetOnRequest(fn EventFunc) { s.onRequest = fn }

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	mux.HandleFunc("POST /responses", s.handleResponses)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /admin/pool", s.handleAdminPool)
	mux.HandleFunc("POST /admin/pool/rotate", s.handleAdminPoolRotate)
	return gatewayTimeoutHandler(s.log(mux), 180*time.Second, `{"error":{"message":"gateway timeout: no upstream data for 180s","type":"gateway_timeout","code":"gateway_timeout"}}`)
}

// gatewayIdleCap is the SECOND cap of a gateway request: even a stream that
// keeps producing data must not live forever (a runaway generation).
const gatewayIdleCap = 30 * time.Minute

// gatewayWatchdog cancels a request after dt WITHOUT DATA progress. A plain
// wall-clock cap cut healthy streams: live 2026-10-22 17:03 a 3000-token ask
// answered nothing for 180s (thinking) and a mid-stream long answer was killed
// at 180s although tokens were still arriving. Keepalives do not count as data:
// `: keepalive` only proves the proxy is alive, not the upstream.
type gatewayWatchdog struct {
	mu    sync.Mutex
	t     *time.Timer
	dt    time.Duration
	dead  time.Time
	timer bool
}

func newGatewayWatchdog(dt time.Duration, cancel context.CancelFunc) *gatewayWatchdog {
	g := &gatewayWatchdog{dt: dt, dead: time.Now().Add(gatewayIdleCap)}
	g.t = time.AfterFunc(dt, cancel)
	g.timer = true
	return g
}

// progress restarts the idle timer (a real data write happened).
func (g *gatewayWatchdog) progress() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.timer {
		return
	}
	if time.Now().After(g.dead) {
		return // the absolute cap is enforced by the timer that is running
	}
	g.t.Reset(g.dt)
}

func (g *gatewayWatchdog) stop() {
	g.mu.Lock()
	g.timer = false
	g.mu.Unlock()
	g.t.Stop()
}

func gatewayTimeoutHandler(h http.Handler, dt time.Duration, msg string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		r = r.WithContext(ctx)
		wd := newGatewayWatchdog(dt, cancel)
		defer wd.stop()
		done := make(chan struct{})
		tw := &gatewayTimeoutWriter{w: w, h: make(http.Header), req: r, wd: wd}
		panicChan := make(chan any, 1)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					panicChan <- p
				}
			}()
			h.ServeHTTP(tw, r)
			close(done)
		}()
		select {
		case p := <-panicChan:
			panic(p)
		case <-done:
			tw.mu.Lock()
			defer tw.mu.Unlock()
			if !tw.sentHeader {
				dst := w.Header()
				for k, vv := range tw.h {
					dst[k] = vv
				}
				if !tw.wroteHeader {
					tw.code = http.StatusOK
				}
				w.WriteHeader(tw.code)
				tw.sentHeader = true
			}
			if tw.wbuf.Len() > 0 {
				_, _ = w.Write(tw.wbuf.Bytes())
				tw.wbuf.Reset()
			}
		case <-ctx.Done():
			tw.mu.Lock()
			defer tw.mu.Unlock()
			// SSE already open: headers (200 + event-stream) are on the
			// wire — a second WriteHeader is ignored by net/http and raw
			// JSON would corrupt the stream. Close with [DONE] instead.
			if tw.sentHeader {
				tw.wbuf.Reset()
				tw.err = http.ErrHandlerTimeout
				_, _ = io.WriteString(tw.w, "data: {\"error\":{\"message\":\"gateway timeout: upstream stalled after stream start\",\"type\":\"gateway_timeout\"}}\n\n")
				_, _ = io.WriteString(tw.w, "data: [DONE]\n\n")
				if f, ok := tw.w.(http.Flusher); ok {
					f.Flush()
				}
				break
			}
			// Pre-stream timeout: headers not sent yet — honest JSON 502.
			// r.Context() is the CLIENT's context: alive means WE cancelled
			// (idle watchdog), dead means the client went away.
			if r.Context().Err() == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, msg)
				tw.err = http.ErrHandlerTimeout
			} else {
				w.WriteHeader(http.StatusBadGateway)
				tw.err = ctx.Err()
			}
		}
	})
}

type gatewayTimeoutWriter struct {
	w           http.ResponseWriter
	h           http.Header
	wbuf        bytes.Buffer
	req         *http.Request
	wd          *gatewayWatchdog
	mu          sync.Mutex
	err         error
	wroteHeader bool
	code        int
	sentHeader  bool
}

func (tw *gatewayTimeoutWriter) Header() http.Header { return tw.h }

// isKeepaliveWrite reports whether an SSE write is a comment-only keepalive
// ("`: keepalive`", stream.go): it proves the hop is alive, NOT that the
// upstream is producing, so it must not restart the idle watchdog.
func isKeepaliveWrite(p []byte) bool {
	t := bytes.TrimSpace(p)
	return len(t) == 0 || t[0] == ':'
}

func (tw *gatewayTimeoutWriter) Write(p []byte) (int, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.err != nil {
		return 0, tw.err
	}
	if tw.wd != nil && !isKeepaliveWrite(p) {
		tw.wd.progress()
	}
	if !tw.wroteHeader {
		tw.writeHeaderLocked(http.StatusOK)
	}
	return tw.wbuf.Write(p)
}
func (tw *gatewayTimeoutWriter) writeHeaderLocked(code int) {
	if tw.err != nil {
		return
	}
	if tw.wroteHeader {
		if tw.req != nil {
			slog.Warn("gateway: superfluous WriteHeader", "code", code)
		}
		return
	}
	tw.wroteHeader = true
	tw.code = code
}
func (tw *gatewayTimeoutWriter) WriteHeader(code int) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	tw.writeHeaderLocked(code)
}

// Flush forwards buffered SSE chunks to the real client immediately.
// Without this, stream=true responses are buffered in wbuf until the
// upstream closes — OMP sees everything as one chunk at the end.
func (tw *gatewayTimeoutWriter) Flush() {
	tw.mu.Lock()
	if tw.err != nil {
		tw.mu.Unlock()
		return
	}
	if !tw.wroteHeader {
		tw.writeHeaderLocked(http.StatusOK)
	}
	var hdr http.Header
	var code int
	var sendHeader bool
	if !tw.sentHeader {
		hdr = tw.h.Clone()
		code = tw.code
		sendHeader = true
		tw.sentHeader = true
	}
	var chunk []byte
	if tw.wbuf.Len() > 0 {
		chunk = append([]byte(nil), tw.wbuf.Bytes()...)
		tw.wbuf.Reset()
	}
	tw.mu.Unlock()
	if sendHeader {
		dst := tw.w.Header()
		for k, vv := range hdr {
			dst[k] = vv
		}
		tw.w.WriteHeader(code)
	}
	if len(chunk) > 0 {
		_, _ = tw.w.Write(chunk)
	}
	if f, ok := tw.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) log(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if s.ring != nil {
			s.ring.Add("info", fmt.Sprintf("openai %s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond)))
		}
	})
}

// handleModels lists all models advertised by providers.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	admin, tok, aerr := s.authenticate(r)
	if aerr != nil {
		writeErr(w, aerr.Status, aerr.Code, aerr.Type, aerr.Message)
		return
	}
	// Unknown/revoked Bearer must not enumerate the catalog: chat calls
	// Authorize per model, models has no model — verify validity only.
	if !admin {
		if _, err := s.keys.Authorize(tok, ""); err != nil {
			if errors.Is(err, keys.ErrUnknownKey) || errors.Is(err, keys.ErrRevoked) {
				status, code, typ, msg := keyError(err)
				writeErr(w, status, code, typ, msg)
				return
			}
		}
	}
	models := s.router.Models()
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{
			"id":       m,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "ai-router",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": out})
}

// handleChat is the chat completions endpoint.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	// Auth FIRST: reject unknown callers before 64MB reads, reasoning
	// rewrites and log spam (unauthenticated DoS surface).
	admin0, _, aerr0 := s.authenticate(r)
	if aerr0 != nil {
		writeErr(w, aerr0.Status, aerr0.Code, aerr0.Type, aerr0.Message)
		return
	}
	_ = admin0
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	// Explicit 413 on truncation: LimitReader cuts silently (nil error) and
	// a >64MB body would mislead as 400 bad JSON. Text-only 500k (~2-5MB)
	// never hits this; only oversized image payloads do. No Content-Length
	// fast-path: returning before consuming the body resets the TCP stream
	// and the client sees ConnectionAborted instead of the 413 JSON.
	if len(body) == 64<<20 {
		writeErr(w, http.StatusRequestEntityTooLarge, "payload_too_large", "invalid_request_error", "request body exceeds 64MB cap (text-only 500k fits ~2-5MB; images excluded)")
		return
	}

	// Request-shape diagnostics: keep a fingerprint of every call and, when
	// AI_ROUTER_DUMP_DIR is set, the body head of failures (reqdump.go).
	dw := &dumpWriter{ResponseWriter: w}
	w = dw
	reqStart := time.Now()
	defer finishRequest(r, body, dw, reqStart)

	// Reasoning priority: client-explicit > GUI per-model override >
	// family auto-max. The GUI override (ProviderEffort) is injected here,
	// BEFORE ensureReasoningMax, so the auto-max never buries it; both
	// injectors passthrough when the client set any reasoning param.
	// The body forwarded to the router has to be the one actually sent,
	// so idemHash and logs are consistent.
	var hintModel struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &hintModel)
	if s.router != nil {
		if eff := s.router.ProviderEffort(hintModel.Model); eff != "" && eff != "auto" {
			body = route.WithReasoning(body, eff)
		} else {
			body = ensureReasoningMax(body)
		}
	} else {
		body = ensureReasoningMax(body)
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", "request body is not valid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", "field 'model' is required")
		return
	}

	slog.Default().Info("req", "remote", r.RemoteAddr, "model", req.Model, "stream", req.Stream, "rid", strings.TrimSpace(r.Header.Get("X-Request-ID")))
	admin, k, aerr := s.authenticate(r)
	if aerr != nil {
		writeErr(w, aerr.Status, aerr.Code, aerr.Type, aerr.Message)
		return
	}
	// Enforce key quotas and model allowlist (admin is exempt).
	if !admin {
		if _, err := s.keys.Authorize(k, req.Model); err != nil {
			if errors.Is(err, keys.ErrQuota) && s.onRequest != nil {
				s.onRequest("ai.quota_exhausted", map[string]any{
					"key": maskKey(k), "model": req.Model,
				})
			}
			status, code, typ, msg := keyError(err)
			writeErr(w, status, code, typ, msg)
			return
		}
	}

	// Idempotency: replay a cached non-stream response for a repeated
	// X-Request-ID or an identical body within a 5s window.
	idemHash := ""
	if s.idemOn && !req.Stream && s.db != nil {
		if rid := strings.TrimSpace(r.Header.Get("X-Request-ID")); rid != "" {
			idemHash = "rid:" + rid
		} else {
			sum := sha256.Sum256(body)
			idemHash = "body:" + hex.EncodeToString(sum[:])
		}
		if status, cached, ok := s.idemGet(idemHash); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-AiRouter-Idempotent", "replay")
			w.WriteHeader(status)
			_, _ = w.Write(cached)
			return
		}
	}

	start := time.Now()
	// Forward real OpenCode session affinity (x-opencode-session et al) to
	// the zen upstream; synthesized per attempt when the client sent none.
	callCtx := route.ContextWithClientHeaders(r.Context(), r.Header)
	res, err := s.router.Call(callCtx, req.Model, body, req.Stream)
	if err != nil && isUpstream500(err) && !hasExplicitReasoning(body) {
		// Upstream 500 на глубоком думании (muse-spark free-tier,
		// 2026-09-19: 500 на xhigh/high/medium, 200 на low): первая
		// попытка шла на xhigh+32768 (gateway default). Ретрай на low —
		// единственный уровень, переживший killer-промпт. OMP видел
		// эти 500 как empty stop и сдавался после 3 попыток.
		if s.ring != nil {
			s.ring.Add("warn", fmt.Sprintf("openai upstream 500, retrying low model=%s", req.Model))
		}
		res, err = s.router.Call(callCtx, req.Model, forceLowReasoning(forcedAnswerBody(body)), req.Stream)
	}
	if err != nil {
		// Отмена клиента: соединение уже мёртво — не пишем 503 в сокет и в лог.
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		status := s.writeRouteError(w, err)
		ev := s.requestEvent(req.Model, nil, nil, k, status, start, r)
		ev.Error = crypto.SafeError(err.Error())
		var use *route.UpstreamStatusError
		if errors.As(err, &use) && use.RetryAfter != "" {
			ev.Error += " retry_after=" + use.RetryAfter
		}
		s.enqueueRecord(ev, "/v1/chat/completions")
		slog.Default().Info("resp", "status", status, "model", req.Model, "error", crypto.SanitizeForLog(err.Error()), "dur_ms", time.Since(start).Milliseconds(), "prompt_tok", len(body))
		if s.ring != nil {
			s.ring.Add("warn", "resp error: "+crypto.SanitizeForLog(err.Error()))
		}
		return
	}
	s.setTelemetry(w, k, req.Model, res.Provider, time.Since(start))
	if s.ring != nil {
		s.ring.Add("info", fmt.Sprintf("openai chat model=%s provider=%s (%s)", req.Model, res.Provider, time.Since(start).Round(time.Millisecond)))
	}

	if req.Stream {
		// A stream that produced only reasoning (think-only) is retried on
		// a fresh lease with an appended "answer now" instruction: the
		// upstream emitted no content and Passthrough buffered everything,
		// so the client saw nothing yet. Only the final attempt emits the
		// (empty) terminal chunk.
		const thinkOnlyRetries = 1
		for attempt := 0; ; attempt++ {
			usage, thinkOnly := s.handleStream(w, r, res, k, req.Model, admin, attempt >= thinkOnlyRetries, attempt == 0)
			if !thinkOnly {
				// Honest status: handleStream relays >=400 with the real
				// code — logging 200 for it corrupts error accounting.
				st := http.StatusOK
				if res.Status >= 400 {
					st = res.Status
				}
				s.enqueueRecord(s.requestEvent(req.Model, res, usage, k, st, start, r), "/v1/chat/completions")
				slog.Default().Info("resp", "status", st, "stream", true, "dur_ms", time.Since(start).Milliseconds())
				return
			}
			if s.ring != nil {
				s.ring.Add("warn", fmt.Sprintf("openai think-only stream, retrying model=%s provider=%s attempt=%d", req.Model, res.Provider, attempt+1))
			}
			res, err = s.router.Call(callCtx, req.Model, forcedAnswerBody(body), req.Stream)
			if err != nil {
				// Stream headers (200 + SSE) are already sent; a second
				// WriteHeader would corrupt the response (superfluous
				// WriteHeader warnings). The client saw a 200 with reasoning
				// preamble — log the retry failure under the SAME status so
				// request_log never shows a transport error masquerading as
				// an "ok" row (2026-09-18: "200 with pool-no-egress text"
				// was this path).
				slog.Default().Warn("think-only retry failed after stream started",
					"err", crypto.SanitizeForLog(err.Error()), "dur_ms", time.Since(start).Milliseconds())
				ev := s.requestEvent(req.Model, nil, nil, k, http.StatusServiceUnavailable, start, r)
				ev.Error = crypto.SafeError(err.Error())
				s.enqueueRecord(ev, "/v1/chat/completions")
				if s.ring != nil {
					s.ring.Add("warn", "think-only retry failed: "+crypto.SanitizeForLog(err.Error()))
				}
				return
			}
		}
	}
	// Non-stream: an upstream 2xx whose first choice carries no content and
	// no tool calls is a think-only / empty answer (muse often replies with
	// reasoning only). The client must not see a "successful" empty 200 —
	// retry once on a fresh lease with an explicit answer instruction, same
	// policy as the stream path. Final attempt relays the honest body.
	const thinkOnlyRetries = 1
	for attempt := 0; ; attempt++ {
		data, rerr := io.ReadAll(io.LimitReader(res.Resp.Body, 64<<20))
		res.Resp.Body.Close()
		final := attempt >= thinkOnlyRetries
		if rerr == nil && !final && thinkOnlyBody(data) {
			if s.ring != nil {
				s.ring.Add("warn", fmt.Sprintf("openai empty non-stream, retrying model=%s provider=%s attempt=%d", req.Model, res.Provider, attempt+1))
			}
			res, err = s.router.Call(callCtx, req.Model, forcedAnswerBody(body), req.Stream)
			if err != nil {
				// res is nil here (Call never returns a result with an error):
				// logging res.Provider panicked the handler and the client saw
				// a connection reset instead of this 502 (review 2026-10-17).
				s.writeRouteError(w, err)
				slog.Default().Info("resp", "status", `"error"`, "dur_ms", time.Since(start).Milliseconds())
				return
			}
			continue
		}
		usage := s.respondNonStream(w, res, k, req.Model, admin, idemHash, data)
		s.enqueueRecord(s.requestEvent(req.Model, res, usage, k, res.Status, start, r), "/v1/chat/completions")
		slog.Default().Info("resp", "status", res.Status, "provider", res.Provider, "stream", false, "dur_ms", time.Since(start).Milliseconds())
		return
	}
}

// forcedAnswerBody prepares the think-only retry: appends an explicit user
// instruction AND drops reasoning to low. The first pass ran at xhigh+32768
// (gateway default); retrying the same depth replays the same upstream 500
// (2026-09-19: killer-промпт 500 на xhigh/high/medium, 200 на low).
// OMP без reasoning видел эти 500 как empty stop и сдавался после 3 попыток.
// Low на ретрае — осознанный даунгрейд глубины ради ответа: лучше короткий
// ответ, чем честный 500/пустой стоп после retry cap.
func forcedAnswerBody(body []byte) []byte {
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	doc.Messages = append(doc.Messages, map[string]any{
		"role": "user",
		"content": "(internal continuation) The previous turn produced no visible answer. " +
			"Reply now with the final answer as plain text.",
	})
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return forceLowReasoning(out)
}

// isUpstream500: ошибка Call с UpstreamStatusError.Status == 500.
// hasExplicitReasoning: клиент сам задал reasoning (не трогаем —
// forceLowReasoning переписал бы чужой explicit).
func isUpstream500(err error) bool {
	var use *route.UpstreamStatusError
	if errors.As(err, &use) && use.Status == http.StatusInternalServerError {
		return true
	}
	return false
}

func hasExplicitReasoning(body []byte) bool {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return false
	}
	for _, k := range []string{"reasoning", "reasoning_effort", "reasoning_level", "thinking", "reasoning_config", "thinking_level"} {
		if _, ok := raw[k]; ok {
			return true
		}
	}
	return false
}

// forceLowReasoning переписывает reasoning-инжект на low без budget_tokens:
// ретрай после think-only/500 идёт на малой глубине (единственный уровень,
// переживший killer-промпт). Клиентский explicit не трогаем (его там нет —
// forcedAnswerBody зовётся только когда инжектил gateway).
func forceLowReasoning(body []byte) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	m["reasoning_effort"] = "low"
	// The object "reasoning" form is only valid for the reasoning-injected
	// families (mirroring ensureReasoningMax). Arbitrary upstreams reject
	// unknown object fields: OVH answers 400 "extra arguments:
	// {reasoning:{effort:low}}" while accepting flat reasoning_effort
	// (live 2026-10-25).
	if model, _ := m["model"].(string); model != "" {
		lm := strings.ToLower(model)
		if strings.Contains(lm, "muse-spark") || strings.Contains(lm, "mimo") || strings.Contains(lm, "x-preview") {
			m["reasoning"] = map[string]any{"effort": "low"}
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// ensureReasoningMax injects the model family's MAXIMUM reasoning level when
// the caller sent no explicit reasoning params. Families differ in the top
// effort label: muse/mimo accept "xhigh", while x-preview validates strictly
// and rejects it with [400] [1210] ("use low, high, or max") — so it maps to
// "max". Models outside these reasoning-heavy families pass through
// untouched: injecting unknown fields into arbitrary upstreams risks 400s.
// Explicit client values are never overridden.
func ensureReasoningMax(body []byte) []byte {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return body
	}
	for _, k := range []string{"reasoning", "reasoning_effort", "reasoning_level", "thinking", "reasoning_config", "thinking_level"} {
		if _, ok := raw[k]; ok {
			return body // explicit — never override
		}
	}
	var hint struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &hint)
	m := strings.ToLower(hint.Model)
	var effort string
	switch {
	case strings.Contains(m, "muse-spark"), strings.Contains(m, "mimo"):
		effort = "xhigh"
	case strings.Contains(m, "x-preview"):
		effort = "max"
	default:
		return body // unknown family: passthrough
	}
	raw["reasoning_effort"] = json.RawMessage(`"` + effort + `"`)
	raw["reasoning"] = json.RawMessage(`{"effort":"` + effort + `","budget_tokens":32768}`)
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

// requestEvent builds the per-request event payload. res is nil for route
// errors; usage is nil when the upstream reported no token usage; vkey is the
// caller's virtual key value ("" for admin) used to resolve the account name.
func (s *Server) requestEvent(model string, res *route.Result, usage *stream.Usage, vkey string, status int, start time.Time, r *http.Request) events.RequestEvent {
	ev := events.RequestEvent{
		Model:          model,
		RequestedModel: model,
		DurationMS:     time.Since(start).Milliseconds(),
		Status:         statusWord(status),
		HTTPStatus:     status,
		RID:            r.Header.Get("X-Request-ID"),
	}
	if s.keys != nil {
		ev.Account = s.keys.NameOf(vkey)
	}
	if res != nil {
		ev.Provider = res.Provider
		ev.ConnectionID = res.ConnectionID
		ev.ComboName = res.ComboName
		ev.ComboStepID = res.ComboStepID
		ev.ComboPath = res.ComboPath
		if p := s.router.Provider(res.Provider); p != nil {
			// Pool lease channel id wins; fall back to static provider egress.
			ev.Proxy = res.Proxy
			if ev.Proxy == "" {
				ev.Proxy = p.Egress
			}
			ev.WaitedMS = res.WaitedMS
		}
	}
	if usage != nil {
		ev.Tokens = events.Tokens{
			Prompt:     usage.PromptTokens,
			Completion: usage.CompletionTokens,
			CacheRead:  usage.CacheRead,
			CacheWrite: usage.CacheWrite,
			Reasoning:  usage.Reasoning,
			Compressed: usage.Compressed,
		}
	}
	return ev
}

// record emits the ai.request event and persists the request log and daily
// rollup. Both steps are best-effort: neither may break the response path.
// Cost is resolved via the keys store's price resolver (0 when unavailable).
func (s *Server) record(ev events.RequestEvent, path string) {
	if s.keys != nil {
		ev.CostCents = s.keys.CostCents(ev.Model, ev.Tokens.Prompt, ev.Tokens.Completion)
	}
	if s.onRequest != nil {
		s.onRequest("ai.request", ev)
	}
	if s.store == nil {
		return
	}
	_ = s.store.InsertRequestLog(ev, "POST", path)
	_ = s.store.IncrDailyUsage(time.Now().UTC().Format("2006-01-02"),
		ev.Tokens.Prompt, ev.Tokens.Completion, ev.Tokens.CacheRead, ev.Tokens.CacheWrite,
		ev.Tokens.Reasoning, ev.Tokens.Compressed, ev.CostCents)
}

func statusWord(code int) string {
	if code >= 200 && code < 400 {
		return "ok"
	}
	return "error"
}

// setTelemetry writes X-AiRouter-* headers when enabled.
func (s *Server) setTelemetry(w http.ResponseWriter, vk, model, provider string, latency time.Duration) {
	if !s.telemetry {
		return
	}
	h := w.Header()
	h.Set("X-AiRouter-Provider", provider)
	if vk != "" {
		h.Set("X-AiRouter-Key", maskKey(vk))
	}
	h.Set("X-AiRouter-Model", model)
	h.Set("X-AiRouter-Latency-Ms", fmt.Sprintf("%d", latency.Milliseconds()))
}

// idemGet looks up a cached idempotency response.
func (s *Server) idemGet(hash string) (int, []byte, bool) {
	if s.db == nil {
		return 0, nil, false
	}
	var status int
	var resp string
	var created string
	err := s.db.QueryRow(`SELECT status, response, created_at FROM idempotency WHERE hash=?`, hash).Scan(&status, &resp, &created)
	if err != nil {
		return 0, nil, false
	}
	if t, err := time.Parse(time.RFC3339, created); err == nil && time.Since(t) > s.idemTTL {
		return 0, nil, false
	}
	return status, []byte(resp), true
}

// idemPut caches a completed non-stream response.
func (s *Server) idemPut(hash string, k string, status int, body []byte) {
	if s.db == nil {
		return
	}
	now := time.Now().UTC()
	expired := now.Add(-s.idemTTL).Format(time.RFC3339)
	_, _ = s.db.Exec(`DELETE FROM idempotency WHERE created_at < ?`, expired)
	_, _ = s.db.Exec(
		`INSERT INTO idempotency(hash, key, response, status, created_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(hash) DO UPDATE SET response=excluded.response, status=excluded.status, created_at=excluded.created_at`,
		hash, k, string(body), status, now.Format(time.RFC3339))
}

func maskKey(v string) string {
	if len(v) <= 8 {
		return "****"
	}
	return v[:6] + "…" + v[len(v)-4:]
}

// thinkOnlyBody reports whether a non-stream upstream body is an empty /
// think-only answer: HTTP was 2xx, first choice carries no content and no
// tool calls (muse free-tier reliably answers with reasoning only). Such a
// body must not be served as a "successful" 200 — the caller retries it.
func thinkOnlyBody(data []byte) bool {
	var v struct {
		Choices []struct {
			Message struct {
				Content   string            `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason any `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &v) != nil || len(v.Choices) == 0 {
		return false
	}
	m := v.Choices[0].Message
	return m.Content == "" && len(m.ToolCalls) == 0
}

// respondNonStream writes a pre-read upstream body and records usage.
func (s *Server) respondNonStream(w http.ResponseWriter, res *route.Result, k string, model string, admin bool, idemHash string, data []byte) *stream.Usage {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.Status)
	if len(data) == 0 {
		_, _ = w.Write([]byte(`{}`))
		return nil
	}
	_, _ = w.Write(data)
	var usage *stream.Usage
	var u struct {
		Usage *stream.Usage `json:"usage"`
	}
	if json.Unmarshal(data, &u) == nil && u.Usage != nil {
		usage = u.Usage
	}
	if usage != nil && !admin {
		actualModel := model
		if res.Model != "" {
			actualModel = res.Model
		}
		_ = s.keys.RecordUsageByKey(k, actualModel, usage.PromptTokens, usage.CompletionTokens)
	}
	if idemHash != "" && res.Status >= 200 && res.Status < 400 {
		s.idemPut(idemHash, k, res.Status, data)
	}
	return usage
}

// handleStream relays SSE and records final-chunk usage. It returns the
// captured usage (for the request event/log) and whether the stream was
// think-only (reasoning without content — the caller retries on a fresh
// lease unless final).
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, res *route.Result, k string, model string, admin, final, writeHeaders bool) (*stream.Usage, bool) {
	defer res.Resp.Body.Close()
	if writeHeaders {
		// Honest upstream error (e.g. muse 403 RegionError): relay the
		// body as a JSON error with the real status, never a fake 200 SSE
		// stream that OpenAI clients read as an empty successful turn.
		if res.Status >= 400 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(res.Status)
			data, _ := io.ReadAll(io.LimitReader(res.Resp.Body, 64<<20))
			if len(data) > 0 {
				_, _ = w.Write(data)
			} else {
				_, _ = w.Write([]byte(`{"error":{"type":"upstream_error","message":"upstream returned no body"}}`))
			}
			return nil, false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
	}
	usage, thinkOnly := stream.Passthrough(w, res.Resp.Body, res.Header.Get("Content-Type"), true, final)
	if !admin && usage.TotalTokens > 0 {
		actualModel := model
		if res.Model != "" {
			actualModel = res.Model
		}
		_ = s.keys.RecordUsageByKey(k, actualModel, usage.PromptTokens, usage.CompletionTokens)
	}
	return &usage, thinkOnly
}

// writeRouteError converts route errors into OpenAI-format responses and
// returns the HTTP status written.
func (s *Server) writeRouteError(w http.ResponseWriter, err error) int {
	msg := err.Error()
	if s.sanitize {
		msg = crypto.SafeError(msg)
	}
	var upstream *route.UpstreamStatusError
	switch {
	case errors.Is(err, route.ErrNoCandidates):
		writeErr(w, http.StatusNotFound, "model_not_found", "invalid_request_error", msg)
		return http.StatusNotFound
	case errors.Is(err, lockout.ErrLocked):
		writeErr(w, http.StatusTooManyRequests, "model_locked", "rate_limit_error", msg)
		return http.StatusTooManyRequests
	case errors.As(err, &upstream):
		st := upstream.Status
		if upstream.RetryAfter != "" {
			w.Header().Set("Retry-After", upstream.RetryAfter)
		}
		switch st {
		case http.StatusForbidden:
			// Account-gate 403 (FreeTier/billing) keeps its own code so the
			// client sees the real reason; region 403s stay region_error.
			if strings.Contains(msg, "account gate:") {
				writeErr(w, http.StatusForbidden, "account_gate", "invalid_request_error", msg)
				return http.StatusForbidden
			}
			// 403 RegionError — honest passthrough, not 502.
			writeErr(w, http.StatusForbidden, "region_error", "invalid_request_error", msg)
			return http.StatusForbidden
		case http.StatusTooManyRequests:
			writeErr(w, http.StatusTooManyRequests, "rate_limit", "rate_limit_error", msg)
			return http.StatusTooManyRequests
		case http.StatusRequestTimeout, 524:
			// 408 / 524 (Cloudflare timeout) → 504 Gateway Timeout.
			writeErr(w, http.StatusGatewayTimeout, "gateway_timeout", "timeout_error", msg)
			return http.StatusGatewayTimeout
		default:
			if st >= 500 && st < 600 {
				writeErr(w, st, "upstream_error", "server_error", msg)
				return st
			}
			if st >= 400 && st < 500 {
				// Honest passthrough for other 4xx (401,404,etc) that leaked
				// as terminal UpstreamStatusError.
				code := "upstream_error"
				typ := "invalid_request_error"
				if st == http.StatusUnauthorized {
					code = "unauthorized"
				} else if st == http.StatusNotFound {
					code = "model_not_found"
				}
				writeErr(w, st, code, typ, msg)
				return st
			}
			writeErr(w, http.StatusBadGateway, "upstream_error", "upstream_error", msg)
			return http.StatusBadGateway
		}
	default:
		writeErr(w, http.StatusBadGateway, "upstream_error", "upstream_error", msg)
		return http.StatusBadGateway
	}
}

// authenticate returns (isAdmin, virtualKey, err). The admin token grants
// full access; otherwise the bearer must be a valid virtual key.
func (s *Server) authenticate(r *http.Request) (bool, string, *apiError) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false, "", &apiError{http.StatusUnauthorized, "unauthorized", "invalid_request_error", "missing bearer token"}
	}
	tok := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if tok == "" {
		return false, "", &apiError{http.StatusUnauthorized, "unauthorized", "invalid_request_error", "missing bearer token"}
	}
	// Constant-time admin compare: Bearer tokens are secrets, plain ==
	// leaks prefix timing (low severity, local network — still free).
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminToken)) == 1 && s.adminToken != "" {
		return true, "", nil
	}
	return false, tok, nil
}

type apiError struct {
	Status  int
	Code    string
	Type    string
	Message string
}

func keyError(err error) (status int, code, typ, msg string) {
	switch {
	case errors.Is(err, keys.ErrUnknownKey):
		return http.StatusUnauthorized, "unauthorized", "invalid_request_error", "invalid API key"
	case errors.Is(err, keys.ErrRevoked):
		return http.StatusUnauthorized, "unauthorized", "invalid_request_error", "API key revoked"
	case errors.Is(err, keys.ErrModel):
		return http.StatusForbidden, "forbidden", "invalid_request_error", err.Error()
	case errors.Is(err, keys.ErrQuota):
		return http.StatusTooManyRequests, "rate_limit", "rate_limit_error", "quota exhausted for key"
	default:
		return http.StatusInternalServerError, "internal_error", "server_error", crypto.SafeError(err.Error())
	}
}

func writeErr(w http.ResponseWriter, status int, code, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
			"code":    code,
		},
	})
}
