package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Request-shape diagnostics.
//
// A 429 from the free lane never says WHICH part of the request the gate
// objected to, so every client-visible failure keeps a compact fingerprint of
// the inbound request (model, stream flag, message/tool counts, size, session
// headers, UA) and — when a dump directory is configured — the body head.
// That is the evidence needed to tell a burned egress apart from a client
// shape the gate refuses.

const (
	dumpDirEnvVar    = "AI_ROUTER_DUMP_DIR"
	dumpAllEnvVar    = "AI_ROUTER_DUMP_ALL"
	dumpBodiesEnvVar = "AI_ROUTER_DUMP_BODIES"
	dumpRingCap      = 200
	dumpBodyHead     = 2048
	dumpErrHead      = 512
	dumpBodyMax      = 16 << 20
)

// reqRecord is one observed gateway request. JSON tags double as the JSONL
// dump schema.
type reqRecord struct {
	TS       string `json:"ts"`
	Path     string `json:"path"`
	Status   int    `json:"status"`
	DurMS    int64  `json:"dur_ms"`
	Remote   string `json:"remote,omitempty"`
	UA       string `json:"ua,omitempty"`
	Model    string `json:"model,omitempty"`
	Stream   bool   `json:"stream"`
	Messages int    `json:"messages"`
	Tools    int    `json:"tools"`
	// MaxTokens is what the CLIENT asked the model to emit. It is the other
	// half of a finish=length verdict: without it a cut answer cannot be told
	// from an upstream cap (2026-11-20: "text is truncated" reached the ring
	// as a 1.4 MB body whose first 2 KB carry no max_tokens).
	MaxTokens  int               `json:"max_tokens,omitempty"`
	BodyLen    int               `json:"body_len"`
	BodySHA    string            `json:"body_sha"`
	SessionHdr string            `json:"session_hdr,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	BodyHead   string            `json:"body_head,omitempty"`
	ErrHead    string            `json:"err_head,omitempty"`
}

var dumpRing struct {
	mu    sync.Mutex
	items []reqRecord
}

// ringAdd keeps the newest dumpRingCap records.
func ringAdd(rec reqRecord) {
	dumpRing.mu.Lock()
	if len(dumpRing.items) >= dumpRingCap {
		copy(dumpRing.items, dumpRing.items[1:])
		dumpRing.items[len(dumpRing.items)-1] = rec
	} else {
		dumpRing.items = append(dumpRing.items, rec)
	}
	dumpRing.mu.Unlock()
}

// RecentRequests returns up to n newest records (oldest first).
func RecentRequests(n int) []reqRecord {
	dumpRing.mu.Lock()
	defer dumpRing.mu.Unlock()
	if n <= 0 || n > len(dumpRing.items) {
		n = len(dumpRing.items)
	}
	out := make([]reqRecord, n)
	copy(out, dumpRing.items[len(dumpRing.items)-n:])
	return out
}

// dumpConfig reports the configured dump directory (empty = no files) and
// whether every request is dumped (not just failures).
func dumpConfig() (string, bool) {
	dir := strings.TrimSpace(os.Getenv(dumpDirEnvVar))
	all := strings.TrimSpace(os.Getenv(dumpAllEnvVar))
	return dir, all == "1" || strings.EqualFold(all, "true")
}

// dumpWriter captures the response status and the head of the error body so
// the record explains why the request failed.
type dumpWriter struct {
	http.ResponseWriter
	status int
	prefix []byte
}

func (d *dumpWriter) WriteHeader(code int) {
	if d.status == 0 {
		d.status = code
	}
	d.ResponseWriter.WriteHeader(code)
}

func (d *dumpWriter) Write(b []byte) (int, error) {
	if d.status == 0 {
		d.status = http.StatusOK
	}
	if len(d.prefix) < dumpErrHead {
		room := dumpErrHead - len(d.prefix)
		if room > len(b) {
			room = len(b)
		}
		d.prefix = append(d.prefix, b[:room]...)
	}
	return d.ResponseWriter.Write(b)
}

func (d *dumpWriter) Flush() {
	if f, ok := d.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer (deadlines, flush).
func (d *dumpWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }

// fingerprintBody extracts the routing-relevant facts from a chat/responses
// body. Unknown shapes leave the zero values.
func fingerprintBody(body []byte) (model string, stream bool, messages, tools, maxTokens int) {
	var probe struct {
		Model               string            `json:"model"`
		Stream              bool              `json:"stream"`
		Messages            []json.RawMessage `json:"messages"`
		Input               []json.RawMessage `json:"input"`
		Tools               []json.RawMessage `json:"tools"`
		MaxTokens           int               `json:"max_tokens"`
		MaxCompletionTokens int               `json:"max_completion_tokens"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return "", false, 0, 0, 0
	}
	messages = len(probe.Messages)
	if messages == 0 {
		messages = len(probe.Input)
	}
	maxTokens = probe.MaxCompletionTokens
	if maxTokens == 0 {
		maxTokens = probe.MaxTokens
	}
	return probe.Model, probe.Stream, messages, len(probe.Tools), maxTokens
}

