package route

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// IPRotateTransport routes provider traffic across the target host's resolved
// IPs. Each request is pinned to one IP; on a 429 the transport immediately
// fans out Fanout parallel requests to Fanout distinct IPs. Every IP that
// answers without 429 joins the shared pool (up to ~200 entries), the next
// request round-robins over that pool, and IPs that 429 again are demoted back
// to "known but unproven" while the previously-good ones stay preferred. If
// every probe 429s, DNS is re-resolved and the fan-out repeats up to Rounds
// times before the request is surfaced as a 429.
type IPRotateTransport struct {
	Host       string
	Port       string
	Fanout     int
	Rounds     int
	Cooldown   time.Duration
	ResolveTTL time.Duration
	SeedIPs    []string

	// resolver resolves the host to candidate IPs. Defaults to net.LookupHost.
	resolver func(context.Context, string) ([]string, error)
	// transportForOverride is a test seam replacing the per-IP RoundTripper.
	transportForOverride func(string) http.RoundTripper

	mu         sync.Mutex
	known      []string
	good       []string
	bad        map[string]time.Time
	cursor     int
	lastResolve time.Time
	transports map[string]*http.Transport
}

func (t *IPRotateTransport) Name() string { return "iprotate" }

// SelfRotating reports that ipRotateRT already fans out across IPs inside
// RoundTrip, so the router keeps a single external attempt for this transport.
func (t *IPRotateTransport) SelfRotating() bool { return true }

func (t *IPRotateTransport) Acquire(ctx context.Context, _ RequestContext, p *provider.Provider) (*Lease, error) {
	if !ipRotateMatches(p.BaseURL, t.Host) {
		return &Lease{Client: newClient(p), Proxy: p.Egress}, nil
	}
	t.ensureSeeded()
	timeout := t.TimeoutS()
	cl := &http.Client{Transport: &ipRotateRT{t: t}, Timeout: timeout}
	return &Lease{Client: cl}, nil
}

func (t *IPRotateTransport) TimeoutS() time.Duration {
	return 300 * time.Second
}

func (t *IPRotateTransport) fanoutN() int {
	if t.Fanout > 0 {
		return t.Fanout
	}
	return 5
}

func (t *IPRotateTransport) roundsN() int {
	if t.Rounds > 0 {
		return t.Rounds
	}
	return 2
}

func (t *IPRotateTransport) cooldown() time.Duration {
	if t.Cooldown > 0 {
		return t.Cooldown
	}
	return 30 * time.Second
}

func (t *IPRotateTransport) resolveTTL() time.Duration {
	if t.ResolveTTL > 0 {
		return t.ResolveTTL
	}
	return 60 * time.Second
}

func ipRotateMatches(baseURL, host string) bool {
	if host == "" {
		return false
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), host)
}

// ensureSeeded populates the pool once from SeedIPs and a DNS lookup.
func (t *IPRotateTransport) ensureSeeded() {
	t.mu.Lock()
	seeded := len(t.known) > 0
	if t.bad == nil {
		t.bad = map[string]time.Time{}
	}
	if t.transports == nil {
		t.transports = map[string]*http.Transport{}
	}
	t.mu.Unlock()
	if !seeded {
		t.mu.Lock()
		for _, ip := range t.SeedIPs {
			if ip = strings.TrimSpace(ip); ip != "" {
				t.addKnownLocked(ip)
			}
		}
		t.mu.Unlock()
		t.refresh()
	}
}

// refresh re-resolves the host, respecting ResolveTTL, and merges new IPs.
func (t *IPRotateTransport) refresh() {
	t.mu.Lock()
	if time.Since(t.lastResolve) < t.resolveTTL() {
		t.mu.Unlock()
		return
	}
	host := t.Host
	resolver := t.resolver
	if resolver == nil {
		resolver = func(ctx context.Context, h string) ([]string, error) { return net.LookupHost(h) }
	}
	t.mu.Unlock()

	ips, err := resolver(context.Background(), host)
	if err != nil || len(ips) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// TTL only on success, otherwise 1 failed DNS would strand pool for TTL.
	t.lastResolve = time.Now()
	for _, ip := range ips {
		if ip = strings.TrimSpace(ip); ip != "" {
			t.addKnownLocked(ip)
		}
	}
}

func (t *IPRotateTransport) addKnownLocked(ip string) {
	for _, k := range t.known {
		if k == ip {
			return
		}
	}
	t.known = append(t.known, ip)
}

// pick returns the next preferred IP: round-robin over the good pool, falling
// back to any known IP that is not currently cooling down.
func (t *IPRotateTransport) pick() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if len(t.good) > 0 {
		for i := 0; i < len(t.good); i++ {
			t.cursor = (t.cursor + 1) % len(t.good)
			ip := t.good[t.cursor]
			if until, bad := t.bad[ip]; !bad || now.After(until) {
				return ip
			}
		}
	}
	for _, ip := range t.known {
		if until, bad := t.bad[ip]; !bad || now.After(until) {
			return ip
		}
	}
	return ""
}

