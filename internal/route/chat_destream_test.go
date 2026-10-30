package route

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

func TestForceChatStreamTrue(t *testing.T) {
	in := []byte(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	out := forceChatStreamTrue(in)
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v["stream"] != true {
		t.Fatalf("stream = %v, want true", v["stream"])
	}
	if v["model"] != "m" {
		t.Fatalf("model lost: %v", v["model"])
	}
	msgs, ok := v["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages lost: %v", v["messages"])
	}
}

func TestForceChatStreamTrueBareBody(t *testing.T) {
	// A body without the flag must gain it, not pass through unchanged.
	out := forceChatStreamTrue([]byte(`{"model":"m"}`))
	if !strings.Contains(string(out), `"stream":true`) {
		t.Fatalf("stream not added: %s", out)
	}
}

func TestAggregateChatSSEContentAndUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","created":123,"model":"m","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"c1","created":123,"model":"m","choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`data: {"id":"c1","created":123,"model":"m","choices":[{"delta":{"content":"lo"},"finish_reason":null}]}`,
		`data: {"id":"c1","created":123,"model":"m","usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9},"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	body := newChatDestreamBody(io.NopCloser(strings.NewReader(sse)), "fallback")
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal aggregate: %v (%s)", err, raw)
	}
	if out.Object != "chat.completion" {
		t.Fatalf("object = %q", out.Object)
	}
	if out.ID != "c1" || out.Model != "m" {
		t.Fatalf("id/model = %q/%q", out.ID, out.Model)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "Hello" {
		t.Fatalf("content = %+v", out.Choices)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish = %q", out.Choices[0].FinishReason)
	}
	if out.Usage.PromptTokens != 7 || out.Usage.CompletionTokens != 2 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestAggregateChatSSEToolCallsAcrossChunks(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c2","created":5,"model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"bash","arguments":"{\"comm"}}]},"finish_reason":null}]}`,
		`data: {"id":"c2","created":5,"model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"echo hi\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"c2","created":5,"model":"m","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
	}, "\n")
	raw, err := io.ReadAll(newChatDestreamBody(io.NopCloser(strings.NewReader(sse)), "m"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, raw)
	}
	if len(out.Choices) != 1 || len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", out.Choices)
	}
	tc := out.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_a" || tc.Function.Name != "bash" {
		t.Fatalf("tool call = %+v", tc)
	}
	if tc.Function.Arguments != `{"command":"echo hi"}` {
		t.Fatalf("args = %q", tc.Function.Arguments)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", out.Choices[0].FinishReason)
	}
}

func TestAggregateChatSSEErrorEvent(t *testing.T) {
	sse := `data: {"error":{"type":"FreeUsageLimitError","message":"Free usage limit reached"}}` + "\n"
	raw, err := io.ReadAll(newChatDestreamBody(io.NopCloser(strings.NewReader(sse)), "m"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "FreeUsageLimitError") {
		t.Fatalf("error lost: %s", raw)
	}
}

// A clean EOF without `[DONE]` and without a finish_reason is a truncated
// stream (egress rotation, idle cut). It must NOT be reported as a finished
// completion: the non-stream client would get a partial answer that looks
// complete and the egress would be recorded as healthy.
func TestAggregateChatSSETruncatedStreamIsError(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c3","created":9,"model":"m","choices":[{"delta":{"content":"par"},"finish_reason":null}]}`,
		`data: {"id":"c3","created":9,"model":"m","choices":[{"delta":{"content":"tial"},"finish_reason":null}]}`,
		``,
	}, "\n")
	_, err := io.ReadAll(newChatDestreamBody(io.NopCloser(strings.NewReader(sse)), "m"))
	if err == nil {
		t.Fatal("truncated stream accepted as a complete completion")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want ErrUnexpectedEOF", err)
	}
}

// [DONE] alone is a complete stream even without an explicit finish_reason
// chunk (some upstreams end with a bare terminator).
func TestAggregateChatSSEDoneOnlyIsComplete(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c4","created":9,"model":"m","choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	raw, err := io.ReadAll(newChatDestreamBody(io.NopCloser(strings.NewReader(sse)), "m"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), `"content":"ok"`) {
		t.Fatalf("content lost: %s", raw)
	}
}

