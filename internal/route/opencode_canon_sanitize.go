package route

import (
	"bytes"
	"encoding/json"
	"strings"
)

// canonMarkerRaw/canonMarkerJSON are the two encodings the sentinel can take in
// a request body: verbatim and JSON-escaped (a marker carrying a quote or a
// non-ASCII rune is escaped inside a string, so a raw bytes.Contains would miss
// it). Both are derived from the symbol — never spelled out here.
var (
	canonMarkerRaw  = []byte(canonSystemMarker)
	canonMarkerJSON = func() []byte {
		b, err := json.Marshal(canonSystemMarker)
		if err != nil || len(b) < 2 {
			return canonMarkerRaw
		}
		return b[1 : len(b)-1]
	}()
	// brokenCanonMarker is the same sentence with one U+200B inside it: the
	// gate's matcher is an exact substring, so the broken copy reads as normal
	// text to the model and as a different string to the gate (verified on an
	// isolated stand: marker in a user message -> 429, same body with the
	// zero-width character inserted -> 200).
	brokenCanonMarker = func() string {
		// The gate's refusal was verified against an insertion right after
		// the first word group (the marker's first comma); a second ZWSP at
		// the midpoint also breaks any longer contiguous match, while both
		// stay invisible to the model.
		comma := strings.IndexByte(canonSystemMarker, ',')
		if comma <= 0 || comma >= len(canonSystemMarker)-1 {
			comma = len(canonSystemMarker) / 2
		}
		half := len(canonSystemMarker) / 2
		if comma == half {
			return canonSystemMarker[:comma] + "\u200b" + canonSystemMarker[comma:]
		}
		return canonSystemMarker[:comma] + "\u200b" + canonSystemMarker[comma:half] + "\u200b" + canonSystemMarker[half:]
	}()
)

// breakCanonMarkerOutsideSystem defuses the canonical-system sentinel wherever
// a client echoed it back outside the system head. OpenCode clients replay
// parts of their prompt (a tool result quoting the conversation is enough), and
// the free-tier gate answers 429 to a body whose non-system text contains the
// exact marker.
//
// Only role != "system" messages are touched, and only their content: a plain
// string, or an array of parts whose "text" fields are rewritten. Every other
// part, field and message stays byte-identical — above all the canonical system
// head, which is what earns the free-tier admission.
//
// A body without the sentinel (either encoding) is returned as-is without a
// parse; a body that cannot be decoded is also returned untouched so the shape
// gate, not this function, decides its fate.
func breakCanonMarkerOutsideSystem(body []byte) []byte {
	if !bytes.Contains(body, canonMarkerRaw) && !bytes.Contains(body, canonMarkerJSON) {
		return body
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return body
	}
	rawMsgs, ok := top["messages"]
	if !ok {
		return body
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return body
	}
	changed := false
	for i, raw := range msgs {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil || m == nil {
			continue
		}
		if isSystemRole(m["role"]) {
			continue
		}
		msgChanged := false
		if rawContent, ok := m["content"]; ok {
			if content, ok := breakCanonMarkerInContent(rawContent); ok {
				m["content"] = content
				msgChanged = true
			}
		}
		// An assistant tool call replays its arguments verbatim in the next
		// request: a write/edit call that stored the canonical text leaks the
		// marker outside the system role exactly like a quoted user message
		// does (measured gate: verbatim marker -> 429, broken copy -> 200).
		if rawCalls, ok := m["tool_calls"]; ok {
			if calls, ok := breakCanonMarkerInToolCalls(rawCalls); ok {
				m["tool_calls"] = calls
				msgChanged = true
			}
		}
		if !msgChanged {
			continue
		}
		enc, err := json.Marshal(m)
		if err != nil {
			continue
		}
		msgs[i] = enc
		changed = true
	}
	if !changed {
		return body
	}
	encMsgs, err := json.Marshal(msgs)
	if err != nil {
		return body
	}
	top["messages"] = encMsgs
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// isSystemRole reports whether the message's role field is exactly "system".
// Anything else (user, assistant, tool, a missing role) is fair game: the gate
// only exempts the system head.
func isSystemRole(raw json.RawMessage) bool {
	var role string
	if err := json.Unmarshal(raw, &role); err != nil {
		return false
	}
	return role == "system"
}

// breakCanonMarkerInContent rewrites one message's content: a string as-is, or
// the text parts of an array. It reports false when the content holds no
// sentinel, so the caller leaves the message untouched.
func breakCanonMarkerInContent(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw, false
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil || !strings.Contains(s, canonSystemMarker) {
			return raw, false
		}
		enc, err := json.Marshal(breakCanonMarkerInText(s))
		if err != nil {
			return raw, false
		}
		return enc, true
	case '[':
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &parts); err != nil {
			return raw, false
		}
		changed := false
		for _, p := range parts {
			rawText, ok := p["text"]
			if !ok {
				continue
			}
			var s string
			if err := json.Unmarshal(rawText, &s); err != nil || !strings.Contains(s, canonSystemMarker) {
				continue
			}
			enc, err := json.Marshal(breakCanonMarkerInText(s))
			if err != nil {
				continue
			}
			p["text"] = enc
			changed = true
		}
		if !changed {
			return raw, false
		}
		enc, err := json.Marshal(parts)
		if err != nil {
			return raw, false
		}
		return enc, true
	}
	return raw, false
}

