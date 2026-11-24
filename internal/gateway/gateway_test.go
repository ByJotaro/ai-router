package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/keys"
	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/route"
	"github.com/ByJotaro/ai-router/internal/store"
)

func fakeUpstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newTestGateway(t *testing.T, provs map[string]*provider.Provider, keyConfs []config.KeyConf) (*Server, *httptest.Server) {
	t.Helper()
	r := route.New(provs)
	ks := keys.New(nil, nil)
	if err := ks.Load(); err != nil {
		t.Fatal(err)
	}
	if err := ks.SetConfig(keyConfs); err != nil {
		t.Fatal(err)
	}
	s := New(r, ks, "admin-token", nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func vk(key string) config.KeyConf { return config.KeyConf{Name: "vk", Key: key} }

func doReq(t *testing.T, base, path, method, bearer string, body any) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, data
}

func TestNonStreamChat(t *testing.T) {
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.Header.Get("Authorization") != "Bearer upkey" {
			http.Error(w, "no auth", 401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion",
			"model":   req["model"],
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "hello"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	})
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, APIKeys: []string{"upkey"}, Models: []string{"m1"}},
	}
	_, srv := newTestGateway(t, provs, []config.KeyConf{vk("vkey")})

	res, body := doReq(t, srv.URL, "/v1/chat/completions", "POST", "vkey", map[string]any{"model": "m1", "messages": []any{}})
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["object"] != "chat.completion" {
		t.Fatalf("bad object: %v", out)
	}
}

func TestAdminTokenBypassesKeyStore(t *testing.T) {
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","choices":[]}`))
	})
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1"}},
	}
	_, srv := newTestGateway(t, provs, nil)
	res, body := doReq(t, srv.URL, "/v1/chat/completions", "POST", "admin-token", map[string]any{"model": "m1"})
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
}

func TestUnknownKey401(t *testing.T) {
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1"}},
	}
	_, srv := newTestGateway(t, provs, []config.KeyConf{vk("vkey")})
	res, body := doReq(t, srv.URL, "/v1/chat/completions", "POST", "wrong", map[string]any{"model": "m1"})
	if res.StatusCode != 401 {
		t.Fatalf("want 401, got %d: %s", res.StatusCode, body)
	}
	var env map[string]any
	_ = json.Unmarshal(body, &env)
	if _, ok := env["error"].(map[string]any); !ok {
		t.Fatalf("want OpenAI error envelope, got %s", body)
	}
}

func TestModelNotAllowed403(t *testing.T) {
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1"}},
	}
	_, srv := newTestGateway(t, provs, []config.KeyConf{{Name: "vk", Key: "vkey", Models: []string{"allowed"}}})
	res, body := doReq(t, srv.URL, "/v1/chat/completions", "POST", "vkey", map[string]any{"model": "m1"})
	if res.StatusCode != 403 {
		t.Fatalf("want 403, got %d: %s", res.StatusCode, body)
	}
}

func TestQuota429(t *testing.T) {
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","choices":[],"usage":{"total_tokens":5}}`))
	})
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1"}},
	}
	_, srv := newTestGateway(t, provs, []config.KeyConf{{Name: "vk", Key: "vkey", QuotaReqs: 1}})
	res1, _ := doReq(t, srv.URL, "/v1/chat/completions", "POST", "vkey", map[string]any{"model": "m1"})
	if res1.StatusCode != 200 {
		t.Fatalf("first req should pass, got %d", res1.StatusCode)
	}
	res2, body := doReq(t, srv.URL, "/v1/chat/completions", "POST", "vkey", map[string]any{"model": "m1"})
	if res2.StatusCode != 429 {
		t.Fatalf("want 429, got %d: %s", res2.StatusCode, body)
	}
	var env map[string]any
	_ = json.Unmarshal(body, &env)
	e := env["error"].(map[string]any)
	if e["code"] != "rate_limit" {
		t.Fatalf("want code rate_limit, got %v", e["code"])
	}
}