// TestDoRequestForcesStreamAndDestreams is the wiring guard: a non-stream
// client on a gated provider must leave with stream:true (the gate answers
// 429 to a non-stream body) and get one chat.completion back, not SSE.
func TestDoRequestForcesStreamAndDestreams(t *testing.T) {
	t.Setenv("AI_ROUTER_SESSION_DB", "off")
	var sawStream *bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Stream *bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &body)
		sawStream = body.Stream
		if body.Stream == nil || !*body.Stream {
			// What the real gate does to a non-stream body.
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"id\":\"x\",\"created\":1,\"model\":\"deepseek-v4.1-flash-free\",\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"id\":\"x\",\"created\":1,\"model\":\"deepseek-v4.1-flash-free\",\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6},\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer up.Close()

	prov := &provider.Provider{Name: "opencode", Type: "openai-compatible", BaseURL: up.URL, AuthMode: "none"}
	res, status, err := (&Router{}).doRequest(context.Background(), up.Client(), prov, nil,
		[]byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],"stream":false}`),
		directHomeIPKey, "")
	if err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if sawStream == nil || !*sawStream {
		t.Fatalf("upstream saw stream=%v, want forced true", sawStream)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("client Content-Type = %q, want application/json", ct)
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = res.Body.Close()
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("client body is not a chat.completion: %v (%s)", err, raw)
	}
	if out.Object != "chat.completion" || len(out.Choices) != 1 || out.Choices[0].Message.Content != "OK" {
		t.Fatalf("aggregated body = %s", raw)
	}
	if out.Usage.TotalTokens != 6 {
		t.Fatalf("usage lost: %s", raw)
	}
}

// TestPoolLaneNonStreamIsDestreamed covers the pooled lane: a non-stream caller
// routed through a lease must also ship stream:true and receive one
// chat.completion, not SSE glued into a JSON body.
func TestPoolLaneNonStreamIsDestreamed(t *testing.T) {
	// The pooled lane refuses an egress with no session identity, so seed a
	// fresh id for it exactly like the other pool tests do.
	now := time.Now()
	seedPool(t, &now, []sessionEntry{
		{id: "ses_pool00000000000000000001", updated: now.UnixMilli()},
		{id: "ses_pool00000000000000000002", updated: now.UnixMilli()},
	})
	var sawStream *bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Stream *bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &body)
		sawStream = body.Stream
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"id\":\"p\",\"created\":2,\"model\":\"deepseek-v4.1-flash-free\",\"choices\":[{\"delta\":{\"content\":\"pooled\"},\"finish_reason\":null}]}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer up.Close()

	api := &fakePoolAPI{leases: []string{up.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()
	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	prov := &provider.Provider{Name: "opencode", Type: "openai-compatible", BaseURL: "http://upstream/v1", AuthMode: "none"}
	r := New(map[string]*provider.Provider{"opencode": prov}, WithTransport(pt))
	res, err := r.Call(context.Background(), "deepseek-v4.1-flash-free",
		[]byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],"stream":false}`), false)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if sawStream == nil || !*sawStream {
		t.Fatalf("pooled upstream saw stream=%v, want forced true", sawStream)
	}
	// The pool lane hands the gateway a live response, not a byte slice: the
	// destreamed body arrives through Result.Resp.
	if res.Resp == nil || res.Resp.Body == nil {
		t.Fatal("pool result carries no response body")
	}
	body, err := io.ReadAll(res.Resp.Body)
	if err != nil {
		t.Fatalf("read pool body: %v", err)
	}
	_ = res.Resp.Body.Close()
	if ct := res.Resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("pool Content-Type = %q, want application/json", ct)
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("pool result is not a chat.completion: %v (%s)", err, body)
	}
	if out.Object != "chat.completion" || len(out.Choices) != 1 || out.Choices[0].Message.Content != "pooled" {
		t.Fatalf("pool result = %s", body)
	}
}
