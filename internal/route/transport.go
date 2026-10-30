package route

import (
	"context"
	"errors"
	"net/http"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// ErrTransportUnavailable marks a failure of the OUTBOUND TRANSPORT (pool has no
// egress, pool saturated, no transport registered) as opposed to a verdict about
// the model or the upstream. It must never feed the per-model lockout: during a
// pool-wide outage every request fails, so the model breaker locked a perfectly
// healthy model for its full 15-minute window (measured on the stand 2026-10-22:
// 277 of 1000 requests in the recovered phase answered `model locked out`).
var ErrTransportUnavailable = errors.New("transport unavailable")

// RequestContext describes one upstream attempt. Implementations may use it
// to choose a fresh proxy/channel for each request or retry.
type RequestContext struct {
	Provider string
	Model    string
	Attempt  int
	Stream   bool
}

// Lease contains the outbound HTTP client and safe-release lifecycle.
type Lease struct {
	Client *http.Client
	Proxy  string
	// WaitedMs is how long the pool made the request wait for an egress
	// before serving it (0 = immediate). Journaled so "served after wait"
	// is a visible category instead of an invisible latency spike.
	WaitedMs int64
	release  func(context.Context) error
}

// Release closes the transport lease. It is idempotent for nil releases.
func (l *Lease) Release(ctx context.Context) error {
	if l == nil || l.release == nil {
		return nil
	}
	fn := l.release
	l.release = nil
	return fn(ctx)
}

// TransportProvider supplies independent outbound transport capabilities.
type TransportProvider interface {
	Name() string
	Acquire(context.Context, RequestContext, *provider.Provider) (*Lease, error)
}

// StateFlusher is implemented by transports holding persistable state
// (pool egress bans): graceful shutdown flushes it synchronously so the
// last async saves before SIGTERM are not lost on restart/promote.
type StateFlusher interface {
	FlushState()
}

// StaticTransport adapts a fixed HTTP proxy URL without lifecycle operations.
type StaticTransport struct{}

func (StaticTransport) Name() string { return "static" }
func (StaticTransport) Acquire(_ context.Context, _ RequestContext, p *provider.Provider) (*Lease, error) {
	return &Lease{Client: newClient(p), Proxy: p.Egress}, nil
}
