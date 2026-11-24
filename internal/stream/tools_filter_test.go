package stream

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The model invents tool names for capabilities it was not given (measured
// 5/6 on inferway qwen). A declared-set filter turns that into a recoverable
// turn instead of the client's "requested unavailable tool" failure.
func TestPassthroughDropsUndeclaredToolCall(t *testing.T) {
	up := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","function":{"name":"add_numbers","arguments":"{\"a\":17}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	lines, _ := runPassthroughFinal(t, up, true, map[string]bool{"get_time": true})
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, `"tool_calls"`) {
		t.Fatalf("undeclared call reached the client as a call: %v", lines)
	}
	// The refusal is stated in text so the agent can recover instead of
	// retrying the same impossible call until its retry cap.
	if !strings.Contains(joined, "add_numbers") || !strings.Contains(joined, "not declared") {
		t.Fatalf("refusal notice missing: %v", lines)
	}
	if strings.Contains(joined, `"finish_reason":"tool_calls"`) {
		t.Fatalf("an empty turn must not claim tool_calls: %v", lines)
	}
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Fatalf("expected a stop close-out: %v", lines)
	}
}

// A declared call passes through untouched, arguments included.
func TestPassthroughKeepsDeclaredToolCall(t *testing.T) {
	up := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","function":{"name":"add_numbers","arguments":"{\"a\":1"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":",\"b\":2}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	lines, _ := runPassthroughFinal(t, up, true, map[string]bool{"add_numbers": true})
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, "add_numbers") || !strings.Contains(joined, `"arguments":"{\"a\":1"`) {
		t.Fatalf("declared call was altered: %v", lines)
	}
	if !strings.Contains(joined, `"finish_reason":"tool_calls"`) {
		t.Fatalf("declared call must keep its finish: %v", lines)
	}
}

// A stream cut mid-arguments must not be announced as a completed call: the
// client would parse unterminated JSON and lose the turn.
func TestPassthroughCutMidArgsSaysLength(t *testing.T) {
	const chunk = "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"t1\",\"function\":{\"name\":\"add_numbers\",\"arguments\":\"{\\\"a\\\":1\"}}]}}]}\n\n"
	var out strings.Builder
	w := &recorder{b: &out}
	_, retry := Passthrough(w, &truncatedReader{r: strings.NewReader(chunk), err: errors.New("read: connection reset by peer")},
		"text/event-stream", false, true, map[string]bool{"add_numbers": true}, false)
	if retry {
		t.Fatal("truncation after a flushed call must not retry")
	}
	body := out.String()
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("cut mid-args must close as length: %q", body)
	}
	if strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("invalid arguments announced as a completed call: %q", body)
	}
}

// A finish=stop turn with nothing in it is not a success: the caller retries
// once on a fresh lease before the client sees an empty assistant message.
func TestPassthroughEmptyTurnRetries(t *testing.T) {
	up := `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	lines, retry := runPassthroughFinal(t, up, false, nil)
	if !retry {
		t.Fatal("empty turn must be retryable on a non-final pass")
	}
	if len(lines) != 0 {
		t.Fatalf("retryable empty turn must not flush anything: %v", lines)
	}
}

// The final pass must emit exactly one finish chunk: a synthesized stop after
// an upstream "length" would let "last wins" erase the truncation verdict.
func TestPassthroughEmptyTurnFinalSingleFinish(t *testing.T) {
	for _, up := range []string{
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n",
	} {
		lines, retry := runPassthroughFinal(t, up, true, nil)
		if retry {
			t.Fatal("final pass must not ask for a retry")
		}
		joined := strings.Join(lines, "|")
		if n := strings.Count(joined, `"finish_reason"`); n != 1 {
			t.Fatalf("finish chunks = %d, want 1: %v", n, lines)
		}
	}
}

// A usage-only chunk and the role preamble must survive the tool filter
// byte-for-byte: dropping them loses the final chunk of every request that
// declares tools, so RecordUsageByKey undercounts.
func TestPassthroughKeepsUsageAndRoleWithTools(t *testing.T) {
	up := `data: {"choices":[{"delta":{"role":"assistant"}}]}

data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}

data: [DONE]

`
	var out strings.Builder
	w := &recorder{b: &out}
	usg, _ := Passthrough(w, strings.NewReader(up), "text/event-stream", false, true,
		map[string]bool{"get_time": true}, false)
	body := out.String()
	if usg.TotalTokens != 5 || usg.PromptTokens != 3 {
		t.Fatalf("usage lost through filter: %+v", usg)
	}
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("role preamble dropped: %q", body)
	}
	if !strings.Contains(body, `"usage":`) {
		t.Fatalf("usage chunk dropped: %q", body)
	}
}

// A delta can carry prose and a foreign call together: dropping the frame
// whole silently truncated the answer (reviewer finding 1).
func TestPassthroughKeepsContentBesideDroppedCall(t *testing.T) {
	up := `data: {"choices":[{"delta":{"role":"assistant"}}]}

