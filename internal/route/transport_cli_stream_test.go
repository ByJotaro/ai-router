package route

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A stream that keeps producing data must survive past the response window.
// Pre-fix code put ONE absolute deadline on the connection
// (conn.SetDeadline(now+responseTimeout)) and cut every stream at that wall.
func TestCLIStreamSurvivesLongResponse(t *testing.T) {
	const total = 3 * time.Second
	const gap = 200 * time.Millisecond
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for end := time.Now().Add(total); time.Now().Before(end); time.Sleep(gap) {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	tr := newSweepTransport(t, srv.URL)
	tr.responseTimeout = time.Second
	resp, err := doRoundTrip(t, tr)
	if err != nil {
		t.Fatalf("stream cut before the body: %v", err)
	}
	defer resp.Body.Close()
	b, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("read cut at %v after %d bytes", rerr, len(b))
	}
	if !strings.Contains(string(b), "[DONE]") {
		t.Fatalf("stream truncated: %d bytes, no [DONE]", len(b))
	}
}

// Silence longer than the idle window is a dead upstream and must cut the read;
// chunks that keep arriving must not.
func TestCLIReadIdleCutsStalledBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: first\n\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		time.Sleep(3 * time.Second)
		fmt.Fprint(w, "data: late\n\n")
	}))
	defer srv.Close()

	tr := newSweepTransport(t, srv.URL)
	tr.responseTimeout = 500 * time.Millisecond
	resp, err := doRoundTrip(t, tr)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	b, rerr := io.ReadAll(resp.Body)
	if rerr == nil {
		t.Fatalf("stalled body read returned no error, got %q", string(b))
	}
	if !strings.Contains(string(b), "first") {
		t.Fatalf("partial data lost: %q", string(b))
	}
	if strings.Contains(string(b), "late") {
		t.Fatalf("idle deadline did not fire: %q", string(b))
	}
}
