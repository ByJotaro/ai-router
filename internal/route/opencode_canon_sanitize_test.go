package route

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// canonBody marshals a request body whose messages are given as generic maps.
// Tests never spell the sentinel out: they build it from the symbol.
func canonBody(t *testing.T, messages ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":    "test-model",
		"stream":   true,
		"messages": messages,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return b
}

// canonMessages decodes the messages array of a body.
func canonMessages(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var top struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	return top.Messages
}

// canonMessageFields decodes one message into its raw fields.
func canonMessageFields(t *testing.T, msg json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(msg, &m); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	return m
}

// canonContentText renders a message's content as plain text: the string
// itself, or the concatenated text parts of an array.
func canonContentText(t *testing.T, msg json.RawMessage) (string, bool) {
	t.Helper()
	raw, ok := canonMessageFields(t, msg)["content"]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("content is neither a string nor a parts array")
	}
	var out strings.Builder
	for _, p := range parts {
		if txt, ok := p["text"]; ok {
			var part string
			if err := json.Unmarshal(txt, &part); err == nil {
				out.WriteString(part)
			}
		}
	}
	return out.String(), true
}

// The canonical system head carries the sentinel by design: it must come out
// byte-identical, down to the raw message encoding, even when a later message
// is rewritten. Only the echoed copy moves.
func TestBreakCanonMarkerSystemMessageUntouched(t *testing.T) {
	system := "canonical head: " + canonSystemMarker + " end of head"
	body := canonBody(t,
		map[string]any{"role": "system", "content": system},
		map[string]any{"role": "user", "content": "quoting the prompt: " + canonSystemMarker},
	)
	out := breakCanonMarkerOutsideSystem(body)
	if bytes.Equal(out, body) {
		t.Fatalf("the echoed user copy was not rewritten")
	}
	orig := canonMessages(t, body)
	got := canonMessages(t, out)
	if len(got) != 2 {
		t.Fatalf("message count changed: %d", len(got))
	}
	if !bytes.Equal(orig[0], got[0]) {
		t.Fatalf("system message encoding changed")
	}
	if sysText, _ := canonContentText(t, got[0]); sysText != system {
		t.Fatalf("system content changed")
	}
	userText, _ := canonContentText(t, got[1])
	if strings.Contains(userText, canonSystemMarker) {
		t.Fatalf("user content still carries the verbatim sentinel")
	}
	if !strings.Contains(userText, brokenCanonMarker) {
		t.Fatalf("user content does not carry the broken sentinel")
	}
}

// A body without the sentinel must not be re-encoded at all: the slice comes
// back as the same bytes (the fast path skips the parse).
func TestBreakCanonMarkerNoOpWhenAbsent(t *testing.T) {
	body := canonBody(t,
		map[string]any{"role": "system", "content": "plain head"},
		map[string]any{"role": "user", "content": "plain question"},
	)
	out := breakCanonMarkerOutsideSystem(body)
	if len(out) == 0 || len(body) == 0 {
		t.Fatalf("unexpected empty body")
	}
	if &out[0] != &body[0] {
		t.Fatalf("body without the sentinel was re-encoded")
	}

	// Malformed JSON must be returned untouched: the shape gate owns it.
	bad := append([]byte(`{"messages":[`), []byte(canonSystemMarker)...)
	out = breakCanonMarkerOutsideSystem(bad)
	if !bytes.Equal(out, bad) {
		t.Fatalf("invalid JSON was rewritten")
	}
}

// String content is rewritten for every non-system role.
func TestBreakCanonMarkerStringContentByRole(t *testing.T) {
	for _, role := range []string{"user", "assistant", "tool"} {
		t.Run(role, func(t *testing.T) {
			content := "before " + canonSystemMarker + " after"
			body := canonBody(t, map[string]any{"role": role, "content": content})
			out := breakCanonMarkerOutsideSystem(body)
			msgs := canonMessages(t, out)
			if len(msgs) != 1 {
				t.Fatalf("message count changed: %d", len(msgs))
			}
			var gotRole string
			if err := json.Unmarshal(canonMessageFields(t, msgs[0])["role"], &gotRole); err != nil || gotRole != role {
				t.Fatalf("role not preserved: %q", gotRole)
			}
			text, _ := canonContentText(t, msgs[0])
			want := strings.ReplaceAll(content, canonSystemMarker, brokenCanonMarker)
			if text != want {
				t.Fatalf("string content not rewritten exactly")
			}
		})
	}
}

