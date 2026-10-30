package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/provider"
)

// Converter-only probes: no network, no router — pure request/response
// mapping between the Responses API and the chat protocol.

func TestResponsesInToChatStringInput(t *testing.T) {
	raw := []byte(`{"model":"m","input":"hello","instructions":"be brief","stream":false,"max_output_tokens":512,"temperature":0.5,"top_p":0.9,"parallel_tool_calls":false,"metadata":{"a":"b"}}`)
	var in responsesIn
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	out, err := responsesInToChat(&in)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		MaxCT *int     `json:"max_completion_tokens"`
		Temp  *float64 `json:"temperature"`
		TopP  *float64 `json:"top_p"`
		Par   *bool    `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	if chat.Model != "m" || chat.Stream {
		t.Fatalf("model/stream = %q/%v", chat.Model, chat.Stream)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Role != "system" || chat.Messages[0].Content != "be brief" {
		t.Fatalf("instructions not leading system: %s", out)
	}
	if chat.Messages[1].Role != "user" || chat.Messages[1].Content != "hello" {
		t.Fatalf("string input not user message: %s", out)
	}
	if chat.MaxCT == nil || *chat.MaxCT != 512 {
		t.Fatalf("max_output_tokens not mapped: %s", out)
	}
	if chat.Temp == nil || *chat.Temp != 0.5 || chat.TopP == nil || *chat.TopP != 0.9 || chat.Par == nil || *chat.Par {
		t.Fatalf("sampling passthrough lost: %s", out)
	}
}

func TestResponsesInToChatItemsAndTools(t *testing.T) {
	raw := []byte(`{"model":"m","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:x"}]},{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}],"tools":[{"type":"function","name":"read","description":"r","parameters":{"type":"object"}}],"tool_choice":"auto","reasoning":{"effort":"high","summary":"auto"}}`)
	var in responsesIn
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	out, err := responsesInToChat(&in)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []map[string]any `json:"messages"`
		Tools    []map[string]any `json:"tools"`
		ToolChoice any            `json:"tool_choice"`
		ReEff    string           `json:"reasoning_effort"`
		Reason   map[string]any   `json:"reasoning"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d: %s", len(chat.Messages), out)
	}
	if chat.Messages[1]["role"] != "assistant" {
		t.Fatalf("function_call not assistant tool_calls: %s", out)
	}
	if chat.Messages[2]["role"] != "tool" || chat.Messages[2]["tool_call_id"] != "c1" {
		t.Fatalf("function_call_output not tool+c1: %s", out)
	}
	if len(chat.Tools) != 1 || chat.Tools[0]["type"] != "function" {
		t.Fatalf("tools not chat-shaped: %s", out)
	}
	if chat.ReEff != "high" || chat.Reason["summary"] != "auto" {
		t.Fatalf("reasoning not merged: %s", out)
	}
}

func TestResponsesInToChatRejects(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","input":""}`,
		`{"model":"m","input":[]}`,
		`{"model":"m","input":42}`,
	} {
		var in responsesIn
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			t.Fatal(err)
		}
		if _, err := responsesInToChat(&in); err == nil {
			t.Fatalf("want error for %s", raw)
		}
	}
}

func TestChatCompletionToResponseShape(t *testing.T) {
	chat := []byte(`{"id":"chatcmpl-1","created":1700000000,"model":"m","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":7}}}`)
	var in responsesIn
	_ = json.Unmarshal([]byte(`{"model":"m","metadata":{"k":"v"},"reasoning":{"effort":"high"}}`), &in)
	obj, usage := chatCompletionToResponse(chat, &in)
	if obj["id"] == "" || obj["object"] != "response" || obj["status"] != "completed" {
		t.Fatalf("envelope broken: %v", obj)
	}
	out, _ := obj["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("want 1 message item, got %v", obj["output"])
	}
	u, _ := obj["usage"].(map[string]any)
	if u["input_tokens"] != int64(10) && u["input_tokens"] != float64(10) {
		t.Fatalf("usage broken: %v", u)
	}
	od, _ := u["output_tokens_details"].(map[string]any)
	if od == nil || (od["reasoning_tokens"] != int64(7) && od["reasoning_tokens"] != float64(7)) {
		t.Fatalf("reasoning_tokens not honest: %v", u)
	}
	if usage == nil || usage.Reasoning != 7 || usage.CacheRead != 4 {
		t.Fatalf("stream.Usage not mapped: %+v", usage)
	}
	if obj["metadata"] == nil {
		t.Fatal("metadata not echoed")
	}
}

func TestChatCompletionToResponseLengthIncomplete(t *testing.T) {
	chat := []byte(`{"id":"x","model":"m","choices":[{"message":{"role":"assistant","content":"half"},"finish_reason":"length"}]}`)
	var in responsesIn
	_ = json.Unmarshal([]byte(`{"model":"m"}`), &in)
	obj, _ := chatCompletionToResponse(chat, &in)
	if obj["status"] != "incomplete" {
		t.Fatalf("length must be incomplete: %v", obj)
	}
	d, _ := obj["incomplete_details"].(map[string]any)
	if d["reason"] != "max_output_tokens" {
		t.Fatalf("reason must be max_output_tokens: %v", obj)
	}
}

func TestTranscodeChatSSEToResponsesEvents(t *testing.T) {
	chatSSE := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\" there\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7,\"completion_tokens_details\":{\"reasoning_tokens\":1}}}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	in := &responsesIn{Model: "m"}
	rec := httptest.NewRecorder()
	usage := transcodeChatSSEToResponses(rec, bytes.NewBufferString(chatSSE), in)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	s := string(body)
	for _, ev := range []string{
		"event: response.created", "event: response.in_progress",
		"event: response.output_text.delta", "event: response.output_text.done",
		"event: response.content_part.done", "event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(s, ev) {
			t.Fatalf("missing %q in:\n%s", ev, s)
		}
	}
	var last map[string]any
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "data: ") {
			p := strings.TrimPrefix(line, "data: ")
			if strings.HasPrefix(p, "{") {
				var m map[string]any
				_ = json.Unmarshal([]byte(p), &m)
				if m["type"] == "response.completed" {
					last = m
				}
			}
		}
	}
	if last == nil {
		t.Fatal("no response.completed event")
	}
	resp, _ := last["response"].(map[string]any)
	if resp["id"] == nil || len(resp["id"].(string)) < 6 || resp["id"].(string)[:5] != "resp_" {
		t.Fatalf("id not resp_*: %v", resp)
	}
	u, _ := resp["usage"].(map[string]any)
	if u == nil || u["total_tokens"] == nil {
		t.Fatalf("completed carries no usage: %v", resp)
	}
	od, _ := u["output_tokens_details"].(map[string]any)
	if od == nil || (od["reasoning_tokens"] != int64(1) && od["reasoning_tokens"] != float64(1)) {
		t.Fatalf("reasoning_tokens lost in stream: %v", u)
	}
	if usage == nil || usage.TotalTokens != 7 {
		t.Fatalf("usage not returned: %+v", usage)
	}
	_ = config.KeyConf{}
	_ = provider.Provider{}
	_ = http.StatusOK
}