data: {"choices":[{"delta":{"content":"It is ","tool_calls":[{"index":0,"function":{"name":"get_weather","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	lines, _ := runPassthroughFinal(t, up, true, map[string]bool{"get_time": true})
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, `"content":"It is "`) {
		t.Fatalf("prose beside a dropped call was lost: %v", lines)
	}
	if strings.Contains(joined, "get_weather") {
		t.Fatalf("foreign call leaked: %v", lines)
	}
	if !strings.Contains(joined, `"role":"assistant"`) {
		t.Fatalf("role preamble lost: %v", lines)
	}
}

// An empty non-nil allow-set means "the client declared nothing": every call
// must be dropped, including the pair withAgentTools injects for -free models.
func TestPassthroughEmptyAllowSetDropsEverything(t *testing.T) {
	up := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"bash","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	lines, _ := runPassthroughFinal(t, up, true, map[string]bool{})
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, "bash") || strings.Contains(joined, `"tool_calls"`) {
		t.Fatalf("tool-less client received a call: %v", lines)
	}
}

// content_blocks is not an OpenAI tool call and no configured provider emits
// it (grep over 803 raw dumps: zero). Counting it made synthesizeFinal claim
// tool_calls for arguments we never validate; the chunk is still relayed
// verbatim by the passthrough.
func TestPassthroughContentBlocksIsNotACall(t *testing.T) {
	up := `data: {"choices":[{"delta":{"content_blocks":[{"type":"tool_use","name":"x"}]}}]}

data: [DONE]

`
	lines, _ := runPassthroughFinal(t, up, true, nil)
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, `"finish_reason":"tool_calls"`) {
		t.Fatalf("content_blocks still counted as a call: %v", lines)
	}
	if !strings.Contains(joined, "content_blocks") {
		t.Fatalf("chunk must still be relayed: %v", lines)
	}
}

// A call whose arguments never arrive must be completed with "{}" BEFORE the
// finish chunk: clients JSON.parse the accumulated string, and "" is not JSON
// (measured on the opencode lane: finish=tool_calls with zero argument bytes).
func TestPassthroughBackfillsEmptyArgs(t *testing.T) {
	up := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"get_time"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	lines := runPassthrough(t, up, nil)
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, `"arguments":"{}"`) {
		t.Fatalf("empty args not backfilled: %v", lines)
	}
	bf, fin := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, `"arguments":"{}"`) && bf < 0 {
			bf = i
		}
		if strings.Contains(ln, `"finish_reason":"tool_calls"`) && fin < 0 {
			fin = i
		}
	}
	if bf < 0 || fin < 0 || bf > fin {
		t.Fatalf("backfill must precede the finish chunk: %v", lines)
	}
}

// A blocked (undeclared) call is not backfilled: it never reached the client.
func TestPassthroughNoBackfillForBlockedCall(t *testing.T) {
	up := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"add_numbers"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	lines, _ := runPassthroughFinal(t, up, true, map[string]bool{"get_time": true})
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, `"arguments":"{}"`) {
		t.Fatalf("blocked call was completed: %v", lines)
	}
}

// A call's arguments can arrive before its name (providers split the name into
// a later delta). Relaying the fragments names the call "" and the client
// answers "Tool  not found" (measured 2026-11-16).
func TestPassthroughHoldsArgsUntilNameArrives(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"arguments\":\"{\\\"command\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"bash\",\"arguments\":\"\\\"echo hi\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	lines, _ := runPassthroughFinal(t, up, true, map[string]bool{"bash": true})
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, `"name":""`) {
		t.Fatalf("nameless call relayed: %v", lines)
	}
	if !strings.Contains(joined, `"name":"bash"`) {
		t.Fatalf("declared call lost: %v", lines)
	}
	// Assembled arguments must stay valid JSON and carry the held fragment.
	var args string
	for _, ln := range lines {
		var ch struct {
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
		if json.Unmarshal([]byte(ln), &ch) != nil {
			continue
		}
		for _, c := range ch.Choices {
			for _, tc := range c.Delta.ToolCalls {
				args += tc.Function.Arguments
			}
		}
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(args), &probe); err != nil {
		t.Fatalf("assembled args invalid: %q (%v)", args, err)
	}
	if probe["command"] != "echo hi" {
		t.Fatalf("held fragment lost: %q", args)
	}
}