func TestModelsEndpoint(t *testing.T) {
	provs := map[string]*provider.Provider{
		"a": {Name: "a", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", Models: []string{"m1", "m2"}},
		"b": {Name: "b", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", Models: []string{"m2"}},
	}
	_, srv := newTestGateway(t, provs, nil)
	res, body := doReq(t, srv.URL, "/v1/models", "GET", "admin-token", nil)
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Data) != 6 {
		t.Fatalf("want 6 models (auto + 2 bare + 3 pinned), got %d: %s", len(out.Data), body)
	}
	if out.Data[0]["id"] != "auto" {
		t.Fatalf("first model = %+v, want auto", out.Data[0])
	}
	var ids []string
	for _, d := range out.Data {
		ids = append(ids, d["id"].(string))
	}
	for _, want := range []string{"a/m1", "a/m2", "b/m2"} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing pinned model %q in %v", want, ids)
		}
	}
}

func TestStreamingSSE(t *testing.T) {
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		chunks := []string{
			`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
			`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
			`data: {"id":"x","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			fmt.Fprintln(w, c)
			fmt.Fprintln(w)
			fl.Flush()
		}
	})
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1"}},
	}
	_, srv := newTestGateway(t, provs, []config.KeyConf{vk("vkey")})

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m1","stream":true,"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vkey")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("want SSE content type, got %q", ct)
	}
	var lines []string
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			lines = append(lines, sc.Text())
		}
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Hel") || !strings.Contains(joined, "lo") {
		t.Fatalf("missing content chunks: %s", joined)
	}
	if !strings.Contains(joined, "[DONE]") {
		t.Fatalf("missing [DONE]: %s", joined)
	}
	if !strings.Contains(joined, `"usage"`) {
		t.Fatalf("missing usage chunk: %s", joined)
	}
}

func TestStreamingNonSSEUpstreamConverted(t *testing.T) {
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[],"usage":{"total_tokens":2}}`))
	})
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1"}},
	}
	_, srv := newTestGateway(t, provs, []config.KeyConf{vk("vkey")})
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m1","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer vkey")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	s := string(body)
	if !strings.Contains(s, "chat.completion") || !strings.Contains(s, "[DONE]") {
		t.Fatalf("bad converted stream: %s", s)
	}
}

func TestToolsPassthrough(t *testing.T) {
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["tools"]; !ok {
			http.Error(w, "tools missing", 400)
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"moscow\"}"}}]},"finish_reason":"tool_calls"}]}`))
	})
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1"}},
	}
	_, srv := newTestGateway(t, provs, []config.KeyConf{vk("vkey")})
	res, body := doReq(t, srv.URL, "/v1/chat/completions", "POST", "vkey", map[string]any{
		"model":    "m1",
		"messages": []any{map[string]any{"role": "user", "content": "weather in moscow?"}},
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}},
	})
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["tool_calls"] == nil {
		t.Fatalf("expected tool_calls in response: %s", body)
	}
}

func TestUpstreamDown502(t *testing.T) {
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", Models: []string{"m1"}},
	}
	_, srv := newTestGateway(t, provs, []config.KeyConf{vk("vkey")})
	res, body := doReq(t, srv.URL, "/v1/chat/completions", "POST", "vkey", map[string]any{"model": "m1"})
	if res.StatusCode != 502 {
		t.Fatalf("want 502, got %d: %s", res.StatusCode, body)
	}
}

// TestUpstream429Honest verifies an exhausted upstream rate limit surfaces
// as a real 429, not a collapsed 502 — the keyless pool's honest-status
// contract.
func TestUpstream429Honest(t *testing.T) {
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit","message":"Rate limit exceeded"}}`))
	})
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1"}},
	}
	_, srv := newTestGateway(t, provs, []config.KeyConf{vk("vkey")})
	res, body := doReq(t, srv.URL, "/v1/chat/completions", "POST", "vkey", map[string]any{"model": "m1"})
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d: %s", res.StatusCode, body)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != "rate_limit" {
		t.Fatalf("want error.code rate_limit, got %s", body)
	}
}

