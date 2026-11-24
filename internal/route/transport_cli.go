package route

// cliTransport relays chat completions through a byte-exact opencode CLI
// ClientHello and request head, in-process, with the egress book the sidecar
// used to keep.
//
// Why this exists: the free-tier gate fingerprints the RAW request bytes (header
// case and order, TLS extension order, session id clock), so the opencode lane
// has been served by a separate Go sidecar (opencode-sidecar-go, :9109) plus a
// bun merge shim (:8880) — three processes and three failure modes for one HTTP
// hop (2026-11-16: the shim hung with its listener gone, discover timed out and
// every request 503'd).
//
// This transport keeps the proven bytes AND the egress book (lastOK recency,
// park until Retry-After, session pin per (conversation, egress, IP)) inside
// ai-router. Gated by provider config `transport.mode: cli`; the sidecar stays
// the default until an isolated A/B proves parity.
//
// Two bugs of the first port are pinned by tests below:
//   - the ClientHello signature-algorithm list must be the captured one;
//   - the request body must be read ONCE per attempt set (GetBody), or the
//     retry writes content-length: 0 and the edge answers 400.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// cliUserAgent is the capture's UA: opencode 1.19.4 on bun 1.4.4. The gate
// compares it verbatim.
// cliUserAgent must be a tuple the free gate still accepts. The gate rejects
// by CLIENT VERSION, not by address: on 2026-11-23 the same body on the same
// lane answered 429 with opencode/1.19.4 and 200 with opencode/1.19.6
// (5 of 8 lanes, seconds apart, fresh session ids on both sides). The value
// here is the tuple captured from the live OMP client.
const cliUserAgent = "opencode/1.19.6 ai-sdk/provider-utils/4.0.23 runtime/bun/1.4.4"

// cliInsecureSkipVerify is a test-only seam: the sweep tests stand up a local
// TLS server with a self-signed certificate.
var cliInsecureSkipVerify = false

// cliHelloSpec is the measured opencode CLI ClientHello, captured 2026-10-22 by
// MITM. The signature list is part of the fingerprint: a different set (even
// with the same ciphers) is answered 429 in ~0.2 s.
func cliHelloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites: []uint16{4865, 4866, 4867, 49195, 49199, 49196, 49200, 52393,
			52392, 49161, 49171, 49162, 49172, 156, 157, 47, 53},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{4588, 29, 23, 24}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0807, 0x0603, 0x0201,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: 4588}, {Group: 29}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{1}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		},
	}
}

// lane is one pool egress: an HTTP proxy port and the address it exits from.
type lane struct {
	proxy string
	ip    string
}

// laneState is the sidecar's book entry: EWMA of P(200), the last 200, and the
// park expiry the upstream named.
type laneState struct {
	// IP is the address this reputation belongs to: a lane that comes back on a
	// new tunnel (a fresh /64) must not inherit the old address's park/score
	// (the sidecar's discover() reset them for exactly this reason).
	IP     string  `json:"ip"`
	Score  float64 `json:"score"`
	LastOK int64   `json:"last_ok"`
	Until  int64   `json:"until"`
	OK     int     `json:"ok"`
	Bad    int     `json:"bad"`
	Seen   int     `json:"seen"`
}

// sidPin is one conversation's session id on one egress.
type sidPin struct {
	SID   string `json:"sid"`
	Proxy string `json:"proxy"`
	IP    string `json:"ip"`
	At    int64  `json:"at"`
}

// CLITransport owns the lane list, the egress book and the sweep.
type CLITransport struct {
	apiURL   string
	token    string
	upstream string
	timeout  time.Duration
	attempts int
	budget   time.Duration
	// responseTimeout bounds one exchange after the TLS handshake. It is
	// deliberately generous (a 180k-token prompt needs 20-70 s) but bounded:
	// the pool path has transport_pool_test.go "hung upstream must be bounded",
	// and a lane that goes silent after the handshake must not hold a request
	// forever.
	responseTimeout time.Duration
	bookPath        string
	// instancePort discriminates this process's state files from a stand
	// that shares LOCALAPPDATA (LIVE :9001 next to a :9003 smoke stand).
	instancePort string
	// wideState/wideTraces are the node-backed lane sources (the merge shim's
	// job); empty disables the wide half.
	wideState  string
	wideTraces string
	// warpCfgPath is where warp-router keeps its API token (reloadToken).
	warpCfgPath string
	// sidecarStatePath is the legacy sidecar book imported on first run.
	sidecarStatePath string

	mu      sync.Mutex
	lanes   []lane
	fetched time.Time
	book    map[string]*laneState
	burnt   map[string]int64 // /64 group -> unix ms until it is known burned
	// poolPorts are the HTTP ports the main pool currently serves; redraws are
	// limited to them (any base, not just the live 8750-8769 range, so a stand
	// on another base exercises the same path). nil = pool not read yet.
	poolPorts  map[int]bool
	burntRetry map[string]int // lane -> redraws issued after landing in a burned /64
	pins       map[string][]sidPin
	// lastRotate is the wall clock of the last accepted pool rotate (0 = never).
	lastRotate atomic.Int64
	// redrawAt throttles per-lane redraws (lane proxy -> unixnano of the last
	// request). A redrawn lane gets a fresh /64, so its old park is cleared
	// once the slot answered. Guarded by mu.
	redrawAt map[string]int64

	// bypassMu guards the manual bypass sweep: only one runs at a time, and
	// the GUI polls the snapshot while the goroutine writes progress.
	bypassMu      sync.Mutex
	bypassState   *BypassSweepState
	bypassRunning bool
	// lastFleetRedraw bounds redraws across the whole daemon: a redraw
	// re-registers the account's WARP peer, which re-handshakes every slot on
	// that account and RSTs their live streams. One redraw per 45 s at most,
	// and none while any lane streams. Guarded by mu.
	lastFleetRedraw time.Time
	// active counts open response bodies per lane proxy. A redraw or a fleet
	// rotate restarts the tunnel, which RSTs every in-flight stream on that
	// slot; both therefore wait until the lane (or the whole fleet) is idle.
	// The request that got the 429 has already moved to the next lane.
	active        map[string]int
	pendingRedraw map[string]bool
	// streak counts consecutive 429s per lane, cleared by any 200. ONE rule
	// for every request: a lane may answer a body-verdict 429 without being
	// punished, but a lane that 429s twice in a row is burned for this turn
	// whatever its earlier 200s were (measured 2026-11-23: lanes 8769/9706
	// held a small 200, so every big turn picked them first, got 429, and
	// never parked them - half the fleet stayed unusable while a test body of
	// the same size answered 200 on a fresh lane). Guarded by mu.
	streak        map[string]int
	pendingRotate string
	// wallUntil is the unix instant the free pool reopens, learned from 429s
	// that named the same reset from unrelated egresses (0 = no wall). While
	// it is in the future nothing local can produce a 200, so redraw/rotate
	// are parked and requests fail fast instead of sweeping the fleet.
	wallUntil  int64
	wallEpochs map[int64]map[string]struct{}
	wallLogged bool
	// wallBody is the upstream 429 body kept for replay while walled.
	wallBody []byte
	// dirtyBook asks the next unlocked caller to persist a book that was
	// repaired (parks cleared when the wall lifted).
	dirtyBook bool
}

// NewCLITransport builds the transport. apiURL is the MERGED pool view
// (warp-router main + wide node lanes), never one half of it: every clean lane
// measured on 2026-11-16 was a wide (mihomo) one.
func NewCLITransport(apiURL, token, upstream string, timeout time.Duration, instancePort string, wideState, wideTraces string) *CLITransport {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	if upstream == "" {
		upstream = "opencode.ai"
	}
	if apiURL == "" {
		apiURL = "http://127.0.0.1:8880"
	}
	if instancePort == "" {
		instancePort = "0"
	}
	t := &CLITransport{
		apiURL:          strings.TrimRight(apiURL, "/"),
		token:           token,
		upstream:        upstream,
		timeout:         timeout,
		instancePort:    instancePort,
		attempts:        48,
		budget:          8 * time.Second,
		responseTimeout: 150 * time.Second,
		bookPath:        filepath.Join(os.Getenv("LOCALAPPDATA"), "ai-router", "cli_egress_book_"+instancePort+".json"),
		wideState:       envOr("AI_ROUTER_WIDE_STATE", orDefault(wideState, `D:\omp_watch\vpnpool\wide_state.json`)),
		wideTraces:      envOr("AI_ROUTER_WIDE_TRACES", orDefault(wideTraces, `D:\omp_watch\vpnpool\wide_traces.txt`)),
		book:            map[string]*laneState{},
		burnt:           map[string]int64{},
		active:          map[string]int{},
		pendingRedraw:   map[string]bool{},
		streak:          map[string]int{},
		pins:            map[string][]sidPin{},
	}
	t.loadBook()
	return t
}

func (t *CLITransport) Name() string { return "cli" }

// SelfRotating reports that the sweep lives in this transport: the router makes
// ONE outer attempt. Four outer attempts multiplied a walled turn into 39-83 s
// (measured 2026-11-18: attempts=3, duration 39 172 / 83 797 / 44 939 ms on
// walled rows). One 8 s sweep fails fast; the client's own retry then gets a
// fresh sweep, and a clean lane is found by the lastOK-first order.
func (t *CLITransport) SelfRotating() bool { return true }

func (t *CLITransport) Acquire(ctx context.Context, rc RequestContext, p *provider.Provider) (*Lease, error) {
	// Timeout: 0 - a client-level timeout covers the WHOLE body read and cut
	// healthy long streams at t.timeout (150 s) even though tokens were still
	// arriving. Bounds live on the socket: dial budget + per-read idle deadline.
	return &Lease{Client: &http.Client{Transport: &cliSweep{t: t}, Timeout: 0}}, nil
}

// loadBook restores the egress knowledge across restarts (a burned address must
// not be re-tested from scratch, and a proven one must be picked first).
func (t *CLITransport) loadBook() {
	b, err := os.ReadFile(t.bookPath)
	if err != nil {
		// First run of the in-process transport: import the sidecar's egress
		// book (v2_state.json). Without it every sweep starts blind on a burned
		// fleet and a walled window answers 429 on nearly every turn (measured
		// 2026-11-18: 78% 429 on the cold in-process path vs 20% on the sidecar,
		// which kept the history).
		t.importSidecarBook()
		return
	}
	var doc struct {
		Book map[string]*laneState `json:"book"`
	}
	if json.Unmarshal(b, &doc) == nil && doc.Book != nil {
		t.book = doc.Book
	}
}