// A continuation frame may omit tool_call index (seen in relayed provider
// shapes). The fragment belongs to the only open call; before the inheritance
// it was held under a key that never got a name, then the "{}" backfill was
// sent and the model's arguments were lost.
func TestPassthroughContinuationWithoutIndexKeepsArgs(t *testing.T) {
	for _, up := range []string{
		// name frame indexed, argument frame without index
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"bash\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\\\"command\\\":\\\"echo hi\\\"}\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n",
		// neither frame carries an index
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c1\",\"function\":{\"name\":\"bash\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\\\"command\\\":\\\"echo hi\\\"}\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n",
	} {
		lines, _ := runPassthroughFinal(t, up, true, map[string]bool{"bash": true})
		joined := strings.Join(lines, "|")
		if strings.Contains(joined, `"arguments":"{}"`) {
			t.Fatalf("call got the empty-args backfill instead of its fragment: %v", lines)
		}
		if !strings.Contains(joined, "echo hi") {
			t.Fatalf("arguments lost: %v", lines)
		}
		if !strings.Contains(joined, `"finish_reason":"tool_calls"`) {
			t.Fatalf("finish lost: %v", lines)
		}
	}
}

// A call that never names itself is not a call: its held arguments are dropped.
func TestPassthroughDropsNamelessCall(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"arguments\":\"{\\\"command\\\":\\\"x\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	lines, _ := runPassthroughFinal(t, up, true, map[string]bool{"bash": true})
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, "tool_calls") {
		t.Fatalf("nameless call reached the client: %v", lines)
	}
}

// Tool calls are held until the turn closes: the client executes them after
// the stream, and holding keeps a transport cut retryable (no partial call
// leaked). A complete call must still arrive before its finish verdict.
func TestPassthroughHoldsCallUntilFinish(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"bash\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\\\"command\\\":\\\"echo hi\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	lines, _ := runPassthroughFinal(t, up, true, map[string]bool{"bash": true})
	joined := strings.Join(lines, "|")
	ci := strings.Index(joined, `"tool_calls"`)
	fi := strings.Index(joined, `"finish_reason":"tool_calls"`)
	if ci < 0 || fi < 0 || ci > fi {
		t.Fatalf("call not relayed before its finish: %v", lines)
	}
	if !strings.Contains(joined, "echo hi") {
		t.Fatalf("arguments lost: %v", lines)
	}
}

// A transport reset in the middle of a call must retry on a fresh lease while
// the client has seen nothing executable; the partial call must not leak (it
// used to force a synthetic "length" that clients report as an output limit).
func TestPassthroughTruncatedCallRetriesWithoutPartial(t *testing.T) {
	const chunk = "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"bash\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\\\"command\\\":\\\"echo\"}}]}}]}\n\n"
	var out strings.Builder
	w := &recorder{b: &out}
	_, retry := Passthrough(w, &truncatedReader{r: strings.NewReader(chunk), err: errors.New("read: connection reset by peer")},
		"text/event-stream", false, false, map[string]bool{"bash": true}, false)
	if !retry {
		t.Fatal("truncated call-only stream must be retryable")
	}
	if strings.Contains(out.String(), "tool_calls") {
		t.Fatalf("partial call leaked to the client: %q", out.String())
	}
}

// A cut after visible text cannot be retried (the client saw the text), but an
// incomplete call must not be handed over: the verdict is a text truncation.
func TestPassthroughCutAfterContentDropsPartialCall(t *testing.T) {
	const chunk = "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"bash\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\\\"command\\\":\\\"echo\"}}]}}]}\n\n"
	var out strings.Builder
	w := &recorder{b: &out}
	_, retry := Passthrough(w, &truncatedReader{r: strings.NewReader(chunk), err: errors.New("read: connection reset by peer")},
		"text/event-stream", false, true, map[string]bool{"bash": true}, false)
	if retry {
		t.Fatal("a cut after visible text must not retry")
	}
	body := out.String()
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("cut after content must close as length: %q", body)
	}
	if strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("incomplete call leaked after cut: %q", body)
	}
	if !strings.Contains(body, "hello") {
		t.Fatalf("visible text lost: %q", body)
	}
}

// Final pass, transport cut, no text: the partial call must not be flushed
// with the length verdict. A real upstream finish=length still delivers it;
// only a scan error (transport cut) drops it.
func TestPassthroughFinalTruncatedCallDropsPartial(t *testing.T) {
	const chunk = "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"bash\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\\\"command\\\":\\\"echo\"}}]}}]}\n\n"
	var out strings.Builder
	w := &recorder{b: &out}
	_, retry := Passthrough(w, &truncatedReader{r: strings.NewReader(chunk), err: errors.New("read: connection reset by peer")},
		"text/event-stream", false, true, map[string]bool{"bash": true}, false)
	if retry {
		t.Fatal("final pass must not retry")
	}
	body := out.String()
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("cut call must close as length: %q", body)
	}
	if strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("partial call leaked on the final pass: %q", body)
	}
}
