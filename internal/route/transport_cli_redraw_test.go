package route

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The park is the window the gate named — nothing more (no 4 h ceiling) and
// nothing less (no shortening a day-scale window).
func TestParkMSIsTheNamedWindow(t *testing.T) {
	cases := map[string]int64{
		"":       5000,
		"15":     15000,
		"7352":   7352000,
		"200000": 200000000, // > 4 h: kept as named
	}
	for ra, want := range cases {
		if got := parkMS(ra); got != want {
			t.Fatalf("parkMS(%q) = %d, want %d", ra, got, want)
		}
	}
}

// A burned daemon-pool lane is replaced at the source: one redraw call per
// cooldown, and the old park is cleared only after the redraw answered.
func TestMaybeRedrawLaneClearsParkAfterRedraw(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/pool/redraw" {
			http.NotFound(w, r)
			return
		}
		var body map[string]int
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["port"] != 8753 {
			t.Errorf("redraw asked for port %v, want 8753", body["port"])
		}
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"redrawn":true}`))
	}))
	defer srv.Close()
	tr := &CLITransport{apiURL: srv.URL, bookPath: filepath.Join(t.TempDir(), "book.json"), book: map[string]*laneState{}}
	proxy := "http://127.0.0.1:8753"
	future := time.Now().Add(time.Hour).UnixMilli()
	tr.book[proxy] = &laneState{Until: future}
	tr.book[proxy+"|big"] = &laneState{Until: future}
	tr.maybeRedrawLane(proxy)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tr.mu.Lock()
		cleared := tr.book[proxy].Until == 0 && tr.book[proxy+"|big"].Until == 0
		tr.mu.Unlock()
		if cleared {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tr.mu.Lock()
	u1, u2 := tr.book[proxy].Until, tr.book[proxy+"|big"].Until
	tr.mu.Unlock()
	if u1 != 0 || u2 != 0 {
		t.Fatalf("redraw answer must clear the park, got plain=%d big=%d", u1, u2)
	}
	// Second call inside the cooldown must not redraw again.
	tr.maybeRedrawLane(proxy)
	time.Sleep(100 * time.Millisecond)
	if n := hits.Load(); n != 1 {
		t.Fatalf("redraw called %d times inside the cooldown, want 1", n)
	}
}

// A rejected redraw (old warp-router without the endpoint, busy pool) must not
// hand the spent address back to the sweep.
func TestMaybeRedrawLaneRejectedKeepsPark(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"pool busy"}`))
	}))
	defer srv.Close()
	tr := &CLITransport{apiURL: srv.URL, bookPath: filepath.Join(t.TempDir(), "book.json"), book: map[string]*laneState{}}
	proxy := "http://127.0.0.1:8754"
	tr.book[proxy] = &laneState{Until: time.Now().Add(time.Hour).UnixMilli()}
	tr.maybeRedrawLane(proxy)
	time.Sleep(300 * time.Millisecond)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.book[proxy].Until == 0 {
		t.Fatal("a rejected redraw cleared the park")
	}
}

// A lane outside the daemon pool has no slot endpoint.
func TestMaybeRedrawLaneIgnoresOtherPorts(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	tr := &CLITransport{apiURL: srv.URL, bookPath: filepath.Join(t.TempDir(), "book.json"), book: map[string]*laneState{}}
	tr.maybeRedrawLane("http://127.0.0.1:9723")
	time.Sleep(100 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatal("a wide/node lane must not call the daemon redraw endpoint")
	}
}

// The proactive pass replaces lanes parked for hours (a day-scale window is a
// dead lane), and only those: short parks are left for their own expiry, wide
// lanes have no endpoint, and max bounds one cycle.
func TestRedrawLongParkedSelection(t *testing.T) {
	var ports []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]int
		_ = json.NewDecoder(r.Body).Decode(&body)
		ports = append(ports, body["port"])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tr := &CLITransport{apiURL: srv.URL, bookPath: filepath.Join(t.TempDir(), "book.json"), book: map[string]*laneState{}}
	now := time.Now()
	tr.book["http://127.0.0.1:8756"] = &laneState{Until: now.Add(2 * time.Hour).UnixMilli()}
	tr.book["http://127.0.0.1:8756|big"] = &laneState{Until: now.Add(2 * time.Hour).UnixMilli()}
	tr.book["http://127.0.0.1:8757"] = &laneState{Until: now.Add(10 * time.Minute).UnixMilli()}
	tr.book["http://127.0.0.1:9723"] = &laneState{Until: now.Add(2 * time.Hour).UnixMilli()}
	tr.redrawLongParked(1)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tr.mu.Lock()
		cleared := tr.book["http://127.0.0.1:8756"].Until == 0
		tr.mu.Unlock()
		if cleared {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tr.mu.Lock()
	long1 := tr.book["http://127.0.0.1:8756"].Until
	short := tr.book["http://127.0.0.1:8757"].Until
	wide := tr.book["http://127.0.0.1:9723"].Until
	tr.mu.Unlock()
	if long1 != 0 {
		t.Fatal("long-parked daemon lane was not redrawn")
	}
	if short == 0 || wide == 0 {
		t.Fatalf("short park / wide lane must be left alone, got short=%d wide=%d", short, wide)
	}
	if len(ports) != 1 || ports[0] != 8756 {
		t.Fatalf("redraw ports = %v, want exactly [8756]", ports)
	}
}

