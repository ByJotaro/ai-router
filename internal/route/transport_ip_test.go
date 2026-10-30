package route

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// rtFunc adapts a func into an http.RoundTripper for tests.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeDialer routes pinned IPs to a transport selected per IP, so the
// transport can be exercised without real sockets.
func fakeDialer(roundTrip func(ip string) http.RoundTripper) func(string) http.RoundTripper {
	return func(ip string) http.RoundTripper {
		return roundTrip(ip)
	}
}

func TestIPRotateFanoutOn429(t *testing.T) {
	var mu sync.Mutex
	good := map[string]bool{}
	statusFor := func(ip string) int {
		mu.Lock()
		defer mu.Unlock()
		if good[ip] {
			return http.StatusOK
		}
		return http.StatusTooManyRequests
	}

	tc := &IPRotateTransport{
		Host:     "opencode.ai",
		Port:     "443",
		Fanout:   5,
		Rounds:   2,
		Cooldown: time.Second,
	}
	tc.resolver = func(context.Context, string) ([]string, error) { return nil, nil }
	tc.ensureSeeded()
	// Seed a pool of IPs: only "9.9.9.200" answers OK on the first wave.
	known := []string{"9.9.9.1", "9.9.9.2", "9.9.9.3", "9.9.9.4", "9.9.9.200"}
	good["9.9.9.200"] = true
	tc.mu.Lock()
	tc.known = known
	tc.mu.Unlock()

	tc.transportForOverride = fakeDialer(func(ip string) http.RoundTripper {
		return rtFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: statusFor(ip),
				Status:     http.StatusText(statusFor(ip)),
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Request:    r,
			}, nil
		})
	})

	client := &http.Client{Transport: &ipRotateRT{t: tc}}
	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The winning IP must have joined the good pool for subsequent rotation.
	tc.mu.Lock()
	defer tc.mu.Unlock()
	found := false
	for _, g := range tc.good {
		if g == "9.9.9.200" {
			found = true
		}
	}
	if !found {
		t.Fatalf("9.9.9.200 not promoted to good pool: %v", tc.good)
	}
}

func TestIPRotateAll429Returns429(t *testing.T) {
	tc := &IPRotateTransport{
		Host:     "opencode.ai",
		Port:     "443",
		Fanout:   3,
		Rounds:   1,
		Cooldown: time.Second,
	}
	tc.resolver = func(context.Context, string) ([]string, error) { return nil, nil }
	tc.ensureSeeded()
	tc.mu.Lock()
	tc.known = []string{"1.1.1.1", "1.1.1.2", "1.1.1.3"}
	tc.mu.Unlock()

	tc.transportForOverride = fakeDialer(func(ip string) http.RoundTripper {
		return rtFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Status:     "429 Too Many Requests",
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Request:    r,
			}, nil
		})
	})

	client := &http.Client{Transport: &ipRotateRT{t: tc}}
	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
}

func TestIPRotateAcquireAppliesOnlyToTargetHost(t *testing.T) {
	tc := &IPRotateTransport{Host: "opencode.ai", Port: "443"}
	tc.resolver = func(context.Context, string) ([]string, error) { return nil, nil }
	ctx := context.Background()

	direct := &provider.Provider{Name: "other", BaseURL: "https://other.example.com/v1"}
	lease, err := tc.Acquire(ctx, RequestContext{}, direct)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.Proxy != "" {
		t.Fatalf("non-target provider must not use iprotate; Proxy=%q", lease.Proxy)
	}

	target := &provider.Provider{Name: "opencode", BaseURL: "https://opencode.ai/zen/v1"}
	lease, err = tc.Acquire(ctx, RequestContext{}, target)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, ok := lease.Client.Transport.(*ipRotateRT); !ok {
		t.Fatalf("target provider transport = %T, want *ipRotateRT", lease.Client.Transport)
	}
}