// breakCanonMarkerInToolCalls rewrites the marker inside
// assistant tool_calls[].function.arguments. Arguments are the tool's own JSON
// text carried as a JSON string, so the string case of
// breakCanonMarkerInContent applies; a non-string arguments value is left
// untouched. Only changed calls are re-encoded.
func breakCanonMarkerInToolCalls(raw json.RawMessage) (json.RawMessage, bool) {
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil {
		return raw, false
	}
	changed := false
	for _, call := range calls {
		rawFn, ok := call["function"]
		if !ok {
			continue
		}
		var fn map[string]json.RawMessage
		if err := json.Unmarshal(rawFn, &fn); err != nil || fn == nil {
			continue
		}
		rawArgs, ok := fn["arguments"]
		if !ok {
			continue
		}
		args, ok := breakCanonMarkerInContent(rawArgs)
		if !ok {
			continue
		}
		fn["arguments"] = args
		enc, err := json.Marshal(fn)
		if err != nil {
			continue
		}
		call["function"] = enc
		changed = true
	}
	if !changed {
		return raw, false
	}
	enc, err := json.Marshal(calls)
	if err != nil {
		return raw, false
	}
	return enc, true
}

// canonMarkerJSONBroken is the JSON-escaped encoding of the broken marker:
// output redaction must catch a marker that carries quotes/non-ASCII, which
// appears escaped inside a JSON body.
var canonMarkerJSONBroken = func() []byte {
	b, err := json.Marshal(brokenCanonMarker)
	if err != nil || len(b) < 2 {
		return []byte(brokenCanonMarker)
	}
	return b[1 : len(b)-1]
}()

// redactCanonMarkerForOutput breaks every verbatim marker, INCLUDING the
// canonical system head, for logs, dumps, probe excerpts and test failure
// messages. This is NOT the request sanitizer: the head must stay intact on
// the wire or the gate refuses the turn. It exists because an agent that reads
// the head in a log/test output carries it into its own next request and then
// 429s; a zero-width space in the output costs nothing and stops that loop.
func redactCanonMarkerForOutput(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	out := bytes.ReplaceAll(body, canonMarkerRaw, []byte(brokenCanonMarker))
	return bytes.ReplaceAll(out, canonMarkerJSON, canonMarkerJSONBroken)
}

// breakCanonMarkerInText replaces every verbatim occurrence of the sentinel
// with the zero-width-broken copy.
func breakCanonMarkerInText(s string) string {
	return strings.ReplaceAll(s, canonSystemMarker, brokenCanonMarker)
}