func TestStatePersistence(t *testing.T) {
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","choices":[],"usage":{"total_tokens":5}}`))
	})
	provs := map[string]*provider.Provider{
		"p": {Name: "p", Type: "openai-compatible", BaseURL: up.URL, Models: []string{"m1"}},
	}
	r := route.New(provs)
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	ks := keys.New(st.DB(), nil)
	_ = ks.Load()
	_ = ks.SetConfig([]config.KeyConf{{Name: "vk", Key: "vkey", QuotaReqs: 1}})
	s := New(r, ks, "admin-token", nil)
	srv := httptest.NewServer(s.Handler())
	res, _ := doReq(t, srv.URL, "/v1/chat/completions", "POST", "vkey", map[string]any{"model": "m1"})
	srv.Close()
	if res.StatusCode != 200 {
		t.Fatalf("req failed: %d", res.StatusCode)
	}
	// Reopen the store from the same DB: usage must persist.
	ks2 := keys.New(st.DB(), nil)
	if err := ks2.Load(); err != nil {
		t.Fatal(err)
	}
	_ = ks2.SetConfig([]config.KeyConf{{Name: "vk", Key: "vkey", QuotaReqs: 1}})
	if _, err := ks2.Authorize("vkey", "m1"); err != keys.ErrQuota {
		t.Fatalf("want ErrQuota after persisted usage, got %v", err)
	}
}