// A failed big turn parks two lanes in the big book, the thin class; the one
// cycle slot must go to a big-parked slot before a plain-parked one.
func TestRedrawLongParkedBigFirst(t *testing.T) {
	var ports []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]int
		_ = json.NewDecoder(r.Body).Decode(&body)
		ports = append(ports, body["port"])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tr := &CLITransport{apiURL: srv.URL, bookPath: filepath.Join(t.TempDir(), "book.json"), book: map[string]*laneState{}}
	park := time.Now().Add(2 * time.Hour).UnixMilli()
	tr.book["http://127.0.0.1:8758"] = &laneState{Until: park}
	tr.book["http://127.0.0.1:8759|big"] = &laneState{Until: park}
	tr.redrawLongParked(1)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tr.mu.Lock()
		cleared := tr.book["http://127.0.0.1:8759|big"].Until == 0
		tr.mu.Unlock()
		if cleared {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(ports) != 1 || ports[0] != 8759 {
		t.Fatalf("redraw ports = %v, want [8759] (big-parked first)", ports)
	}
}

// The park-rotate must never restart a slot under a live answer: the stream
// keeps its tunnel, and the slot is redrawn the moment the body closes.
func TestRedrawWaitsForActiveStream(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"redrawn":true}`))
	}))
	defer srv.Close()
	tr := &CLITransport{apiURL: srv.URL, bookPath: filepath.Join(t.TempDir(), "book.json"), book: map[string]*laneState{}, active: map[string]int{}, pendingRedraw: map[string]bool{}}
	proxy := "http://127.0.0.1:8757"
	tr.book[proxy] = &laneState{Until: time.Now().Add(time.Hour).UnixMilli()}
	body := tr.trackLaneBody(proxy, io.NopCloser(strings.NewReader("stream")))
	tr.maybeRedrawLane(proxy)
	time.Sleep(150 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Fatalf("redraw fired under a live stream: %d calls", n)
	}
	tr.mu.Lock()
	pending := tr.pendingRedraw[proxy]
	tr.mu.Unlock()
	if !pending {
		t.Fatal("deferred redraw was not remembered")
	}
	_ = body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("redraw after stream close: %d calls, want 1", n)
	}
	tr.mu.Lock()
	u := tr.book[proxy].Until
	_, stillPending := tr.pendingRedraw[proxy]
	tr.mu.Unlock()
	if u != 0 || stillPending {
		t.Fatalf("redraw must clear park and pending, until=%d pending=%v", u, stillPending)
	}
}

// A fleet rotate is deferred while any lane streams and fires once the last
// body closes.
func TestRotateWaitsForActiveStreams(t *testing.T) {
	var rotateHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pool/rotate") {
			rotateHits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"respawned":20}`))
	}))
	defer srv.Close()
	tr := &CLITransport{apiURL: srv.URL, bookPath: filepath.Join(t.TempDir(), "book.json"), book: map[string]*laneState{}, active: map[string]int{}, pendingRedraw: map[string]bool{}}
	proxy := "http://127.0.0.1:8758"
	body := tr.trackLaneBody(proxy, io.NopCloser(strings.NewReader("stream")))
	tr.rotateFleet("lane sweep exhausted")
	time.Sleep(150 * time.Millisecond)
	if n := rotateHits.Load(); n != 0 {
		t.Fatalf("rotate fired under a live stream: %d calls", n)
	}
	_ = body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for rotateHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := rotateHits.Load(); n != 1 {
		t.Fatalf("rotate after stream close: %d calls, want 1", n)
	}
}

