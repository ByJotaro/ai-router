package route

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// Inbound OpenCode headers forwarded from the gateway request to the opencode
// zen upstream. zen 2026-09 rejects /responses without a session as
// 400 MissingSessionID; since 2026-09-17 free-tier additionally requires a
// SERVER-KNOWN session ("free tier can only be used from within OpenCode"):
// TUI-minted ses_* from the local opencode.db pass on any egress/UA while
// generated/random ids fail everywhere (MITM isolation matrix). Missing
// sessions therefore come from the harvest pool first (see sessionpool.go),
// generated fallback last.
type ctxKeyClientHeaders struct{}

// opencodeHeaderAllow is the exact whitelist forwarded from the inbound
// request. Nothing else from the client reaches upstream: doRequest builds a
// fresh request and only UA/Authorization are re-set there.
var opencodeHeaderAllow = []string{
	"x-opencode-session",
	"x-opencode-session-id",
	"x-session-affinity",
	"x-session-id",
	"x-opencode-client",
	"x-opencode-project",
	"x-opencode-request",
}

// sessionHeaderAliases are the header names the CLI uses for the same session
// id depending on the provider kind: the native opencode provider sends
// x-opencode-session/-session-id, while a custom openai-compatible provider
// sends x-session-affinity / x-session-id / x-opencode-session-id only. Without
// this mapping the gateway dropped the real affinity and fell back to a
// harvested (often quota-closed) session, surfacing upstream 429s.
var sessionHeaderAliases = []string{
	"x-opencode-session",
	"x-session-affinity",
	"x-opencode-session-id",
	"x-session-id",
}