// TestGatewayTimeoutIsIdleBased: a STREAM that keeps producing data must not be
// killed by a wall-clock cap. Live 2026-10-22 17:03: a long answer was cut at
// exactly 180s while tokens were still arriving (and a 3000-token ask hung at
// 180s before the first token).
func TestGatewayTimeoutIsIdleBased(t *testing.T) {
	h := gatewayTimeoutHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 10; i++ {
			_, _ = io.WriteString(w, "data: {\"n\":"+strconv.Itoa(i)+"}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}), 60*time.Millisecond, `{"error":{"message":"gateway timeout"}}`)

	srv := httptest.NewServer(h)
	defer srv.Close()
	start := time.Now()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: живой поток не должен обрезаться по wall-clock (body=%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"n":9`) {
		t.Fatalf("поток оборван на середине: %s", body)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("поток завершился за %s - тест не пересёк старый 60ms-дедлайн", d)
	}
}

// TestGatewayTimeoutCutsStalledStream: silence (no data) still times out, and
// the client gets a truthful SSE error instead of a silent EOF.
func TestGatewayTimeoutCutsStalledStream(t *testing.T) {
	h := gatewayTimeoutHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"n\":0}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}), 60*time.Millisecond, `{"error":{"message":"gateway timeout"}}`)

	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "gateway_timeout") {
		t.Fatalf("заглохший стрим обязан получить gateway_timeout, body=%s", body)
	}
}

// TestGatewayKeepaliveDoesNotResetIdle: `: keepalive` proves the hop is alive,
// not the upstream - a stalled stream that keeps sending comments must still be
// cut, otherwise a dead upstream hangs the request for the full idle cap.
func TestGatewayKeepaliveDoesNotResetIdle(t *testing.T) {
	h := gatewayTimeoutHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
				_, _ = io.WriteString(w, ": keepalive\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
	}), 60*time.Millisecond, `{"error":{"message":"gateway timeout"}}`)

	srv := httptest.NewServer(h)
	defer srv.Close()
	start := time.Now()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("keepalive держал мёртвый стрим %s вместо обрыва по idle", d)
	}
	if !strings.Contains(string(body), "gateway_timeout") {
		t.Fatalf("keepalive-only стрим обязан быть срезан по idle, body=%q", body)
	}
}

// TestRetryAfterEmptyKeepsAgentShape: an agent turn (the client declared
// tools) keeps its tool list on the retry and gains exactly one "execute now"
// user turn. The old path sent forcedAnswerBody, which dropped the tool list
// and asked for "the final answer as plain text": the model then abandoned
// tool calls and wrote prose that stopped mid-sentence, while the client
// recorded a clean stop (the 2026-11-19 OMP cut, reproduced 2026-11-20 from a
// MITM dump of the retry). A byte-identical retry reproduced the same
// think-only reply instead (subagents "stopped at a plan"), hence the nudge.
func TestRetryAfterEmptyKeepsAgentShape(t *testing.T) {
	body := []byte(`{"model":"m1","stream":true,"max_tokens":4000,"reasoning_effort":"max",` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"edit","parameters":{"type":"object"}}}]}`)
	out := retryBodyAfterEmpty(body)
	var doc struct {
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(doc.Tools) != 1 {
		t.Fatalf("agent tools dropped: %s", out)
	}
	if len(doc.Messages) != 2 || doc.Messages[1].Role != "user" {
		t.Fatalf("execute-now nudge missing: %s", out)
	}
}

// TestRetryAfterEmptyChatNudgesWithoutLoweringReasoning: a plain chat turn
// still gets the explicit "answer now" instruction, but the reasoning depth
// must survive untouched — answers are never silently made shallower.
func TestRetryAfterEmptyChatNudgesWithoutLoweringReasoning(t *testing.T) {
	body := []byte(`{"model":"m1","stream":true,"reasoning_effort":"max",` +
		`"reasoning":{"effort":"max","budget_tokens":32768},` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	var m map[string]any
	if err := json.Unmarshal(retryBodyAfterEmpty(body), &m); err != nil {
		t.Fatal(err)
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("nudge message missing: %v", m["messages"])
	}
	if got, _ := m["reasoning_effort"].(string); got != "max" {
		t.Fatalf("reasoning_effort=%q, want max (no downgrade)", got)
	}
	if r, _ := m["reasoning"].(map[string]any); r == nil || r["effort"] != "max" {
		t.Fatalf("reasoning=%v, want effort max", m["reasoning"])
	}
}

// TestReasoningPolicyNeverLowersClient: the operator setting is a floor. A
// client that asked for max keeps max when the panel says low — the policy
// may only raise the level.
func TestReasoningPolicyNeverLowersClient(t *testing.T) {
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	p := &provider.Provider{
		Name: "p", Type: "openai-compatible", BaseURL: up.URL,
		APIKeys: []string{"upkey"}, Models: []string{"m1"},
		ModelReasoning: map[string]string{"m1": "low"},
	}
	s, _ := newTestGateway(t, map[string]*provider.Provider{"p": p}, nil)
	body := []byte(`{"model":"m1","reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`)
	var m map[string]any
	if err := json.Unmarshal(s.applyReasoningPolicy(body, "m1"), &m); err != nil {
		t.Fatal(err)
	}
	if got, _ := m["reasoning_effort"].(string); got != "max" {
		t.Fatalf("policy lowered client reasoning to %q, want max", got)
	}
}

// TestEffortRankOrdersLadder pins the ordering the floor rule depends on.
func TestEffortRankOrdersLadder(t *testing.T) {
	order := []string{"none", "low", "medium", "high", "xhigh", "max"}
	for i := 1; i < len(order); i++ {
		if effortRank(order[i]) <= effortRank(order[i-1]) {
			t.Fatalf("%s must outrank %s", order[i], order[i-1])
		}
	}
	if effortRank("") != 0 || effortRank("bogus") != 0 {
		t.Fatal("unknown effort must rank lowest")
	}
}

// TestThinkOnlyRetryKeepsAgentTools is the regression for the 2026-11-19 OMP
// turn "у меня нет доступного вызова task": when the first attempt emitted
// only reasoning, the gateway retried with forcedAnswerBody, which emptied the
// tool list (backfilled to read+bash only) and told the model to answer in
// plain text. The model then reported tools it could not see and produced a
// prose answer that stopped mid-sentence. Every forwarded attempt of an agent
// turn must carry the client's tools (task included) and no synthetic turn.
func TestThinkOnlyRetryKeepsAgentTools(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		bodies = append(bodies, req)
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n <= 2 { // first attempts: reasoning only, no content, no finish
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	p := &provider.Provider{
		Name: "p", Type: "openai-compatible", BaseURL: up.URL,
		APIKeys: []string{"upkey"}, Models: []string{"m1"},
	}
	_, srv := newTestGateway(t, map[string]*provider.Provider{"p": p}, []config.KeyConf{vk("vkey")})
	var tools []any
	for _, n := range []string{"task", "bash", "read", "write", "edit", "grep", "glob", "todo", "wait"} {
		tools = append(tools, map[string]any{
			"type":     "function",
			"function": map[string]any{"name": n, "parameters": map[string]any{"type": "object"}},
		})
	}
	res, body := doReq(t, srv.URL, "/v1/chat/completions", "POST", "vkey", map[string]any{
		"model": "m1", "stream": true, "reasoning_effort": "max", "max_tokens": 4096,
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "запусти 15 агентов"},
		},
		"tools": tools,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	mu.Lock()
	got := append([]map[string]any(nil), bodies...)
	mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("retry did not happen: %d upstream calls", len(got))
	}
	for i, b := range got {
		arr, _ := b["tools"].([]any)
		names := map[string]bool{}
		for _, e := range arr {
			if em, ok := e.(map[string]any); ok {
				if fn, ok := em["function"].(map[string]any); ok {
					if n, ok := fn["name"].(string); ok {
						names[n] = true
					}
				}
			}
		}
		if !names["task"] {
			t.Fatalf("attempt %d reached upstream without the task tool (tools=%v)", i+1, names)
		}
		// Attempt 1 is the client's body untouched; the think-only retry adds
		// exactly one "execute now" user turn — the model must be told to act,
		// not handed its own plan back (2026-11-20 subagent stall).
		msgs, _ := b["messages"].([]any)
		want := 2
		if i > 0 {
			want = 3
		}
		if len(msgs) != want {
			t.Fatalf("attempt %d has %d messages, want %d", i+1, len(msgs), want)
		}
		if eff, _ := b["reasoning_effort"].(string); eff != "max" {
			t.Fatalf("attempt %d reasoning_effort=%q, want max", i+1, eff)
		}
		if mt, ok := b["max_tokens"].(float64); !ok || mt != 4096 {
			t.Fatalf("attempt %d max_tokens=%v, want 4096", i+1, b["max_tokens"])
		}
	}
}

// fillReasoningAsAnswer must touch only a genuinely empty answer. A content
// array (multimodal/structured blocks) is real output: replacing it with the
// reasoning prose silently rewrote the reply.
func TestFillReasoningAsAnswerKeepsContentBlocks(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"answer"}],"reasoning_content":"thought"}}]}`)
	out := fillReasoningAsAnswer(in)
	if string(out) != string(in) {
		t.Fatalf("content blocks were overwritten: %s", out)
	}
	empty := []byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"thought"}}]}`)
	filled := fillReasoningAsAnswer(empty)
	if !strings.Contains(string(filled), `"content":"thought"`) {
		t.Fatalf("empty answer must be filled from reasoning: %s", filled)
	}
}