// importSidecarBook translates the sidecar's v2_state.json into this book.
func (t *CLITransport) importSidecarBook() {
	path := t.sidecarStatePath
	if path == "" {
		path = `D:\omp_watch\opencode-sidecar\v2_state.json`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc struct {
		Egress map[string]struct {
			Proxy  string `json:"proxy"`
			IP     string `json:"ip"`
			OK     int    `json:"ok"`
			Bad    int    `json:"bad"`
			Until  int64  `json:"until"`
			LastOK int64  `json:"lastOK"`
			Seen   int    `json:"seen"`
		} `json:"egress"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.Egress) == 0 {
		return
	}
	book := make(map[string]*laneState, len(doc.Egress))
	for key, row := range doc.Egress {
		proxy := row.Proxy
		if proxy == "" {
			if i := strings.Index(key, "|"); i > 0 {
				proxy = key[:i]
			} else {
				proxy = key
			}
		}
		score := 0.0
		if n := row.OK + row.Bad; n > 0 {
			score = float64(row.OK) / float64(n)
		}
		book[proxy] = &laneState{Score: score, LastOK: row.LastOK, Until: row.Until, OK: row.OK, Bad: row.Bad, Seen: row.Seen}
	}
	t.book = book
	slog.Default().Warn("cli: imported sidecar egress book", "lanes", len(book))
}

func (t *CLITransport) saveBook() {
	t.mu.Lock()
	snapshot := make(map[string]*laneState, len(t.book))
	for k, v := range t.book {
		c := *v
		snapshot[k] = &c
	}
	t.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(t.bookPath), 0o755)
	if b, err := json.Marshal(map[string]any{"book": snapshot}); err == nil {
		tmp := t.bookPath + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			_ = os.Rename(tmp, t.bookPath)
		}
	}
}

// lanesNow returns the pool's merged view, refreshed every 60 s.
// lanesNow builds the fleet view from the two real sources — the warp-router
// API (main) and the wide lane state — exactly like the bun merge shim did
// (merge_shim.mjs:85-120), so :8880 is no longer needed:
//   - main: GET <api_url>/api/v1/pool, keep state=="up" with http_addr+public_ip;
//   - wide: wide_state.json rows {http,i} mapped to addresses through
//     wide_traces.txt ("<port> <ip> <colo>"), dropped when the trace is older
//     than 15 min, and kept only while the port answers;
//   - merge: one lane per public IP, main first (one address = one window).
//
// A main read failure keeps the previous view instead of resizing the fleet.
// filterBurntLanes drops lanes whose /64 group is parked burned. The gate's
// allowance is per prefix, so an attempt on a burned group is a guaranteed
// 429 that also burns a client-visible attempt; the lane still gets renewed by
// the park-redraw loop, and a fresh /64 re-enters the sweep.
func (t *CLITransport) filterBurntLanes(in []lane) []lane {
	t.mu.Lock()
	now := time.Now().UnixMilli()
	// Drop expired entries so the map cannot grow without bound.
	for g, until := range t.burnt {
		if until <= now {
			delete(t.burnt, g)
		}
	}
	if len(t.burnt) == 0 {
		t.mu.Unlock()
		return in
	}
	out := in[:0]
	dropped := 0
	var redraw []string
	cd := t.redrawCooldownLocked(time.Now())
	for _, l := range in {
		if until, ok := t.burnt[group64(l.ip)]; ok && until > now {
			dropped++
			// A dropped lane is invisible to the sweep, and an address change
			// resets its book entry (Until=0), so redrawLongParked would never
			// pick it: without this path the slot idles for the whole park TTL
			// (~2h). Redraw it on the next free node, bounded to 3 attempts —
			// after that the prefix is treated as settled until the park
			// expires.
			if t.redrawAt[l.proxy] != 0 && time.Since(time.Unix(0, t.redrawAt[l.proxy])) < cd {
				continue
			}
			if t.burntRetry[l.proxy] >= 3 {
				continue
			}
			if t.burntRetry == nil {
				t.burntRetry = map[string]int{}
			}
			t.burntRetry[l.proxy]++
			redraw = append(redraw, l.proxy)
			continue
		}
		// The lane came back clean: its retry budget starts over.
		delete(t.burntRetry, l.proxy)
		out = append(out, l)
	}
	if dropped > 0 {
		slog.Default().Info("cli: skip burned /64 groups", "dropped", dropped, "kept", len(out), "redraw", len(redraw))
	}
	t.mu.Unlock()
	for _, p := range redraw {
		go t.maybeRedrawLane(p)
	}
	return out
}

func (t *CLITransport) lanesNow() []lane {
	t.mu.Lock()
	if len(t.lanes) > 0 && time.Since(t.fetched) < 60*time.Second {
		out := t.lanes
		t.mu.Unlock()
		return out
	}
	t.mu.Unlock()

	main, mainErr := t.mainLanes()
	// An ANSWERED pool with zero up slots is not a failed read: a stand with
	// its own wide fleet deliberately runs without the live main half. Only
	// an explicit env opt-in treats that answer as a valid (wide-only) view;
	// live keeps the resize guard, where an empty main means a broken read and
	// a wide-only fleet is a set of burned lanes.
	allowEmptyMain := os.Getenv("AI_ROUTER_ALLOW_EMPTY_MAIN") == "1"
	if mainErr != nil || (len(main) == 0 && !allowEmptyMain) {
		// A failed main read is NOT an empty fleet: the wide half alone is a
		// set of burned lanes, and overwriting the view with it resizes the
		// sweep onto dead addresses (measured hazard the shim documented:
		// "an empty answer would make it resize the direct fleet"). Keep the
		// last good view and try again after the cache window.
		t.mu.Lock()
		prev := t.lanes
		t.mu.Unlock()
		reason := "empty answer"
		if mainErr != nil {
			reason = mainErr.Error()
		}
		if len(prev) > 0 {
			slog.Default().Warn("cli: main pool unusable, keeping last good view", "lanes", len(prev), "reason", reason)
			return prev
		}
		slog.Default().Warn("cli: main pool unusable and no previous view", "reason", reason)
		return nil
	}
	if len(main) == 0 {
		slog.Default().Info("cli: main pool empty, serving wide-only view (AI_ROUTER_ALLOW_EMPTY_MAIN=1)")
	}
	wide := t.wideLanes()
	// One lane per /64 GROUP, not per address: the gate's allowance is per
	// prefix, so three slots inside one burned /64 are three wasted sweep
	// attempts (measured: 2a09:bac1:47a0:170 carried 4 slots, 70x429 / 0x200).
	// Distinct groups are what "spread the pool" means; keep the best-scoring
	// address of each group.
	best := map[string]lane{}
	order := []string{}
	consider := func(l lane) {
		key := group64(l.ip)
		cur, ok := best[key]
		if !ok {
			best[key] = l
			order = append(order, key)
			return
		}
		if t.laneScore(l.proxy) > t.laneScore(cur.proxy) {
			best[key] = l
		}
	}
	for _, l := range append(main, wide...) {
		if l.ip == "" {
			continue
		}
		consider(l)
	}
	out := make([]lane, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	out = t.filterBurntLanes(out)
	// A lane that came back on a new address is a NEW egress: its park and score
	// belong to the old /64 and must not follow it.
	t.mu.Lock()
	for _, l := range out {
		// Both books carry the address: a |big| entry whose ip stayed empty
		// never reset on a new tunnel (and rotate64 logged an empty ip).
		for _, key := range []string{l.proxy, bookKey(l.proxy, true)} {
			st := t.book[key]
			if st == nil {
				continue
			}
			if st.IP != "" && l.ip != "" && st.IP != l.ip {
				slog.Default().Warn("cli: lane address changed, reputation reset",
					"key", key, "old_ip", st.IP, "new_ip", l.ip)
				*st = laneState{IP: l.ip, LastOK: st.LastOK}
				continue
			}
			st.IP = l.ip
		}
	}
	t.mu.Unlock()
	t.mu.Lock()
	t.lanes = out
	t.fetched = time.Now()
	t.mu.Unlock()
	return out
}

// group64 is the address group the gate's allowance is keyed on: the /64 for
// IPv6 (first four hextets) and the /24 for IPv4.
// group64 canonicalises an address to its /64 prefix. net.ParseIP is used
// instead of a string split: the pool writers disagree on form ("2a09:bac1:5500::3"
// vs "2a09:bac1:5500:0:0:0:0:3" are the SAME /64, but split on ":" yields
// "…:5500:" vs "…:5500:0"). A key mismatch would silently defeat the whole
// burned-prefix registry.
func group64(ip string) string {
	if ip == "" {
		return ""
	}
	if strings.Contains(ip, ":") {
		if p := net.ParseIP(ip); p != nil {
			if v6 := p.To16(); v6 != nil {
				// A 16-byte IP with the host half zeroed: net.IP.String() only
				// formats 4/16-byte slices, an 8-byte one prints "?hex".
				prefix := make(net.IP, net.IPv6len)
				copy(prefix, v6[:8])
				return prefix.String() + "/64"
			}
		}
		parts := strings.Split(ip, ":")
		if len(parts) >= 4 {
			return strings.Join(parts[:4], ":")
		}
		return ip
	}
	if p := net.ParseIP(ip); p != nil && p.To4() != nil {
		return net.IP(p.To4()[:3]).String() + ".0/24"
	}
	parts := strings.Split(ip, ".")
	if len(parts) >= 3 {
		return strings.Join(parts[:3], ".")
	}
	return ip
}

// laneScore reads a lane's book score (0 when unknown).
func (t *CLITransport) laneScore(proxy string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if st := t.book[proxy]; st != nil {
		return st.Score
	}
	return 0
}

// mainLanes reads the warp-router pool (direct WARP slots).
func (t *CLITransport) mainLanes() ([]lane, error) {
	req, err := http.NewRequest("GET", strings.TrimRight(t.apiURL, "/")+"/api/v1/pool", nil)
	if err != nil {
		return nil, fmt.Errorf("main pool: bad api url")
	}
	if t.token != "" {
		req.Header.Set("authorization", "Bearer "+t.token)
	}
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	// warp-router rotates its API token (the shim reloaded on 401 for exactly
	// this reason). Without the reload the main half is lost until ai-router
	// restarts, and a wide-only view is a fleet of burned lanes.
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		if t.reloadToken() {
			req2, derr := http.NewRequest("GET", strings.TrimRight(t.apiURL, "/")+"/api/v1/pool", nil)
			if derr != nil {
				return nil, derr
			}
			req2.Header.Set("authorization", "Bearer "+t.token)
			resp, err = (&http.Client{Timeout: 4 * time.Second}).Do(req2)
			if err != nil {
				return nil, err
			}
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("main pool: http %d", resp.StatusCode)
	}
	var pr struct {
		Backends []struct {
			HTTPAddr string `json:"http_addr"`
			PublicIP string `json:"public_ip"`
			State    string `json:"state"`
		} `json:"backends"`
	}
	if json.NewDecoder(resp.Body).Decode(&pr) != nil {
		return nil, fmt.Errorf("main pool: bad json")
	}
	var out []lane
	ports := map[int]bool{}
	for _, b := range pr.Backends {
		if b.HTTPAddr == "" || b.PublicIP == "" || b.State != "up" {
			continue
		}
		out = append(out, lane{proxy: "http://" + b.HTTPAddr, ip: b.PublicIP})
		if _, p, ok := strings.Cut(b.HTTPAddr, ":"); ok {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				ports[n] = true
			}
		}
	}
	t.mu.Lock()
	t.poolPorts = ports
	t.mu.Unlock()
	return out, nil
}

// wideLanes reads the node-backed (mihomo + WARP) lane state files.
func (t *CLITransport) wideLanes() []lane {
	stateFile := t.wideState
	tracesFile := t.wideTraces
	if stateFile == "" || tracesFile == "" {
		return nil
	}
	if fi, err := os.Stat(tracesFile); err != nil || time.Since(fi.ModTime()) > 15*time.Minute {
		return nil // stale trace: the wide half is unverifiable
	}
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		return nil
	}
	var rows []struct {
		HTTP int `json:"http"`
	}
	if json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	ipByPort := map[int]string{}
	tb, err := os.ReadFile(tracesFile)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(tb), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		port, perr := strconv.Atoi(f[0])
		if perr != nil {
			continue
		}
		ipByPort[port] = f[1]
	}
	var out []lane
	for _, r := range rows {
		ip, ok := ipByPort[r.HTTP]
		if !ok || r.HTTP <= 0 {
			continue
		}
		if !portOpen(r.HTTP) {
			continue
		}
		out = append(out, lane{proxy: fmt.Sprintf("http://127.0.0.1:%d", r.HTTP), ip: ip})
	}
	return out
}

// reloadToken re-reads warp-router's API token (it rotates at runtime).
func (t *CLITransport) reloadToken() bool {
	path := t.warpCfgPath
	if path == "" {
		path = filepath.Join(os.Getenv("LOCALAPPDATA"), "warp-router", "config.yaml")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "auth_token:") {
			continue
		}
		tok := strings.TrimSpace(strings.TrimPrefix(line, "auth_token:"))
		tok = strings.Trim(tok, `"'`)
		if tok != "" && tok != t.token {
			t.mu.Lock()
			t.token = tok
			t.mu.Unlock()
			slog.Default().Warn("cli: warp token reloaded")
			return true
		}
	}
	return false
}

// portOpen reports whether a local lane accepts a TCP connection (the shim
// dropped lanes whose listener was gone).
func portOpen(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// peek reads a book entry without creating it: the picker must not materialise
// empty entries, or "score 0" would look like a sacrifice for every lane it
// merely inspected.
func (t *CLITransport) peek(key string) *laneState {
	return t.book[key]
}

// state returns (creating) the book entry for a lane under the lock.
func (t *CLITransport) state(proxy string) *laneState {
	st := t.book[proxy]
	if st == nil {
		st = &laneState{}
		t.book[proxy] = st
	}
	return st
}

// laneAdmittedWindow is how long a lane's 200 keeps its "proven" status for the
// 429 verdict. A gate 429 seconds after a 200 on the same lane is about the
// request body (an echoed sentinel, a shape the gate refuses), not about the
// egress.
const laneAdmittedWindow = 90 * time.Second

// laneJustAdmitted reports whether this proxy's book proved a 200 within
// laneAdmittedWindow, in either the plain (small-body) book or its "|big"
// sibling. Both are keyed by proxy, and a big-body success lands in the latter
// while the former may still carry a recent small-body 200 for the same lane.
func (t *CLITransport) laneJustAdmitted(proxy string, now int64) bool {
	window := laneAdmittedWindow.Milliseconds()
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, key := range []string{proxy, bookKey(proxy, true)} {
		if st := t.book[key]; st != nil && st.LastOK > 0 && now-st.LastOK <= window {
			return true
		}
	}
	return false
}

// laneJustAdmittedBook is laneJustAdmitted scoped to one book. A BIG body must
// consult the big book only: a lane that served a 40 KB turn a minute ago says
// nothing about a 700 KB one, and treating the small 200 as proof turned every
// big turn into "body verdict, not parked" on the same warm lane while ten
// untouched /64s answered 200 (measured 2026-11-23 14:14: 8/20 lanes 429 with
// a 12 h Retry-After, 10/20 answered 200, yet every >300 KB turn 429'd on lane
// 9717, which had only small 200s in its book).
func (t *CLITransport) laneJustAdmittedBook(proxy string, big bool, now int64) bool {
	window := laneAdmittedWindow.Milliseconds()
	t.mu.Lock()
	defer t.mu.Unlock()
	if st := t.book[bookKey(proxy, big)]; st != nil && st.LastOK > 0 && now-st.LastOK <= window {
		return true
	}
	return false
}

// dumpFailed429 keeps the body a 429 verdict refused (only >200 KB, at most
// five files), because "prompt_tok" in the response log is len(body), not
// tokens, and the free-tier refusal itself names no size. Without the bytes a
// "this session always 429s" report cannot be told apart from a fleet problem.
//
// The stored copy is redacted for output: every verbatim canonical marker —
// including the system head that legitimately rides on every gated request —
// is broken with a zero-width space. A dump is read by agents; the head in a
// dump becomes the head in their next request and that request 429s. Every
// other byte is identical, so shape/size diagnosis still works; only a
// byte-exact replay of the head is impossible from the dump.
var failed429Dump struct {
	mu   sync.Mutex
	seen map[string]bool
}

func dumpFailed429(body []byte) {
	if len(body) < 200_000 {
		return
	}
	sum := sha256.Sum256(body)
	key := fmt.Sprintf("%x", sum[:4])
	failed429Dump.mu.Lock()
	if failed429Dump.seen == nil {
		failed429Dump.seen = map[string]bool{}
	}
	if failed429Dump.seen[key] || len(failed429Dump.seen) >= 5 {
		failed429Dump.mu.Unlock()
		return
	}
	failed429Dump.seen[key] = true
	failed429Dump.mu.Unlock()
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "ai-router", "fail429")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	name := filepath.Join(dir, "body-"+key+".json")
	if err := os.WriteFile(name, redactCanonMarkerForOutput(body), 0o644); err != nil {
		return
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	tools, _ := m["tools"].([]any)
	msgs, _ := m["messages"].([]any)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slog.Default().Warn("cli: dumped refused body", "file", name, "bytes", len(body), "keys", strings.Join(keys, ","), "tools", len(tools), "messages", len(msgs))
}

// pick returns the best lane not already tried, preferring a recent 200 and
// skipping parked ones. Caller holds t.mu.
func bookKey(proxy string, big bool) string {
	if big {
		return proxy + "|big"
	}
	return proxy
}

func (t *CLITransport) pick(convKey string, lanes []lane, tried map[string]bool, now int64, big bool, preferFresh bool) *lane {
	// The free gate's allowance is a DAILY per-/64 bucket (UTC): a lane whose
	// last 200 predates today's 00:00 UTC still has its whole bucket, while a
	// high score from yesterday is history, not capacity. A 429 on a warm lane
	// must try an untouched-today lane next, not the next stale-scored one
	// (measured 2026-11-23: two sessions held the only lanes with today's 200s
	// and every other conversation looped over stale-scored lanes at 100% 429).
	dayStart := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	admittedToday := func(st *laneState) bool { return st != nil && st.LastOK >= dayStart }
	pinned := map[string]bool{}
	for _, p := range t.pins[convKey] {
		pinned[p.Proxy] = true
	}
	// Big bodies (a >300 KB prompt burns a lane's daily allowance in one turn)
	// are SACRIFICIAL: burn what is already burned. Prefer the most-deeply
	// parked lane, else the worst-scoring one, never a warm lane — otherwise the
	// empty big book (all Score=0, u=0.2) picks the first lane in the list,
	// which is a main warm lane, and the 61% "wall after >300 KB" class keeps
	// eating the pool one lane per big turn.
	// Big bodies (>300 KB) get the SAME lane quality as small ones: the
	// warmest lane. The attempt cap, not the pick, bounds the damage — one big
	// turn parks at most one lane. Picking the worst lane here made every big
	// body 429 while the warm lane served small requests (measured 2026-11-18:
	// 400 KB 0/3 on live, 5/5 before the sacrificial pick landed).
	if big {
		var soonestParked, warm, fresh *lane
		var soonestParkedUntil int64
		bestWarm, bestFresh := -1.0, -1.0
		for i := range lanes {
			if tried[lanes[i].proxy] {
				continue
			}
			st := t.peek(bookKey(lanes[i].proxy, true))
			if st == nil {
				st = &laneState{}
			}
			if st.Until > now {
				if soonestParked == nil || st.Until < soonestParkedUntil {
					soonestParked, soonestParkedUntil = &lanes[i], st.Until
				}
				continue
			}
			normalToday := t.book[lanes[i].proxy] != nil && admittedToday(t.book[lanes[i].proxy])
			// A lane with a 200 today (either book) has a spent-or-bound /64:
			// it is a warm candidate at best. A lane without one holds an
			// untouched daily bucket and is the one a 429 should try next.
			if !admittedToday(st) && !normalToday {
				if fs := 0.2 / (1 + float64(st.Seen)); fs > bestFresh {
					bestFresh, fresh = fs, &lanes[i]
				}
				continue
			}
			score := 0.0
			if admittedToday(st) {
				score = st.Score
				// Recency of a real BIG 200: an 800 KB turn succeeds on the lane
				// that just admitted one and 429s on its neighbour (measured
				// 2026-11-21: the same body_sha answered 429, then 200 nine
				// seconds later on another lane). A stale high score alone picked
				// the wrong lane and one big attempt per turn makes that fatal.
				if now-st.LastOK <= 10*60_000 {
					score += st.Score
				}
			}
			if normalToday && score == 0 {
				score = t.book[lanes[i].proxy].Score
			}
			if score > bestWarm {
				bestWarm, warm = score, &lanes[i]
			}
		}
		if preferFresh && fresh != nil {
			return fresh
		}
		if warm != nil {
			return warm
		}
		if fresh != nil {
			return fresh
		}
		return soonestParked
	}
	var best *lane
	bestU := -1.0
	var soonest *lane
	for i := range lanes {
		if tried[lanes[i].proxy] {
			continue
		}
		st := t.peek(bookKey(lanes[i].proxy, big))
		if st == nil {
			st = &laneState{}
		}
		if soonest == nil || st.Until < func() int64 {
			if p := t.peek(bookKey(soonest.proxy, big)); p != nil {
				return p.Until
			}
			return 0
		}() {
			soonest = &lanes[i]
		}
	}
	for i := range lanes {
		l := &lanes[i]
		if tried[l.proxy] {
			continue
		}
		st := t.state(l.proxy)
		if st.Until > now {
			continue
		}
		u := 0.5*st.Score + 0.2/(1+float64(st.Seen))
		// Recency of a real 200: a proven lane must beat an untested one, while
		// `Until` stays a pure park expiry.
		if st.LastOK > 0 && now-st.LastOK <= 10*60_000 {
			u += st.Score
		}
		if pinned[l.proxy] {
			u += 0.3
		}
		// On a retry, prefer a lane whose daily bucket is untouched: a lane
		// that already admitted today is either spent or bound to another
		// session, which is the 100%-429 class for a new conversation.
		if preferFresh && (admittedToday(st) || (t.book[l.proxy] != nil && admittedToday(t.book[l.proxy]))) {
			u -= 10
		}
		if u > bestU {
			bestU, best = u, l
		}
	}
	if best != nil {
		return best
	}
	// Every lane is parked: degrade to the one whose window expires soonest
	// instead of failing the turn. A wall is per address and lifts at different
	// times, so a parked lane is a guess, not a verdict (measured 2026-11-17:
	// with the persisted book all 56 lanes were parked and every request died in
	// ~1 s with "no egress answered").
	return soonest
}

// bind returns the session id for (conversation, egress, IP): the gate's free
// allowance is per (address, sid), so an address that left the pool or came back
// on a new tunnel must never inherit an old id.
func (t *CLITransport) bind(convKey, proxy, ip string, now int64) string {
	// Reuse the pinned sid: the free budget is NOT per sid (measured 2026-11-22,
	// isolated stand: 80/80 x 200 on one sid+one /64, and the SAME sid answered
	// 16/16 on three freshly drawn /64s right after two burned prefixes 429ed).
	// The old "~40 per restart" was a dead warp-router plus the local wall latch,
	// not an id cap; minting a fresh id per attempt only adds session churn.
	for _, p := range t.pins[convKey] {
		if p.Proxy == proxy && p.IP == ip && p.SID != "" {
			return p.SID
		}
	}
	// A real TUI-minted id first: the gate no longer accepts a ses_* id on
	// FORMAT alone. Measured 2026-11-20 from this host: an id taken from
	// opencode.db answered 200, every generated id got FreeUsageLimitError —
	// so a minted id is a guarantee of one 429 and a fake "daily quota" in the
	// client's face. Generation stays as the last resort (no db, no harvest).
	sid := ""
	ekey := proxy
	if ekey == "" {
		ekey = directHomeIPKey
	}
	// The egress identity is (proxy, public IP), not the local proxy port: a
	// lane keeps its port while warp-router rotates the worker's exit, so a
	// proxy-only key handed the SAME id to a NEW address — the cross-address
	// correlation the pool's owner map exists to prevent (review 2026-11-20 F1).
	if ip != "" {
		ekey += "|" + ip
	}
	if opencodeDBPath() != "" {
		sid = getSessionPool().NextFor(ekey)
	}
	if sid == "" {
		sid = mintCLIID("ses_", convKey, now)
	}
	list := append(t.pins[convKey], sidPin{SID: sid, Proxy: proxy, IP: ip, At: now})
	if len(list) > 8 {
		list = list[len(list)-8:]
	}
	t.pins[convKey] = list
	return sid
}

func (t *CLITransport) unpin(convKey, proxy string) {
	list := t.pins[convKey]
	kept := list[:0]
	for _, p := range list {
		if p.Proxy != proxy {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		delete(t.pins, convKey)
		return
	}
	t.pins[convKey] = kept
}

// mark records one attempt: 200 clears the park, a refusal parks the lane for
// the window the upstream named (capped at 15 min — the shortest observed
// window was 722 s, and a lane that recovers early must not wait hours).
// lastLaneSuccessUnix is the wall clock of the last 200 any CLI lane served.
// The fleet-wide rebuild escalation consults it: a fleet that still has a
// working lane must not be killed and re-minted (the user's rule — rotate the
// burned slot, leave the others serving).
var lastLaneSuccessUnix atomic.Int64

func (t *CLITransport) mark(proxy string, ok bool, parkMS int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state(proxy)
	st.Seen++
	const alpha = 0.25
	if ok {
		st.OK++
		st.Score = st.Score*(1-alpha) + alpha
		st.Until = 0
		st.LastOK = time.Now().UnixMilli()
		lastLaneSuccessUnix.Store(st.LastOK / 1000)
		return
	}
	st.Bad++
	st.Score = st.Score * (1 - alpha)
	if parkMS > 0 {
		until := time.Now().UnixMilli() + parkMS
		st.Until = until
		// The zen allowance is per IPv6 /64, not per address: a burned group
		// refuses every lane inside it, so the whole prefix is out of the
		// sweep until the park expires (measured 2026-11-21: 2a09:bac1:47a0:170
		// carried 4 slots, 70x429 / 0x200).
		if g := group64(st.IP); g != "" {
			if prev, ok := t.burnt[g]; !ok || until > prev {
				t.burnt[g] = until
			}
		}
	}
}

// upstreamAddr is the dial target: host:port when the config carries one
// (tests point it at a local listener), else the HTTPS default.
func (t *CLITransport) upstreamAddr() string {
	if strings.Contains(t.upstream, ":") {
		return t.upstream
	}
	return t.upstream + ":443"
}

// cliSweep walks the lane list until one answers.
type cliSweep struct{ t *CLITransport }

func (s *cliSweep) RoundTrip(req *http.Request) (*http.Response, error) {
	// Read the body ONCE: a retried attempt that re-reads req.Body writes
	// content-length: 0 and the edge answers 400 (measured 2026-11-17).
	var body []byte
	var err error
	if req.GetBody != nil {
		var rc io.ReadCloser
		if rc, err = req.GetBody(); err == nil {
			body, err = io.ReadAll(rc)
			_ = rc.Close()
		}
	} else if req.Body != nil {
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	// Big prompts burn a lane's daily allowance in one turn (measured
	// 2026-11-18: 61% of lane walls happen right after a >300 KB request,
	// median last-200 465 KB, p95 1232 KB). Isolate them: a huge turn may
	// only park lanes from its own book, so the warm pool that serves small
	// requests stays alive.
	// Control for the big-body gate: with the bypass set, a big body uses the
	// NORMAL pick (unparked lanes), so "zen refuses 400 KB" is separable from
	// "the pick can only test still-parked lanes".
	// The gate 429s any tool schema whose parameters.properties contain
	// old_string/new_string (the OMP `edit` tool's argument names) — measured
	// 2026-11-15 on an admitting lane and re-confirmed 2026-11-18: the same
	// body with oldString/newString answers 200. Rename them in the schema
	// (and in `required`); renameToolCallArgs renames the call arguments back.
	if nb, changed := renameForbiddenProps(body); changed {
		body = nb
	}
	convKey := req.Header.Get("x-opencode-session")
	if convKey == "" {
		convKey = req.Header.Get("x-opencode-session-id")
	}
	t := s.t
	// One rule for every turn: the sweep walks the whole fleet (a 780 KB body
	// answered 200 on 8759/8761/8765 while the six lanes it reached first said
	// 429 - the old 8 s dial budget stopped exactly there). The deadline is 30 s
	// because a full pass over a burned fleet takes ~20 s, and the router's own
	// 180 s upstream timeout turned longer sweeps into a 502 the client could not
	// act on: a burned /64 must surface as 429 with its Retry-After, not as a
	// gateway error. The per-attempt dial budget stays t.budget.
	deadline := time.Now().Add(30 * time.Second)
	tried := map[string]bool{}
	var lastBody []byte
	lastStatus := 0
	// A big body gets exactly ONE lane attempt: every attempt parks the lane it
	// touched, so a 48-lane sweep on a 1.2 MB turn killed up to 48 warm lanes
	// at once (measured 2026-11-18: warm set 2 -> 1 and the 429 share 49% ->
	// 75% over 10 min WITH the sacrificial pick). Sacrificial pick decides
	// WHICH lane dies; the cap decides HOW MANY die. Small requests keep the
	// full sweep.
	attempts := t.attempts
	// A 429 mid-sweep means the lane's /64 does not hold THIS body: the next
	// attempt must be an untouched-today prefix, not the next warm lane whose
	// bucket a neighbour conversation already spent.
	preferFresh := false
	for i := 0; i < attempts; i++ {
		if time.Now().After(deadline) || req.Context().Err() != nil {
			break
		}
		// lanesNow takes t.mu itself: fetch BEFORE locking (Go mutexes are not
		// reentrant, and the deadlock hung every request with zero bytes). An
		// empty view right after a shim/pool restart must not fail the request
		// in microseconds: retry briefly, the pool is usually back within a
		// second (measured 2026-11-17 live cutover: 64 ms "no egress answered"
		// while the merged view was still warming).
		lanes := t.lanesNow()
		if len(lanes) == 0 {
			for wait := 0; wait < 3 && len(lanes) == 0; wait++ {
				time.Sleep(250 * time.Millisecond)
				lanes = t.lanesNow()
			}
		}
		t.mu.Lock()
		nowMS := time.Now().UnixMilli()
		l := t.pick(convKey, lanes, tried, nowMS, false, preferFresh)
		if l == nil {
			// Nothing left to try AND nothing unparked: the whole fleet is
			// burned, which is exactly the state a new address fixes. A sweep
			// that merely used up its attempt cap must not rotate (every
			// rotate respawns 20 workers for 1-2 min).
			available := 0
			for _, ln := range lanes {
				if tried[ln.proxy] {
					continue
				}
				if st := t.peek(ln.proxy); st == nil || st.Until <= nowMS {
					available++
				}
			}
			t.mu.Unlock()
			if len(lanes) > 0 && available == 0 {
				t.rotateFleet("fleet fully parked")
			}
			break
		}
		tried[l.proxy] = true
		sid := t.bind(convKey, l.proxy, l.ip, time.Now().UnixMilli())
		t.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		// A per-attempt deadline of "whatever is left" cuts a healthy lane
		// mid-read once earlier attempts spent the budget (measured 2026-11-17:
		// "read tcp ...: i/o timeout" on lane 9736 after a sweep). Give each
		// attempt a floor, and cap the ceiling so a stalled lane cannot eat the
		// whole turn.
		// Scale with the body: a 800 KB prompt needs the lane to upload it and
		// the model to start answering, which measured 10-12 s — a flat 10 s cap
		// cut it (2026-11-17: stand 502 "read tcp ...:9735: i/o timeout" while
		// the sidecar path answered 200 in 11.2 s). 10 s base + 1 s per 50 KB,
		// ceiling 60 s.
		perAttempt := remaining + 10*time.Second + time.Duration(len(body)/50_000)*time.Second
		if perAttempt < 3*time.Second {
			perAttempt = 3 * time.Second
		}
		if perAttempt > 60*time.Second {
			perAttempt = 60 * time.Second
		}

		// The 5th argument bounds dial + CONNECT + TLS: a lane that accepts TCP
		// and then stays silent must cost the sweep budget, not the body-scaled
		// response cap. Measured 2026-11-19: with `perAttempt` (up to 60 s) as
		// the dial budget a walled turn took 169 s (3 x 56 s) before the 429,
		// and every silent lane ate a whole sweep slot. The response phase keeps
		// its own generous deadline (t.responseTimeout).
		dialBudget := t.budget
		if remaining < dialBudget {
			dialBudget = remaining
		}
		resp, rerr := cliDo(req, body, t.upstreamAddr(), l.proxy, sid, dialBudget, t.responseTimeout, remaining)
		if rerr != nil {
			slog.Default().Debug("cli: attempt", "bytes", len(body), "lane", l.proxy, "err", rerr)
		} else {
			slog.Default().Debug("cli: attempt", "bytes", len(body), "lane", l.proxy, "status", resp.StatusCode)
		}
		if rerr != nil {
			// A lane that accepted the connection and then went silent (dial,
			// CONNECT, handshake or response timeout) must not be re-picked on
			// the very next request: park it for a real window, not the 8 s
			// sweep budget. Otherwise every turn eats the same 150 s timeout.
			parkMS := t.budget.Milliseconds()
			var nerr net.Error
			if errors.As(rerr, &nerr) && nerr.Timeout() {
				parkMS = 60_000
			}
			t.mark(l.proxy, false, parkMS)
			continue
		}
		if resp.StatusCode >= 500 {
			// A gate 5xx is a verdict about THAT lane, not about the request:
			// marking it healthy and returning it (the old behaviour) both
			// burned the lane's score and handed the owner a 503 they could not
			// influence. Park it like a timeout and try the next lane.
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
			_ = resp.Body.Close()
			// A big body's 5xx burns its own book: parking the normal key
			// would spend a warm lane on a request that never reached a warm
			// lane's allowance.
			t.mark(l.proxy, false, 60_000)
			lastStatus, lastBody = resp.StatusCode, b
			continue
		}
		if resp.StatusCode >= 400 && resp.StatusCode != http.StatusTooManyRequests {
			// A client-side error (400/404/413 …) is the same on every lane:
			// relay it as-is without touching the book.
			return resp, nil
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
			_ = resp.Body.Close()
			// A lane that admitted a 200 moments ago is not a burned egress: the
			// gate refuses the BODY (an echoed sentinel, a shape it will not
			// accept), not the address. Parking, redrawing or unpinning here
			// trades a proven lane for a handshake lottery — the failure mode
			// where every turn peels one lane and a body-shaped 429 empties the
			// fleet. Report the verdict unchanged, leave the book and the pin
			// intact; lanes with no recent 200 keep the park/redraw path below.
			t.mu.Lock()
			t.streak[l.proxy]++
			n429 := t.streak[l.proxy]
			t.mu.Unlock()
			if n429 < 2 && t.laneJustAdmitted(l.proxy, time.Now().UnixMilli()) {
				// The lane is not burned (no park, no redraw): the gate refused
				// this BODY. But refusing to walk on made one body verdict the
				// end of the turn even with the rest of the fleet up, so a big
				// body keeps sweeping — the next pick is pushed to an
				// untouched-today /64.
				if i+1 < attempts {
					slog.Default().Warn("cli: 429 on a lane that just admitted - body verdict, trying a fresh lane", "lane", l.proxy, "bytes", len(body), "attempt", i+1)
					preferFresh = true
					lastStatus, lastBody = resp.StatusCode, b
					continue
				}
				slog.Default().Warn("cli: 429 on a lane that just admitted - body verdict, not parked", "lane", l.proxy, "bytes", len(body))
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Status:     "429 Too Many Requests",
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader(b)),
					Request:    req,
				}, nil
			}
			// The gate answers FreeUsageLimitError for a session id it has never
			// seen (measured 2026-11-20: a db-minted id 200s, a generated one
			// always gets this body). It is a verdict about the LANE/SESSION
			// pair, not about the account's day, so drop the pin and let the
			// next lane bind a different harvested id.
			if isFreeUsageLimitError(b) {
				t.mu.Lock()
				t.unpin(convKey, l.proxy)
				t.mu.Unlock()
			}
			// A 429 for a body the gate never saw in OpenCode shape (no
			// canonical system head, <2 tools, stream:false — GUI health,
			// combo steps, hubmod probes, invalid JSON) is a verdict about
			// the BODY, not the egress. The pool transport already refuses to
			// blame an egress for a shape-less 403; the CLI sweep must do the
			// same for 429, or every unshaped request parks one lane for the
			// gate's hours-class Retry-After and the fleet dies lane by lane
			// (measured 2026-11-19: 65 lanes parked, waves of 60-170 s 429s).
			if !gateShapeOK(req.Context()) {
				slog.Default().Warn("cli: 429 from a body without gate shape — egress not parked", "lane", l.proxy, "bytes", len(body))
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Status:     "429 Too Many Requests",
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader(b)),
					Request:    req,
				}, nil
			}
			// Park = the window the gate names, and nothing else. Shortening it
			// (the old 4 h ceiling) re-tests a burned address; lengthening it
			// hides a lane that recovered. A redraw gives the slot a new /64
			// and clears this park once the slot answers (maybeRedrawLane).
			// The gate's Retry-After names ~2h, but the /64 window recovers in
			// ~2 minutes (measured 2026-11-21: same /64, fresh book, 200 at
			// +2min after a 429). Parking for the header value starved the
			// fleet 60x longer than the wall. Cap the 429 park at 3 minutes so
			// the lane returns and the sweep keeps supply; the burned-group
			// TTL follows the same park.
			// The gate's Retry-After is the only honest timer here, and a
			// day-scale one cannot be outrun by a redraw. Measured 2026-11-22
			// 15:00: five virgin node exits (US/NL), the WARP fleet and the
			// home IP all named the SAME daily reset (~00:00Z, 11 h left), and
			// every fresh WARP registration was refused too. Redrawing on that
			// 429 stormed one registration every ~8 s for hours with zero
			// 200s (log 14:40-15:07: 6-10 "lane redrawn" per minute, 0 OK).
			// The gateway names a day-scale window on EVERY 429 (measured
			// 2026-11-22 17:09: 10-11 h left, same instant on unrelated /64s),
			// and that number does not tell a fresh prefix from a spent one.
			// Waiting it out is what made rotation look dead: the old pool-wall
			// gate parked redraw/rotate until the named instant and answered
			// every later request from memory, so only a process restart (which
			// forgot the wall) brought requests back. The one lever that buys
			// supply is a NEW /64 — a redraw re-handshakes the slot in ~4 s
			// (measured 2026-11-22: 12 registrations -> 9 distinct /64s, ~75%
			// new on each draw) — so park briefly and rotate the burned lane at
			// once instead of trusting the header.
			park := int64(60_000)
			preferFresh = true
			t.mark(l.proxy, false, park)
			// Cooldown-free: this lane just proved its /64 is spent. The brake
			// exists to stop churn, not to keep a burned address in the sweep.
			t.redrawLane(l.proxy, true)
			t.mu.Lock()
			t.unpin(convKey, l.proxy)
			t.mu.Unlock()
			lastStatus, lastBody = resp.StatusCode, b
			continue
		}
		t.mark(l.proxy, true, 0)
		t.mu.Lock()
		delete(t.streak, l.proxy)
		t.mu.Unlock()
		t.saveBook()
		if resp.Body != nil {
			resp.Body = t.trackLaneBody(l.proxy, newToolArgRewriter(resp.Body, resp.Header.Get("Content-Type")))
		}
		return resp, nil
	}
	// Every lane refused: a burned fleet is re-registered (new WARP address per
	// worker) instead of only parked. The CLI transport had NO rotation at all,
	// so a burned fleet sat parked for the full 4 h window and every turn was a
	// 429 until someone restarted warp-router by hand.
	if lastStatus == http.StatusTooManyRequests {
		t.rotateFleet("lane sweep exhausted")
	}
	// Every lane refused or errored. The fleet is a set of tunnel ADDRESSES, and
	// the gate can refuse an address wholesale while the very same session
	// answers 200 from the host's own line (measured 2026-11-20: 15/15 WARP
	// lanes 429 on a db-minted session the local CLI served fine direct). The
	// last word therefore belongs to a direct attempt on the home egress.
	if req.Context().Err() == nil && lastStatus >= 400 {
		t.mu.Lock()
		sid := t.bind(convKey, "", "", time.Now().UnixMilli())
		t.mu.Unlock()
		resp, rerr := cliDo(req, body, t.upstreamAddr(), "", sid, t.budget, t.responseTimeout, t.budget)
		if rerr != nil {
			slog.Default().Debug("cli: direct fallback", "err", rerr)
		} else {
			slog.Default().Debug("cli: direct fallback", "bytes", len(body), "status", resp.StatusCode)
			if resp.StatusCode < 400 {
				t.mark("", true, 0)
				t.saveBook()
				if resp.Body != nil {
					resp.Body = newToolArgRewriter(resp.Body, resp.Header.Get("Content-Type"))
				}
				return resp, nil
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
			_ = resp.Body.Close()
			lastStatus, lastBody = resp.StatusCode, b
		}
	}
	t.saveBook()
	if lastStatus == http.StatusTooManyRequests {
		dumpFailed429(body)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(lastBody)),
			Request:    req,
		}, nil
	}
	// Every lane refused or errored: that is a rate-limit verdict, not a broken
	// upstream. Returning 429 lets the router retry on a fresh sweep (its
	// attempts budget) instead of surfacing a 5xx to the client — 502s were
	// landing on the user for turns the next sweep served in 2 s.
	// A lane 5xx is as much a verdict about the egress as a 429 is, and the
	// owner must never receive it: report the wall and let the router retry.
	if lastStatus == 0 || lastStatus >= 500 {
		lastStatus = http.StatusTooManyRequests
	}
	return &http.Response{
		StatusCode: lastStatus,
		Status:     fmt.Sprintf("%d %s", lastStatus, http.StatusText(lastStatus)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"every tried egress refused (free usage limit)"}}`))),
		Request:    req,
	}, nil
}

// cliDo writes one attempt: uTLS CLI hello through the lane proxy, raw request
// head, body, raw response parse (chunked or Content-Length).
func cliDo(req *http.Request, body []byte, upstream, proxy, sid string, budget, responseTimeout, headTimeout time.Duration) (*http.Response, error) {
	return cliDoUA(req, body, upstream, proxy, sid, budget, responseTimeout, headTimeout, activeBypassUA())
}

// cliDoUA is cliDo with an explicit User-Agent: the manual bypass sweep tests
// UA variants on a fresh egress without touching the live override.
func cliDoUA(req *http.Request, body []byte, upstream, proxy, sid string, budget, responseTimeout, headTimeout time.Duration, ua string) (*http.Response, error) {
	path := req.URL.Path
	if path == "" {
		path = "/inference/v1/chat/completions"
	}
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}
	msg := mintCLIID("msg_", sid, time.Now().UnixMilli())
	conn, err := dialCLITLS(proxy, upstream, budget)
	if err != nil {
		return nil, err
	}
	// The budget must bound the WHOLE attempt: a half-dead lane accepts TCP and
	// then stays silent through CONNECT, the TLS handshake or the response head,
	// which used to hold the request far past the sweep budget (measured
	// 2026-11-17: the stand answered 000 after 60 s while the budget was 8 s).
	// Tight deadline covers dial + CONNECT + TLS handshake (a half-dead lane must
	// not hang the sweep). The response phase is different: a 180k-token prompt
	// needs the lane to upload ~700 KB and the model to think, and the sidecar
	// path answers such a turn in 20-70 s. A body-scaled cap here cut real
	// traffic (measured 2026-11-17: 502 "read tcp ...:9735: i/o timeout" on
	// 181k-token prompts). Give the exchange a generous ceiling instead; the
	// router's own request timeout still bounds the turn.
	if responseTimeout <= 0 {
		responseTimeout = 150 * time.Second
	}
	// Phase deadlines, NOT one absolute deadline on the connection. A single
	// SetDeadline(responseTimeout) severed every stream older than 150 s mid-sentence
	// (the client then loses finish_reason=tool_calls and the following tool calls
	// never run). Write phase is body-scaled; the read phase is an IDLE deadline
	// refreshed before every read (see cliLimitedReader/cliChunkedReader).
	writeBudget := 8*time.Second + time.Duration(len(body)/50_000)*time.Second
	if writeBudget > 120*time.Second {
		writeBudget = 120 * time.Second
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeBudget))
	head := fmt.Sprintf("POST %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"accept: */*\r\n"+
		"accept-encoding: gzip, deflate, br, zstd\r\n"+
		"authorization: Bearer public\r\n"+
		"content-type: application/json\r\n"+
		"user-agent: %s\r\n"+
		"x-opencode-client: cli\r\n"+
		"x-opencode-project: global\r\n"+
		"x-opencode-request: %s\r\n"+
		"x-opencode-session: %s\r\n"+
		"x-opencode-session-id: %s\r\n"+
		"content-length: %d\r\n\r\n",
		path, upstream, ua, msg, sid, sid, len(body))
	if os.Getenv("AI_ROUTER_CLI_DUMP") == "1" {
		_ = os.WriteFile(filepath.Join(os.Getenv("TEMP"), "ai_router_cli_raw.txt"), append([]byte(head), body...), 0o644)
	}
	// Per-request MITM capture: the body that actually left the router plus the
	// raw upstream stream, one file pair per attempt. AI_ROUTER_DUMP_BODIES keeps
	// the inbound body, so a turn can be diffed (tools, messages, reasoning) and
	// the upstream's own terminal frame inspected without TLS interception.
	mitmBase := ""
	if dir := strings.TrimSpace(os.Getenv("AI_ROUTER_MITM_DIR")); dir != "" {
		if os.MkdirAll(dir, 0o755) == nil {
			mitmBase = filepath.Join(dir, fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102-150405.000"), msg))
			_ = os.WriteFile(mitmBase+".req", append([]byte(head), body...), 0o644)
		}
	}
	if _, werr := conn.Write([]byte(head)); werr != nil {
		conn.Close()
		return nil, werr
	}
	if _, werr := conn.Write(body); werr != nil {
		conn.Close()
		return nil, werr
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	// The HEAD wait is short on purpose: a lane that accepts TCP and then says
	// nothing is a dead end, and with the 150 s response ceiling one such lane
	// ate the whole sweep (measured 2026-11-23 15:12: three 780 KB-1.38 MB turns
	// sat 180 s and the router gave 502). Once headers arrive the body readers
	// refresh their own idle deadline, so long streams are untouched.
	headWait := headTimeout
	if headWait <= 0 || headWait > 20*time.Second {
		headWait = 20 * time.Second
	}
	if headWait > responseTimeout {
		headWait = responseTimeout
	}
	_ = conn.SetReadDeadline(time.Now().Add(headWait))
	line, rerr := br.ReadString('\n')
	if rerr != nil {
		conn.Close()
		return nil, rerr
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		conn.Close()
		return nil, fmt.Errorf("cli: bad status line %q", strings.TrimSpace(line))
	}
	status, _ := strconv.Atoi(fields[1])
	hdr := http.Header{}
	chunked := false
	contentLength := int64(-1)
	for {
		l, herr := br.ReadString('\n')
		if herr != nil || l == "\r\n" || l == "\n" {
			break
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		lk := strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch lk {
		case "transfer-encoding":
			chunked = strings.Contains(strings.ToLower(v), "chunked")
			continue
		case "content-length":
			if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
				contentLength = n
			}
			continue
		case "connection", "content-encoding", "set-cookie", "alt-svc", "nel", "report-to":
			continue
		}
		hdr.Set(k, v)
	}
	var rc io.ReadCloser
	if chunked {
		rc = &cliChunkedReader{r: br, c: conn, idle: responseTimeout}
	} else {
		rc = &cliLimitedReader{r: br, c: conn, n: contentLength, idle: responseTimeout}
	}
	if mitmBase != "" {
		rc = &mitmTeeReader{rc: rc, path: mitmBase + ".sse"}
	}
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        hdr,
		Body:          rc,
		ContentLength: contentLength,
		Request:       req,
	}, nil
}

// mitmTeeReader copies every byte the upstream sent to a file while the
// passthrough consumes it. Failure to open the dump never disturbs the stream:
// diagnostics must not break an answer.
type mitmTeeReader struct {
	rc   io.ReadCloser
	path string
	f    *os.File
}

func (m *mitmTeeReader) Read(p []byte) (int, error) {
	n, err := m.rc.Read(p)
	if n > 0 {
		if m.f == nil {
			m.f, _ = os.Create(m.path)
		}
		if m.f != nil {
			_, _ = m.f.Write(p[:n])
		}
	}
	return n, err
}

func (m *mitmTeeReader) Close() error {
	if m.f != nil {
		_ = m.f.Close()
	}
	return m.rc.Close()
}

// cliLimitedReader serves Content-Length bodies (or the rest of the connection).
type cliLimitedReader struct {
	r    io.Reader
	c    net.Conn
	n    int64
	idle time.Duration
}

func (l *cliLimitedReader) Read(p []byte) (int, error) {
	// Idle deadline per read: the stream may live for hours as long as bytes keep
	// coming, but silence longer than the idle window is a dead upstream.
	if l.idle > 0 {
		_ = l.c.SetReadDeadline(time.Now().Add(l.idle))
	}
	if l.n == 0 {
		return 0, io.EOF
	}
	if l.n > 0 && int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	if l.n > 0 {
		l.n -= int64(n)
	}
	return n, err
}

func (l *cliLimitedReader) Close() error { return l.c.Close() }

// cliChunkedReader decodes a chunked body and closes the connection.
type cliChunkedReader struct {
	r    io.Reader
	c    net.Conn
	n    int64
	idle time.Duration
	// pending carries an error hit while consuming the chunk trailer (the
	// inter-chunk CRLF, where the idle deadline usually fires). Swallowing it
	// turned an idle-timeout mid-stream into a clean EOF: the client then saw a
	// truncated answer with no finish_reason instead of a retryable error.
	pending error
}

func (ch *cliChunkedReader) Read(p []byte) (int, error) {
	if ch.idle > 0 {
		_ = ch.c.SetReadDeadline(time.Now().Add(ch.idle))
	}
	if ch.n == 0 {
		if ch.pending != nil {
			e := ch.pending
			ch.pending = nil
			return 0, e
		}
		line, err := readLine(ch.r)
		if err != nil {
			return 0, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return 0, io.EOF
		}
		size, perr := strconv.ParseInt(line, 16, 64)
		if perr != nil {
			return 0, perr
		}
		if size == 0 {
			return 0, io.EOF
		}
		ch.n = size
	}
	if int64(len(p)) > ch.n {
		p = p[:ch.n]
	}
	n, err := ch.r.Read(p)
	ch.n -= int64(n)
	if ch.n == 0 {
		if _, terr := readLine(ch.r); terr != nil {
			ch.pending = terr // datalen ok, but the boundary is dead: report on the NEXT read
		}
	}
	if n > 0 {
		return n, nil // bytes first; a pending boundary error is surfaced next call
	}
	if err != nil {
		return 0, err
	}
	if ch.pending != nil {
		e := ch.pending
		ch.pending = nil
		return 0, e
	}
	return n, err
}

func (ch *cliChunkedReader) Close() error { return ch.c.Close() }

func readLine(r io.Reader) (string, error) {
	var b strings.Builder
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return b.String(), nil
			}
			b.WriteByte(one[0])
		}
		if err != nil {
			return b.String(), err
		}
	}
}

// dialCLITLS opens a uTLS connection with the CLI hello through an HTTP CONNECT
// proxy (the pool egress).
func dialCLITLS(proxy, addr string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	target := addr
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil && u.Host != "" {
			target = u.Host
		}
	}
	raw, err := d.Dial("tcp", target)
	if err != nil {
		return nil, err
	}
	_ = raw.SetDeadline(time.Now().Add(timeout))
	if proxy != "" && target != addr {
		if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr); err != nil {
			raw.Close()
			return nil, err
		}
		br := bufio.NewReader(raw)
		line, err := br.ReadString('\n')
		if err != nil {
			raw.Close()
			return nil, err
		}
		if !strings.Contains(line, "200") {
			raw.Close()
			return nil, fmt.Errorf("proxy CONNECT: %s", strings.TrimSpace(line))
		}
		for {
			l, err := br.ReadString('\n')
			if err != nil || l == "\r\n" || l == "\n" {
				break
			}
		}
		raw = &cliBufferedConn{Conn: raw, r: br}
	}
	host, _, _ := net.SplitHostPort(addr)
	// Hello capture seam: the parity A/B needs the client's ClientHello bytes
	// to diff against the sidecar's; uTLS writes them through the conn.
	var rec *cliRecorder
	if os.Getenv("AI_ROUTER_CLI_HELLO_DUMP") == "1" {
		rec = &cliRecorder{Conn: raw}
		raw = rec
	}
	c := utls.UClient(raw, &utls.Config{ServerName: host, InsecureSkipVerify: cliInsecureSkipVerify}, utls.HelloCustom)
	if err := c.ApplyPreset(cliHelloSpec()); err != nil {
		raw.Close()
		return nil, err
	}
	if err := c.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	if rec != nil {
		_ = os.WriteFile(filepath.Join(os.Getenv("TEMP"), "ai_router_cli_hello.bin"), rec.buf.Bytes(), 0o644)
	}
	return c, nil
}

// cliRecorder tees the client's handshake writes so the ClientHello can be
// compared byte-for-byte with the sidecar's.
type cliRecorder struct {
	net.Conn
	buf bytes.Buffer
}

func (r *cliRecorder) Write(b []byte) (int, error) {
	r.buf.Write(b)
	return r.Conn.Write(b)
}

type cliBufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *cliBufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// mintCLIID reproduces the bun mintSession: ~(ms*0x1000+counter) as 12 hex plus
// 14 base62 chars seeded by FNV-1a(seedKey|now). The gate validates the clock
// half, so both halves must match the CLI bit for bit.
func mintCLIID(prefix, seedKey string, nowMs int64) string {
	h := uint32(2166136261)
	seed := seedKey + "|" + strconv.FormatInt(nowMs, 10)
	for i := 0; i < len(seed); i++ {
		h ^= uint32(seed[i])
		h *= 16777619
	}
	counter := uint64(1 + h%16)
	value := ^(uint64(nowMs)*0x1000 + counter)
	timeHex := fmt.Sprintf("%012x", value&0xFFFFFFFFFFFF)
	x := h
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var b [14]byte
	for i := range b {
		x = uint32(x*1664525 + 1013904223)
		b[i] = alphabet[x%62]
	}
	return prefix + timeHex + string(b[:])
}

// ------------------------------------------------------------ tool schemas --

// The OMP `edit` argument names the gate refuses and their replacements live
// in the active tool kit (bypass.go): the sidecar measured the rename to be
// the difference between 429 and 200, and the manual bypass sweep enumerates
// alternate kits. Default kit = camel (oldString/newString).

// renameForbiddenProps renames those properties (and their `required` entries)
// in every tool schema. The response side renames the arguments back.
func renameForbiddenProps(body []byte) ([]byte, bool) {
	return renameForbiddenPropsWith(body, activeToolKit())
}

func renameForbiddenPropsWith(body []byte, kit *toolKit) ([]byte, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return body, false
	}
	raw, ok := doc["tools"]
	if !ok {
		return body, false
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil || len(tools) == 0 {
		return body, false
	}
	changed := false
	for i := range tools {
		fnRaw, ok := tools[i]["function"]
		if !ok {
			continue
		}
		var fn map[string]json.RawMessage
		if json.Unmarshal(fnRaw, &fn) != nil {
			continue
		}
		paramsRaw, ok := fn["parameters"]
		if !ok {
			continue
		}
		var params map[string]json.RawMessage
		if json.Unmarshal(paramsRaw, &params) != nil {
			continue
		}
		if propsRaw, ok := params["properties"]; ok {
			var props map[string]json.RawMessage
			if json.Unmarshal(propsRaw, &props) == nil {
				renamed := make(map[string]json.RawMessage, len(props))
				touched := false
				for k, v := range props {
					if nk, hit := kit.Schema[k]; hit {
						renamed[nk] = v
						touched = true
						continue
					}
					renamed[k] = v
				}
				if touched {
					if nb, err := json.Marshal(renamed); err == nil {
						params["properties"] = nb
						changed = true
					}
				}
			}
		}
		if reqRaw, ok := params["required"]; ok {
			var req []string
			if json.Unmarshal(reqRaw, &req) == nil {
				touched := false
				for j, r := range req {
					if nr, hit := kit.Schema[r]; hit {
						req[j] = nr
						touched = true
					}
				}
				if touched {
					if nb, err := json.Marshal(req); err == nil {
						params["required"] = nb
						changed = true
					}
				}
			}
		}
		if nb, err := json.Marshal(params); err == nil {
			fn["parameters"] = nb
		}
		if nb, err := json.Marshal(fn); err == nil {
			tools[i]["function"] = nb
		}
	}
	if !changed {
		return body, false
	}
	nb, err := json.Marshal(tools)
	if err != nil {
		return body, false
	}
	doc["tools"] = nb
	out, err := json.Marshal(doc)
	if err != nil {
		return body, false
	}
	return out, true
}

// renameArgsStream rewrites complete occurrences of the kit's reverse pairs
// and returns a tail that may be the beginning of a pattern, so a key split
// across two SSE fragments is still renamed. Nothing else is buffered:
// fragments reach the client as they arrive.
func renameArgsStream(buf []byte, kit *toolKit) (out, rest []byte) {
	out = append([]byte(nil), buf...)
	for _, p := range kit.Pairs {
		out = bytes.ReplaceAll(out, []byte(p[0]), []byte(p[1]))
	}
	keep := 0
	for _, p := range kit.Pairs {
		pat := []byte(p[0])
		m := len(pat) - 1
		if m > len(out) {
			m = len(out)
		}
		for k := m; k > keep; k-- {
			if bytes.HasSuffix(out, pat[:k]) {
				keep = k
				break
			}
		}
	}
	if keep > 0 {
		return out[:len(out)-keep], out[len(out)-keep:]
	}
	return out, nil
}

// sseArgRewriter renames oldString/newString back inside streamed tool-call
// arguments. The arguments arrive as text fragments, so they are buffered per
// tool-call index and re-emitted as one chunk with the renamed properties —
// otherwise the model's oldString/newString reach the client and OMP's edit
// validator rejects the call ("old_string must be a string (was missing)").
type sseArgRewriter struct {
	r       io.ReadCloser
	br      *bufio.Reader
	kit     *toolKit
	out     bytes.Buffer
	pending map[int]*bytes.Buffer
	order   []int
	done    bool
	// err keeps the underlying read error (idle timeout, reset) instead of
	// reporting it as io.EOF. Swallowing it made a dead upstream look like a
	// finished answer: the client got a truncated stream with no finish_reason
	// and therefore never ran the pending tool calls.
	err error
}

func newSSEArgRewriter(r io.ReadCloser) *sseArgRewriter {
	return &sseArgRewriter{r: r, br: bufio.NewReaderSize(r, 64<<10), kit: activeToolKit(), pending: map[int]*bytes.Buffer{}}
}

func (t *sseArgRewriter) Read(p []byte) (int, error) {
	for t.out.Len() == 0 {
		if t.done {
			if t.err != nil && t.err != io.EOF {
				e := t.err
				t.err = nil
				return 0, e
			}
			return 0, io.EOF
		}
		line, err := t.br.ReadBytes('\n')
		if len(line) > 0 {
			t.feed(line)
		}
		if err != nil {
			t.flushAll()
			t.done = true
			t.err = err
			if t.out.Len() == 0 {
				if err != io.EOF {
					t.err = nil
					return 0, err
				}
				return 0, io.EOF
			}
		}
	}
	return t.out.Read(p)
}

func (t *sseArgRewriter) Close() error { return t.r.Close() }

func (t *sseArgRewriter) write(b []byte) {
	if len(b) > 0 {
		t.out.Write(b)
	}
}

func (t *sseArgRewriter) feed(line []byte) {
	idx := bytes.Index(line, []byte("data:"))
	if idx < 0 {
		t.write(line)
		return
	}
	payload := bytes.TrimLeft(bytes.TrimRight(line[idx+5:], "\r\n"), " \t")
	if len(payload) == 0 || payload[0] != '{' {
		t.write(line)
		return
	}
	var chunk map[string]any
	if json.Unmarshal(payload, &chunk) != nil {
		t.write(line)
		return
	}
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.write(line)
		return
	}
	touched := false
	var finishReason any
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if fr, ok := cm["finish_reason"]; ok && fr != nil && fr != "" {
			finishReason = fr
			delete(cm, "finish_reason")
		}
		delta, ok := cm["delta"].(map[string]any)
		if !ok {
			continue
		}
		calls, ok := delta["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, call := range calls {
			callMap, ok := call.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := callMap["function"].(map[string]any)
			if !ok {
				continue
			}
			frag, ok := fn["arguments"].(string)
			if !ok || frag == "" {
				continue
			}
			index := 0
			if v, ok := callMap["index"].(float64); ok {
				index = int(v)
			}
			buf := t.pending[index]
			if buf == nil {
				buf = &bytes.Buffer{}
				t.pending[index] = buf
				t.order = append(t.order, index)
			}
			buf.WriteString(frag)
			src := buf.Bytes()
			out, rest := renameArgsStream(src, t.kit)
			buf.Reset()
			if len(rest) > 0 {
				_, _ = buf.Write(rest)
			}
			if len(out) > 0 {
				fn["arguments"] = string(out)
			} else {
				delete(fn, "arguments")
			}
			// Always relay the chunk: a first fragment is often tiny and fully
			// carried, and that chunk is the one that carries the tool NAME and
			// id. Dropping it (the 2026-11-20 regression) left the client with
			// a nameless call and empty arguments.
			touched = true
		}
	}
	// A chunk that carried finish_reason was stripped of it above; its content
	// (or tool-call deltas) must still reach the client. The upstream batches
	// the last content delta together with finish_reason, and dropping the
	// whole chunk cut every long text answer mid-word (2026-11-23).
	if touched || finishReason != nil {
		if nb, err := json.Marshal(chunk); err == nil {
			// "data: " with the space: the SSE passthrough only recognises
			// that exact prefix (stream.go), and a rewritten chunk without it
			// is dropped on the floor — tool arguments vanished and the
			// client received the "{}" backfill instead.
			t.write(append([]byte("data: "), append(nb, '\n')...))
		}
	}
	if finishReason != nil {
		t.flushAll()
		fin := map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": finishReason,
		}}}
		if nb, err := json.Marshal(fin); err == nil {
			t.write(append([]byte("data: "), append(nb, '\n')...))
		}
		return
	}
	if touched {
		return
	}
	t.write(line)
}

func (t *sseArgRewriter) flushIndex(index int) {
	buf := t.pending[index]
	if buf == nil || buf.Len() == 0 {
		return
	}
	// Already renamed fragment-wise; a dangling partial pattern cannot be
	// completed at end of stream, so the carry ships as-is.
	args := append([]byte(nil), buf.Bytes()...)
	buf.Reset()
	chunk := map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    index,
					"function": map[string]any{"arguments": string(args)},
				}},
			},
		}},
	}
	if nb, err := json.Marshal(chunk); err == nil {
		t.write(append([]byte("data: "), append(nb, '\n')...))
	}
}

func (t *sseArgRewriter) flushAll() {
	for _, index := range t.order {
		t.flushIndex(index)
	}
	t.pending = map[int]*bytes.Buffer{}
	t.order = nil
}

// toolArgRewriter wraps a response body: non-streaming JSON is rewritten whole,
// SSE is rewritten per tool-call argument fragment by sseArgRewriter.
type toolArgRewriter struct {
	r   io.ReadCloser
	sse *sseArgRewriter
	buf bytes.Buffer
	got bool
}

func newToolArgRewriter(r io.ReadCloser, contentType string) io.ReadCloser {
	if strings.Contains(contentType, "event-stream") {
		return newSSEArgRewriter(r)
	}
	return &toolArgRewriter{r: r}
}

func (t *toolArgRewriter) Read(p []byte) (int, error) {
	if !t.got {
		t.got = true
		body, err := io.ReadAll(t.r)
		if err != nil {
			return 0, err
		}
		t.buf.Write(renameToolCallArgs(body))
	}
	if t.buf.Len() == 0 {
		return 0, io.EOF
	}
	return t.buf.Read(p)
}

func (t *toolArgRewriter) Close() error { return t.r.Close() }

// renameToolCallArgs rewrites oldString/newString inside
// choices[].message.tool_calls[].function.arguments (a JSON string), leaving
// everything else byte-identical.
func renameToolCallArgs(body []byte) []byte {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	choicesRaw, ok := doc["choices"]
	if !ok {
		return body
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(choicesRaw, &choices) != nil {
		return body
	}
	for _, ch := range choices {
		msgRaw, ok := ch["message"]
		if !ok {
			continue
		}
		var msg map[string]json.RawMessage
		if json.Unmarshal(msgRaw, &msg) != nil {
			continue
		}
		callsRaw, ok := msg["tool_calls"]
		if !ok {
			continue
		}
		var calls []map[string]json.RawMessage
		if json.Unmarshal(callsRaw, &calls) != nil {
			continue
		}
		for _, call := range calls {
			fnRaw, ok := call["function"]
			if !ok {
				continue
			}
			var fn map[string]json.RawMessage
			if json.Unmarshal(fnRaw, &fn) != nil {
				continue
			}
			argsRaw, ok := fn["arguments"]
			if !ok {
				continue
			}
			var argsStr string
			if json.Unmarshal(argsRaw, &argsStr) != nil {
				continue
			}
			var args map[string]json.RawMessage
			if json.Unmarshal([]byte(argsStr), &args) != nil {
				continue
			}
			touched := false
			for from, to := range activeToolKit().Reverse {
				if v, ok := args[from]; ok {
					args[to] = v
					delete(args, from)
					touched = true
				}
			}
			if touched {
				if nb, err := json.Marshal(args); err == nil {
					if eb, err := json.Marshal(string(nb)); err == nil {
						fn["arguments"] = eb
						if fb, err := json.Marshal(fn); err == nil {
							call["function"] = fb
						}
					}
				}
			}
		}
		if cb, err := json.Marshal(calls); err == nil {
			msg["tool_calls"] = cb
			if mb, err := json.Marshal(msg); err == nil {
				ch["message"] = mb
			}
		}
	}
	if cb, err := json.Marshal(choices); err == nil {
		doc["choices"] = cb
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// envOr returns the environment value when set, else the default. Used by the
// stand to point at an isolated wide fleet without touching the live files.
func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// isFreeUsageLimitError reports the gate's free-tier refusal body. The gate
// answers it with a day-scale Retry-After for a session id it does not know
// (measured 2026-11-20: a db-minted id 200s from the egress that minted it,
// every generated id gets this body) — so the sweep treats it as a verdict
// about the lane/session pair, never as an account-wide budget.
func isFreeUsageLimitError(body []byte) bool {
	return bytes.Contains(body, []byte("FreeUsageLimitError")) ||
		bytes.Contains(body, []byte("Free usage limit"))
}

// rotateFleet re-registers every WARP device through the pool API, so each
// worker comes back on a NEW address. That is the only way a burned fleet gets
// fresh upstream windows: the pool transport has done this for months
// (maybeRotatePool / forceRotatePool), but the CLI transport — the one the live
// config uses — never did, so a burned fleet just sat parked for the 4 h park
// cap and every turn answered 429 until warp-router was restarted by hand.
//
// Two things make a rotate useless if it is done alone, and both are handled
// here: the lane cache (60 s) would keep serving the old addresses, and the book
// is keyed by tunnel port, so every park earned by the old address would follow
// the port onto the new one — the fleet rotates and the sweep still skips every
// lane as "burned". Parks are therefore lifted for the whole book on success.
//
// Throttled to one rotate per 15 min: a rotate respawns the fleet for 1-2 min,
// and the measured failure rate in the minute after a rotate is 27.9% vs 7-20%
// otherwise (2026-10-18: 14 rotates in 5 h).
func (t *CLITransport) rotateFleet(reason string) {
	const minInterval = 15 * time.Minute
	last := t.lastRotate.Load()
	now := time.Now()
	if last != 0 && now.Sub(time.Unix(0, last)) < minInterval {
		return
	}
	// A fleet rotate respawns all 20 slots: every in-flight answer dies. Wait
	// for the streams to end and rotate right after, instead of cutting them.
	t.mu.Lock()
	if !t.idleLocked() {
		t.pendingRotate = reason
		t.mu.Unlock()
		slog.Default().Info("cli: fleet rotate deferred until active streams finish", "reason", reason)
		return
	}
	t.mu.Unlock()
	//
	// But a fleet that still serves must not be re-rolled: /pool/rotate is a
	// rolling restart of EVERY slot, so with 21 lanes answering 200s (two
	// workhorses at 101/58 OK) a rotate trades working addresses for a
	// handshake lottery. Burned slots expire by their own parks (the named Retry-After) and a
	// per-slot redraw replaces them; the fleet-wide rotate is reserved for the
	// moment NOTHING served recently.
	t.mu.Lock()
	recentOK := int64(0)
	for _, st := range t.book {
		if st != nil && st.LastOK > recentOK {
			recentOK = st.LastOK
		}
	}
	t.mu.Unlock()
	// The book row is wiped when a lane changes address (lanesNow resets the
	// reputation on a new IP), so the fleet-wide last success is the honest
	// signal here. Measured 2026-11-21 06:22: 17 redraws blanked LastOK and
	// the rotate fired 6.4 min after a real 200 — which was the CORRECT call
	// (a fleet-wide wall with zero 200s until the rotate). Window is 5 min,
	// not 10: a 10-min gate would have delayed that recovery by ~3 min.
	if g := lastLaneSuccessUnix.Load(); g*1000 > recentOK {
		recentOK = g * 1000
	}
	if recentOK > 0 {
		if age := now.Sub(time.UnixMilli(recentOK)); age < 5*time.Minute {
			slog.Default().Info("cli: rotate skipped, lanes still serving",
				"reason", reason, "last_ok_s_ago", int(age.Seconds()))
			return
		}
	}
	if !t.lastRotate.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	u := strings.TrimSuffix(t.apiURL, "/") + "/api/v1/pool/rotate"
	tok := t.token
	go func() {
		req, err := http.NewRequest(http.MethodPost, u, nil)
		if err != nil {
			t.lastRotate.Store(last)
			return
		}
		if tok != "" {
			req.Header.Set("authorization", "Bearer "+tok)
		}
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			slog.Default().Warn("cli: pool rotate failed", "err", err, "reason", reason)
			t.lastRotate.Store(last)
			return
		}
		defer resp.Body.Close()
		var out struct {
			Respawned int `json:"respawned"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<14)).Decode(&out)
		if resp.StatusCode >= 300 {
			slog.Default().Warn("cli: pool rotate rejected", "status", resp.StatusCode, "reason", reason)
			t.lastRotate.Store(last)
			return
		}
		slog.Default().Warn("cli: pool rotated", "status", resp.StatusCode, "respawned", out.Respawned, "reason", reason)
		t.mu.Lock()
		for _, st := range t.book {
			if st != nil {
				st.Until = 0
			}
		}
		t.fetched = time.Time{}
		t.lanes = nil
		t.mu.Unlock()
		t.saveBook()
	}()
}

