package route

import (
	"context"
	"fmt"

	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/proxypool"
)

// HubTransport adapts the legacy egress action API to TransportProvider.
// It is optional and remains independent from endpoint routing.
type HubTransport struct{ Pool *proxypool.Client }

func (h HubTransport) Name() string { return "hub-egress" }

func (h HubTransport) Acquire(ctx context.Context, req RequestContext, p *provider.Provider) (*Lease, error) {
	if h.Pool == nil {
		return nil, fmt.Errorf("hub egress pool is nil")
	}
	pr, err := h.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	cl, err := proxiedThrough(nil, pr)
	if err != nil {
		_ = h.Pool.ReleaseAcquired(ctx, pr)
		return nil, err
	}
	return &Lease{Client: cl, Proxy: pr.ID, release: func(ctx context.Context) error {
		return h.Pool.ReleaseAcquired(ctx, pr)
	}}, nil
}
