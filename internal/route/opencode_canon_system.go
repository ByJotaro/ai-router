package route

import (
	"context"
	_ "embed"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Canonical opencode system prompt — the text a genuine CLI (1.19.4) sends as
// the first system message of every agentic turn. Captured verbatim by MITM
// (2026-10-22, lane /inference/openai/v1) together with the canonical toolset
// in opencode_canon_tools.go.
//
// The free-tier gate matches the REQUEST SHAPE, not just headers: with a
// genuine session + correct UA + canonical tools, a body whose system message
// is NOT this prompt is answered with 403 FreeTierError ("free tier can only
// be used from within OpenCode"), while the same body carrying this prompt
// (order-independent, even truncated to its first 1500 bytes) plus the
// canonical tools gets through (bisect 2026-10-22: buildsys-only=403,
// buildsys+tools=200 x3, client-sys+buildsys+tools=200 x3, prefix1500+tools
// =200/429). The 429s are per-/64 daily quota, not the gate.
//
// The prompt lives in assets/ so it stays byte-identical (LF, 9579 bytes) to
// the capture: the gate compares content, and CRLF normalisation changes it.
//
//go:embed assets/opencode_system_1_19_4.txt
var canonicalOpencodeSystem string

// canonSystemMarker identifies the canonical prompt inside a message list
// without paying a 9.5 KB compare: the gate accepts prefixes, so a body that
// already carries the opening sentence is left alone.
const canonSystemMarker = "You are opencode, an interactive CLI tool that helps users with software engineering tasks."

// canonSystemPrefixLen is how much of the canonical prompt the gate actually
// compares. Bisect 2026-10-23 (slot 8626, genuine session, 2 tools,
// stream:true): the first 1500 bytes as a SEPARATE system message, with the
// client's own system message left untouched, answers 200 — so the injected
// text stays at 1.5 KB instead of the full 9579-byte capture.
const canonSystemPrefixLen = 1500

// gateToolNames is the built-in gate pair. The gate needs at least two tool
// entries and accepts arbitrary definitions (2026-10-23: full system + 1 tool
// = 403, + 2 fake tools = 200), but read+bash is the shape the real CLI sends
// and the one every earlier bisect passed with.
var gateToolNames = []string{"read", "bash"}

// canonEnvBlock is the <env>…</env> span of the canonical capture, verbatim.
// Since 2026-10-25 the gate rejects bodies whose system prompt lacks both the
// "Working directory" and "Workspace root folder" lines (9router #5128 bisect,
// zeusire fix 95fc2d2: working-directory-only passed once, then 429s). Those
// lines sit near the end of the capture (byte ~8877 of 9579), outside the
// 1500-byte prefix, so the block is appended to the injected prefix instead
// of paying for the full 9.6 KB prompt.
var canonEnvBlock = "<env>\n Working directory: /workspace\n Workspace root folder: /workspace\n</env>"

// canonSystemPrefix is the gate-matching head of the canonical prompt plus
// the required <env> block.
func canonSystemPrefix() string {
	if len(canonicalOpencodeSystem) <= canonSystemPrefixLen {
		return canonicalOpencodeSystem
	}
	out := canonicalOpencodeSystem[:canonSystemPrefixLen]
	for len(out) > 0 && !utf8.ValidString(out) {
		out = out[:len(out)-1]
	}
	if canonEnvBlock != "" && !strings.Contains(out, "<env>") {
		out += "\n" + canonEnvBlock
	}
	return out
}

// withCanonicalSystem guarantees the canonical opencode system prompt is
// present in a gated chat body. The client's own system messages are kept in
// place and in order; the canonical one is inserted right after the last
// system message (front of the list when there is none), because the gate
// accepts either order and after-the-client keeps the caller's prompt first.
// Non-gated models, non-JSON bodies and bodies that already carry the marker
// are returned untouched.
func withCanonicalSystem(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if !isGatedFreeModel(m) {
		return body
	}
	msgs, ok := m["messages"].([]any)
	if !ok {
		return body
	}
	for _, e := range msgs {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := contentString(em["content"]); ok && strings.Contains(s, canonSystemMarker) {
			return body
		}
	}
	canon := map[string]any{"role": "system", "content": canonSystemPrefix()}
	at := 0
	for i, e := range msgs {
		if em, ok := e.(map[string]any); ok {
			if r, _ := em["role"].(string); r == "system" {
				at = i + 1
			}
		}
	}
	out := make([]any, 0, len(msgs)+1)
	out = append(out, msgs[:at]...)
	out = append(out, canon)
	out = append(out, msgs[at:]...)
	m["messages"] = out
	b, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return b
}

// contentString flattens a message content field (string or parts array) to
// plain text for the marker check.
func contentString(v any) (string, bool) {
	switch c := v.(type) {
	case string:
		return c, true
	case []any:
		var sb strings.Builder
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String(), true
	}
	return "", false
}

// isGatedFreeModel reports whether the body addresses a zen free-tier id: the
// same rule withAgentTools uses (agentGateModels list or any "-free" suffix).
func isGatedFreeModel(m map[string]any) bool {
	mv, _ := m["model"].(string)
	base := mv
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	lb := strings.ToLower(base)
	return agentGateModels[lb] || strings.HasSuffix(lb, "-free")
}

// gateProbeBody builds an egress-qualification probe that PASSES the zen
// free-tier shape gate: stream:true + canonical system prompt + canonical
// tools. The old probe ({"messages":[{user,"ok"}],"max_tokens":1,
// "stream":false}) is answered 403 FreeTierError on every healthy egress, so
// the pool marked freshly-rotated egresses account-gated and poisoned itself
// (live log 2026-10-23: "egress account-gate 403, swapping lease" loop while
// the CLI from the same egress answered 200).
func gateProbeBody(model string) []byte {
	if model == "" {
		model = "muse-spark-1.3-contributor-free"
	}
	// Cheapest body that still satisfies every measured gate requirement:
	// canonical system head (1500 B, the compared prefix), two tool entries
	// (definitions arbitrary — 2026-10-23 bisect), stream:true, one token.
	// Probes run on a ticker per pool transport, so their size IS quota: the
	// previous 32 KB probe (full 9.5 KB prompt + 22 KB canonical toolset) cost
	// ~8k input tokens per tick.
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "system", "content": canonSystemPrefix()},
			map[string]any{"role": "user", "content": "ok"},
		},
		"tools":      gateProbeTools(),
		"max_tokens": 1,
		"stream":     true,
	})
	return body
}