// parkMS translates the gate's Retry-After into the lane's cooldown: exactly
// the window the upstream named (a day-scale value is a real day window, not
// something to shorten), 5 s when the header is missing — just enough to move
// the sweep to another lane for this turn.
func parkMS(ra string) int64 {
	if n, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && n > 0 {
		return int64(n) * 1000
	}
	return 5000
}

// parkRedrawLoop keeps the pool from silently shrinking: a lane parked for
// hours (a day-scale Retry-After) is a dead lane, and waiting for the next 429
// to trigger a redraw lets the fleet rot down to the few lanes that still
// answer - the state where only a manual restart helps. Every 5 min it redraws
// up to maxParkedRedraws lanes whose park is still >30 min away;
// maybeRedrawLane keeps the per-lane cooldown and the daemon-pool filter.
// Wide/node lanes have no slot endpoint and are left to wide_repair.
func (t *CLITransport) parkRedrawLoop() {
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	t.redrawLongParked(3)
	for range tick.C {
		t.redrawLongParked(3)
	}
}

// redrawLongParked redraws up to max lanes parked for more than 30 min.
func (t *CLITransport) redrawLongParked(max int) {
	if max <= 0 {
		return
	}
	now := time.Now().UnixMilli()
	t.mu.Lock()
	// The BIG book is the thin one: a >300 KB turn needs a lane that admitted
	// one recently, and each failed big turn parks two of them (two attempts).
	// Replace big-parked slots first, then the plain-parked ones; a slot has
	// one address, so a redraw covers both rows.
	bigParked, plainParked := map[string]bool{}, map[string]bool{}
	for k, st := range t.book {
		if st == nil || st.Until-now <= 30*60_000 {
			continue
		}
		proxy := strings.TrimSuffix(k, "|big")
		if port := lanePort(proxy); port < 8750 || port > 8769 {
			continue
		}
		if strings.HasSuffix(k, "|big") {
			bigParked[proxy] = true
		} else {
			plainParked[proxy] = true
		}
	}
	t.mu.Unlock()
	i := 0
	for _, set := range []map[string]bool{bigParked, plainParked} {
		for proxy := range set {
			if i >= max {
				return
			}
			i++
			t.maybeRedrawLane(proxy)
		}
	}
}

