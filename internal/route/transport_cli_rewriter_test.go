package route

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

type scriptedStep struct {
	data []byte
	err  error
}

// scriptedReadCloser replays canned reads: the shape of a stream that produced
// data and then died (idle timeout / reset) instead of finishing.
type scriptedReadCloser struct {
	steps []scriptedStep
	i     int
}

func (s *scriptedReadCloser) Read(p []byte) (int, error) {
	if s.i >= len(s.steps) {
		return 0, io.EOF
	}
	st := s.steps[s.i]
	s.i++
	n := copy(p, st.data)
	if n < len(st.data) {
		// keep the remainder for the next call
		s.steps[s.i-1] = scriptedStep{data: st.data[n:], err: st.err}
		s.i--
		return n, nil
	}
	return n, st.err
}

func (s *scriptedReadCloser) Close() error { return nil }

// A read error underneath the rewriter must reach the caller as that error, not
// as io.EOF: a truncated stream reported as a clean end is what makes the agent
// stop after one tool call (no finish_reason, so the pending calls never run).
func TestSSEArgRewriterPropagatesReadError(t *testing.T) {
	r := &scriptedReadCloser{steps: []scriptedStep{
		{data: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")},
		{err: os.ErrDeadlineExceeded},
	}}
	rw := newSSEArgRewriter(r)
	got, err := io.ReadAll(rw)
	if err == nil {
		t.Fatalf("rewriter swallowed the read error, returned %d bytes and nil error", len(got))
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("got %v, want os.ErrDeadlineExceeded", err)
	}
	if len(got) == 0 {
		t.Fatalf("data before the failure was lost")
	}
}

// A genuine EOF must still be a clean end of stream.
func TestSSEArgRewriterCleanEOFStaysEOF(t *testing.T) {
	r := &scriptedReadCloser{steps: []scriptedStep{
		{data: []byte("data: x\n\n")},
		{err: io.EOF},
	}}
	rw := newSSEArgRewriter(r)
	got, err := io.ReadAll(rw)
	if err != nil {
		t.Fatalf("clean EOF surfaced as %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("data lost on clean EOF")
	}
}

// The first fragment of a tool call often carries the NAME and a tiny args
// prefix that is fully held back by the rename carry. That chunk must still be
// relayed: dropping it left the client with a nameless call and empty
// arguments (2026-11-20 regression).
func TestSSEArgRewriterKeepsNameOnCarriedFragment(t *testing.T) {
	up := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"edit","arguments":"{\"oldStr"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ing\":\"x\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"
	rw := newSSEArgRewriter(io.NopCloser(strings.NewReader(up)))
	out, err := io.ReadAll(rw)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(out)
	// The SSE passthrough only parses "data: " lines; a rewritten chunk written
	// as "data:{...}" is dropped and the client sees the "{}" backfill instead.
	for _, ln := range strings.Split(text, "\n") {
		if strings.Contains(ln, `"arguments"`) && !strings.HasPrefix(ln, "data: ") {
			t.Fatalf("rewritten chunk lost the SSE prefix: %q", ln)
		}
	}
	if !strings.Contains(text, `"name":"edit"`) || !strings.Contains(text, `"id":"c1"`) {
		t.Fatalf("name/id chunk dropped: %s", text)
	}
	if strings.Contains(text, "oldString") {
		t.Fatalf("renamed key leaked: %s", text)
	}
	// Assembled arguments must be valid and renamed back.
	var args strings.Builder
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var d struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(line), &d) != nil {
			continue
		}
		for _, c := range d.Choices {
			for _, tc := range c.Delta.ToolCalls {
				args.WriteString(tc.Function.Arguments)
			}
		}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args.String()), &m); err != nil {
		t.Fatalf("assembled args invalid: %v (%q)", err, args.String())
	}
	if m["old_string"] != "x" {
		t.Fatalf("rename round trip failed: %v", m)
	}
}

// The upstream's last content delta usually shares its chunk with
// finish_reason. Dropping that whole chunk to re-emit a clean finish cut every
// long text answer mid-word (2026-11-23: deepseek content_bytes=2073/4519/9035/
// 10692, tails mid-word) while tool calls survived. The content must reach the
// client, then the synthesized finish.
func TestSSEArgRewriterKeepsContentOnFinishChunk(t *testing.T) {
	up := `data: {"choices":[{"delta":{"content":"tail text"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":" final words"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	rw := newSSEArgRewriter(io.NopCloser(strings.NewReader(up)))
	out, err := io.ReadAll(rw)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "final words") {
		t.Fatalf("final content delta dropped from the stream: %s", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("finish chunk lost: %s", text)
	}
}

func TestSSEArgRewriterInterleavedCalls(t *testing.T) {
	up := `data: {"choices":[{"delta":{"tool_calls":[` +
		`{"index":0,"id":"c0","type":"function","function":{"name":"read","arguments":"{\"oldStr"}},` +
		`{"index":1,"id":"c1","type":"function","function":{"name":"edit","arguments":"{\"newStr"}}` +
		`]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ing\":\"a\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"ing\":\"b\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"
	rw := newSSEArgRewriter(io.NopCloser(strings.NewReader(up)))
	out, err := io.ReadAll(rw)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	byIndex := map[int]string{}
	names := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var d struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d) != nil {
			continue
		}
		for _, c := range d.Choices {
			for _, tc := range c.Delta.ToolCalls {
				if tc.Function.Name != "" {
					names[tc.Index] = tc.Function.Name
				}
				byIndex[tc.Index] += tc.Function.Arguments
			}
		}
	}
	if names[0] != "read" || names[1] != "edit" {
		t.Fatalf("names lost: %v", names)
	}
	for idx, want := range map[int]string{0: "a", 1: "b"} {
		var m map[string]any
		if err := json.Unmarshal([]byte(byIndex[idx]), &m); err != nil {
			t.Fatalf("call %d args invalid: %v (%q)", idx, err, byIndex[idx])
		}
		key := "old_string"
		if idx == 1 {
			key = "new_string"
		}
		if m[key] != want {
			t.Fatalf("call %d: %s = %v, want %q (args %q)", idx, key, m[key], want, byIndex[idx])
		}
	}
}