// Array content: text parts are rewritten, non-text parts and their extra
// fields survive.
func TestBreakCanonMarkerArrayPartsPreserved(t *testing.T) {
	cacheControl := map[string]any{"type": "ephemeral"}
	body := canonBody(t,
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{
				"type":          "text",
				"text":          "prefix " + canonSystemMarker + " suffix",
				"cache_control": cacheControl,
			},
			map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:image/png;base64,AAAA"},
			},
		}},
	)
	out := breakCanonMarkerOutsideSystem(body)
	msgs := canonMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("message count changed: %d", len(msgs))
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(canonMessageFields(t, msgs[0])["content"], &parts); err != nil {
		t.Fatalf("parts not decodable: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("part count changed: %d", len(parts))
	}
	var text string
	if err := json.Unmarshal(parts[0]["text"], &text); err != nil {
		t.Fatalf("text part not a string: %v", err)
	}
	if want := "prefix " + brokenCanonMarker + " suffix"; text != want {
		t.Fatalf("text part not rewritten exactly")
	}
	var gotCache map[string]any
	if err := json.Unmarshal(parts[0]["cache_control"], &gotCache); err != nil || gotCache["type"] != "ephemeral" {
		t.Fatalf("cache_control lost from the text part")
	}
	var typ string
	if err := json.Unmarshal(parts[1]["type"], &typ); err != nil || typ != "image_url" {
		t.Fatalf("image part type lost")
	}
	var img struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(parts[1]["image_url"], &img); err != nil || img.URL != "data:image/png;base64,AAAA" {
		t.Fatalf("image part payload lost")
	}
	if _, ok := parts[1]["text"]; ok {
		t.Fatalf("image part grew a text field")
	}
}

// Everything that is not the touched content stays as it was: top-level fields,
// tools, and the message fields around the content.
func TestBreakCanonMarkerOtherFieldsPreserved(t *testing.T) {
	body := []byte(`{"model":"test-model","stream":true,"temperature":0.25,"tools":[{"type":"function","function":{"name":"f"}}],"messages":[`)
	encMsgs, err := json.Marshal([]any{
		map[string]any{"role": "user", "content": "hey " + canonSystemMarker},
		map[string]any{
			"role":         "tool",
			"name":         "shell",
			"tool_call_id": "call_1",
			"content":      "result " + canonSystemMarker,
		},
	})
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	body = append(body, encMsgs[1:len(encMsgs)-1]...)
	body = append(body, []byte(`]}`)...)

	out := breakCanonMarkerOutsideSystem(body)
	var origTop, gotTop map[string]json.RawMessage
	if err := json.Unmarshal(body, &origTop); err != nil {
		t.Fatalf("decode original: %v", err)
	}
	if err := json.Unmarshal(out, &gotTop); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	for _, key := range []string{"model", "stream", "temperature", "tools"} {
		if !bytes.Equal(origTop[key], gotTop[key]) {
			t.Fatalf("top-level field %q changed", key)
		}
	}
	msgs := canonMessages(t, out)
	if len(msgs) != 2 {
		t.Fatalf("message count changed: %d", len(msgs))
	}
	toolMsg := canonMessageFields(t, msgs[1])
	var role, name, callID string
	if err := json.Unmarshal(toolMsg["role"], &role); err != nil || role != "tool" {
		t.Fatalf("tool role lost")
	}
	if err := json.Unmarshal(toolMsg["name"], &name); err != nil || name != "shell" {
		t.Fatalf("tool name lost")
	}
	if err := json.Unmarshal(toolMsg["tool_call_id"], &callID); err != nil || callID != "call_1" {
		t.Fatalf("tool_call_id lost")
	}
	text, _ := canonContentText(t, msgs[1])
	if strings.Contains(text, canonSystemMarker) || !strings.Contains(text, brokenCanonMarker) {
		t.Fatalf("tool content not rewritten")
	}
}

// An assistant tool call replays its arguments in every later request: a
// write/edit that stored the canonical text must be defused too, or the gate
// sees the verbatim marker outside the system role and 429s the turn.
func TestBreakCanonMarkerToolCallArguments(t *testing.T) {
	args := `{"content":"` + canonSystemMarker + `"}`
	body := canonBody(t, map[string]any{
		"role": "assistant",
		"tool_calls": []any{map[string]any{
			"id":   "call_1",
			"type": "function",
			"function": map[string]any{
				"name":      "write",
				"arguments": args,
			},
		}},
	})
	out := breakCanonMarkerOutsideSystem(body)
	msgs := canonMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("message count changed: %d", len(msgs))
	}
	var calls []map[string]json.RawMessage
	rawCalls, ok := canonMessageFields(t, msgs[0])["tool_calls"]
	if !ok {
		t.Fatalf("tool_calls lost")
	}
	if err := json.Unmarshal(rawCalls, &calls); err != nil || len(calls) != 1 {
		t.Fatalf("tool_calls not decodable: %v", err)
	}
	var fn map[string]json.RawMessage
	if err := json.Unmarshal(calls[0]["function"], &fn); err != nil {
		t.Fatalf("function not decodable: %v", err)
	}
	var got string
	if err := json.Unmarshal(fn["arguments"], &got); err != nil {
		t.Fatalf("arguments is not a string: %v", err)
	}
	if strings.Contains(got, canonSystemMarker) {
		t.Fatalf("verbatim marker left in tool arguments")
	}
	if want := strings.ReplaceAll(args, canonSystemMarker, brokenCanonMarker); got != want {
		t.Fatalf("arguments changed beyond the marker")
	}
	var id, name string
	if err := json.Unmarshal(calls[0]["id"], &id); err != nil {
		t.Fatalf("call id not a string: %v", err)
	}
	if err := json.Unmarshal(fn["name"], &name); err != nil {
		t.Fatalf("function name lost")
	}
	if id != "call_1" || name != "write" {
		t.Fatalf("call identity changed: %q %q", id, name)
	}
}

