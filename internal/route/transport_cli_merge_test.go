package route

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openListener returns a live local port and its address string.
func openListener(t *testing.T) (int, net.Listener) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l.Addr().(*net.TCPAddr).Port, l
}

// TestLanesNowMergesMainAndWide pins the merge rules the bun shim used: main
// first, one lane per public IP, wide lanes only while their port answers and
// the trace is fresh.
func TestLanesNowMergesMainAndWide(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backends": []map[string]any{
				{"http_addr": "127.0.0.1:9001", "public_ip": "2a09:main:1", "state": "up"},
				{"http_addr": "127.0.0.1:9002", "public_ip": "2a09:main:2", "state": "down"},
			},
		})
	}))
	defer srv.Close()

	widePort, wideL := openListener(t)
	defer wideL.Close()
	deadPort, deadL := openListener(t)
	deadL.Close() // its listener is gone: the merge must drop it

	dir := t.TempDir()
	stateFile := filepath.Join(dir, "wide_state.json")
	tracesFile := filepath.Join(dir, "wide_traces.txt")
	must(t, os.WriteFile(stateFile, []byte(fmt.Sprintf(
		`[{"http":%d,"i":0},{"http":%d,"i":1}]`, widePort, deadPort)), 0o644))
	must(t, os.WriteFile(tracesFile, []byte(fmt.Sprintf(
		"%d 2a09:wide:1 NRT\n%d 2a09:wide:2 NRT\n", widePort, deadPort)), 0o644))

	tr := &CLITransport{apiURL: srv.URL, wideState: stateFile, wideTraces: tracesFile}
	lanes := tr.lanesNow()

	if len(lanes) != 2 {
		t.Fatalf("want 2 lanes (1 main up + 1 wide live), got %d: %+v", len(lanes), lanes)
	}
	if lanes[0].ip != "2a09:main:1" {
		t.Fatalf("main lane must come first: %+v", lanes)
	}
	if lanes[1].ip != "2a09:wide:1" {
		t.Fatalf("wide lane missing: %+v", lanes)
	}
}

// One address is one window: a wide lane duplicating a main IP is dropped.
func TestLanesNowDedupesByIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backends": []map[string]any{{"http_addr": "127.0.0.1:9001", "public_ip": "2a09:same", "state": "up"}},
		})
	}))
	defer srv.Close()
	port, l := openListener(t)
	defer l.Close()
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "s.json")
	tracesFile := filepath.Join(dir, "t.txt")
	must(t, os.WriteFile(stateFile, []byte(fmt.Sprintf(`[{"http":%d,"i":0}]`, port)), 0o644))
	must(t, os.WriteFile(tracesFile, []byte(fmt.Sprintf("%d 2a09:same NRT\n", port)), 0o644))

	tr := &CLITransport{apiURL: srv.URL, wideState: stateFile, wideTraces: tracesFile}
	if got := tr.lanesNow(); len(got) != 1 {
		t.Fatalf("duplicate address not deduped: %+v", got)
	}
}

// A stale trace (older than 15 min) drops the wide half entirely: the shim did
// the same, because an unverifiable address is worse than no lane.
func TestLanesNowDropsStaleWide(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backends": []map[string]any{{"http_addr": "127.0.0.1:9001", "public_ip": "2a09:main", "state": "up"}},
		})
	}))
	defer srv.Close()
	port, l := openListener(t)
	defer l.Close()
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "s.json")
	tracesFile := filepath.Join(dir, "t.txt")
	must(t, os.WriteFile(stateFile, []byte(fmt.Sprintf(`[{"http":%d,"i":0}]`, port)), 0o644))
	must(t, os.WriteFile(tracesFile, []byte(fmt.Sprintf("%d 2a09:wide NRT\n", port)), 0o644))
	old := time.Now().Add(-20 * time.Minute)
	must(t, os.Chtimes(tracesFile, old, old))

	tr := &CLITransport{apiURL: srv.URL, wideState: stateFile, wideTraces: tracesFile}
	got := tr.lanesNow()
	if len(got) != 1 || got[0].ip != "2a09:main" {
		t.Fatalf("stale wide half must be dropped: %+v", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A lane that comes back on a NEW address must be available immediately: its
// park and score belong to the old /64 (a fresh clean address inherited a 4 h
// park and the sweep skipped it before this fix).
func TestLanesNowResetsReputationOnAddressChange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backends": []map[string]any{{"http_addr": "127.0.0.1:9001", "public_ip": "2a09:new", "state": "up"}},
		})
	}))
	defer srv.Close()

	tr := &CLITransport{
		apiURL: srv.URL,
		book: map[string]*laneState{
			"http://127.0.0.1:9001": {IP: "2a09:old", Score: 0.9, Until: time.Now().Add(4 * time.Hour).UnixMilli(), OK: 50},
		},
	}
	lanes := tr.lanesNow()
	if len(lanes) != 1 {
		t.Fatalf("lanes: %+v", lanes)
	}
	st := tr.book["http://127.0.0.1:9001"]
	if st == nil {
		t.Fatal("book entry vanished")
	}
	if st.Until != 0 || st.OK != 0 || st.Score != 0 || st.IP != "2a09:new" {
		t.Fatalf("reputation followed the address change: %+v", st)
	}
}

