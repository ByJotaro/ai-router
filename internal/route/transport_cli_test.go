package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newSweepTransport points the transport at one local TLS server with the sweep
// knobs shrunk so a refusal is fast to observe.
func newSweepTransport(t *testing.T, serverURL string) *CLITransport {
	t.Helper()
	cliInsecureSkipVerify = true
	t.Cleanup(func() { cliInsecureSkipVerify = false })
	host := strings.TrimPrefix(serverURL, "https://")
	tr := NewCLITransport("", "", host, 10*time.Second, "9003", "", "")
	tr.budget = 2 * time.Second
	tr.attempts = 1
	tr.responseTimeout = 2 * time.Second
	tr.bookPath = t.TempDir() + "/book.json"
	// NewCLITransport loaded the production book before the path was overridden.
	tr.book = map[string]*laneState{}
	tr.pins = map[string][]sidPin{}
	// One lane, dialled directly (empty proxy): the sweep must exercise its
	// status handling against the local server.
	tr.lanes = []lane{{proxy: "", ip: ""}}
	tr.fetched = time.Now()
	return tr
}

func doRoundTrip(t *testing.T, tr *CLITransport) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		"https://upstream.invalid/inference/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-opencode-session", "ses_test")
	client := &http.Client{Transport: &cliSweep{t: tr}, Timeout: 10 * time.Second}
	return client.Do(req)
}

// A gate 5xx is a verdict about the lane: the sweep parks it and reports the
// wall as 429, never handing the 503 to the client (the owner's complaint).
func TestSweepTurnsLane5xxInto429(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"upstream down"}`))
	}))
	defer srv.Close()
	tr := newSweepTransport(t, srv.URL)
	resp, err := doRoundTrip(t, tr)
	if err != nil {
		t.Fatalf("sweep returned an error instead of a status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("lane 5xx surfaced as %d, want 429", resp.StatusCode)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, st := range tr.book {
		if st.Bad == 0 || st.Until == 0 {
			t.Fatalf("5xx lane not parked: %+v", st)
		}
	}
}

// A 4xx is the same on every lane: relay it untouched, and do not mark the lane
// healthy.
func TestSweepRelaysClientError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"context_length_exceeded"}`))
	}))
	defer srv.Close()
	tr := newSweepTransport(t, srv.URL)
	resp, err := doRoundTrip(t, tr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("client error rewritten to %d", resp.StatusCode)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, st := range tr.book {
		if st.OK != 0 {
			t.Fatalf("4xx marked the lane healthy: %+v", st)
		}
	}
}

// A 200 passes through and marks the lane healthy so the next request picks it
// first.
func TestSweepServes200AndMarksLane(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	}))
	defer srv.Close()
	tr := newSweepTransport(t, srv.URL)
	resp, err := doRoundTrip(t, tr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, st := range tr.book {
		if st.OK == 0 || st.LastOK == 0 {
			t.Fatalf("served lane not marked healthy: %+v", st)
		}
	}
}