// laneRedrawMu serializes /pool/redraw calls from this process. warp-router's
// RedrawOne takes a TryLock (rotateMu) and answers 409 ErrRedrawBusy when a
// redraw is already running, so a parallel batch (redrawLongParked hands up to
// 3 lanes at once) rejects its own neighbours. Measured 2026-11-21: 139 of 300
// redraw attempts came back 409 for exactly this reason.
var laneRedrawMu sync.Mutex

// startParkRedraw starts the loop once per instance (the route group creates
// one CLI transport per provider, all sharing the same book file).
func (t *CLITransport) startParkRedraw() {
	if _, loaded := parkRedrawStarted.LoadOrStore(t.instancePort, struct{}{}); loaded {
		return
	}
	go t.parkRedrawLoop()
}

var parkRedrawStarted sync.Map

// lanePort extracts the local port from a lane proxy ("http://127.0.0.1:8750").
func lanePort(proxy string) int {
	i := strings.LastIndex(proxy, ":")
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(proxy[i+1:])
	return n
}

// maybeRedrawLane replaces a burned lane at the source: warp-router restarts
// ONE pool slot, keeps its account and the handshake draws a new /64 (4 s,
// neighbours untouched). Only the daemon pool (8750-8769) has slot endpoints;
// node/static lanes are left to their parks. The slot's old park is cleared
// only after the redraw answer, so a failed call never hands a spent address
// back to the sweep.
// redrawCooldownFor throttles how often ONE lane is re-drawn. The brake
// exists because a redraw restarts the slot (new WARP handshake) and burns a
// fresh /64: during a fleet-wide wall every new address is refused too, so a
// fast loop would only churn registrations.
//
// "The fleet serves" is a SHARE, not a single global 200: measured 2026-11-21
// 14:06-14:12 the book had main_fresh10m=2/20 while lastLaneSuccessUnix was
// fresh, so the old single-signal rule kept the 2-minute branch and the pool
// redrew a lane every 10-20 s in a continuous churn. A fleet where only a
// handful of groups answer is walled for the rest.
func (t *CLITransport) redrawCooldownFor(now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.redrawCooldownLocked(now)
}

