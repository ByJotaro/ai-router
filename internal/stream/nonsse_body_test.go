package stream

import (
	"strings"
	"testing"
)

// runNonSSE feeds a plain JSON body with a non-SSE content type, the shape a
// provider that ignored stream:true answers with.
func runNonSSE(body string, allowed map[string]bool) []string {
	var out strings.Builder
	w := &recorder{b: &out}
	Passthrough(w, strings.NewReader(body), "application/json", false, true, allowed, false)
	var lines []string
	for _, ln := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(ln, "data: ") {
			lines = append(lines, strings.TrimPrefix(ln, "data: "))
		}
	}
	return lines
}

// An upstream that ignores stream:true answers a plain chat.completion body.
// Relaying it as one data event hands the client message.tool_calls it never
// parses (turn looks empty, 3 retries). Convert to deltas, filter undeclared
// names, and emit usage.
func TestPassthroughNonSSEBodyBecomesDeltas(t *testing.T) {
	up := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"echo MATRIX_OK\"}"}},{"id":"call_2","type":"function","function":{"name":"add_numbers","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
	lines := runNonSSE(up, map[string]bool{"bash": true})
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, "add_numbers") {
		t.Fatalf("undeclared call leaked from a non-SSE body: %v", lines)
	}
	if !strings.Contains(joined, `"name":"bash"`) || !strings.Contains(joined, `echo MATRIX_OK`) {
		t.Fatalf("declared call not emitted as a delta: %v", lines)
	}
	if !strings.Contains(joined, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason lost: %v", lines)
	}
	if !strings.Contains(joined, `"total_tokens":10`) {
		t.Fatalf("usage lost: %v", lines)
	}
	if !strings.HasSuffix(strings.TrimSpace(lines[len(lines)-1]), "[DONE]") && !strings.Contains(joined, "[DONE]") {
		t.Fatalf("missing [DONE]: %v", lines)
	}
}

// An error body (no choices) is relayed verbatim, never mangled into deltas.
func TestPassthroughNonSSEErrorBodyVerbatim(t *testing.T) {
	up := `{"error":{"type":"invalid_request","message":"bad"}}`
	lines := runNonSSE(up, nil)
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, `"invalid_request"`) {
		t.Fatalf("error body mangled: %v", lines)
	}
}

// A body whose every call is undeclared used to close as
// finish_reason:"tool_calls" with ZERO call frames and no explanation — the
// non-stream twin of the empty stop the incident was about. It must close as a
// plain stop with the refusal notice, like the SSE path.
func TestPassthroughNonSSEAllCallsFilteredTellsWhy(t *testing.T) {
	up := `{"id":"chatcmpl-2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_9","type":"function","function":{"name":"playwright_browser_navigate","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	lines := runNonSSE(up, map[string]bool{"read": true, "bash": true})
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, `"tool_calls"`) {
		t.Fatalf("filtered call still announced as a tool_calls frame: %v", lines)
	}
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Fatalf("a fully blocked body must close as stop, got: %v", lines)
	}
	if !strings.Contains(joined, "refused") || !strings.Contains(joined, "playwright_browser_navigate") {
		t.Fatalf("refusal notice missing or unnamed: %v", lines)
	}
	if !strings.HasSuffix(strings.TrimSpace(lines[len(lines)-1]), "[DONE]") {
		t.Fatalf("missing [DONE]: %v", lines)
	}
}
