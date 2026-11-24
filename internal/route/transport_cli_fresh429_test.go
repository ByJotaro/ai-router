package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gated429Request builds a request the sweep blames on the lane: the gate shape
// is set, so a 429 would normally park, redraw and unpin.
func gated429Request(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		"https://upstream.invalid/inference/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"system","content":"head"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-opencode-session", "ses_test")
	return req.WithContext(withGateShape(req.Context(), true))
}

func wallServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"wall"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A lane that admitted a 200 seconds ago is not a burned egress: the 429 is a
// verdict about the body. It must pass through unchanged, with no park, no
// redraw bookkeeping and no session unpin.
func TestFreshLane429NotParked(t *testing.T) {
	srv := wallServer(t)
	tr := newSweepTransport(t, srv.URL)
	admitted := time.Now().Add(-5 * time.Second).UnixMilli()
	tr.mu.Lock()
	tr.book[""] = &laneState{LastOK: admitted, Score: 1, OK: 3}
	tr.mu.Unlock()

	client := &http.Client{Transport: &cliSweep{t: tr}, Timeout: 10 * time.Second}
	resp, err := client.Do(gated429Request(t))
	if err != nil {
		t.Fatalf("sweep returned an error instead of the 429: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("fresh-lane 429 surfaced as %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"FreeUsageLimitError"`) {
		t.Fatalf("upstream 429 body not relayed")
	}
	tr.mu.Lock()
	st := tr.book[""]
	_, bigCreated := tr.book["|big"]
	redraws := len(tr.redrawAt)
	tr.mu.Unlock()
	if st == nil {
		t.Fatalf("book row vanished")
	}
	if st.Bad != 0 || st.Until != 0 {
		t.Fatalf("fresh lane was marked bad/parked: %+v", st)
	}
	if st.LastOK != admitted {
		t.Fatalf("fresh lane's LastOK changed: %d", st.LastOK)
	}
	if bigCreated {
		t.Fatalf("a big-body book row was created by a 429")
	}
	if redraws != 0 {
		t.Fatalf("fresh lane was queued for a redraw: %d", redraws)
	}
}

// The old path must stay for a lane whose last 200 is old: park it and hand the
// wall back as 429.
func TestStaleLane429StillParked(t *testing.T) {
	srv := wallServer(t)
	tr := newSweepTransport(t, srv.URL)
	tr.mu.Lock()
	tr.book[""] = &laneState{LastOK: time.Now().Add(-10 * time.Minute).UnixMilli(), Score: 0, Bad: 1}
	tr.mu.Unlock()

	client := &http.Client{Transport: &cliSweep{t: tr}, Timeout: 10 * time.Second}
	resp, err := client.Do(gated429Request(t))
	if err != nil {
		t.Fatalf("sweep returned an error instead of the 429: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("stale-lane 429 surfaced as %d", resp.StatusCode)
	}
	tr.mu.Lock()
	st := tr.book[""]
	tr.mu.Unlock()
	if st == nil {
		t.Fatalf("book row vanished")
	}
	if st.Bad == 0 || st.Until == 0 {
		t.Fatalf("stale lane was not parked: %+v", st)
	}
}

// The freshness test must read both book rows: a big-body 200 lands in the
// "|big" row while the plain row may be cold.
func TestLaneJustAdmittedChecksBothBooks(t *testing.T) {
	tr := &CLITransport{book: map[string]*laneState{}}
	now := time.Now().UnixMilli()
	if tr.laneJustAdmitted("http://127.0.0.1:1", now) {
		t.Fatalf("empty book reported fresh")
	}
	tr.book["http://127.0.0.1:1"] = &laneState{LastOK: now - int64((2 * time.Minute).Milliseconds())}
	if tr.laneJustAdmitted("http://127.0.0.1:1", now) {
		t.Fatalf("stale plain row reported fresh")
	}
	tr.book[bookKey("http://127.0.0.1:1", true)] = &laneState{LastOK: now - 1000}
	if !tr.laneJustAdmitted("http://127.0.0.1:1", now) {
		t.Fatalf("fresh big row not seen")
	}
	if tr.laneJustAdmitted("http://127.0.0.1:2", now) {
		t.Fatalf("another lane's row leaked")
	}
}
