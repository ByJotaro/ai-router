package route

import (
	"encoding/json"
	"strings"
	"testing"
)

// The zen gateway rejects a restrictive tool_choice on thinking models (live
// 2026-10-27: "Thinking mode does not support this tool_choice", ten 400s).
// "auto" and an absent field must reach upstream untouched; everything else is
// dropped so the request can be served at all.
func TestNormalizeOpencodeToolChoice(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantDrop bool
		wantSame string // expected exact body when nothing changes
	}{
		{
			name:     "absent stays absent",
			body:     `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			wantSame: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:     "auto passes verbatim",
			body:     `{"model":"m","tool_choice":"auto","messages":[]}`,
			wantSame: `{"model":"m","tool_choice":"auto","messages":[]}`,
		},
		{
			name:     "null passes verbatim",
			body:     `{"model":"m","tool_choice":null,"messages":[]}`,
			wantSame: `{"model":"m","tool_choice":null,"messages":[]}`,
		},
		{name: "none is dropped", body: `{"model":"m","tool_choice":"none","messages":[]}`, wantDrop: true},
		{name: "required is dropped", body: `{"model":"m","tool_choice":"required","messages":[]}`, wantDrop: true},
		{
			name:     "named function is dropped",
			body:     `{"model":"m","tool_choice":{"type":"function","function":{"name":"read"}},"messages":[]}`,
			wantDrop: true,
		},
		{
			name:     "unparseable body is untouched",
			body:     `{"model":`,
			wantSame: `{"model":`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeOpencodeToolChoice([]byte(tc.body))
			if tc.wantSame != "" {
				if string(got) != tc.wantSame {
					t.Fatalf("body changed:\n got %s\nwant %s", got, tc.wantSame)
				}
				return
			}
			if !tc.wantDrop {
				t.Fatalf("case not covered")
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(got, &doc); err != nil {
				t.Fatalf("result is not JSON: %v (%s)", err, got)
			}
			if _, ok := doc["tool_choice"]; ok {
				t.Fatalf("tool_choice survived: %s", got)
			}
			if _, ok := doc["model"]; !ok {
				t.Fatalf("unrelated member lost: %s", got)
			}
		})
	}
}

// Content that looks like markup must not be HTML-escaped by the re-encode:
// prompt text carries <env> blocks and diff snippets, and a rewritten body is
// still evidence of what the client sent.
func TestNormalizeOpencodeToolChoiceKeepsMarkup(t *testing.T) {
	body := `{"model":"m","tool_choice":"none","messages":[{"role":"system","content":"<env>\na & b\n</env>"}]}`
	got := string(normalizeOpencodeToolChoice([]byte(body)))
	if strings.Contains(got, `\u003c`) || strings.Contains(got, `\u0026`) {
		t.Fatalf("markup was escaped: %s", got)
	}
	if !strings.Contains(got, "<env>") || !strings.Contains(got, "a & b") {
		t.Fatalf("content lost: %s", got)
	}
}
