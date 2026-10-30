package route

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// zen sends added(name only) + done(FULL arguments) without delta events.
// Adapter must forward done arguments or client sees empty path/content.
func TestDoneEventForwardsArguments(t *testing.T) {
	sse := "event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"function_call\",\"name\":\"write\",\"call_id\":\"call_1\"}}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"function_call\",\"name\":\"write\",\"call_id\":\"call_1\",\"arguments\":\"{\\\"path\\\":\\\"/tmp/x.txt\\\"}\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"write\",\"arguments\":\"{\\\"path\\\":\\\"/tmp/x.txt\\\"}\"}]}}\n\n"
	up := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}
	out, err := wrapResponsesStream(up, "m", true)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(out.Body)
	if !strings.Contains(string(b), "/tmp/x.txt") {
		t.Fatalf("done arguments lost in stream output:\n%s", b)
	}
}

// deltas + done(full blob): done must NOT duplicate (client would append twice).
func TestDoneNoDuplicateWhenDeltas(t *testing.T) {
	deltaArgs := `{"path":"a"}`
	esc, _ := json.Marshal(deltaArgs)
	sse := "event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"function_call\",\"name\":\"write\",\"call_id\":\"call_1\"}}\n\n" +
		"event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"rs_1\",\"delta\":" + string(esc) + "}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"function_call\",\"name\":\"write\",\"call_id\":\"call_1\",\"arguments\":" + string(esc) + "}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	up := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}
	out, err := wrapResponsesStream(up, "m", true)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(out.Body)
	n := strings.Count(string(b), "path")
	if n != 1 {
		t.Fatalf("expected 1 args chunk (delta only), got %d:\n%s", n, b)
	}
}

// TestWrapStreamsDeltasLive: сквозной контракт — чанки обязаны течь по мере
// чтения апстрима, а не после EOF. Медленный upstream (чанк, пауза, чанк):
// первый chat-chunk должен прийти ДО закрытия тела.
func TestWrapStreamsDeltasLive(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		for _, ev := range []string{
			`data: {"type":"response.output_text.delta","delta":"Hel"}`,
			`data: {"type":"response.output_text.delta","delta":"lo"}`,
		} {
			_, _ = pw.Write([]byte(ev + "\n\n"))
			time.Sleep(150 * time.Millisecond)
		}
		_, _ = pw.Write([]byte(`data: {"type":"response.completed","response":{"status":"completed","output":[]}}` + "\n\n"))
		_ = pw.Close()
	}()
	up := &http.Response{StatusCode: 200, Header: make(http.Header), Body: pr}
	up.Header.Set("Content-Type", "text/event-stream")
	out, err := wrapResponsesStream(up, "m", true)
	if err != nil {
		t.Fatal(err)
	}
	if ct := out.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("want SSE out, got %q", ct)
	}
	firstAt := time.Now()
	gotFirst := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out.Body)
		for sc.Scan() {
			ln := sc.Text()
			if strings.HasPrefix(ln, "data: ") && strings.Contains(ln, "Hel") {
				gotFirst <- ln
				return
			}
		}
		close(gotFirst)
	}()
	select {
	case ln, ok := <-gotFirst:
		if !ok {
			t.Fatal("first delta never arrived")
		}
		if d := time.Since(firstAt); d > 2*time.Second {
			t.Fatalf("first delta took %v (buffered, want live): %s", d, ln)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for live delta (stream is buffered, not passthrough)")
	}
	rest, _ := io.ReadAll(out.Body)
	if !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("missing [DONE] in tail:\n%s", rest)
	}
}

// TestWrapErrorWithoutContentHonest: ошибка-без-контента — честный HTTP
// статус, а не "200 с 0 чанков". failed + RegionError до первой дельты.
func TestWrapErrorWithoutContentHonest(t *testing.T) {
	sse := "event: response.failed\n" +
		`data: {"type":"response.failed","response":{"status":"failed","error":{"type":"invalid_request_error","code":"region_error","message":"Region not supported"}}}` + "\n\n"
	up := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}
	out, err := wrapResponsesStream(up, "m", true)
	if err != nil {
		t.Fatal(err)
	}
	if out.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for RegionError-without-content", out.StatusCode)
	}
	b, _ := io.ReadAll(out.Body)
	if !strings.Contains(string(b), "Region not supported") {
		t.Fatalf("error message lost:\n%s", b)
	}
}

// TestWrapContentThenErrorStreams: контент-then-error — 200 SSE, ошибка
// идёт in-band событием (стандарт OpenAI), а не HTTP-ошибкой.
func TestWrapContentThenErrorStreams(t *testing.T) {
	sse := `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		`data: {"type":"error","error":{"type":"server_error","message":"boom"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"status":"completed","output":[]}}` + "\n\n"
	up := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}
	out, err := wrapResponsesStream(up, "m", true)
	if err != nil {
		t.Fatal(err)
	}
	if out.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error after content is in-band)", out.StatusCode)
	}
	b, _ := io.ReadAll(out.Body)
	if !strings.Contains(string(b), `"content":"hi"`) {
		t.Fatalf("content chunk lost:\n%s", b)
	}
	if !strings.Contains(string(b), "boom") {
		t.Fatalf("in-band error lost:\n%s", b)
	}
	if !strings.Contains(string(b), "[DONE]") {
		t.Fatalf("missing [DONE]:\n%s", b)
	}
}