// A rejected redraw (409 busy slot) must not burn the 10-minute cooldown: the
// slot never restarted, so the next pass has to retry it.
func TestRejectedRedrawDoesNotBurnCooldown(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	tr := &CLITransport{apiURL: srv.URL, bookPath: filepath.Join(t.TempDir(), "book.json"), book: map[string]*laneState{}, active: map[string]int{}, pendingRedraw: map[string]bool{}}
	proxy := "http://127.0.0.1:8762"
	tr.book[proxy] = &laneState{Until: time.Now().Add(time.Hour).UnixMilli()}
	tr.maybeRedrawLane(proxy)
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// The rejection clears redrawAt in the POST goroutine; wait for it before
	// the retry, otherwise the second call races the response read.
	for time.Now().Before(deadline) {
		tr.mu.Lock()
		_, busy := tr.redrawAt[proxy]
		tr.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tr.maybeRedrawLane(proxy)
	deadline = time.Now().Add(2 * time.Second)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("rejected redraw retried %d times, want 2 (cooldown burnt?)", n)
	}
}

// The fleet rotate gate must trust the fleet-wide last 200, not the book rows
// that lanesNow wipes on an address change: a 200 a minute ago must block the
// rotate even with an empty book.
func TestRotateGateUsesFleetLastSuccess(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"respawned":20}`))
	}))
	defer srv.Close()
	old := lastLaneSuccessUnix.Load()
	lastLaneSuccessUnix.Store(time.Now().Add(-time.Minute).Unix())
	t.Cleanup(func() { lastLaneSuccessUnix.Store(old) })
	tr := &CLITransport{apiURL: srv.URL, bookPath: filepath.Join(t.TempDir(), "book.json"), book: map[string]*laneState{}, active: map[string]int{}, pendingRedraw: map[string]bool{}}
	tr.rotateFleet("lane sweep exhausted")
	time.Sleep(150 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Fatalf("rotate fired although a lane served 200 a minute ago: %d", n)
	}
}

// Parallel redraw requests (redrawLongParked hands up to 3 at once) must reach
// warp-router one at a time: RedrawOne TryLocks rotateMu and answers 409 for
// every concurrent neighbour.
func TestConcurrentLaneRedrawsAreSerialized(t *testing.T) {
	var cur, maxCur atomic.Int32
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			m := maxCur.Load()
			if n <= m || maxCur.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(120 * time.Millisecond)
		cur.Add(-1)
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"redrawn":true}`))
	}))
	defer srv.Close()
	tr := &CLITransport{apiURL: srv.URL, bookPath: filepath.Join(t.TempDir(), "book.json"), book: map[string]*laneState{}, active: map[string]int{}, pendingRedraw: map[string]bool{}}
	for _, p := range []string{"http://127.0.0.1:8753", "http://127.0.0.1:8754", "http://127.0.0.1:8755"} {
		tr.book[p] = &laneState{Until: time.Now().Add(time.Hour).UnixMilli()}
		tr.lastFleetRedraw = time.Time{} // exercise serialization, not the fleet brake
		tr.maybeRedrawLane(p)
	}
	deadline := time.Now().Add(3 * time.Second)
	for hits.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := hits.Load(); n != 3 {
		t.Fatalf("redraw hits %d, want 3", n)
	}
	if m := maxCur.Load(); m != 1 {
		t.Fatalf("redraw concurrency %d, want 1", m)
	}
}

// A parked lane is renewed in 2 minutes while the fleet still serves (the wall
// is per-address); during a fleet-wide wall the 10-minute brake stays.
func TestRedrawCooldownFollowsFleetShare(t *testing.T) {
	now := time.Now()
	mk := func(fresh, stale int) *CLITransport {
		tr := &CLITransport{book: map[string]*laneState{}}
		for i := 0; i < fresh; i++ {
			k := "http://127.0.0.1:" + strconv.Itoa(8750+i)
			tr.book[k] = &laneState{LastOK: now.Add(-2 * time.Minute).UnixMilli()}
		}
		for i := 0; i < stale; i++ {
			k := "http://127.0.0.1:" + strconv.Itoa(8750+fresh+i)
			tr.book[k] = &laneState{LastOK: now.Add(-40 * time.Minute).UnixMilli()}
		}
		return tr
	}
	if d := mk(2, 18).redrawCooldownFor(now); d != 10*time.Minute {
		t.Fatalf("2/20 fresh cooldown %v, want 10m", d)
	}
	if d := mk(3, 17).redrawCooldownFor(now); d != 2*time.Minute {
		t.Fatalf("3/20 fresh cooldown %v, want 2m (three groups)", d)
	}
	if d := mk(5, 15).redrawCooldownFor(now); d != 2*time.Minute {
		t.Fatalf("5/20 fresh cooldown %v, want 2m (25%%)", d)
	}
}