func clientSession(h http.Header) string {
	for _, k := range sessionHeaderAliases {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

// ContextWithClientHeaders captures the whitelisted inbound header into ctx
// so the router can forward real session affinity (gateway calls this before
// Call). A missing value is synthesized per upstream attempt in
// setOpencodeHeaders, so keyless harnesses (omp, curl) that send no session
// header still pass the MissingSessionID gate.
func ContextWithClientHeaders(ctx context.Context, h http.Header) context.Context {
	if len(h) == 0 {
		return ctx
	}
	kept := http.Header{}
	for _, k := range opencodeHeaderAllow {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			kept.Set(k, v)
		}
	}
	if kept.Get("x-opencode-session") == "" {
		if ses := clientSession(kept); ses != "" {
			kept.Set("x-opencode-session", ses)
		}
	}
	if len(kept) == 0 {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyClientHeaders{}, kept)
}

// clientHeader returns the inbound-whitelisted value carried in ctx, if any.
func clientHeader(ctx context.Context, name string) string {
	if ctx == nil {
		return ""
	}
	if kept, ok := ctx.Value(ctxKeyClientHeaders{}).(http.Header); ok && kept != nil {
		return strings.TrimSpace(kept.Get(name))
	}
	return ""
}

// isOpencodeUpstream reports whether p talks to the opencode zen Console
// backend (the only one gating on x-opencode-* session headers).
func isOpencodeUpstream(p *provider.Provider) bool {
	if p == nil {
		return false
	}
	if strings.HasPrefix(p.Name, "opencode") {
		return true
	}
	// Matches both the current lane (inference/v1, verified 2026-10-14 by
	// MITM: native TUI POSTs {base}/responses there and gets 200) and the
	// legacy zen/v1 lane.
	return strings.Contains(p.BaseURL, "opencode.ai/")
}

// randID returns n random chars from [0-9a-zA-Z], matching the real client
// id alphabet (ses_f8518614fffebuELtjHJdDG6X3, msg_07ae79ef2001TxdS46fhvjmi2V).
func randID(n int) string {
	const abc = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	for i := range b {
		b[i] = abc[int(b[i])%len(abc)]
	}
	return string(b)
}

// fallbackSession is the process-stable anonymous session stamped when the
// inbound client sent none. Stable — NOT per-request random — so anonymous
// harnesses (omp, curl) share upstream prompt-cache affinity across turns:
// zen keys cache on (session, content), and per-request randomness made
// every turn pay full prefill (60-200k tokens), fueling the 2026-09-09 429
// storm. Real inbound sessions still win verbatim (affinity preserved).
// Per-egress fallback identities. The harvest pool should cover every live
// egress with its own genuine session, but when it runs dry the single
// process-wide fallback made all egresses share one id — exactly the
// cross-egress correlation the upstream bans on ("bans by shared factors,
// not only by IP"). fallbackSessionFor derives a stable id per egress key, so
// each IP keeps its own client identity and prompt-cache affinity.
var (
	fallbackMu    sync.Mutex
	fallbackByKey = map[string]string{}
	// fallbackAtByKey stamps when each egress's fallback session was minted;
	// a fallback id is re-minted after fallbackTTL so one egress never ships
	// a many-hours-old session (a single conversation spanning a whole day
	// is a fabrication signal).
	fallbackAtByKey = map[string]time.Time{}
	fallbackTTL     = 45 * time.Minute
	// fallbackSeed is PER-EGRESS: a process-wide seed (sync.Once) meant all
	// fallback sessions shared a common 8-hex-char pattern in the middle of
	// the id, which is a cross-IP link signal. Each egress now gets its own
	// seed, so the derived hex8 is uncorrelated across IPs.
	fallbackSeedByKey = map[string]string{}
)

func fallbackSessionFor(key string) string {
	if key == "" {
		// No key = no known egress. Returning a process-wide id here is the
		// cross-egress link in waiting: every caller without an egress would
		// ship the same session. "" makes the caller omit the header instead
		// (honest 400) rather than look like one client on many IPs.
		return ""
	}
	fallbackMu.Lock()
	defer fallbackMu.Unlock()
	if id, ok := fallbackByKey[key]; ok {
		if at, ok := fallbackAtByKey[key]; ok && time.Since(at) < fallbackTTL {
			return id
		}
	}
	if _, ok := fallbackSeedByKey[key]; !ok {
		fallbackSeedByKey[key] = randID(8)
	}
	// TUI 1.19.4 shape (MITM 2026-10-24): ses_ + 12 hex + 14 alnum = 30
	// chars. The free gate validates the FORMAT, not server knowledge — a
	// synthetic 30-char id passes wherever the old 22-char generated one
	// (and pre-1.19 28-char harvested ids) drew the gate-403.
	//
	// The 12-hex block is NOT filler: per identifier.ts it is the low 48
	// bits of ~(unixMilli*4096+counter) (descending). Genuine ids observed
	// 2026-10-25 share the inverted-timestamp prefix (ses_e8cb91d82ffe…,
	// ses_e8cb51e6fffe…); a hash there decodes to an impossible creation
	// time, which is a one-line server-side fabrication check.
	id := "ses_" + descendingHex12() + randID(14)
	fallbackByKey[key] = id
	fallbackAtByKey[key] = time.Now()
	return id
}

// descStamp serializes (millisecond, counter) exactly like identifier.ts:
// calls within the same millisecond increment a counter living in the low
// 12 bits of the stamped value.
var descStamp struct {
	mu      sync.Mutex
	lastMs  int64
	counter int64
}

// descendingHex12 mirrors identifier.ts descending(): hex of the low 48
// bits of the bitwise NOT of (unixMilli*4096 + counter).
func descendingHex12() string {
	descStamp.mu.Lock()
	defer descStamp.mu.Unlock()
	now := time.Now().UnixMilli()
	if now != descStamp.lastMs {
		descStamp.lastMs = now
		descStamp.counter = 0
	}
	descStamp.counter++
	if descStamp.counter > 16 {
		descStamp.counter = 1
	}
	v := ^(now*4096 + descStamp.counter)
	return fmt.Sprintf("%012x", uint64(v)&0xFFFFFFFFFFFF)
}

func fnv32(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * 16777619
	}
	return h
}

func hex8(v uint32) string {
	const hexdig = "0123456789abcdef"
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = hexdig[v&0xf]
		v >>= 4
	}
	return string(b)
}

// hex12 widens the per-egress hash to the 12-hex block of the 30-char TUI
// ses_* shape (12 hex + 14 alnum after the prefix).
func hex12(v uint32) string {
	return hex8(v) + hex8(fnv32("hex12|" + strconv.FormatUint(uint64(v), 10)))[:4]
}