// finishRequest records a completed gateway call. It always feeds the
// in-memory ring; files are written only when a dump directory is configured
// and the call failed (or dump-all is on).
func finishRequest(r *http.Request, body []byte, dw *dumpWriter, start time.Time) {
	if r == nil {
		return
	}
	status := 0
	var errHead string
	if dw != nil {
		status = dw.status
		errHead = strings.TrimSpace(string(dw.prefix))
	}
	model, stream, msgs, ntools, maxTokens := fingerprintBody(body)
	sum := sha256.Sum256(body)
	rec := reqRecord{
		TS:        start.UTC().Format(time.RFC3339Nano),
		Path:      r.URL.Path,
		Status:    status,
		DurMS:     time.Since(start).Milliseconds(),
		Remote:    r.RemoteAddr,
		UA:        r.Header.Get("User-Agent"),
		Model:     model,
		Stream:    stream,
		Messages:  msgs,
		Tools:     ntools,
		MaxTokens: maxTokens,
		BodyLen:   len(body),
		BodySHA:   hex.EncodeToString(sum[:8]),
	}
	for _, k := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-id",
		"x-session-affinity", "x-opencode-project", "x-opencode-client", "x-opencode-request"} {
		if v := strings.TrimSpace(r.Header.Get(k)); v != "" {
			if rec.Headers == nil {
				rec.Headers = map[string]string{}
			}
			rec.Headers[k] = v
			if k == "x-opencode-session" || rec.SessionHdr == "" {
				rec.SessionHdr = v
			}
		}
	}
	if status != 0 && (status < 200 || status >= 300) {
		rec.ErrHead = truncate(errHead, dumpErrHead)
	}
	ringAdd(rec)
	// Full inbound bodies: the ring keeps only counts, so a client report like
	// "the tools are cut" (2026-11-20: OMP saw 4 tools instead of 14) cannot be
	// checked against what the client actually sent. One file per request, named
	// ts-bodyhash, so a turn can be replayed or diffed against the body the
	// router forwarded (AI_ROUTER_CLI_DUMP).
	if dir := strings.TrimSpace(os.Getenv(dumpBodiesEnvVar)); dir != "" && len(body) > 0 && len(body) <= dumpBodyMax {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			name := strings.ReplaceAll(rec.TS, ":", "-") + "-" + rec.BodySHA + ".json"
			_ = os.WriteFile(filepath.Join(dir, name), body, 0o644)
		}
	}
	dir, all := dumpConfig()
	if dir == "" || (status >= 200 && status < 300 && !all) {
		return
	}
	rec.BodyHead = truncate(string(body), dumpBodyHead)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "requests.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	enc, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%s\n", enc)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
