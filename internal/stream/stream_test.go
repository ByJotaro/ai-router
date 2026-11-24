package stream

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// runPassthrough feeds upstream SSE text through Passthrough (final attempt)
// and returns the "data:" payloads written to the client.
func runPassthrough(t *testing.T, upstream string, allowed map[string]bool) []string {
	t.Helper()
	lines, _ := runPassthroughFinal(t, upstream, true, allowed)
	// The relay now appends a synthesized usage event when the upstream sent
	// none; the assertions below look at the finish event, so drop usage-only
	// frames here.
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.Contains(ln, `"usage":`) && strings.Contains(ln, `"choices":[]`) {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// runPassthroughSuppressed is runPassthroughFinal with reasoning suppressed,
// which is what the gateway does on a think-only retry: the client already
// received the first attempt's reasoning and only the answer slot repeats.
func runPassthroughSuppressed(t *testing.T, upstream string, final bool, allowed map[string]bool) ([]string, bool) {
	t.Helper()
	var out strings.Builder
	w := &recorder{b: &out}
	_, thinkOnly := Passthrough(w, strings.NewReader(upstream), "text/event-stream", false, final, allowed, true)
	var lines []string
	if out.Len() > 0 {
		for _, ln := range strings.Split(out.String(), "\n") {
			if strings.HasPrefix(ln, "data: ") {
				ev := strings.TrimPrefix(ln, "data: ")
				if strings.Contains(ev, `"usage":`) && strings.Contains(ev, `"choices":[]`) {
					continue
				}
				lines = append(lines, ev)
			}
		}
	}
	return lines, thinkOnly
}

// runPassthroughFinal is runPassthrough with an explicit final flag; when
// final is false a think-only stream returns without writing anything.
func runPassthroughFinal(t *testing.T, upstream string, final bool, allowed map[string]bool) ([]string, bool) {
	t.Helper()
	var out strings.Builder
	w := &recorder{b: &out}
	_, thinkOnly := Passthrough(w, strings.NewReader(upstream), "text/event-stream", false, final, allowed, false)
	var lines []string
	if out.Len() > 0 {
		for _, ln := range strings.Split(out.String(), "\n") {
			if strings.HasPrefix(ln, "data: ") {
				ev := strings.TrimPrefix(ln, "data: ")
				if strings.Contains(ev, `"usage":`) && strings.Contains(ev, `"choices":[]`) {
					continue
				}
				lines = append(lines, ev)
			}
		}
	}
	return lines, thinkOnly
}

type recorder struct {
	b *strings.Builder
}

func (r *recorder) Header() http.Header         { return http.Header{} }
func (r *recorder) Write(p []byte) (int, error) { return r.b.Write(p) }
func (r *recorder) WriteHeader(int)             {}
func (r *recorder) Flush()                      {}
func TestPassthroughThinkOnlyGetsMarker(t *testing.T) {
	// opencode thinks, calls a tool, repeats, then the stream ends without
	// any text answer and without a finish_reason.
	up := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":null}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"t1\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]}}]}\n\n"
	lines := runPassthrough(t, up, nil)
	last := lines[len(lines)-2]
	if !strings.Contains(last, `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected tool_calls finish, got: %s", last)
	}
	if strings.Contains(last, "think only") {
		t.Fatalf("tool-call stream must not get a fake text answer: %s", last)
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("expected trailing [DONE], got: %v", lines)
	}
}

func TestPassthroughReasoningOnlyNoToolsGetsEmptyStop(t *testing.T) {
	// Model only emitted reasoning and then the stream died — no text, no
	// tools, no finish_reason. The client must see an empty stop chunk (no
	// synthesized "think only" text) so it treats the turn as silent.
	up := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"deep thinking\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"more\"}}]}\n\n"
	lines := runPassthrough(t, up, nil)
	last := lines[len(lines)-2]
	if strings.Contains(last, "think only") {
		t.Fatalf("no synthesized think-only text, got: %s", last)
	}
	if !strings.Contains(last, `"finish_reason":"stop"`) {
		t.Fatalf("expected stop reason, got: %s", last)
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("expected trailing [DONE], got: %v", lines)
	}
}

func TestPassthroughCutTextStreamGetsLength(t *testing.T) {
	// Normal text stream cut off mid-word (upstream TCP drop): the client
	// still needs a finish_reason or OpenAI SDKs report "stream closed
	// before a finish_reason was received". It must be "length", not
	// "stop": the answer is partial and a successful stop makes the client
	// accept it as complete (the 2026-11-19 OMP cut).
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n"
	lines := runPassthrough(t, up, nil)
	last := lines[len(lines)-2]
	if !strings.Contains(last, `"finish_reason":"length"`) {
		t.Fatalf("expected length for cut text stream, got: %s", last)
	}
	if strings.Contains(last, `"finish_reason":"stop"`) {
		t.Fatalf("cut text must not be reported as a clean stop: %s", last)
	}
	if strings.Contains(last, "think only") {
		t.Fatalf("text stream must not get think-only marker: %s", last)
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("expected trailing [DONE], got: %v", lines)
	}
}

func TestPassthroughUpstreamDoneKept(t *testing.T) {
	// A well-formed stream with its own finish_reason and [DONE] passes
	// through untouched (plus the single trailing [DONE] once).
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	lines := runPassthrough(t, up, nil)
	if got := strings.Count(strings.Join(lines, "|"), "[DONE]"); got != 1 {
		t.Fatalf("expected exactly one [DONE], got %d: %v", got, lines)
	}
	var finish int
	for _, l := range lines {
		if strings.Contains(l, `"finish_reason":"stop"`) {
			finish++
		}
	}
	if finish != 1 {
		t.Fatalf("expected exactly one finish_reason, got %d: %v", finish, lines)
	}
}

func TestPassthroughErrorFinishBecomesStop(t *testing.T) {
	// Upstream delivers content then closes with finish_reason "error".
	// The client must receive "stop" — "Provider returned error
	// finish_reason" aborts the whole turn otherwise.
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"here is the answer\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"error\"}]}\n\n"
	lines := runPassthrough(t, up, nil)
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, `"finish_reason":"error"`) {
		t.Fatalf("error finish leaked to client: %v", lines)
	}
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Fatalf("expected downgraded stop finish, got: %v", lines)
	}
	if got := strings.Count(joined, "[DONE]"); got != 1 {
		t.Fatalf("expected exactly one [DONE], got %d: %v", got, lines)
	}
	// Content must survive the rewrite untouched.
	if !strings.Contains(joined, "here is the answer") {
		t.Fatalf("content lost during sanitize: %v", lines)
	}
}
func TestPassthroughStopFinishWithoutContentGetsEmptyStop(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"planning\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	lines := runPassthrough(t, up, nil)
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, "think only") {
		t.Fatalf("no synthesized think-only text, got: %v", lines)
	}
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Fatalf("expected stop finish, got: %v", lines)
	}
}

func TestPassthroughReasoningThenLengthIsRelayedHonestly(t *testing.T) {
	// Reasoning flows to the client, but the terminal verdict must not
	// overtake the answer: on the final attempt the reasoning is salvaged
	// into the content slot, and only THEN does the upstream's own "length"
	// arrive (review 2026-11-20 F1: relaying the finish live produced
	// finish -> content -> synthesized stop, and the last-wins stop
	// mislabeled a truncated turn as complete).
	up := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"x\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n"
	lines := runPassthrough(t, up, nil)
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, "think only") {
		t.Fatalf("no synthesized think-only text, got: %v", lines)
	}
	if !strings.Contains(joined, `"reasoning_content":"x"`) {
		t.Fatalf("reasoning must reach the client, got: %v", lines)
	}
	contentAt, lengthAt, stopAt := -1, -1, -1
	for i, ln := range lines {
		switch {
		case strings.Contains(ln, `"content":"x"`) && contentAt < 0:
			contentAt = i
		case strings.Contains(ln, `"finish_reason":"length"`) && lengthAt < 0:
			lengthAt = i
		case strings.Contains(ln, `"finish_reason":"stop"`):
			stopAt = i
		}
	}
	// Default policy (2026-11-22): reasoning is NOT promoted into the answer
	// slot — the client already saw it as reasoning_content.
	if contentAt >= 0 {
		t.Fatalf("default must not salvage reasoning as content, got: %v", lines)
	}
	if lengthAt < 0 {
		t.Fatalf("expected the upstream length finish, got: %v", lines)
	}
	// Opt-in legacy behavior: AI_ROUTER_REASONING_AS_ANSWER=1.
	reasoningAsAnswer = true
	legacy := runPassthrough(t, up, nil)
	reasoningAsAnswer = false
	lc, ll := -1, -1
	for i, ln := range legacy {
		if strings.Contains(ln, `"content":"x"`) && lc < 0 {
			lc = i
		}
		if strings.Contains(ln, `"finish_reason":"length"`) && ll < 0 {
			ll = i
		}
	}
	if lc < 0 || ll < 0 || lc > ll {
		t.Fatalf("legacy flag must salvage content before the length finish, got: %v", legacy)
	}
	if stopAt >= 0 {
		t.Fatalf("a held upstream verdict must not be replaced by a synthesized stop: %v", lines)
	}
	if !strings.Contains(joined, "[DONE]") {
		t.Fatalf("expected [DONE], got: %v", lines)
	}
}

// TestPassthroughNonFinalHoldsVerdictForRetry: a reasoning-only stream on a
// retryable attempt must not flush its finish_reason — the gateway retries
// into the SAME response, and a verdict followed by a second generation is
// not a valid turn (review 2026-11-20 F1).
func TestPassthroughNonFinalHoldsVerdictForRetry(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"x\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n"
	lines, thinkOnly := runPassthroughFinal(t, up, false, nil)
	if !thinkOnly {
		t.Fatalf("reasoning-only non-final attempt must stay retryable")
	}
	for _, ln := range lines {
		if strings.Contains(ln, "finish_reason") {
			t.Fatalf("a retryable attempt must not flush a terminal verdict: %v", lines)
		}
	}
}

// TestPassthroughBlockedNoticeOnlyOnFinalAttempt: the refusal notice is an
// assistant content delta; emitting it on a discarded attempt duplicates it
// into the retry (review 2026-11-20 F2).
func TestPassthroughBlockedNoticeOnlyOnFinalAttempt(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"t1\",\"function\":{\"name\":\"playwright_browser_navigate\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"
	allowed := map[string]bool{"read": true, "bash": true}
	nonFinal, _ := runPassthroughFinal(t, up, false, allowed)
	for _, ln := range nonFinal {
		if strings.Contains(ln, "refused") {
			t.Fatalf("non-final attempt must not emit the refusal notice: %v", nonFinal)
		}
	}
	final, _ := runPassthroughFinal(t, up, true, allowed)
	notices := 0
	for _, ln := range final {
		if strings.Contains(ln, "refused") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("final attempt must carry exactly one refusal notice, got %d: %v", notices, final)
	}
}

func TestPassthroughToolCallFinishNoTextKeptAsIs(t *testing.T) {
	// Agent turn ends with tool_calls and no text: no fake marker.
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"t\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"
	lines := runPassthrough(t, up, nil)
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, "think only") {
		t.Fatalf("tool-call finish must not get fake text: %v", lines)
	}
	if !strings.Contains(joined, `"finish_reason":"tool_calls"`) {
		t.Fatalf("tool_calls finish lost: %v", lines)
	}
}

// TestPassthroughReasoningIsRetryableWithSuppressedRetry: reasoning is
// streamed live, so a reasoning-only stream has already shown the client the
// preamble. A retry is still allowed because the gateway suppresses reasoning
// on the retry attempt (suppressReasoning), so nothing is duplicated and the
// answer slot is the only thing repeated.
func TestPassthroughReasoningIsRetryableWithSuppressedRetry(t *testing.T) {
	up := `data: {"choices":[{"delta":{"reasoning_content":"thinking hard"}}]}

data: [DONE]
`
	lines, thinkOnly := runPassthroughFinal(t, up, false, nil)
	if !thinkOnly {
		t.Fatalf("a reasoning-only stream before the final attempt must stay retryable")
	}
	if !strings.Contains(strings.Join(lines, "|"), `"reasoning_content":"thinking hard"`) {
		t.Fatalf("reasoning must be flushed live: %v", lines)
	}
	retryLines, retryThinkOnly := runPassthroughSuppressed(t, up, true, nil)
	if retryThinkOnly {
		t.Fatalf("final attempt must not report thinkOnly")
	}
	for _, ln := range retryLines {
		if strings.Contains(ln, "reasoning_content") {
			t.Fatalf("retry must not replay reasoning: %v", retryLines)
		}
	}
}

// TestPassthroughThinkOnlyFinalMarker: on the final attempt the reasoning the
// upstream did produce is relayed as content, then a stop chunk and [DONE]. A
// bare empty stop is fatal for the client (OMP: "Assistant returned an empty
// stop after retry cap"), so it must never be the terminal shape. Reasoning
// itself is suppressed on that attempt, so it is delivered once, as content.
func TestPassthroughThinkOnlyFinalMarker(t *testing.T) {
	up := `data: {"choices":[{"delta":{"reasoning_content":"thinking hard"}}]}

data: [DONE]
`
	lines, thinkOnly := runPassthroughSuppressed(t, up, true, nil)
	if thinkOnly {
		t.Fatalf("final attempt must not report thinkOnly")
	}
	// Default policy (2026-11-22): no fabricated answer chunk — the honest
	// shape is the terminal stop + [DONE].
	if len(lines) != 2 {
		t.Fatalf("default expected stop + [DONE], got: %v", lines)
	}
	if !strings.Contains(lines[0], `"finish_reason":"stop"`) || lines[1] != "[DONE]" {
		t.Fatalf("expected stop chunk + [DONE], got: %v", lines)
	}
	// Opt-in legacy behavior: AI_ROUTER_REASONING_AS_ANSWER=1.
	reasoningAsAnswer = true
	legacy, _ := runPassthroughSuppressed(t, up, true, nil)
	reasoningAsAnswer = false
	if len(legacy) != 3 || !strings.Contains(legacy[0], `"content":"thinking hard"`) {
		t.Fatalf("legacy flag expected answer chunk + stop + [DONE], got: %v", legacy)
	}
}

// TestPassthroughThinkThenContentFlushesBuffered: reasoning held until the
// first content chunk, then everything in order.
func TestPassthroughThinkThenContentFlushesBuffered(t *testing.T) {
	up := `data: {"choices":[{"delta":{"reasoning_content":"think A"}}]}

data: {"choices":[{"delta":{"reasoning_content":"think B"}}]}

data: {"choices":[{"delta":{"content":"hello"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]
`
	lines, thinkOnly := runPassthroughFinal(t, up, false, nil)
	if thinkOnly {
		t.Fatalf("content stream must not report thinkOnly")
	}
	if len(lines) != 5 || lines[0] == "" || !strings.Contains(lines[2], "hello") || lines[4] != "[DONE]" {
		t.Fatalf("buffered reasoning order lost: %v", lines)
	}
	if !strings.Contains(lines[0], "think A") || !strings.Contains(lines[1], "think B") {
		t.Fatalf("reasoning must precede content, got first lines: %v", lines[:2])
	}
}

// silenceReader produces no bytes and never ends (a wedged stream).
type silenceReader struct{}

func (silenceReader) Read([]byte) (int, error) { return 0, nil }

// TestStreamIdleReaderCutsSilentStream: a stream that produces no bytes
// past the idle deadline is terminated (EOF) so the caller rotates.
func TestStreamIdleReaderCutsSilentStream(t *testing.T) {
	r := &streamIdleReader{r: silenceReader{}, deadline: time.Now().Add(-time.Second)}
	buf := make([]byte, 16)
	n, err := r.Read(buf)
	if err != io.EOF {
		t.Fatalf("silent past deadline must EOF, got err=%v n=%d", err, n)
	}
}

// TestStreamIdleReaderKeepsFlowingOnData: any bytes (reasoning OR content)
// slide the deadline; a reader past its old deadline but serving data
// keeps returning data, not EOF.
func TestStreamIdleReaderKeepsFlowingOnData(t *testing.T) {
	r := &streamIdleReader{r: strings.NewReader("data"), deadline: time.Now().Add(-time.Second)}
	buf := make([]byte, 16)
	n, err := r.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("data must flow past old deadline, got err=%v n=%d", err, n)
	}
	if !time.Now().Before(r.deadline) {
		t.Fatalf("successful read must slide deadline forward")
	}
}

// truncatedReader emits a prefix, then fails hard — the shape of an upstream
// socket cut mid-stream (or the 16MB scanner cap).
type truncatedReader struct {
	r    io.Reader
	err  error
	done bool
}

func (t *truncatedReader) Read(p []byte) (int, error) {
	if !t.done {
		n, _ := t.r.Read(p)
		if n > 0 {
			return n, nil
		}
		t.done = true
	}
	return 0, t.err
}

// TestPassthroughTruncationAfterContentDoesNotRetry: once content reached the
// client, a truncation must close out honestly — asking the caller to retry
// replays the whole answer into the same response writer (duplicated SSE +
// double upstream spend, review 2026-10-17 r09 F1).
func TestPassthroughTruncationAfterContentDoesNotRetry(t *testing.T) {
	const chunk = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"
	var out strings.Builder
	w := &recorder{b: &out}
	_, retry := Passthrough(w, &truncatedReader{r: strings.NewReader(chunk), err: errors.New("read: connection reset by peer")},
		"text/event-stream", false, false, nil, false)
	if retry {
		t.Fatal("truncation after flushed content must not ask for a retry")
	}
	body := out.String()
	if n := strings.Count(body, `"content":"hello"`); n != 1 {
		t.Fatalf("content written %d times, want 1 (duplicated stream): %q", n, body)
	}
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("truncated content must close with length: %q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("truncated stream must close with [DONE]: %q", body)
	}
}

// TestPassthroughTruncationBeforeContentRetries: nothing reached the client,
// so a fresh lease is the right answer (unchanged behaviour).
func TestPassthroughTruncationBeforeContentRetries(t *testing.T) {
	var out strings.Builder
	w := &recorder{b: &out}
	_, retry := Passthrough(w, &truncatedReader{r: strings.NewReader(""), err: errors.New("read: connection reset by peer")},
		"text/event-stream", false, false, nil, false)
	if !retry {
		t.Fatal("silent truncation must stay retryable")
	}
	if out.Len() != 0 {
		t.Fatalf("nothing must be written before retry, got %q", out.String())
	}
}

// A suppressed retry chunk that carries the finish verdict along with a
// reasoning delta must reach the client: dropping it (the "empty payload"
// shortcut) left content + [DONE] with no finish_reason, which clients report
// as a completed turn without a verdict.
func TestPassthroughSuppressedRetryKeepsFinish(t *testing.T) {
	up := `data: {"choices":[{"delta":{"reasoning_content":"thought","finish_reason":"stop"}}]}

data: [DONE]
`
	lines, thinkOnly := runPassthroughSuppressed(t, up, true, nil)
	if thinkOnly {
		t.Fatalf("final attempt must not report thinkOnly")
	}
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason dropped with the suppressed reasoning: %v", lines)
	}
	if !strings.Contains(joined, "[DONE]") {
		t.Fatalf("[DONE] missing: %v", lines)
	}
}

// A final attempt that delivers nothing at all must name the condition: a bare
// stop is read by the client as "empty stop after retry cap" and blamed on its
// own context. Before the final attempt the same body stays retryable.
func TestPassthroughEmptyFinalNamesCondition(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{}}]}\n\ndata: [DONE]\n\n"
	lines, retry := runPassthroughFinal(t, up, false, nil)
	if !retry {
		t.Fatalf("empty non-final stream must stay retryable")
	}
	lines, retry = runPassthroughFinal(t, up, true, nil)
	if retry {
		t.Fatalf("final attempt must not report thinkOnly")
	}
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, `"upstream_empty"`) || !strings.Contains(joined, "[DONE]") {
		t.Fatalf("empty final stream must be named, got: %v", lines)
	}
}
