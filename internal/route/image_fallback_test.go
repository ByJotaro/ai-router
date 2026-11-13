package route

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ByJotaro/ai-router/internal/provider"
)

func TestStripImageParts(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]},` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,BB"}}]}]}`)
	out, ok := stripImageParts(body)
	if !ok {
		t.Fatal("expected a change")
	}
	if strings.Contains(string(out), "image_url") {
		t.Fatalf("image part survived: %s", out)
	}
	if !strings.Contains(string(out), `"look"`) {
		t.Fatalf("text part lost: %s", out)
	}
	if !strings.Contains(string(out), "image was attached") {
		t.Fatalf("empty content not annotated: %s", out)
	}
	if _, ok := stripImageParts([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)); ok {
		t.Fatal("a text-only body must not be rewritten")
	}
}

func TestImageUnavailableRetryStripsImages(t *testing.T) {
	var calls int32
	var lastHadImages atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		raw, _ := io.ReadAll(r.Body)
		hasImages := strings.Contains(string(raw), `"image_url"`)
		lastHadImages.Store(hasImages)
		if hasImages {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"[invalid_request_error] image input is not available on this engine"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	r := New(map[string]*provider.Provider{
		"any": {Name: "any", Type: "openai-compatible", BaseURL: srv.URL},
	})
	var omitted bool
	r.OnEvent = func(typ string, _ any) {
		if typ == "ai.image_omitted" {
			omitted = true
		}
	}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]}`)
	res, err := r.Call(context.Background(), "m", body, false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 after the stripped retry, got %d", res.Resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("want 2 upstream calls, got %d", got)
	}
	if lastHadImages.Load() {
		t.Fatal("the retry still carried image parts")
	}
	if !omitted {
		t.Fatal("expected ai.image_omitted event")
	}
}

func TestNonImageBadRequestIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad tool schema"}}`))
	}))
	defer srv.Close()

	r := New(map[string]*provider.Provider{
		"any": {Name: "any", Type: "openai-compatible", BaseURL: srv.URL},
	})
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]}`)
	res, err := r.Call(context.Background(), "m", body, false)
	if err == nil {
		if res == nil || res.Resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("want the honest 400 result, got %+v", res)
		}
	} else {
		var se *UpstreamStatusError
		if !errors.As(err, &se) || se.Status != http.StatusBadRequest {
			t.Fatalf("want a 400 error, got %v", err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("a non-image 400 must not be retried, calls=%d", got)
	}
}