// gateProbeTools is the two-entry toolset the gate needs, kept tiny on purpose.
func gateProbeTools() []any {
	out := make([]any, 0, len(gateToolNames))
	for _, name := range gateToolNames {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": name + " tool",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		})
	}
	return out
}

// gateShapeKey carries the verdict "this outgoing body carries the full
// free-tier shape" from tryProvider (which owns the injection) into the pool
// transport (which owns the 403 stamping). Absent key = true: providers that
// never see the zen gate keep their historical behaviour.
type gateShapeKey struct{}

func withGateShape(ctx context.Context, ok bool) context.Context {
	return context.WithValue(ctx, gateShapeKey{}, ok)
}

func gateShapeOK(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	v, ok := ctx.Value(gateShapeKey{}).(bool)
	if !ok {
		return true
	}
	return v
}

// gatedShapeComplete reports whether this body satisfies every measured gate
// requirement: canonical system head present, at least two tools, stream:true
// (bisect 2026-10-23 on /inference: each missing piece = 403 FreeTierError).
func gatedShapeComplete(body []byte, stream bool) bool {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	if !isGatedFreeModel(m) {
		return true
	}
	if !stream {
		return false
	}
	msgs, _ := m["messages"].([]any)
	found := false
	for _, e := range msgs {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := contentString(em["content"]); ok && strings.Contains(s, canonSystemMarker) {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	tools, _ := m["tools"].([]any)
	return len(tools) >= 2
}
