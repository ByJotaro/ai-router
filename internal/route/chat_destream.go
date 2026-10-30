package route

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// Non-stream clients on the opencode free tier.
//
// The zen gate treats `"stream": false` as a non-OpenCode fingerprint: the
// real CLI always streams, so a non-stream body is answered with
// 429 FreeUsageLimitError ("Free usage limit reached") on an otherwise clean
// egress. Measured 2026-10-26, identical body/headers/session, only the flag
// differs: stream=false -> 429 (Retry-After ~41400s), stream=true -> 200,
// both direct from Bun and through the transport sidecar.
//
// The chat lane therefore always ships stream:true upstream for gated
// providers and de-streams the SSE back into one chat.completion object for
// callers that asked for JSON (same contract as the /responses lane, see
// forceStreamTrue/wrapResponsesStream).

// forceChatStreamTrue sets stream:true on a chat body, preserving every
// other field byte-for-shape (json round-trip through a map).
func forceChatStreamTrue(body []byte) []byte {
	var v map[string]json.RawMessage
	if err := json.Unmarshal(body, &v); err != nil || v == nil {
		return body
	}
	v["stream"] = json.RawMessage("true")
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}

// chatDestreamBody turns an upstream chat SSE stream into a single
// chat.completion JSON document on first Read. Close is delegated, so the
// lease/pool release path is untouched (the body is closed exactly once, by
// the same owner as before).
type chatDestreamBody struct {
	rc        io.ReadCloser
	model     string
	once      sync.Once
	closeOnce sync.Once
	buf       []byte
	off       int
	err       error
}

func newChatDestreamBody(rc io.ReadCloser, model string) *chatDestreamBody {
	return &chatDestreamBody{rc: rc, model: model}
}

func (b *chatDestreamBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		defer b.closeUpstream()
		b.buf, b.err = aggregateChatSSE(b.rc, b.model)
	})
	if b.err != nil {
		return 0, b.err
	}
	if b.off >= len(b.buf) {
		return 0, io.EOF
	}
	n := copy(p, b.buf[b.off:])
	b.off += n
	return n, nil
}

func (b *chatDestreamBody) Close() error { return b.closeUpstream() }

// closeUpstream releases the upstream body exactly once: after the
// aggregation read AND a later Close (the pool wraps bodies in a
// cancel-on-close reader whose Close is idempotent-friendly but still
// forwards to the underlying body).
func (b *chatDestreamBody) closeUpstream() error {
	var err error
	b.closeOnce.Do(func() { err = b.rc.Close() })
	return err
}

// aggregateChatSSE folds `data:` chunk events into one non-stream
// chat.completion body. Unknown events are ignored; a mid-stream error event
// becomes the aggregated error object so the client sees the real cause
// instead of an empty completion.
func aggregateChatSSE(r io.Reader, model string) ([]byte, error) {
	var (
		id      string
		created int64
		mdl     string
		content strings.Builder
		reason  strings.Builder
		finish   string
	terminal bool
		usage   json.RawMessage
	)
	type call struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Fn   struct {
			Name string `json:"name"`
			Args string `json:"arguments"`
		} `json:"function"`
	}
	calls := map[int]*call{}
	order := []int{}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			terminal = true
			continue
		}
		var ch struct {
			ID      string          `json:"id"`
			Created int64           `json:"created"`
			Model   string          `json:"model"`
			Usage   json.RawMessage `json:"usage"`
			Error   *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			Choices []struct {
				Delta struct {
					Content          *string `json:"content"`
					ReasoningContent *string `json:"reasoning_content"`
					Reasoning        *string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			continue
		}
		if ch.Error != nil {
			out, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"type":    ch.Error.Type,
					"message": ch.Error.Message,
				},
			})
			return out, nil
		}
		if ch.ID != "" {
			id = ch.ID
		}
		if ch.Created != 0 {
			created = ch.Created
		}
		if ch.Model != "" {
			mdl = ch.Model
		}
		if len(ch.Usage) > 0 && string(ch.Usage) != "null" {
			usage = ch.Usage
		}
		for _, c := range ch.Choices {
			if c.Delta.Content != nil {
				content.WriteString(*c.Delta.Content)
			}
			if c.Delta.ReasoningContent != nil {
				reason.WriteString(*c.Delta.ReasoningContent)
			} else if c.Delta.Reasoning != nil {
				reason.WriteString(*c.Delta.Reasoning)
			}
			for _, tc := range c.Delta.ToolCalls {
				cur, ok := calls[tc.Index]
				if !ok {
					cur = &call{Type: "function"}
					calls[tc.Index] = cur
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Type != "" {
					cur.Type = tc.Type
				}
				if tc.Function.Name != "" {
					cur.Fn.Name = tc.Function.Name
				}
				cur.Fn.Args += tc.Function.Arguments
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
				terminal = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// A stream that ended without a terminal marker (no `data: [DONE]` and no
	// chunk carrying a finish_reason) is truncated — e.g. WARP egress rotation
	// or a 180s idle cut. Reporting it as a complete completion would hand the
	// non-stream client a partial answer that looks finished, and record the
	// egress as successful. Surface the truncation as an error instead.
	if !terminal {
		return nil, io.ErrUnexpectedEOF
	}
	if mdl == "" {
		mdl = model
	}
	if id == "" {
		id = "chatcmpl-router"
	}
	if finish == "" {
		if len(calls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reason.Len() > 0 {
		msg["reasoning_content"] = reason.String()
	}
	if len(order) > 0 {
		list := make([]any, 0, len(order))
		for _, idx := range order {
			c := calls[idx]
			if c.ID == "" {
				c.ID = "call_" + itoa(idx)
			}
			if c.Fn.Args == "" {
				c.Fn.Args = "{}"
			}
			list = append(list, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Fn.Name,
					"arguments": c.Fn.Args,
				},
			})
		}
		msg["tool_calls"] = list
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   mdl,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if len(usage) > 0 {
		out["usage"] = usage
	}
	return json.Marshal(out)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