// The sanitizer must run before the head injection. withCanonicalSystem treats
// a marker in ANY role as "head already present"; sanitizing afterwards broke
// the echo but left the request without the canonical head, so the gate
// rejected a healthy egress (403/429) on every turn quoting its own prompt.
func TestCanonEchoDoesNotSuppressHeadInjection(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		// withCanonicalSystem/withAgentTools only act on gated ids (… -free).
		"model":  "deepseek-v4.1-flash-free",
		"stream": true,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": "quoting the prompt: " + canonSystemMarker,
		}},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	hasSystemMarker := func(b []byte) bool {
		for _, msg := range canonMessages(t, b) {
			var role string
			_ = json.Unmarshal(canonMessageFields(t, msg)["role"], &role)
			if role != "system" {
				continue
			}
			if text, ok := canonContentText(t, msg); ok && strings.Contains(text, canonSystemMarker) {
				return true
			}
		}
		return false
	}
	oldOrder := breakCanonMarkerOutsideSystem(withCanonicalSystem(withAgentTools(body)))
	if hasSystemMarker(oldOrder) {
		t.Fatalf("test assumption stale: the old order already injected the head")
	}
	shaped := withCanonicalSystem(breakCanonMarkerOutsideSystem(withAgentTools(body)))
	if !hasSystemMarker(shaped) {
		t.Fatalf("sanitized echo still suppressed head injection")
	}
	for _, msg := range canonMessages(t, shaped) {
		var role string
		_ = json.Unmarshal(canonMessageFields(t, msg)["role"], &role)
		if role == "system" {
			continue
		}
		if text, ok := canonContentText(t, msg); ok && strings.Contains(text, canonSystemMarker) {
			t.Fatalf("verbatim echo left outside the system role")
		}
	}
}

// Live probe for the exact incident: a client message that quotes the
// canonical marker must still be admitted (echo sanitized, head injected).
// Without the fix the injector bailed out, the request left head-less and the
// gate answered 429 on a healthy egress. Run only against a promoted router:
//
//	AI_ROUTER_LIVE_CANON_PROBE=1 AI_ROUTER_TOKEN=… go test ./internal/route -run LiveCanon -count=1
func TestLiveCanonEchoAdmitted(t *testing.T) {
	if os.Getenv("AI_ROUTER_LIVE_CANON_PROBE") == "" {
		t.Skip("set AI_ROUTER_LIVE_CANON_PROBE=1 to probe the promoted router")
	}
	token := strings.TrimSpace(os.Getenv("AI_ROUTER_TOKEN"))
	if token == "" {
		t.Fatal("AI_ROUTER_TOKEN is required")
	}
	body, err := json.Marshal(map[string]any{
		"model":      "opencode/deepseek-v4.1-flash-free",
		"stream":     false,
		"max_tokens": 8,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": "Reply with OK only. Quoted prompt: " + canonSystemMarker,
		}},
	})
	if err != nil {
		t.Fatalf("marshal probe: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:9001/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("probe request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("live probe: %v", err)
	}
	defer resp.Body.Close()
	// Do not print the body: the model may echo the quoted marker.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live probe status %d: the gate rejected a shaped echo", resp.StatusCode)
	}
}

// Output redaction is not the request sanitizer: it must break the marker even
// in a system head, which the request path deliberately preserves. Without it,
// a test/log line carrying the head becomes the head in an agent's next
// request, and that request 429s.
func TestRedactCanonMarkerForOutputBreaksHead(t *testing.T) {
	body := canonBody(t, map[string]any{"role": "system", "content": canonSystemMarker})
	out := redactCanonMarkerForOutput(body)
	if bytes.Contains(out, canonMarkerRaw) || bytes.Contains(out, canonMarkerJSON) {
		t.Fatalf("verbatim marker left in output")
	}
	if !bytes.Contains(out, []byte(brokenCanonMarker)) {
		t.Fatalf("broken marker missing from output")
	}
	if kept := breakCanonMarkerOutsideSystem(body); !bytes.Equal(kept, body) {
		t.Fatalf("request sanitizer rewrote the system head")
	}
}
