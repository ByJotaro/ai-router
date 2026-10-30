package route

import (
	"encoding/json"
	"strings"
	"testing"
)

// The opencode inference lane gates on tool COUNT and body shape, not on the
// eleven-name canonical set (bisect 2026-10-23 on /inference: 1 tool = 403,
// 2 tools with arbitrary definitions = 200, stream:false = 403). Gated models
// therefore get exactly the gate pair backfilled (read+bash) with the real
// captured definitions; appending the whole 22 KB canonical toolset made every
// request carry schemas the client never declared.
func TestWithAgentToolsBackfillsGatePair(t *testing.T) {
	body := []byte(`{"model":"opencode/mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`)
	out := withAgentTools(body)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != len(gateToolNames) {
		t.Fatalf("tools len = %d, want %d (gate pair)", len(tools), len(gateToolNames))
	}
	byName := map[string]map[string]any{}
	for _, tl := range tools {
		fn, _ := tl.(map[string]any)["function"].(map[string]any)
		n, _ := fn["name"].(string)
		byName[n] = fn
	}
	read, ok := byName["read"]
	if !ok {
		t.Fatalf("tools = %v, want read+bash", byName)
	}
	// The captured definition (not a stub) is what ships: a one-word
	// description / empty schema has 403'd on other lanes before.
	desc, _ := read["description"].(string)
	if len(desc) < 40 {
		t.Fatalf("read description = %q, want full TUI definition", desc)
	}
	params, ok := read["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("read parameters = %v, want JSON schema", read["parameters"])
	}
	if _, ok := params["properties"]; !ok {
		t.Fatalf("read parameters = %v, want populated properties", params)
	}
	if _, ok := byName["bash"]; !ok {
		t.Fatalf("tools = %v, want bash too", byName)
	}
}

func TestWithAgentToolsBackfillsMissing(t *testing.T) {
	body := []byte(`{"model":"mimo-v2.6-flash-free","messages":[],"tools":[{"type":"function","function":{"name":"read"}}]}`)
	out := withAgentTools(body)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	tools, ok := m["tools"].([]any)
	// The stub client read is REPLACED by the canonical definition; bash is
	// appended because the gate needs two tools.
	if !ok || len(tools) != len(gateToolNames) {
		t.Fatalf("tools len = %d, want %d", len(tools), len(gateToolNames))
	}
	// every entry must now carry a real definition
	for i, tl := range tools {
		fn, _ := tl.(map[string]any)["function"].(map[string]any)
		if isStubTool(fn) {
			t.Fatalf("tools[%d] still a stub: %v", i, fn["name"])
		}
	}
}

func TestWithAgentToolsBackfillsCustom(t *testing.T) {
	body := []byte(`{"model":"mimo-v2.6-flash-free","messages":[],"tools":[{"type":"function","function":{"name":"custom","description":"c","parameters":{"type":"object"}}}]}`)
	out := withAgentTools(body)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	tools := m["tools"].([]any)
	if len(tools) != 1+len(gateToolNames) {
		t.Fatalf("tools len = %d, want 1+%d", len(tools), len(gateToolNames))
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "custom" {
		t.Fatalf("tools[0] = %v, want custom first", fn)
	}
}

func TestWithAgentToolsBadShapes(t *testing.T) {
	out := withAgentTools([]byte(`not json`))
	if string(out) != `not json` {
		t.Fatalf("non-JSON changed: %q", string(out))
	}
	// tools:"oops" (не массив) — passthrough без tools, не 500.
	out = withAgentTools([]byte(`{"model":"mimo-v2.6-flash-free","tools":"oops"}`))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["tools"] != "oops" {
		t.Fatalf("tools = %v, want untouched oops", m["tools"])
	}
	// tools:[42] (без валидных имён) — добить гейтовую пару.
	out = withAgentTools([]byte(`{"model":"mimo-v2.6-flash-free","tools":[42]}`))
	m = nil
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1+len(gateToolNames) {
		t.Fatalf("tools = %v, want [42 + gate pair]", m["tools"])
	}
}

func TestWithAgentToolsSkipsNonGated(t *testing.T) {
	// Негейтованная модель (платная zen / любой другой id) — тело байт-в-байт.
	body := []byte(`{"model":"opencode/gpt-6.1-sol","messages":[]}`)
	if got := string(withAgentTools(body)); got != string(body) {
		t.Fatalf("non-gated body changed: %s", got)
	}
}

// muse-contributor-free под тем же free-gate: без канонического тулсета
// 403 [gate-terminal] «used from within OpenCode» (2026-10-12/14).
func TestWithAgentToolsGatesMuseContributor(t *testing.T) {
	for _, model := range []string{
		"opencode/muse-spark-1.3-contributor-free",
		"muse-spark-1.2-contributor-free",
	} {
		body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`)
		out := withAgentTools(body)
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		tools, ok := m["tools"].([]any)
		if !ok || len(tools) != len(gateToolNames) {
			t.Fatalf("%s: tools len = %d, want %d", model, len(tools), len(gateToolNames))
		}
		names := []string{}
		for _, tl := range tools {
			fn, _ := tl.(map[string]any)["function"].(map[string]any)
			n, _ := fn["name"].(string)
			names = append(names, n)
		}
		if !strings.Contains(strings.Join(names, ","), "read") || !strings.Contains(strings.Join(names, ","), "bash") {
			t.Fatalf("%s: names = %v, want gate pair read+bash", model, names)
		}
	}
}

// 2026-10-17: гейт больше не по ручному списку — любой "-free" id.
// glm-5.3/deepseek-v4.1 подтверждены живой MITM-бисекцией (stream:true +
// живая session/msg + read+bash -> 200).
func TestWithAgentToolsGatesAnyFreeID(t *testing.T) {
	for _, model := range []string{
		"opencode/glm-5.3-flash-free",
		"opencode/deepseek-v4.1-flash-free",
		"opencode/some-future-model-free",
	} {
		body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`)
		out := withAgentTools(body)
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		tools, ok := m["tools"].([]any)
		if !ok || len(tools) != len(gateToolNames) {
			t.Fatalf("%s: tools len = %v, want %d", model, len(tools), len(gateToolNames))
		}
	}
}

// Regression: the responses path must ALSO carry canonical definitions
// (converter receives chat tools and re-emits them flat).
func TestChatToolsToResponsesCanonicalForOpencode(t *testing.T) {
	raw := json.RawMessage(`[{"type":"function","function":{"name":"custom","description":"c","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}}]`)
	out := chatToolsToResponses(raw, true)
	var tools []map[string]any
	if err := json.Unmarshal(out, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1+len(canonicalOpencodeTools) {
		t.Fatalf("tools len = %d, want 1+%d", len(tools), len(canonicalOpencodeTools))
	}
	if tools[0]["name"] != "custom" {
		t.Fatalf("tools[0] = %v, want client custom first", tools[0])
	}
	// a canonical read entry must carry a real schema (empty properties = gate 403)
	for _, tl := range tools[1:] {
		if tl["name"] == "read" {
			p, _ := tl["parameters"].(map[string]any)
			pr, _ := p["properties"].(map[string]any)
			if len(pr) == 0 {
				t.Fatal("read parameters empty, gate 403")
			}
			return
		}
	}
	t.Fatal("no canonical read entry backfilled")
}