// A tool-call body legitimately carries content:null; overwriting it with the
// reasoning prose made the client render internal thinking as the assistant's
// visible answer next to the call (review 2026-11-20 F3).
func TestFillReasoningAsAnswerKeepsToolCallBodies(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":"thought","tool_calls":[{"id":"t","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`)
	if out := fillReasoningAsAnswer(in); string(out) != string(in) {
		t.Fatalf("tool-call body was rewritten: %s", out)
	}
}

// When the think-only retry itself fails (429/503 on a free lane), headers are
// already on the wire. The body must still end with a verdict and [DONE];
// otherwise the client waits on an open stream until its own timeout
// (measured 183 s "think-only retry failed after stream started").
func TestThinkOnlyRetryFailureClosesStream(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	up := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream down"}}`)
	})
	p := &provider.Provider{
		Name: "p", Type: "openai-compatible", BaseURL: up.URL,
		APIKeys: []string{"upkey"}, Models: []string{"m1"},
	}
	_, srv := newTestGateway(t, map[string]*provider.Provider{"p": p}, []config.KeyConf{vk("vkey")})
	res, body := doReq(t, srv.URL, "/v1/chat/completions", "POST", "vkey", map[string]any{
		"model": "m1", "stream": true, "max_tokens": 4096,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	text := string(body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, text)
	}
	if !strings.Contains(text, `"upstream_retry_failed"`) || !strings.Contains(text, "[DONE]") {
		t.Fatalf("failed retry must be relayed as an error event and closed: %q", text)
	}
}

// The router accepts the flat responses tool shape ({"type":"function",
// "name":...}), so the filter must read it too: a flat-shaped agent body used
// to parse to an empty set, silently dropping every declared call (review
// 2026-11-20 F4). An unknown shape with tools present fails OPEN, never to an
// empty set.
func TestDeclaredToolNamesFlatAndFailOpen(t *testing.T) {
	flat := []byte(`{"model":"m","tools":[{"type":"function","name":"read"}]}`)
	set := declaredToolNames(flat)
	if !set["read"] {
		t.Fatalf("flat tool name not parsed: %v", set)
	}
	unknown := []byte(`{"model":"m","tools":[{"weird":{"shape":true}}]}`)
	if got := declaredToolNames(unknown); got != nil {
		t.Fatalf("unknown tool shape must fail open (nil), got %v", got)
	}
	if got := declaredToolNames([]byte(`{"model":"m"}`)); got == nil || len(got) != 0 {
		t.Fatalf("tool-less body must stay an empty non-nil set, got %v", got)
	}
}
