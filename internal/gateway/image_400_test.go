package gateway

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// A text-only engine instance refuses the images even after the router's
// strip-retry. A 400 is terminal for clients that retry only 429/5xx (OMP:
// `status === 429 || status >= 500`), while a retry on another egress may
// reach a multimodal instance: the gateway must answer a retryable 503 with a
// short Retry-After and keep the engine's own message.
func TestImageUnavailableBecomesRetryable(t *testing.T) {
	var calls int32
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"image input is not available on this engine"}}`))
	})
	_, srv := newTestGateway(t, map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL},
	}, nil)
	body := map[string]any{
		"model":  "m",
		"stream": false,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA"}},
		}}},
	}
	res, data := doReq(t, srv.URL, "/v1/chat/completions", http.MethodPost, "admin-token", body)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503 (retryable), got %d: %s", res.StatusCode, data)
	}
	// No outward Retry-After: OMP retries 5xx itself, and an RA is the lever
	// that escalates a provider into a long block on this stack.
	if got := res.Header.Get("Retry-After"); got != "" {
		t.Fatalf("want no Retry-After, got %q", got)
	}
	if !strings.Contains(string(data), "image input is not available") {
		t.Fatalf("the engine message was lost: %s", data)
	}
	// First call carries the image, the second is the router's stripped retry:
	// both are refused by this mock, which is exactly the exhausted case.
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("want 2 upstream calls, got %d", got)
	}
}
