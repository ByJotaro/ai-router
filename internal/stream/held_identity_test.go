package stream

import (
	"strings"
	"testing"
)

// Arguments-first ordering: the id/type ride on the first fragment, so the
// released call must keep them (an identity-less call cannot be executed, and
// every other writer in the repo fabricates one rather than emit empty).
func TestPassthroughHeldCallKeepsID(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_B\",\"type\":\"function\",\"function\":{\"arguments\":\"{\\\"command\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"bash\",\"arguments\":\"\\\"echo hi\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	lines, _ := runPassthroughFinal(t, up, true, map[string]bool{"bash": true})
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, `"id":"call_B"`) {
		t.Fatalf("held call lost its id: %v", lines)
	}
	if strings.Contains(joined, `"id":""`) {
		t.Fatalf("identity-less call relayed: %v", lines)
	}
	if !strings.Contains(joined, `"type":"function"`) {
		t.Fatalf("held call lost its type: %v", lines)
	}
}

// A co-located finish_reason "error" must sanitize the FILTERED payload: the
// raw one still carries the foreign call the filter removed.
func TestPassthroughErrorFinishKeepsFilter(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"e0\",\"function\":{\"name\":\"add_numbers\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"error\"}]}\n\n" +
		"data: [DONE]\n\n"
	lines, _ := runPassthroughFinal(t, up, true, map[string]bool{"bash": true})
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, "add_numbers") {
		t.Fatalf("foreign call resurrected by the error-finish branch: %v", lines)
	}
}