// The book file is per instance: a stand on :9003 must not park the live
// :9001 lanes (and the reverse).
func TestBookPathIsPerInstance(t *testing.T) {
	a := NewCLITransport("", "", "", time.Second, "9001", "", "")
	b := NewCLITransport("", "", "", time.Second, "9003", "", "")
	if a.bookPath == b.bookPath {
		t.Fatalf("instances share a book file: %s", a.bookPath)
	}
}

// The gate's allowance is per /64 group: two lanes inside one burned prefix are
// two wasted sweep attempts. The view keeps one lane per group, preferring the
// better-scoring address.
func TestLanesNowKeepsOneLanePerGroup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backends": []map[string]any{
				{"http_addr": "127.0.0.1:9001", "public_ip": "2a09:bac1:47a0:170::1", "state": "up"},
				{"http_addr": "127.0.0.1:9002", "public_ip": "2a09:bac1:47a0:170::2", "state": "up"},
				{"http_addr": "127.0.0.1:9003", "public_ip": "2a09:bac5:5991:2828::1", "state": "up"},
			},
		})
	}))
	defer srv.Close()

	tr := &CLITransport{
		apiURL: srv.URL,
		book: map[string]*laneState{
			"http://127.0.0.1:9002": {IP: "2a09:bac1:47a0:170::2", Score: 0.9, LastOK: time.Now().UnixMilli()},
		},
	}
	lanes := tr.lanesNow()
	if len(lanes) != 2 {
		t.Fatalf("want one lane per /64 group (2 groups), got %d: %+v", len(lanes), lanes)
	}
	seen := map[string]bool{}
	for _, l := range lanes {
		if seen[group64(l.ip)] {
			t.Fatalf("duplicate group kept: %+v", lanes)
		}
		seen[group64(l.ip)] = true
	}
	if lanes[0].proxy != "http://127.0.0.1:9002" {
		t.Fatalf("better-scoring address of the group must win: %+v", lanes)
	}
}

// A big prompt takes the warmest lane like a small one: picking the worst
// (sacrificial) lane made every big body 429 while the warm lane served small
// requests (measured 2026-11-18: 400 KB 0/3 on live). The attempt cap bounds
// the burn, not the pick.
func TestPickBigTakesWarmLane(t *testing.T) {
	now := time.Now().UnixMilli()
	tr := &CLITransport{
		book: map[string]*laneState{
			// warm lanes: high score, not parked
			"http://127.0.0.1:9001": {IP: "2a09:warm1", Score: 0.95, LastOK: now},
			"http://127.0.0.1:9002": {IP: "2a09:warm2", Score: 0.90, LastOK: now},
			// a lane already parked in the big book
			"http://127.0.0.1:9003|big": {IP: "2a09:cold", Until: now + 3_600_000},
			// a cold lane (low score) in the big book
			"http://127.0.0.1:9004|big": {IP: "2a09:colder", Score: 0.01},
		},
	}
	lanes := []lane{{proxy: "http://127.0.0.1:9001"}, {proxy: "http://127.0.0.1:9002"},
		{proxy: "http://127.0.0.1:9003"}, {proxy: "http://127.0.0.1:9004"}}
	got := tr.pick("conv", lanes, map[string]bool{}, now, true, false)
	if got == nil || got.proxy != "http://127.0.0.1:9001" {
		t.Fatalf("big pick must take the warmest lane, got %+v", got)
	}
	// with every lane parked, the soonest-expiring one is the fallback
	tr.book["http://127.0.0.1:9001|big"] = &laneState{Until: now + 3_600_000}
	tr.book["http://127.0.0.1:9002|big"] = &laneState{Until: now + 60_000}
	tr.book["http://127.0.0.1:9004|big"] = &laneState{Until: now + 120_000}
	got = tr.pick("conv", lanes, map[string]bool{}, now, true, false)
	if got == nil || got.proxy != "http://127.0.0.1:9002" {
		t.Fatalf("big pick must fall back to the soonest expiry, got %+v", got)
	}
}

