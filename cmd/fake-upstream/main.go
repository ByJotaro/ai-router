// Command fake-upstream is an OpenAI-compatible chat completions server for
// e2e tests. Behaviour is driven by the requested model:
//
//	any model          -> normal completion (stream or non-stream), usage
//	<model>-fail       -> HTTP 500
//	<model>-429        -> HTTP 429
//	<model>-401        -> HTTP 401
//
// A request whose body contains a "tools" array returns a tool_calls message.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9321", "listen address")
	tag := flag.String("tag", "A", "provider tag embedded in responses")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []any{
				map[string]any{"id": "test-model", "object": "model", "owned_by": "fake-" + *tag},
				map[string]any{"id": "test-model-fail", "object": "model", "owned_by": "fake-" + *tag},
			},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			Model   string `json:"model"`
			Stream  bool   `json:"stream"`
			Tools   []any  `json:"tools"`
			Message string `json:"-"`
		}
		_ = json.Unmarshal(body, &req)

		switch {
		case *tag == "A" && strings.HasSuffix(req.Model, "-fail"):
			http.Error(w, "upstream exploded", 500)
			return
		case strings.HasSuffix(req.Model, "-429"):
			http.Error(w, "rate limited", 429)
			return
		case strings.HasSuffix(req.Model, "-401"):
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "bad key", 401)
			return
		}

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			fl := w.(http.Flusher)
			id := fmt.Sprintf("chatcmpl-fake-%s", *tag)
			for i, piece := range []string{"Hel", "lo", " from ", *tag} {
				fmt.Fprintf(w, "data: %s\n\n", jsonChunk(map[string]any{
					"id": id, "object": "chat.completion.chunk", "model": req.Model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": piece}, "finish_reason": nil}},
				}))
				fl.Flush()
				if i == 0 {
					time.Sleep(50 * time.Millisecond)
				}
			}
			fmt.Fprintf(w, "data: %s\n\n", jsonChunk(map[string]any{
				"id": id, "object": "chat.completion.chunk", "model": req.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
			}))
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}

		w.Header().Set("Content-Type", "application/json")
		content := "hello from fake-" + *tag
		var msg map[string]any
		if len(req.Tools) > 0 {
			msg = map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "get_weather", "arguments": `{"city":"moscow"}`},
				}},
			}
		} else {
			msg = map[string]any{"role": "assistant", "content": content}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": fmt.Sprintf("chatcmpl-fake-%s", *tag), "object": "chat.completion", "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
		})
	})

	log.Printf("fake-upstream %s listening on %s", *tag, *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func jsonChunk(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
