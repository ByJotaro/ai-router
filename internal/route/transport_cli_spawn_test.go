package route

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSpawnAndRetireWideLane pins the drain-and-replace path: a replacement
// worker is spawned on a free port pair (row appended atomically), and the
// retire step kills it and removes the row so the pool does not grow forever.
func TestSpawnAndRetireWideLane(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LOCALAPPDATA", base)
	acc := "00000000-0000-0000-0000-000000000001"
	if err := os.MkdirAll(filepath.Join(base, "warp-router", "data", "accounts", acc), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "wide_state.json")
	rows := []map[string]any{{"i": 0, "node": 19100, "acc": acc, "socks": 10100, "http": 9700}}
	b, _ := json.Marshal(rows)
	if err := os.WriteFile(state, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_ROUTER_WARP_ROUTER_EXE", "cmd")
	tr := &CLITransport{wideState: state, active: map[string]int{}, pendingRedraw: map[string]bool{}, book: map[string]*laneState{}}
	lane := "http://127.0.0.1:8750"
	tr.spawnWideLane(lane)
	read := func() []map[string]any {
		raw, err := os.ReadFile(state)
		if err != nil {
			t.Fatal(err)
		}
		var out []map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got := read()
	if len(got) != 2 {
		t.Fatalf("rows after spawn = %d, want 2", len(got))
	}
	hp, _ := got[1]["http"].(float64)
	sp, _ := got[1]["socks"].(float64)
	if int(hp) < 9710 || int(hp) >= 9800 {
		t.Fatalf("spawned http port = %v, want a free port in [9710,9800)", got[1]["http"])
	}
	if int(sp) != int(hp)+400 {
		t.Fatalf("spawned socks port = %v, want %v", got[1]["socks"], int(hp)+400)
	}
	tr.retireWideLane(lane)
	got = read()
	if len(got) != 1 {
		t.Fatalf("rows after retire = %d, want 1", len(got))
	}
	if _, ok := wideSpawned[lane]; ok {
		t.Fatalf("spawn record still present after retire")
	}
}
