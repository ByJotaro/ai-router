package route

import (
	"encoding/json"
	"strings"
	"testing"
)

// The zen free-tier shape gate is content-matched (2026-10-22 bisect): the
// canonical system prompt plus the canonical toolset is the difference
// between 403 FreeTierError and 200 on the same egress/session/headers.

func TestWithCanonicalSystemInjectsAfterClientSystem(t *testing.T) {
	body := []byte(`{"model":"opencode/deepseek-v4.1-flash-free","messages":[` +
		`{"role":"system","content":"You are OMP."},` +
		`{"role":"user","content":"hi"}]}`)
	out := withCanonicalSystem(body)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d: %s", len(msgs), out)
	}
	roles := []string{}
	for _, e := range msgs {
		roles = append(roles, e.(map[string]any)["role"].(string))
	}
	if roles[0] != "system" || roles[1] != "system" || roles[2] != "user" {
		t.Fatalf("client system must stay first, canonical second: %v", roles)
	}
	if got := msgs[0].(map[string]any)["content"]; got != "You are OMP." {
		t.Fatalf("client system content rewritten: %v", got)
	}
	// Only the gate-matched head ships (1500 B measurably passes), not the
	// whole 9579-byte capture.
	if got := msgs[1].(map[string]any)["content"].(string); got != canonSystemPrefix() {
		t.Fatalf("canonical head not injected verbatim (len %d want %d)", len(got), len(canonSystemPrefix()))
	}
}

func TestWithCanonicalSystemIdempotent(t *testing.T) {
	once := withCanonicalSystem([]byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}]}`))
	twice := withCanonicalSystem(once)
	if string(once) != string(twice) {
		t.Fatalf("second pass changed the body:\n%s\n%s", once, twice)
	}
	if n := strings.Count(string(twice), canonSystemMarker); n != 1 {
		t.Fatalf("canonical marker count = %d, want 1", n)
	}
}

func TestWithCanonicalSystemPrependsWhenNoSystemMessage(t *testing.T) {
	out := withCanonicalSystem([]byte(`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}]}`))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("want canonical system first, got %s", out)
	}
}

func TestWithCanonicalSystemSkipsNonGatedModels(t *testing.T) {
	body := []byte(`{"model":"kilocode/z-ai/glm-4.6","messages":[{"role":"user","content":"hi"}]}`)
	if got := string(withCanonicalSystem(body)); got != string(body) {
		t.Fatalf("non-gated model rewritten: %s", got)
	}
	if got := string(withCanonicalSystem([]byte(`not json`))); got != "not json" {
		t.Fatalf("non-JSON body rewritten: %s", got)
	}
}

// The pool's own qualification probes must pass the same gate the live traffic
// does; the pre-fix shape ({"max_tokens":1,"stream":false}, no system/tools)
// is 403 on every healthy egress and poisoned the pool.
func TestGateProbeBodyPassesShapeGate(t *testing.T) {
	raw := gateProbeBody("deepseek-v4.1-flash-free")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("probe body is not JSON: %v", err)
	}
	if m["stream"] != true {
		t.Fatalf("probe must stream (stream:false is gate-403): %s", raw)
	}
	msgs := m["messages"].([]any)
	if len(msgs) < 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("probe must carry the canonical system message: %s", raw)
	}
	if msgs[0].(map[string]any)["content"] != canonSystemPrefix() {
		t.Fatal("probe system message must be the gate-matched canonical head")
	}
	// Probes are billed against the free-tier quota, so they stay at the
	// measured minimum: two tool entries, tiny bodies (the pre-shrink probe
	// carried 32 KB and cost ~8k input tokens per tick).
	tools := m["tools"].([]any)
	if len(tools) != len(gateToolNames) {
		t.Fatalf("probe tools = %d, want %d gate entries", len(tools), len(gateToolNames))
	}
	for _, tl := range tools {
		fn, _ := tl.(map[string]any)["function"].(map[string]any)
		if fn["name"] == "" || fn["parameters"] == nil {
			t.Fatalf("probe tool lost its shape: %v", fn)
		}
	}
	if len(raw) > 4096 {
		t.Fatalf("probe body = %d bytes, want the cheap shape (<4 KB)", len(raw))
	}
	if probe := gateProbeBody(""); !strings.Contains(string(probe), "muse-spark-1.3-contributor-free") {
		t.Fatalf("empty model must fall back to the prewarm default: %s", probe)
	}
}

// Cross-egress identity isolation: when the harvest pool is dry, egresses must
// NOT share one fallback session (upstream bans correlate shared client
// factors, not only IPs).
func TestFallbackSessionPerEgress(t *testing.T) {
	a := fallbackSessionFor("ip-a")
	b := fallbackSessionFor("ip-b")
	if a == b {
		t.Fatalf("two egresses share fallback session %q", a)
	}
	if a != fallbackSessionFor("ip-a") {
		t.Fatal("fallback session not stable for the same egress")
	}
	if len(a) < 20 {
		t.Fatalf("fallback session too short to look like ses_*: %q", a)
	}
	// An empty key has no egress: it must yield NO identity, not a shared one.
	if got := fallbackSessionFor(""); got != "" {
		t.Fatalf("empty key produced shared fallback session %q", got)
	}
}
