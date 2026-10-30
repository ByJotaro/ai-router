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
func runPassthrough(t *testing.T, upstream string) []string {
	t.Helper()
	lines, _ := runPassthroughFinal(t, upstream, true)
	return lines
}

// runPassthroughFinal is runPassthrough with an explicit final flag; when
// final is false a think-only stream returns without writing anything.
func runPassthroughFinal(t *testing.T, upstream string, final bool) ([]string, bool) {
	t.Helper()
	var out strings.Builder
	w := &recorder{b: &out}
	_, thinkOnly := Passthrough(w, strings.NewReader(upstream), "text/event-stream", false, final)
	var lines []string
	if out.Len() > 0 {
		for _, ln := range strings.Split(out.String(), "\n") {
			if strings.HasPrefix(ln, "data: ") {
				lines = append(lines, strings.TrimPrefix(ln, "data: "))
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
	lines := runPassthrough(t, up)
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
	lines := runPassthrough(t, up)
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

func TestPassthroughCutTextStreamGetsStop(t *testing.T) {
	// Normal text stream cut off mid-word (upstream TCP drop): the client
	// still needs a finish_reason or OpenAI SDKs report "stream closed
	// before a finish_reason was received".
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n"
	lines := runPassthrough(t, up)
	last := lines[len(lines)-2]
	if !strings.Contains(last, `"finish_reason":"stop"`) {
		t.Fatalf("expected stop for cut text stream, got: %s", last)
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
	lines := runPassthrough(t, up)
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
	lines := runPassthrough(t, up)
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
	lines := runPassthrough(t, up)
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, "think only") {
		t.Fatalf("no synthesized think-only text, got: %v", lines)
	}
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Fatalf("expected stop finish, got: %v", lines)
	}
}

func TestPassthroughLengthFinishWithoutContentGetsEmptyStop(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"x\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n"
	lines := runPassthrough(t, up)
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, "think only") {
		t.Fatalf("no synthesized think-only text, got: %v", lines)
	}
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Fatalf("expected empty stop finish, got: %v", lines)
	}
}



func TestPassthroughToolCallFinishNoTextKeptAsIs(t *testing.T) {
	// Agent turn ends with tool_calls and no text: no fake marker.
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"t\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"
	lines := runPassthrough(t, up)
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, "think only") {
		t.Fatalf("tool-call finish must not get fake text: %v", lines)
	}
	if !strings.Contains(joined, `"finish_reason":"tool_calls"`) {
		t.Fatalf("tool_calls finish lost: %v", lines)
	}
}

// TestPassthroughThinkOnlyRetryable: a reasoning-only stream that closes
// without content must not write anything when final=false (the caller
// retries on a fresh lease) and must report thinkOnly.
func TestPassthroughThinkOnlyRetryable(t *testing.T) {
	up := `data: {"choices":[{"delta":{"reasoning_content":"thinking hard"}}]}

data: [DONE]
`
	lines, thinkOnly := runPassthroughFinal(t, up, false)
	if !thinkOnly {
		t.Fatalf("expected thinkOnly=true, got false")
	}
	if len(lines) != 0 {
		t.Fatalf("retryable think-only must not flush anything, got: %v", lines)
	}
}

// TestPassthroughThinkOnlyFinalMarker: on the final attempt a plain empty
// terminal chunk and [DONE] are emitted — no synthesized "think only" text.
func TestPassthroughThinkOnlyFinalMarker(t *testing.T) {
	up := `data: {"choices":[{"delta":{"reasoning_content":"thinking hard"}}]}

data: [DONE]
`
	lines, thinkOnly := runPassthroughFinal(t, up, true)
	if thinkOnly {
		t.Fatalf("final attempt must not report thinkOnly")
	}
	if len(lines) != 2 || strings.Contains(lines[0], "think only") || !strings.Contains(lines[0], `"finish_reason":"stop"`) || lines[1] != "[DONE]" {
		t.Fatalf("expected empty stop chunk + [DONE], got: %v", lines)
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
	lines, thinkOnly := runPassthroughFinal(t, up, false)
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
		"text/event-stream", false, false)
	if retry {
		t.Fatal("truncation after flushed content must not ask for a retry")
	}
	body := out.String()
	if n := strings.Count(body, `"content":"hello"`); n != 1 {
		t.Fatalf("content written %d times, want 1 (duplicated stream): %q", n, body)
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
		"text/event-stream", false, false)
	if !retry {
		t.Fatal("silent truncation must stay retryable")
	}
	if out.Len() != 0 {
		t.Fatalf("nothing must be written before retry, got %q", out.String())
	}
}
