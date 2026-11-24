package route

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The first run of the in-process transport must import the sidecar's egress
// book: a cold book sweeps blind on a burned fleet and answers 429 on nearly
// every turn (measured 2026-11-18: 78% vs 20% on the sidecar path).
func TestImportSidecarBook(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "v2_state.json")
	doc := map[string]any{
		"egress": map[string]any{
			"http://127.0.0.1:9719|inference": map[string]any{
				"proxy": "http://127.0.0.1:9719", "ip": "2a09:good",
				"ok": 200, "bad": 0, "until": 0, "lastOK": 1763500000000, "seen": 200,
			},
			"http://127.0.0.1:9736|inference": map[string]any{
				"proxy": "http://127.0.0.1:9736", "ip": "2a09:burned",
				"ok": 0, "bad": 40, "until": 1763600000000, "lastOK": 0, "seen": 40,
			},
		},
	}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(state, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	tr := &CLITransport{
		bookPath:         filepath.Join(dir, "book.json"), // does not exist yet
		sidecarStatePath: state,
		book:             map[string]*laneState{},
	}
	tr.loadBook()

	if len(tr.book) != 2 {
		t.Fatalf("book not imported: %+v", tr.book)
	}
	good := tr.book["http://127.0.0.1:9719"]
	burned := tr.book["http://127.0.0.1:9736"]
	if good == nil || good.Score != 1 || good.LastOK == 0 {
		t.Fatalf("good lane not imported with its reputation: %+v", good)
	}
	if burned == nil || burned.Until == 0 || burned.Bad != 40 {
		t.Fatalf("burned lane not imported with its park: %+v", burned)
	}
}
