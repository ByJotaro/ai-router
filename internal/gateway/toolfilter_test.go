package gateway

import (
	"encoding/json"
	"testing"
)

func TestFilterDeclaredToolCallsDropsForeignName(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
		`{"id":"1","type":"function","function":{"name":"get_time","arguments":"{}"}},` +
		`{"id":"2","type":"function","function":{"name":"add_numbers","arguments":"{\"a\":1}"}}` +
		`]},"finish_reason":"tool_calls"}]}`)
	out := filterDeclaredToolCalls(in, map[string]bool{"get_time": true})
	var doc struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten body is not JSON: %v (%s)", err, out)
	}
	calls := doc.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "get_time" {
		t.Fatalf("want only get_time, got %s", out)
	}
}

func TestFilterDeclaredToolCallsEmptiesAndStops(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
		`{"id":"2","type":"function","function":{"name":"add_numbers","arguments":"{}"}}` +
		`]},"finish_reason":"tool_calls"}]}`)
	out := filterDeclaredToolCalls(in, map[string]bool{"get_time": true})
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	c0 := doc["choices"].([]any)[0].(map[string]any)
	msg := c0["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("foreign call survived: %s", out)
	}
	if fr := c0["finish_reason"].(string); fr != "stop" {
		t.Fatalf("finish_reason = %q, want stop", fr)
	}
}

// Nothing to drop (or no allow-set at all) must return the original bytes: a
// rebuild would reorder fields and reserialize numbers for every plain answer.
func TestFilterDeclaredToolCallsLeavesCleanBodyByteExact(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"total_tokens":3}}`)
	if out := filterDeclaredToolCalls(in, map[string]bool{"get_time": true}); string(out) != string(in) {
		t.Fatalf("clean body rewritten: %s", out)
	}
	if out := filterDeclaredToolCalls(in, nil); string(out) != string(in) {
		t.Fatalf("no allow-set must be a no-op: %s", out)
	}
}

// An EMPTY non-nil allow-set means "the client declared no tools": every call
// must be dropped, matching the stream path (withAgentTools injects read/bash
// into -free bodies and a tool-less client cannot run them).
func TestFilterDeclaredToolCallsEmptySetDropsAll(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
		`{"id":"1","type":"function","function":{"name":"bash","arguments":"{}"}}` +
		`]},"finish_reason":"tool_calls"}]}`)
	out := filterDeclaredToolCalls(in, map[string]bool{})
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	c0 := doc["choices"].([]any)[0].(map[string]any)
	msg := c0["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("tool-less client received a call: %s", out)
	}
}
