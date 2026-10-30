package route

import (
	"bytes"
	"encoding/json"
)

// normalizeOpencodeToolChoice drops a chat `tool_choice` that the zen gateway
// refuses while the model runs in thinking mode:
//
//	Upstream request failed: [invalid_request_error] Thinking mode does not
//	support this tool_choice
//
// Measured 2026-10-27 18:08-18:17 local: ten client 400s on
// opencode/deepseek-v4.1-flash-free with bodies of 17-21 KB, all carrying a
// tool_choice the gateway would not take. "auto" and an absent field pass
// verbatim; any other value ("none", "required", a named function) is removed
// so tools stay available and the request is served. The /responses adapter
// already applies the same rule (normalizeToolChoice); the chat lane did not.
//
// The body is re-encoded without HTML escaping so string content survives
// byte-for-byte; only the removed member and JSON key order change.
func normalizeOpencodeToolChoice(body []byte) []byte {
	var probe struct {
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return body
	}
	raw := bytes.TrimSpace(probe.ToolChoice)
	if len(raw) == 0 || string(raw) == "null" {
		return body
	}
	if normalizeToolChoice(raw) != nil {
		return body // "auto" (or an equivalent empty/auto object): forwarded as-is
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	delete(doc, "tool_choice")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return body
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}