// redrawCooldownLocked is redrawCooldownFor for callers that already hold t.mu.
func (t *CLITransport) redrawCooldownLocked(now time.Time) time.Duration {
	total, fresh := 0, 0
	for k, st := range t.book {
		if st == nil || strings.HasSuffix(k, "|big") {
			continue
		}
		port := lanePort(k)
		if port < 8750 || port > 8769 {
			continue
		}
		total++
		if st.LastOK > 0 && now.Sub(time.UnixMilli(st.LastOK)) < 10*time.Minute {
			fresh++
		}
	}
	// ≥25% of daemon slots, or at least 3 distinct groups, recently served:
	// the wall is per-address and a parked lane deserves a fast renew.
	if total > 0 && (fresh >= 3 || fresh*4 >= total) {
		return 2 * time.Minute
	}
	return 10 * time.Minute
}

func (t *CLITransport) maybeRedrawLane(proxy string) {
	t.redrawLane(proxy, false)
}

// redrawLane replaces one slot's WARP handshake. force skips the periodic
// cooldown: a lane that just answered 429 has proven its /64 spent, and waiting
// out the brake only hands the next requests to a dead address.
func (t *CLITransport) redrawLane(proxy string, force bool) {
	redrawCooldown := t.redrawCooldownFor(time.Now())
	port := lanePort(proxy)
	t.mu.Lock()
	if t.poolPorts != nil {
		if !t.poolPorts[port] {
			t.mu.Unlock()
			return
		}
	} else if port < 8750 || port > 8769 {
		// The pool has not been read yet: keep the legacy live range as the
		// guard until mainLanes learns the real slot ports.
		t.mu.Unlock()
		return
	}
	if t.redrawAt == nil {
		t.redrawAt = map[string]int64{}
	}
	if t.pendingRedraw == nil {
		t.pendingRedraw = map[string]bool{}
	}
	if t.active[proxy] > 0 {
		// Restarting the slot now would RST the live answer on it. The lane
		// drains (no new requests: it is parked) and the replacement lane is
		// spawned on a NEW port, so the pool never waits for this stream.
		t.pendingRedraw[proxy] = true
		t.mu.Unlock()
		slog.Default().Info("cli: lane redraw deferred until its stream finishes", "lane", proxy, "active", t.active[proxy])
		go t.spawnWideLane(proxy)
		return
	}
	// A forced redraw bypasses the fleet-wide brake, not the per-slot floor: a
	// /64 that was just re-handshaked is the freshest address this process can
	// get, and re-rolling it inside half a minute only churns the account
	// (measured 2026-11-22 17:19-17:22: a load burst turned the cooldown-free
	// path into 7 126 redraws in 3 min, most of them rejected).
	if force && redrawCooldown > 30*time.Second {
		redrawCooldown = 30 * time.Second
	}
	if last := t.redrawAt[proxy]; last != 0 && time.Since(time.Unix(0, last)) < redrawCooldown {
		t.mu.Unlock()
		return
	}
	t.redrawAt[proxy] = time.Now().UnixNano()
	t.mu.Unlock()
	api := strings.TrimSuffix(t.apiURL, "/") + "/api/v1/pool/redraw"
	tok := t.token
	go func() {
		// One redraw at a time per process: the warp-router answers 409 the
		// moment a second RedrawOne starts, and a rejected call left the slot
		// parked. Serialising here removes the self-inflicted 409s.
		laneRedrawMu.Lock()
		defer laneRedrawMu.Unlock()
		body, _ := json.Marshal(map[string]int{"port": port})
		req, err := http.NewRequest(http.MethodPost, api, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if tok != "" {
			req.Header.Set("authorization", "Bearer "+tok)
		}
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			// A failed call must not burn the per-lane cooldown: the slot never
			// restarted, so the park is still there and the next loop pass has
			// to retry (measured 2026-11-21: 409s left lanes idle for 10 min).
			t.mu.Lock()
			delete(t.redrawAt, proxy)
			t.lastFleetRedraw = time.Time{}
			t.mu.Unlock()
			slog.Default().Warn("cli: lane redraw failed", "lane", proxy, "err", err)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.mu.Lock()
			delete(t.redrawAt, proxy)
			t.lastFleetRedraw = time.Time{}
			t.mu.Unlock()
			slog.Default().Warn("cli: lane redraw rejected", "lane", proxy, "status", resp.StatusCode)
			return
		}
		t.mu.Lock()
		for _, k := range []string{proxy, proxy + "|big"} {
			if st := t.book[k]; st != nil {
				st.Until = 0
			}
		}
		t.mu.Unlock()
		t.saveBook()
		slog.Default().Info("cli: lane redrawn", "lane", proxy, "port", port)
	}()
}

