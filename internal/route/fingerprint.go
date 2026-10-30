package route

import (
	"context"
	"crypto/rand"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"

	tls "github.com/refraction-networking/utls"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// fingerprintDiversity provides per-request variation of HTTP headers that are
// otherwise constant across all clients (and therefore a cross-IP link signal
// for upstreams that key on client identity rather than on IP alone).
//
// It is deliberately conservative: it never touches headers that upstream gates
// require verbatim (opencode's pinned CLI User-Agent, Authorization, the
// x-opencode-* session triple, Content-Type). Only cosmetic/accept headers and
// non-gated providers' User-Agent are varied.

var uaPool = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:133.0) Gecko/20100101 Firefox/133.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0",
}

var acceptLangPool = []string{
	"en-US,en;q=0.9",
	"en-US,en;q=0.9,ru;q=0.8",
	"ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7",
	"en-GB,en;q=0.9,en-US;q=0.8",
	"de-DE,de;q=0.9,en;q=0.8",
	"fr-FR,fr;q=0.9,en;q=0.8",
}

var acceptEncPool = []string{
	"gzip, deflate, br, zstd",
	"gzip, deflate, br",
	"gzip, deflate",
	"br, gzip, deflate",
}

// randPick returns a random element from a non-empty slice.
func randPick(pool []string) string {
	if len(pool) == 0 {
		return ""
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(pool))))
	if err != nil {
		return pool[0]
	}
	return pool[n.Int64()]
}

// applyFingerprintDiversity varies cosmetic/accept headers on an already built
// request. It MUST NOT overwrite headers that upstream gates require verbatim
// (User-Agent for opencode, Authorization, Content-Type, x-opencode-*).
// For non-gated providers it also randomizes the User-Agent.
func applyFingerprintDiversity(req *http.Request, p *provider.Provider) {
	if req == nil {
		return
	}
	isOpencode := isOpencodeUpstream(p)

	// Match the real OpenCode CLI request (MITM capture 2026-10-22):
	// NO Accept-Language header. Accept-Encoding is always
	// "gzip, deflate, br, zstd" (constant, not varied).
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	}

	// User-Agent: opencode lanes are pinned to the CLI UA by leaseUA() —
	// changing it breaks the gate. Other providers get a randomized browser UA.
	if !isOpencode && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", randPick(uaPool))
	}
}

// disableKeepAlive sets DisableKeepAlives on the transport so each request
// opens a fresh TCP+TLS connection instead of reusing a pooled one. Shared
// keep-alive connections link requests across egresses (same conn = same IP
// in the upstream's view), which defeats per-IP diversity.
func disableKeepAlive(tr *http.Transport) {
	if tr != nil {
		tr.DisableKeepAlives = true
	}
}

// headerOrderShuffle is a no-op placeholder: Go's net/http sorts headers
// alphabetically in HTTP/2 and there is no public API to change the order
// without a custom RoundTripper. Tracked as a follow-up (requires utls or a
// custom h2 framer). Keeping the function here documents the gap.
func headerOrderShuffle(_ *http.Request) {}

// utlsSpecs is a pool of client hello specs that mimic real browsers. Each
// request picks one at random, so the JA3 fingerprint varies across egresses.
var utlsSpecs = []tls.ClientHelloID{
	tls.HelloChrome_120,
	tls.HelloFirefox_120,
	tls.HelloSafari_16_0,
	tls.HelloEdge_106,
}

// utlsDialTLS dials a TLS connection using a randomized client hello spec,
// so the JA3 fingerprint varies across requests instead of being Go's
// default (which is identical for every Go program and a cross-IP link signal).
func utlsDialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tcpConn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host := addr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	cfg := &tls.Config{ServerName: host, NextProtos: []string{"http/1.1"}}
	spec := utlsSpecs[0]
	if n, err := rand.Int(rand.Reader, big.NewInt(int64(len(utlsSpecs)))); err == nil {
		spec = utlsSpecs[n.Int64()]
	}
	uConn := tls.UClient(tcpConn, cfg, spec)
	if err := uConn.HandshakeContext(ctx); err != nil {
		tcpConn.Close()
		return nil, err
	}
	return uConn, nil
}
