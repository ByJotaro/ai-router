package route

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A fleet that still serves must not be re-rolled: /pool/rotate restarts every
// slot (measured 2026-11-20: 21 lanes serving 200s, two workhorses at 101/58 OK,
// while a sweep exhaustion rotated all 20). The rotate is reserved for a fleet
// with no recent success.
func TestRotateFleetSkippedWhileLanesServe(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"respawned": 20})
	}))
	defer srv.Close()

	serving := &CLITransport{
		apiURL:   srv.URL,
		bookPath: filepath.Join(t.TempDir(), "book.json"),
		book: map[string]*laneState{
			"http://127.0.0.1:8751": {LastOK: time.Now().UnixMilli(), Score: 1, OK: 58},
		},
		pins: map[string][]sidPin{},
	}
	serving.rotateFleet("test")
	time.Sleep(300 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatalf("rotate hit the pool API while a lane served a 200: %d", hits.Load())
	}

	cold := &CLITransport{
		apiURL:   srv.URL,
		bookPath: filepath.Join(t.TempDir(), "book.json"),
		book: map[string]*laneState{
			"http://127.0.0.1:8751": {LastOK: time.Now().Add(-30 * time.Minute).UnixMilli(), Score: 0, Bad: 45},
		},
		pins: map[string][]sidPin{},
	}
	cold.rotateFleet("test")
	deadline := time.Now().Add(3 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatalf("a fleet with no recent 200 did not rotate")
	}
}
