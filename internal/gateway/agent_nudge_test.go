package gateway

import (
	"encoding/json"
	"testing"
)

// An agent retry must carry an explicit "execute now" turn: a byte-identical
// retry reproduced the same think-only reply and the agent stopped at a plan.
func TestNudgeAgentBody(t *testing.T) {
	body := []byte(`{"model":"opencode/x","stream":true,"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}],"messages":[{"role":"system","content":"sys"},{"role":"user","content":"do it"}]}`)
	out := nudgeAgentBody(body)
	var doc struct {
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(doc.Tools) != 1 {
		t.Fatalf("tools dropped: %d", len(doc.Tools))
	}
	if n := len(doc.Messages); n != 3 {
		t.Fatalf("want 3 messages, got %d", n)
	}
	last := doc.Messages[len(doc.Messages)-1]
	if last.Role != "user" || last.Content == "" {
		t.Fatalf("nudge missing: %+v", last)
	}
	// second pass must not stack nudges
	out2 := nudgeAgentBody(out)
	var doc2 struct {
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal(out2, &doc2)
	if len(doc2.Messages) != 3 {
		t.Fatalf("nudge stacked: %d messages", len(doc2.Messages))
	}
}