// trackLaneBody counts a lane's open response body: maintenance must never
// restart the tunnel under a live answer (that RSTs the stream mid-word).
func (t *CLITransport) trackLaneBody(proxy string, rc io.ReadCloser) io.ReadCloser {
	t.mu.Lock()
	if t.active == nil {
		t.active = map[string]int{}
	}
	t.active[proxy]++
	t.mu.Unlock()
	return &laneStreamBody{ReadCloser: rc, t: t, proxy: proxy}
}

type laneStreamBody struct {
	io.ReadCloser
	t     *CLITransport
	proxy string
	once  sync.Once
}

func (b *laneStreamBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.release()
	}
	return n, err
}

func (b *laneStreamBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

func (b *laneStreamBody) release() { b.once.Do(func() { b.t.releaseLane(b.proxy) }) }

// releaseLane drops one open body and runs the maintenance that was deferred
// for this lane (or the whole fleet) as soon as nothing streams on it.
func (t *CLITransport) releaseLane(proxy string) {
	t.mu.Lock()
	if t.active[proxy] > 0 {
		t.active[proxy]--
	}
	idle := t.active[proxy] == 0 && t.pendingRedraw[proxy]
	if idle {
		delete(t.pendingRedraw, proxy)
	}
	rotate := ""
	if t.pendingRotate != "" && t.idleLocked() {
		rotate = t.pendingRotate
		t.pendingRotate = ""
	}
	t.mu.Unlock()
	if idle {
		t.maybeRedrawLane(proxy)
		go t.retireWideLane(proxy)
	}
	if rotate != "" {
		t.rotateFleet(rotate)
	}
	// Drain one deferred redraw once the whole fleet is idle; otherwise the
	// per-lane queue waits for the next 429 on that same lane.
	t.mu.Lock()
	if len(t.pendingRedraw) > 0 && t.idleLocked() {
		for p := range t.pendingRedraw {
			delete(t.pendingRedraw, p)
			t.mu.Unlock()
			t.redrawLane(p, false)
			return
		}
	}
	t.mu.Unlock()
}

// wideSpawnMu guards the replacement bookkeeping: a burst of 429s must not
// spawn two workers on the same port, and the row removal must not race the
// spawn.
var (
	wideSpawnMu sync.Mutex
	wideSpawned = map[string]*wideSpawnRec{}
)

type wideSpawnRec struct {
	cmd  *exec.Cmd
	http int
}

// writeWideState writes the wide state atomically (temp + rename).
func writeWideState(path string, rows any) {
	b, err := json.MarshalIndent(rows, "", " ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// spawnWideLane starts one extra node-backed WARP worker on a free port pair
// and appends it to the wide state, so a draining lane is replaced without
// touching the main pool (a pool resize restarts every slot and cuts streams).
// The replacement is retired by retireWideLane once the old lane drained.
func (t *CLITransport) spawnWideLane(proxy string) {
	if t.wideState == "" {
		return
	}
	wideSpawnMu.Lock()
	defer wideSpawnMu.Unlock()
	if _, ok := wideSpawned[proxy]; ok {
		return
	}
	type wideRow struct {
		I        int    `json:"i"`
		Node     int    `json:"node"`
		NodeExit string `json:"node_exit,omitempty"`
		Acc      string `json:"acc"`
		Socks    int    `json:"socks"`
		HTTP     int    `json:"http"`
	}
	var state []wideRow
	if b, err := os.ReadFile(t.wideState); err == nil {
		_ = json.Unmarshal(b, &state)
	}
	if len(state) >= 40 {
		return
	}
	accRoot := filepath.Join(os.Getenv("LOCALAPPDATA"), "warp-router", "data", "accounts")
	ents, err := os.ReadDir(accRoot)
	if err != nil || len(ents) == 0 {
		return
	}
	used := map[int]bool{}
	for _, s := range state {
		used[s.HTTP] = true
		used[s.Socks] = true
	}
	hp, sp := 0, 0
	for p := 9710; p < 9800; p++ {
		if used[p] || used[p+400] {
			continue
		}
		ln, e1 := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if e1 != nil {
			continue
		}
		_ = ln.Close()
		ln2, e2 := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p+400))
		if e2 != nil {
			continue
		}
		_ = ln2.Close()
		hp, sp = p, p+400
		break
	}
	if hp == 0 {
		return
	}
	// Reuse the node of an existing live row: a made-up port is often dead and
	// a dead node yields a dead lane.
	node := 0
	for i := len(state) - 1; i >= 0; i-- {
		if state[i].Node > 0 {
			node = state[i].Node
			break
		}
	}
	if node == 0 {
		return
	}
	acc := state[len(state)-1].Acc
	if acc == "" {
		acc = ents[len(state)%len(ents)].Name()
	}
	exe := envOr("AI_ROUTER_WARP_ROUTER_EXE", filepath.Join(os.Getenv("LOCALAPPDATA"), "warp-router", "warp-router.exe"))
	cmd := exec.Command(exe, "-pool-backend",
		"-slot", filepath.Join(accRoot, acc),
		"-socks", fmt.Sprintf("127.0.0.1:%d", sp),
		"-http", fmt.Sprintf("127.0.0.1:%d", hp),
		"-proxy", fmt.Sprintf("127.0.0.1:%d", node),
		"-endpoint", "188.114.96.2:908")
	if err := cmd.Start(); err != nil {
		slog.Default().Warn("cli: spawn replacement lane failed", "err", err)
		return
	}
	wideSpawned[proxy] = &wideSpawnRec{cmd: cmd, http: hp}
	state = append(state, wideRow{I: len(state), Node: node, Acc: acc, Socks: sp, HTTP: hp})
	writeWideState(t.wideState, state)
	slog.Default().Info("cli: replacement lane spawned", "http", hp, "socks", sp, "acc", acc, "node", node)
}

// retireWideLane stops the replacement spawned for proxy and removes its row,
// so the pool returns to its own size instead of growing forever.
func (t *CLITransport) retireWideLane(proxy string) {
	if t.wideState == "" {
		return
	}
	wideSpawnMu.Lock()
	defer wideSpawnMu.Unlock()
	rec, ok := wideSpawned[proxy]
	if !ok {
		return
	}
	delete(wideSpawned, proxy)
	if rec.cmd != nil && rec.cmd.Process != nil {
		_ = rec.cmd.Process.Kill()
		_, _ = rec.cmd.Process.Wait()
	}
	var state []map[string]any
	if b, err := os.ReadFile(t.wideState); err == nil {
		_ = json.Unmarshal(b, &state)
	}
	out := state[:0]
	for _, row := range state {
		if h, _ := row["http"].(float64); int(h) == rec.http {
			continue
		}
		out = append(out, row)
	}
	writeWideState(t.wideState, out)
	slog.Default().Info("cli: replacement lane retired", "http", rec.http)
}

// idleLocked reports whether no lane streams are open. mu must be held.
func (t *CLITransport) idleLocked() bool {
	for _, n := range t.active {
		if n > 0 {
			return false
		}
	}
	return true
}
