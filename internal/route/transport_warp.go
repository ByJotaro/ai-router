package route

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// WarpTransport uses warp-router's standalone HTTP API and local proxy.
// It does not require the hub or the egress module.
type WarpTransport struct {
	APIURL     string
	Token      string
	ProxyURL   string
	PerRequest bool
	HTTPClient *http.Client
}

func (w *WarpTransport) Name() string { return "warp" }

func (w *WarpTransport) Acquire(ctx context.Context, req RequestContext, p *provider.Provider) (*Lease, error) {
	proxyURL := w.ProxyURL
	if proxyURL == "" {
		proxyURL = p.Transport.ProxyURL
	}
	if proxyURL == "" {
		return nil, fmt.Errorf("warp proxy_url is empty")
	}
	if w.PerRequest || p.Transport.PerRequest {
		if err := w.rotate(ctx); err != nil {
			return nil, err
		}
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("warp proxy: %w", err)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(u)
	cl := &http.Client{Transport: tr, Timeout: 300 * time.Second}
	if w.HTTPClient != nil {
		cl.Timeout = w.HTTPClient.Timeout
	}
	return &Lease{Client: cl, Proxy: proxyURL}, nil
}

func (w *WarpTransport) rotate(ctx context.Context) error {
	if strings.TrimSpace(w.APIURL) == "" {
		return fmt.Errorf("warp api_url is empty")
	}
	for _, endpoint := range []string{"/api/v1/rotate", "/api/v1/connect"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(w.APIURL, "/")+endpoint, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+w.Token)
		res, err := w.client().Do(req)
		if err != nil {
			return err
		}
		var payload map[string]any
		_ = json.NewDecoder(res.Body).Decode(&payload)
		res.Body.Close()
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			return nil
		}
		if endpoint == "/api/v1/rotate" && res.StatusCode == http.StatusNotFound {
			continue
		}
		return fmt.Errorf("warp %s: status %d", endpoint, res.StatusCode)
	}
	return fmt.Errorf("warp rotate endpoint unavailable")
}

func (w *WarpTransport) client() *http.Client {
	if w.HTTPClient != nil {
		return w.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}