// pickN returns up to n distinct IPs, preferring the good pool.
func (t *IPRotateTransport) pickN(n int) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	var out []string
	seen := map[string]bool{}
	// shuffle via rand.Perm for even load spread across fanout
	var goodShuffled []string
	if len(t.good) > 0 {
		perm := rand.Perm(len(t.good))
		goodShuffled = make([]string, len(t.good))
		for i, idx := range perm {
			goodShuffled[i] = t.good[idx]
		}
	} else {
		goodShuffled = t.good
	}
	var knownShuffled []string
	if len(t.known) > 0 {
		perm := rand.Perm(len(t.known))
		knownShuffled = make([]string, len(t.known))
		for i, idx := range perm {
			knownShuffled[i] = t.known[idx]
		}
	} else {
		knownShuffled = t.known
	}
	add := func(cands []string) {
		for _, ip := range cands {
			if len(out) >= n {
				return
			}
			if seen[ip] {
				continue
			}
			if until, bad := t.bad[ip]; bad && !now.After(until) {
				continue
			}
			seen[ip] = true
			out = append(out, ip)
		}
	}
	add(goodShuffled)
	add(knownShuffled)
	return out
}

func (t *IPRotateTransport) markGood(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ip == "" {
		return
	}
	if t.bad == nil {
		t.bad = map[string]time.Time{}
	}
	delete(t.bad, ip)
	for _, g := range t.good {
		if g == ip {
			return
		}
	}
	t.good = append(t.good, ip)
	t.addKnownLocked(ip)
}

func (t *IPRotateTransport) markBad(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ip == "" {
		return
	}
	if t.bad == nil {
		t.bad = map[string]time.Time{}
	}
	t.bad[ip] = time.Now().Add(t.cooldown())
	for i, g := range t.good {
		if g == ip {
			t.good = append(t.good[:i], t.good[i+1:]...)
			if i <= t.cursor && t.cursor > 0 {
				t.cursor--
			}
			if len(t.good) > 0 {
				t.cursor %= len(t.good)
			} else {
				t.cursor = 0
			}
			break
		}
	}
}

// transportFor returns (and caches) a transport pinned to ip.
func (t *IPRotateTransport) transportFor(ip string) http.RoundTripper {
	t.mu.Lock()
	if t.transportForOverride != nil {
		override := t.transportForOverride
		t.mu.Unlock()
		return override(ip)
	}
	if tr, ok := t.transports[ip]; ok {
		t.mu.Unlock()
		return tr
	}
	port := t.Port
	if port == "" {
		port = "443"
	}
	if t.transports == nil {
		t.transports = map[string]*http.Transport{}
	}
	tr := &http.Transport{
		ForceAttemptHTTP2: true,
		DialContext: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 15 * time.Second}
			return d.DialContext(ctx, "tcp", net.JoinHostPort(ip, port))
		},
	}
	t.transports[ip] = tr
	t.mu.Unlock()
	return tr
}

func (t *IPRotateTransport) direct(r *http.Request) (*http.Response, error) {
	return http.DefaultTransport.RoundTrip(r)
}

func (t *IPRotateTransport) dialPinned(r *http.Request, ip string) (*http.Response, error) {
	return t.transportFor(ip).RoundTrip(r)
}

// ipRotateRT implements http.RoundTripper for one acquired lease.
type ipRotateRT struct {
	t *IPRotateTransport
}

func (rt *ipRotateRT) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = b
		req.Body.Close()
	}
	t := rt.t

	// First attempt: one pinned IP.
	ip := t.pick()
	if ip == "" {
		t.refresh()
		ip = t.pick()
	}
	if ip != "" {
		r := cloneRequest(req, body)
		resp, err := t.dialPinned(r, ip)
		if err == nil && resp.StatusCode != http.StatusTooManyRequests {
			t.markGood(ip)
			return resp, nil
		}
		if err == nil {
			drainClose(resp)
		}
		t.markBad(ip)
	} else if t.hasKnown() {
		// Known IPs exist but all in cooldown: go straight to fanout.
	} else {
		// No IP known at all (DNS/seed empty): direct fallback.
		r := cloneRequest(req, body)
		resp, err := t.direct(r)
		if err == nil && resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		if err == nil {
			drainClose(resp)
		}
	}

	// Fan-out: Rounds rounds of Fanout parallel probes on distinct IPs.
	for round := 0; round < t.roundsN(); round++ {
		ips := t.pickN(t.fanoutN())
		if len(ips) == 0 {
			t.refresh()
			ips = t.pickN(t.fanoutN())
		}
		if len(ips) == 0 {
			break
		}
		type probe struct {
			resp *http.Response
			err  error
			ip   string
		}
		ch := make(chan probe, len(ips))
		for _, pip := range ips {
			go func(pip string) {
				r := cloneRequest(req, body)
				resp, err := t.dialPinned(r, pip)
				ch <- probe{resp, err, pip}
			}(pip)
		}
		var winner *http.Response
		for range ips {
			pr := <-ch
			if pr.err != nil {
				t.markBad(pr.ip)
				continue
			}
			if pr.resp.StatusCode == http.StatusTooManyRequests {
				drainClose(pr.resp)
				t.markBad(pr.ip)
				continue
			}
			t.markGood(pr.ip)
			if winner == nil {
				winner = pr.resp
				continue
			}
			pr.resp.Body.Close()
		}
		if winner != nil {
			return winner, nil
		}
		t.refresh()
	}
	return synthetic429(req), nil
}

// hasKnown reports whether we know any IP.
func (t *IPRotateTransport) hasKnown() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.known) > 0
}

func cloneRequest(req *http.Request, body []byte) *http.Request {
	r := req.Clone(req.Context())
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return r
}

func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func synthetic429(req *http.Request) *http.Response {
	body := io.NopCloser(strings.NewReader(`{"error":{"message":"iprotate: all IPs rate-limited","type":"rate_limit_error","code":"rate_limit"}}`))
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Header:     http.Header{},
		Body:       body,
		Request:    req,
	}
}