// setEgressSession stamps the FULL session identity triple for one egress:
// both session headers AND an x-opencode-request minted INSIDE that session.
//
// It must be used by every egress-stamping path INSTEAD of relying on
// setOpencodeHeaders for the session triple. setOpencodeHeaders fills the
// process-wide pool default (alive[0]) and derives its msg_* for that default;
// a hot path that then overwrote only x-opencode-session left
// x-opencode-session-id = the SAME id on every egress and an x-opencode-request
// that belonged to another session — i.e. exactly the cross-IP link the free
// tier must never see (and a contradictory header pair upstream).
func setEgressSession(h http.Header, ses string) {
	if h == nil || ses == "" {
		return
	}
	h.Set("x-opencode-session", ses)
	h.Set("x-opencode-session-id", ses)
	// Re-derive the request id for THIS session: MsgIDPool is session-keyed,
	// so an id from the default session is a link signal, not a fallback.
	h.Del("x-opencode-request")
	if rq := getMsgIDPool().NextFor(ses); rq != "" {
		h.Set("x-opencode-request", rq)
	}
}

// setOpencodeHeaders stamps the zen CLI header set onto an already built
// upstream request. Forwarded inbound values win (affinity preserved); missing
// non-session headers are synthesized to full-CLI shape (x-opencode-client
// "cli", x-opencode-project "global", Authorization "Bearer public",
// Accept "*/*"). No-op for non-zen providers. Never overwrites a header
// already set.
//
// It does NOT invent a session id: a process-wide default (alive[0]) is one id
// on every egress, i.e. the exact cross-IP link the free tier must never see.
// Lanes that know their egress stamp it themselves — setEgressSession on the
// pool path, setOpencodeHeadersAt(…, egressKey) on the direct/home legs.
func setOpencodeHeaders(req *http.Request, ctx context.Context, p *provider.Provider) {
	if req == nil || !isOpencodeUpstream(p) {
		return
	}
	if req.Header.Get("x-opencode-session") == "" {
		// Only a genuine inbound session (real CLI affinity) is reused here.
		if ses := clientHeader(ctx, "x-opencode-session"); ses != "" {
			req.Header.Set("x-opencode-session", ses)
			// Server checks BOTH session headers (verified 2026-10-13 recipe:
			// x-opencode-session AND x-opencode-session-id = real ses_*).
			if req.Header.Get("x-opencode-session-id") == "" {
				req.Header.Set("x-opencode-session-id", ses)
			}
		}
	}
	// CLI parity, measured from the 2026-10-22 MITM capture (reqs.jsonl): the
	// real client always sends `Authorization: Bearer public` (free lane) and
	// `Accept: */*`. Ours went out with NEITHER, i.e. an anonymous stranger the
	// gate had never seen. Keys still win: a keyed provider (zen) sets its own
	// Authorization before this runs.
	if req.Header.Get("Authorization") == "" && p.AuthMode == "none" && !p.HasKeys() {
		req.Header.Set("Authorization", "Bearer public")
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}
	if req.Header.Get("x-opencode-client") == "" {
		cli := clientHeader(ctx, "x-opencode-client")
		if cli == "" {
			cli = "cli"
		}
		req.Header.Set("x-opencode-client", cli)
	}
	if req.Header.Get("x-opencode-project") == "" {
		proj := clientHeader(ctx, "x-opencode-project")
		if proj == "" {
			proj = "global"
		}
		req.Header.Set("x-opencode-project", proj)
	}
	if req.Header.Get("x-opencode-request") == "" {
		rq := clientHeader(ctx, "x-opencode-request")
		if rq == "" {
			// Genuine request id minted by the TUI INSIDE this very session.
			// Never a generated msg_* (a fabricated id is a gate signal) and
			// never an id from a sibling session (that links two egresses).
			rq = getMsgIDPool().NextFor(req.Header.Get("x-opencode-session"))
		}
		if rq != "" {
			req.Header.Set("x-opencode-request", rq)
		}
	}
}

// setOpencodeHeadersAt stamps the CLI set AND pins the session of THIS egress.
// Used by lanes that run without a pool lease (direct home-IP leg, direct
// fallback, /responses adapter): they have a real, single egress, so they get
// their own stable session instead of borrowing the pool's shared default.
func setOpencodeHeadersAt(req *http.Request, ctx context.Context, p *provider.Provider, egressKey string) {
	setOpencodeHeaders(req, ctx, p)
	if req == nil || !isOpencodeUpstream(p) || egressKey == "" {
		return
	}
	if req.Header.Get("x-opencode-session") != "" {
		return
	}
	// Genuine TUI-minted id reserved for this egress (owner map makes it
	// exclusive); generated ids are the last resort — they 403 the gate but
	// keep the header shape, same contract as stickySession.
	ses := getSessionPool().NextFor(egressKey)
	if ses == "" {
		ses = fallbackSessionFor(egressKey)
	}
	setEgressSession(req.Header, ses)
}