// The address reset must apply to the |big| book too (its entries never got an
// ip before, so a new tunnel inherited the old park).
func TestLanesNowResetsBigBookIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backends": []map[string]any{{"http_addr": "127.0.0.1:9001", "public_ip": "2a09:new", "state": "up"}},
		})
	}))
	defer srv.Close()
	tr := &CLITransport{
		apiURL: srv.URL,
		book: map[string]*laneState{
			"http://127.0.0.1:9001|big": {IP: "2a09:old", Score: 0.8, Until: time.Now().Add(time.Hour).UnixMilli()},
		},
	}
	tr.lanesNow()
	st := tr.book["http://127.0.0.1:9001|big"]
	if st == nil || st.IP != "2a09:new" || st.Until != 0 || st.Score != 0 {
		t.Fatalf("big book not reset on address change: %+v", st)
	}
}

// Soonest expiry keeps big bodies alive: the deepest-parked pick re-picked the
// same 4 h-windowed lane with attempts=1 and no big body could reach 200.
func TestPickBigPrefersSoonestExpiry(t *testing.T) {
	now := time.Now().UnixMilli()
	tr := &CLITransport{book: map[string]*laneState{
		"http://127.0.0.1:9001|big": {IP: "2a09:a", Until: now + 3_600_000, Score: 0.9},
		"http://127.0.0.1:9002|big": {IP: "2a09:b", Until: now + 60_000, Score: 0.9},
	}}
	lanes := []lane{{proxy: "http://127.0.0.1:9001"}, {proxy: "http://127.0.0.1:9002"}}
	got := tr.pick("conv", lanes, map[string]bool{}, now, true, false)
	if got == nil || got.proxy != "http://127.0.0.1:9002" {
		t.Fatalf("big pick must take the soonest-expiring lane, got %+v", got)
	}
}

// The gate 429s a tool schema carrying old_string/new_string; the request must
// be renamed and the response's tool-call arguments renamed back.
func TestRenameForbiddenProps(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"type":"function","function":{"name":"edit","parameters":{"type":"object","properties":{"filePath":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["filePath","old_string","new_string"]}}}]}`)
	out, changed := renameForbiddenProps(body)
	if !changed {
		t.Fatal("expected a change")
	}
	if bytes.Contains(out, []byte(`"old_string"`)) || bytes.Contains(out, []byte(`"new_string"`)) {
		t.Fatalf("forbidden props still present: %s", out)
	}
	if !bytes.Contains(out, []byte(`"oldString"`)) || !bytes.Contains(out, []byte(`"newString"`)) {
		t.Fatalf("renamed props missing: %s", out)
	}
	var doc struct {
		Tools []struct {
			Function struct {
				Parameters struct {
					Required []string `json:"required"`
				} `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(out, &doc) != nil {
		t.Fatal("renamed body is not valid JSON")
	}
	if got := doc.Tools[0].Function.Parameters.Required; len(got) != 3 || got[1] != "oldString" || got[2] != "newString" {
		t.Fatalf("required not renamed: %v", got)
	}
	// a body without tools is untouched
	if _, ch := renameForbiddenProps([]byte(`{"model":"m"}`)); ch {
		t.Fatal("body without tools must not change")
	}
}

func TestRenameToolCallArgs(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"edit","arguments":"{\"filePath\":\"a.go\",\"oldString\":\"x\",\"newString\":\"y\"}"}}]}}]}`)
	out := renameToolCallArgs(body)
	var doc struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(out, &doc) != nil {
		t.Fatalf("rewritten body invalid: %s", out)
	}
	args := doc.Choices[0].Message.ToolCalls[0].Function.Arguments
	if !strings.Contains(args, `"old_string"`) || !strings.Contains(args, `"new_string"`) {
		t.Fatalf("arguments not renamed back: %s", args)
	}
}

// Streamed tool-call arguments must come back with OMP's property names: the
// model answers with oldString/newString (it saw the renamed schema) and OMP's
// edit validator rejects the call otherwise.
func TestSSEArgRewriter(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"edit\",\"arguments\":\"{\\\"filePath\\\":\\\"a.go\\\",\\\"oldSt\"}}]}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"ring\\\":\\\"x\\\",\\\"newString\\\":\\\"y\\\"}\"}}]}}]}\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n" +
		"data: [DONE]\n"
	rc := newToolArgRewriter(io.NopCloser(strings.NewReader(sse)), "text/event-stream")
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`old_string`)) || !bytes.Contains(out, []byte(`new_string`)) {
		t.Fatalf("arguments not renamed back: %s", out)
	}
	if bytes.Contains(out, []byte(`oldString`)) || bytes.Contains(out, []byte(`newString`)) {
		t.Fatalf("renamed props leaked to the client: %s", out)
	}
	if !bytes.Contains(out, []byte(`finish_reason`)) {
		t.Fatalf("finish_reason lost: %s", out)
	}
}
