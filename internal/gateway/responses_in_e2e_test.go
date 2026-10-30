package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/provider"
)

func fakeChatUpstream(t *testing.T) *provider.Provider {
	t.Helper()
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			if s, _ := req["stream"].(bool); s {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7,\"completion_tokens_details\":{\"reasoning_tokens\":1}}}\n\n")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "chatcmpl-e2e", "object": "chat.completion", "created": 1700000000, "model": req["model"],
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "hello"}, "finish_reason": "stop"}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13,
					"prompt_tokens_details":      map[string]any{"cached_tokens": 4},
					"completion_tokens_details": map[string]any{"reasoning_tokens": 7}},
			})
			return
		}
		http.NotFound(w, r)
	})
	return &provider.Provider{Name: "p", Type: "openai-compatible", BaseURL: up.URL, APIKeys: []string{"upkey"}, Models: []string{"m1"}}
}

func TestResponsesE2ENonStream(t *testing.T) {
	p := fakeChatUpstream(t)
	_, srv := newTestGateway(t, map[string]*provider.Provider{"p": p}, []config.KeyConf{vk("vkey")})
	res, body := doReq(t, srv.URL, "/v1/responses", "POST", "vkey", map[string]any{
		"model": "m1", "input": "hi", "instructions": "be brief",
		"metadata": map[string]any{"probe": "e2e"},
	})
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["object"] != "response" || obj["status"] != "completed" {
		t.Fatalf("envelope broken: %s", body)
	}
	id, _ := obj["id"].(string)
	if len(id) < 6 || id[:5] != "resp_" {
		t.Fatalf("id not resp_*: %s", body)
	}
	out, _ := obj["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("want 1 output item: %s", body)
	}
	u, _ := obj["usage"].(map[string]any)
	if u == nil || u["total_tokens"] == nil {
		t.Fatalf("no usage: %s", body)
	}
	od, _ := u["output_tokens_details"].(map[string]any)
	if od == nil || od["reasoning_tokens"] == nil {
		t.Fatalf("reasoning_tokens missing: %s", body)
	}
	// Alias path must behave identically.
	res2, body2 := doReq(t, srv.URL, "/responses", "POST", "vkey", map[string]any{"model": "m1", "input": "hi"})
	if res2.StatusCode != 200 {
		t.Fatalf("alias status %d: %s", res2.StatusCode, body2)
	}
}

func TestResponsesE2EStream(t *testing.T) {
	p := fakeChatUpstream(t)
	_, srv := newTestGateway(t, map[string]*provider.Provider{"p": p}, []config.KeyConf{vk("vkey")})
	payload := `{"model":"m1","input":"hi","stream":true}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/responses", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vkey")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var events []string
	var completed *struct {
		Type     string `json:"type"`
		Response *struct {
			ID     string `json:"id"`
			Object string `json:"object"`
			Status string `json:"status"`
			Output []any  `json:"output"`
			Usage  *struct {
				Total int64 `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	var curEvent string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			curEvent = strings.TrimPrefix(line, "event: ")
			events = append(events, curEvent)
			continue
		}
		if strings.HasPrefix(line, "data: ") && curEvent == "response.completed" {
			var m struct {
				Type     string `json:"type"`
				Response *struct {
					ID     string `json:"id"`
					Object string `json:"object"`
					Status string `json:"status"`
					Output []any  `json:"output"`
					Usage  *struct {
						Total int64 `json:"total_tokens"`
					} `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) == nil && m.Type == "response.completed" {
				cc := m
				completed = &struct {
					Type     string `json:"type"`
					Response *struct {
						ID     string `json:"id"`
						Object string `json:"object"`
						Status string `json:"status"`
						Output []any  `json:"output"`
						Usage  *struct {
							Total int64 `json:"total_tokens"`
						} `json:"usage"`
					} `json:"response"`
				}{Type: cc.Type, Response: cc.Response}
			}
		}
	}
	for _, want := range []string{"response.created", "response.in_progress", "response.output_text.delta", "response.output_text.done", "response.completed"} {
		found := false
		for _, e := range events {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing event %q (got %v)", want, events)
		}
	}
	if completed == nil || completed.Response == nil {
		t.Fatal("no response.completed payload")
	}
	if completed.Response.Object != "response" || completed.Response.Status != "completed" {
		t.Fatalf("completed envelope broken: %+v", completed.Response)
	}
	if len(completed.Response.Output) == 0 {
		t.Fatal("completed carries no output")
	}
	if completed.Response.Usage == nil || completed.Response.Usage.Total != 7 {
		t.Fatalf("completed usage broken: %+v", completed.Response.Usage)
	}
}

func TestResponsesE2EUpstream403Honest(t *testing.T) {
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"Region not supported","type":"invalid_request_error","code":"region_error"}}`)
	})
	p := &provider.Provider{Name: "p", Type: "openai-compatible", BaseURL: up.URL, APIKeys: []string{"upkey"}, Models: []string{"m1"}}
	_, srv := newTestGateway(t, map[string]*provider.Provider{"p": p}, []config.KeyConf{vk("vkey")})
	res, body := doReq(t, srv.URL, "/v1/responses", "POST", "vkey", map[string]any{"model": "m1", "input": "hi"})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", res.StatusCode, body)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["error"]; !ok {
		t.Fatalf("want error envelope: %s", body)
	}
}

func TestResponsesE2EValidation(t *testing.T) {
	p := fakeChatUpstream(t)
	_, srv := newTestGateway(t, map[string]*provider.Provider{"p": p}, []config.KeyConf{vk("vkey")})
	res, _ := doReq(t, srv.URL, "/v1/responses", "POST", "vkey", map[string]any{"model": "m1"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing input must be 400, got %d", res.StatusCode)
	}
	res2, _ := doReq(t, srv.URL, "/v1/responses", "POST", "vkey", map[string]any{"input": "hi"})
	if res2.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing model must be 400, got %d", res2.StatusCode)
	}
	res3, _ := doReq(t, srv.URL, "/v1/responses", "POST", "wrong", map[string]any{"model": "m1", "input": "hi"})
	if res3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad key must be 401, got %d", res3.StatusCode)
	}
}
