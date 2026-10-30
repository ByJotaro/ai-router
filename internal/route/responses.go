package route

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ByJotaro/ai-router/internal/crypto"
	"github.com/ByJotaro/ai-router/internal/provider"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// This file adapts OpenAI Responses-protocol upstreams (zen muse-spark
// answers only POST /responses) to the chat/completions surface every
// client speaks. Request: chat body -> responses body. Response: the
// upstream responses-SSE stream is re-encoded into chat-completions SSE
// by wrapResponsesStream; non-stream JSON is mapped too.

// responsesBody is the subset of the /responses request we emit.
// MaxOutputTokens budgets reasoning+completion together (unlike chat
// max_tokens which caps visible completion only): same value yields
// shorter visible answers on thinking models. Forwarded from chat
// max_completion_tokens (preferred) or legacy max_tokens, clamped to
// [16|1024,128000] — floor 16 plain, 1024 with reasoning (small caps
// collide with the 32k reasoning budget -> 400/empty); ceiling is the
// server-side cap (128k, xhigh think + completion fit inside).
type responsesBody struct {
	Model           string          `json:"model"`
	Input           []any           `json:"input"`
	Stream          bool            `json:"stream"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Tools           json.RawMessage `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	Reasoning       *struct {
		Effort       string `json:"effort,omitempty"`
		BudgetTokens *int   `json:"budget_tokens,omitempty"`
	} `json:"reasoning,omitempty"`
}

type responsesInput struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatToolsToResponses rewrites chat-format tools into the flat Responses
// shape: {"type":"function","name":...,"description":...,"parameters":...}.
// zen validates strictly and gates free-tier on the toolset (live 2026-09-18):
// entries without a description+parameters object fail with 400 tools[N]
// (H2 minimal), and sets without read+bash get account-gate 403 (D2/S_bash/C1
// vs E_rb/F_br) — so defaults are filled and the gate pair is backfilled
// (forced bash+read).
func chatToolsToResponses(raw json.RawMessage, opencode bool) json.RawMessage {
	var in []map[string]any
	if len(bytes.TrimSpace(raw)) == 0 {
		in = nil
	} else if err := json.Unmarshal(raw, &in); err != nil {
		return raw // pass through unknown shapes verbatim
	}
	out := make([]map[string]any, 0, len(in)+2)
	have := map[string]bool{}
	for _, t := range in {
		if typ, _ := t["type"].(string); typ != "" && typ != "function" {
			out = append(out, t) // non-function tool: keep verbatim
			continue
		}
		name, desc, params, strict := responseToolFields(t)
		if name == "" {
			continue // unnamed entries never validate; drop instead of 400
		}
		have[name] = true
		emit := map[string]any{
			"type": "function", "name": name,
			"description": desc, "parameters": params,
		}
		if strict != nil {
			emit["strict"] = strict
		}
		out = append(out, emit)
	}
	// Forced canonical toolset for opencode upstreams: the inference lane
	// 403s stub toolsets (empty parameters / one-word descriptions fail even
	// with a genuine session — MITM bisect 2026-10-14). Stub-shaped copies of
	// canonical names are dropped; real definitions backfilled; other client
	// tools are kept verbatim.
	if opencode {
		kept := out[:0]
		for _, t := range out {
			if n, _ := t["name"].(string); n != "" && canonicalFlatNames[n] && isStubTool(t) {
				continue
			}
			kept = append(kept, t)
		}
		out = ensureCanonicalTools(kept)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return b
}

// responseToolFields extracts name/description/parameters from one chat tool
// (nested chat shape) or an already-flat responses tool, applying the
// defaults zen requires (non-empty description, parameters object).
func responseToolFields(t map[string]any) (string, string, any, any) {
	name, desc := "", ""
	var params any
	if fn, ok := t["function"].(map[string]any); ok && fn != nil {
		name, _ = fn["name"].(string)
		desc, _ = fn["description"].(string)
		if p, ok := fn["parameters"]; ok && p != nil {
			params = p
		}
	}
	if name == "" {
		name, _ = t["name"].(string)
	}
	if desc == "" {
		desc, _ = t["description"].(string)
	}
	if params == nil {
		params = t["parameters"]
	}
	if desc == "" {
		desc = name
	}
	// strict is a sibling of parameters on both shapes; the free-tier gate
	// requires it on the responses tools (MITM 2026-10-14: dropping it 403s).
	var strict any
	if fn, ok := t["function"].(map[string]any); ok && fn != nil {
		if s, ok := fn["strict"]; ok {
			strict = s
		}
	}
	if strict == nil {
		if s, ok := t["strict"]; ok {
			strict = s
		}
	}
	// zen needs a parameters OBJECT; missing/non-object shapes all 400.
	needDefault := true
	if m, ok := params.(map[string]any); ok && m != nil {
		if typ, _ := m["type"].(string); typ == "object" || typ == "" {
			needDefault = false
			if _, ok := m["properties"]; !ok {
				m["properties"] = map[string]any{}
			}
			params = m
		}
	}
	if needDefault {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return name, desc, params, strict
}

// normalizeToolChoice maps chat tool_choice to what zen /responses accepts
// (live 2026-09-18: only "auto" passes; "none"/named choices 400). "auto"
// passes verbatim, missing stays missing, everything else is dropped.
func normalizeToolChoice(raw json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(bytes.TrimSpace(raw)))
	if s == "" || s == "null" {
		return nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if str == "auto" {
			return raw
		}
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	if typ, _ := obj["type"].(string); typ == "auto" || typ == "" && len(obj) == 0 {
		return raw
	}
	return nil
}

// chatWantsStream parses the stream flag from JSON (not substring match:
// `"stream": true` with space — e.g. Python json.dumps — missed Contains).
func chatWantsStream(body []byte) bool {
	var v struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Stream == nil {
		return strings.Contains(string(body), `"stream":true`)
	}
	return *v.Stream
}

// clampOutputCap maps chat max_completion_tokens/max_tokens onto responses
// max_output_tokens with reasoning-aware floor + server-side cap.
// reasoning-budget guard (2026-09-19): max_output_tokens покрывает
// reasoning+completion ВМЕСТЕ — кап ниже budget_tokens роняет stripe
// целиком (live: nomax 200, max1024 500 на том же промпте). Поэтому при
// reasoning кап не опускаем ниже budget (дефолт-инжект 32768): клиентский
// max_tokens режет только completion-хвост, а не думание.
func clampOutputCap(mct, mt *int, reasoning bool, budget int) *int {
	want := -1
	if mct != nil && *mct > 0 {
		want = *mct
	} else if mt != nil && *mt > 0 {
		want = *mt
	} else {
		return nil
	}
	floor := 16
	if reasoning {
		floor = 1024
		if budget > floor {
			floor = budget
		}
	}
	if want < floor {
		want = floor
	}
	if want > 128000 {
		slog.Default().Debug("responses output cap clamped", "want", want, "cap", 128000)
		want = 128000
	}
	return &want
}

// effectiveReasoningEffort merges the two reasoning shapes the gateway may
// leave: string reasoning_effort (preferred, Cycle2 none-fix) and the raw
// reasoning object (gateway passthrough on explicit keys never injects the
// string, so object-only clients reached the converter with reasoning=false
// and lost the block upstream). String wins; "none" disables either way.
// A non-null object without effort (budget-only) still counts as reasoning
// for the clamp floor but forwards no effort label.
func effectiveReasoningEffort(str string, obj json.RawMessage) (string, bool) {
	if str != "" {
		return str, str != "none"
	}
	if len(bytes.TrimSpace(obj)) == 0 || string(bytes.TrimSpace(obj)) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(obj, &s); err == nil {
		return s, s != "" && s != "none"
	}
	var o struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(obj, &o); err == nil && o.Effort != "" {
		return o.Effort, o.Effort != "none"
	}
	return "", true
}

func chatToResponses(body []byte, opencode bool) ([]byte, error) {
	var in struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
			ToolCallID string `json:"tool_call_id,omitempty"`
			Name       string `json:"name,omitempty"`
		} `json:"messages"`
		Tools               json.RawMessage `json:"tools,omitempty"`
		ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
		ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
		Reasoning           json.RawMessage `json:"reasoning,omitempty"`
		MaxTokens           *int            `json:"max_tokens,omitempty"`
		MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	out := responsesBody{Model: in.Model, Stream: in.Stream, Tools: chatToolsToResponses(in.Tools, opencode), ToolChoice: normalizeToolChoice(in.ToolChoice)}
	// Reasoning merges string + object shapes (object-only explicit clients
	// otherwise clamp at floor 16 and lose the block upstream). Precedence
	// max_completion_tokens > max_tokens; pointer keeps omitempty honest
	// (absent stays absent = zen default). Clamped: floor 16 plain / 1024
	// with reasoning, ceiling 128000 (server-side cap — xhigh think +
	// completion fit inside; never shrunk by us).
	effort, reasoning := effectiveReasoningEffort(in.ReasoningEffort, in.Reasoning)
	var bgt struct {
		BudgetTokens *int `json:"budget_tokens"`
	}
	_ = json.Unmarshal(in.Reasoning, &bgt)
	budget := 0
	if bgt.BudgetTokens != nil && *bgt.BudgetTokens > 0 {
		budget = *bgt.BudgetTokens
	}
	if outCap := clampOutputCap(in.MaxCompletionTokens, in.MaxTokens, reasoning, budget); outCap != nil {
		out.MaxOutputTokens = outCap
	}
	pendingCallID := ""
	callGenCount := 0
	for _, m := range in.Messages {
		// Assistant turn that invoked tools -> one function_call item each
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				cid := tc.ID
				if cid == "" {
					// omp sends empty ids; zen requires call_id >= 1 char.
					// Счётчик инкрементим: иначе параллельные tool_call
					// получают один call_gen_0 и zen спарит output не туда.
					cid = fmt.Sprintf("call_gen_%d", callGenCount)
					callGenCount++
				}
				out.Input = append(out.Input, map[string]any{
					"type":      "function_call",
					"call_id":   cid,
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				})
				pendingCallID = cid
			}
			if text := flattenContent(m.Content); strings.TrimSpace(text) != "" {
				out.Input = append(out.Input, map[string]any{"role": "assistant", "content": text})
			}
			continue
		}
		// Tool result -> function_call_output referencing the last emitted
		// function_call (omp omits tool_call_id on these).
		if m.Role == "tool" {
			text := flattenContent(m.Content)
			cid := m.ToolCallID
			if cid == "" {
				cid = pendingCallID
			}
			if cid == "" {
				// Tool result with no matching call in history: zen needs
				// a function_call before the output, synthesize one.
				cid = fmt.Sprintf("call_gen_%d", len(out.Input))
				name := m.Name
				if name == "" {
					name = "tool"
				}
				out.Input = append(out.Input, map[string]any{
					"type":      "function_call",
					"call_id":   cid,
					"name":      name,
					"arguments": "{}",
				})
			}
			item := map[string]any{
				"type":    "function_call_output",
				"call_id": cid,
				"output":  text,
			}
			out.Input = append(out.Input, item)
			continue
		}
		text, images := splitContent(m.Content)
		if strings.TrimSpace(text) == "" && len(images) == 0 {
			continue
		}
		role := m.Role
		switch role {
		case "developer", "system", "assistant", "user":
		default:
			role = "user"
		}
		if len(images) == 0 {
			out.Input = append(out.Input, responsesInput{Role: role, Content: text})
			continue
		}
		// Vision message: content array with input_text + input_image parts
		// (Responses API shape) so the upstream model sees the picture.
		parts := make([]any, 0, len(images)+1)
		if strings.TrimSpace(text) != "" {
			parts = append(parts, map[string]any{"type": "input_text", "text": text})
		}
		for _, img := range images {
			parts = append(parts, img)
		}
		out.Input = append(out.Input, map[string]any{"role": role, "content": parts})
	}
	if len(out.Input) == 0 {
		out.Input = []any{map[string]any{"role": "user", "content": "(empty context)"}}
	}
	if reasoning {
		var b struct {
			BudgetTokens *int `json:"budget_tokens"`
		}
		_ = json.Unmarshal(in.Reasoning, &b)
		out.Reasoning = &struct {
			Effort       string `json:"effort,omitempty"`
			BudgetTokens *int   `json:"budget_tokens,omitempty"`
		}{Effort: effort, BudgetTokens: b.BudgetTokens}
	}
	return json.Marshal(out)
}

// contentPart is one chat message content part: text or image_url
// (OpenAI vision shape). Images are forwarded as input_image items so
// vision-capable upstreams (muse-spark 1.3) actually see the picture —
// flattenContent-as-text silently drops them.
type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	} `json:"image_url,omitempty"`
}

// splitContent separates a chat message content into text + image items.
// String content -> text only. Parts array -> text concatenated plus one
// input_image per image_url part (data: URLs pass through verbatim).
func splitContent(raw json.RawMessage) (string, []map[string]any) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil
	}
	var b strings.Builder
	var imgs []map[string]any
	for _, p := range parts {
		switch p.Type {
		case "image_url":
			if p.ImageURL != nil && p.ImageURL.URL != "" {
				imgs = append(imgs, map[string]any{
					"type":      "input_image",
					"image_url": p.ImageURL.URL,
				})
			}
		case "text":
			b.WriteString(p.Text)
		default:
			// Unknown part types (input_audio etc): keep text if present,
			// never fail the whole message.
			b.WriteString(p.Text)
		}
	}
	return b.String(), imgs
}

// flattenContent renders a chat message content (string or parts array)
// as plain text.
func flattenContent(raw json.RawMessage) string {
	text, _ := splitContent(raw)
	return text
}

// forceStreamTrue rewrites a converted /responses body to stream:true.
// Used for zen upstreams only (see responsesUpstream).
func forceStreamTrue(converted []byte) []byte {
	var v map[string]any
	if err := json.Unmarshal(converted, &v); err != nil {
		return converted
	}
	v["stream"] = true
	out, err := json.Marshal(v)
	if err != nil {
		return converted
	}
	return out
}

// responsesUpstream sends the converted body to POST {base}/responses with
func responsesUpstream(client *http.Client, baseURL, ua, apiKey string, converted []byte, ctx context.Context, p *provider.Provider, egressKey string) (*http.Response, error) {
	if isOpencodeUpstream(p) {
		// zen free-tier gates non-stream requests with account-gate 403
		// ("can only be used from within OpenCode"): real clients always
		// stream. Force stream upstream; non-stream callers are de-streamed
		// client-side by wrapResponsesStream/transcoder.
		converted = forceStreamTrue(converted)
	}
	u := strings.TrimRight(baseURL, "/") + "/responses"
	// С ctx: отмена клиента и poolTryDeadline прерывают висячий POST.
	// Без него (http.NewRequest) saturation держит коннекты дольше дедлайна.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(converted))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	crypto.StripUpstreamHeaders(req.Header, crypto.DefaultStripHeaders)
	applyFingerprintDiversity(req, p)
	tok := apiKey
	if tok == "" {
		tok = "public" // zen's keyless token for /responses
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	setOpencodeHeadersAt(req, ctx, p, egressKey)
	slog.Debug("[responses] ->", "url", u)
	// Use the caller-provided client: for pool leases it carries the
	// egress proxy; a bare client would bypass the pool from the home IP.
	res, err := client.Do(req)
	return res, err
}

// responsesError mirrors the upstream /responses error envelope.
type responsesError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// inferResponsesStatus maps a responses error envelope + top-level status
// into an honest HTTP status for passthrough. 0 means "no error, success".
func inferResponsesStatus(err *responsesError, status string) int {
	if err != nil {
		// Account gate (FreeTier/billing) outranks every other mapping:
		// it must surface as 403 so the pool/request path treats it as a
		// terminal gate verdict instead of a generic-failed 500 (which
		// burns dead-model budget) — 2026-09-17 spark.
		if isAccountGateBody([]byte(err.Code + " " + err.Type + " " + err.Message)) {
			return http.StatusForbidden
		}
		code := strings.ToLower(strings.TrimSpace(err.Code))
		typ := strings.ToLower(strings.TrimSpace(err.Type))
		msg := strings.ToLower(strings.TrimSpace(err.Message))
		// 403 RegionError — muse on unsupported regions returns
		// type=invalid_request_error code=region_error message~="Region"
		if strings.Contains(code, "region") || strings.Contains(typ, "region") || strings.Contains(msg, "region") {
			return http.StatusForbidden
		}
		// 429 rate limit
		if code == "rate_limit" || code == "rate_limit_error" || typ == "rate_limit_error" || strings.Contains(code, "rate_limit") || strings.Contains(msg, "rate limit") || strings.Contains(msg, "quota") {
			return http.StatusTooManyRequests
		}
		// 408 / 524 timeout (Cloudflare). Gateway will map 408/524 → 504.
		if code == "408" || code == "524" || typ == "408" || typ == "524" || strings.Contains(msg, " 524") || strings.Contains(msg, "524 ") || strings.Contains(msg, "gateway timeout") {
			return 524
		}
		if strings.Contains(code, "timeout") || strings.Contains(typ, "timeout") || strings.Contains(msg, "timeout") {
			// Distinguish 408 vs 524 by explicit code, else default 524.
			if code == "408" || strings.Contains(msg, "408") {
				return http.StatusRequestTimeout
			}
			return 524
		}
		if strings.Contains(msg, "408") || code == "408" {
			return http.StatusRequestTimeout
		}
		// 5xx
		if typ == "server_error" || typ == "internal_server_error" || code == "internal_error" || code == "server_error" || strings.Contains(msg, "internal server error") || strings.Contains(msg, "internal error") {
			return http.StatusInternalServerError
		}
		if code == "500" || code == "502" || code == "503" || code == "504" {
			if c := code; len(c) == 3 && c[0] == '5' {
				if n := int(c[0]-'0')*100 + int(c[1]-'0')*10 + int(c[2]-'0'); n >= 500 && n < 600 {
					return n
				}
			}
		}
		// Generic failed with an error object but no specific mapping → 500
		if status == "failed" || status == "incomplete" {
			return http.StatusInternalServerError
		}
		// For error objects on otherwise successful statuses, still surface as 500
		// if they carry a message — better honest than silent success.
		if err.Message != "" || err.Code != "" || err.Type != "" {
			return http.StatusInternalServerError
		}
	}
	if status == "failed" || status == "incomplete" {
		return http.StatusInternalServerError
	}
	return 0
}

func openAIErrorPayload(err *responsesError, fallbackStatus string, fallbackCode int) []byte {
	code := "upstream_error"
	typ := "server_error"
	msg := "upstream error"
	if err != nil {
		if err.Code != "" {
			code = err.Code
		}
		if err.Type != "" {
			typ = err.Type
		}
		if err.Message != "" {
			msg = err.Message
		}
	} else if fallbackStatus != "" {
		msg = "responses status: " + fallbackStatus
		if fallbackCode != 0 {
			code = http.StatusText(fallbackCode)
		}
	}
	// Normalize type for OpenAI envelope: must be one of the known strings
	// but preserve original when possible (RegionError etc).
	payload := map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}}
	b, _ := json.Marshal(payload)
	return b
}

// wrapResponsesStream re-encodes a responses-SSE body into chat-completions
// SSE so the gateway's passthrough keeps working unchanged. It also
// honours the upstream responses error envelope honestly: a payload whose
// top-level status is failed/incomplete or whose error object maps to 403/
// 429/524/5xx via inferResponsesStatus is surfaced with that HTTP status
// when no content was emitted. When content was already emitted, the typed
// error is relayed verbatim as an SSE data event (the client already saw
// the answer). The stream is never forced to 200 when the payload is an
// error.
//
// Streaming path is PASS-THROUGH (vLLM/LiteLLM style): the upstream SSE is
// scanned and chat-completions chunks are emitted into the pipe as deltas
// arrive — headers (200 + text/event-stream) go out BEFORE the body is
// read, so TTFB is time-to-first-delta, not time-to-EOF. Error detection
// happens on the FIRST decisive event: seeing any content (delta,
// function_call, completed-with-output) commits to 200; seeing
// failed/incomplete/error with no content commits to the honest HTTP
// status. This matches the OpenAI contract: an error either arrives as an
// HTTP error (nothing emitted) or as an in-stream event after content —
// never "200 with 0 chunks and 0 tokens".
func wrapResponsesStream(upstream *http.Response, model string, clientWantsStream bool) (*http.Response, error) {
	// Fast sniff: peek at the first bytes to decide JSON vs SSE without
	// consuming the stream. JSON (stream:false upstream) still needs the
	// whole object, but that path is a single small body, not a stream.
	peek := make([]byte, 2048)
	n, _ := io.ReadFull(upstream.Body, peek)
	peek = peek[:n]
	trimmed := bytes.TrimSpace(peek)
	isSSE := bytes.HasPrefix(trimmed, []byte("data: ")) ||
		bytes.HasPrefix(trimmed, []byte("event: ")) ||
		bytes.Contains(trimmed, []byte("\ndata: "))
	restored := io.MultiReader(bytes.NewReader(peek), upstream.Body)
	if !isSSE {
		return wrapResponsesJSON(upstream, restored, model)
	}
	// SSE path: stream through a pipe. Headers commit optimistically to
	// 200 + SSE — the standard for every OpenAI-compatible proxy (vLLM
	// emits its error chunks in-stream too). A failed-without-content
	// payload is still surfaced as an honest HTTP status via a MID-STREAM
	// upgrade: the first decisive event decides; when it is an error with
	// no content, the pipe is replaced by a short JSON error body and the
	// status is set before any byte is written downstream. In practice the
	// gateway writes headers from res.Status AFTER this function returns
	// but reads the body lazily — so no byte flows before the decision.
	//
	// Decision scan reads events until the verdict is known (first content
	// vs terminal error), buffering ONLY that prefix (a few KB, not the
	// whole stream), then replays it into the transcoder.
	type decision struct {
		verdict string // "stream" | "error"
		status  int
		errObj  *responsesError
		prefix  []byte // raw bytes consumed during the scan
	}
	decide := func() decision {
		var buf bytes.Buffer
		sc := bufio.NewScanner(io.TeeReader(restored, &buf))
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var lastStatus string
		var lastErr *responsesError
		var seenError bool
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				continue
			}
			var ev responsesEvent
			if json.Unmarshal([]byte(payload), &ev) != nil {
				continue
			}
			switch ev.Type {
			case "response.output_text.delta", "response.reasoning_summary_text.delta":
				if strings.TrimSpace(ev.Delta) != "" {
					return decision{verdict: "stream", prefix: buf.Bytes()}
				}
			case "response.output_item.added":
				if ev.Item != nil && ev.Item.Type == "function_call" {
					return decision{verdict: "stream", prefix: buf.Bytes()}
				}
			case "response.function_call_arguments.delta":
				return decision{verdict: "stream", prefix: buf.Bytes()}
			case "response.completed", "response.failed", "response.incomplete":
				if ev.Response != nil {
					lastStatus = ev.Response.Status
					if ev.Response.Error != nil {
						lastErr = ev.Response.Error
						seenError = true
					}
				}
				// Terminal WITHOUT any content delta before it: decide now.
				if inferred := inferResponsesStatus(lastErr, lastStatus); inferred != 0 && seenError {
					return decision{verdict: "error", status: inferred, errObj: lastErr, prefix: buf.Bytes()}
				}
				if lastStatus == "completed" {
					return decision{verdict: "stream", prefix: buf.Bytes()}
				}
				return decision{verdict: "error", status: http.StatusInternalServerError, errObj: lastErr, prefix: buf.Bytes()}
			case "response.error":
				seenError = true
				if ev.Response != nil && ev.Response.Error != nil {
					lastErr = ev.Response.Error
					lastStatus = ev.Response.Status
					if lastStatus == "" {
						lastStatus = "failed"
					}
				}
				return decision{verdict: "error", status: inferOr500(lastErr, lastStatus), errObj: lastErr, prefix: buf.Bytes()}
			default:
				var typed responsesTypedError
				if json.Unmarshal([]byte(payload), &typed) == nil && typed.Type == "error" && typed.Error != nil {
					return decision{verdict: "error", status: inferOr500(typed.Error, ""), errObj: typed.Error, prefix: buf.Bytes()}
				}
			}
			// Safety: a healthy stream always decides within a few KB
			// (created/in_progress + first delta). Never buffer more.
			if buf.Len() > 64*1024 {
				return decision{verdict: "stream", prefix: buf.Bytes()}
			}
		}
		// EOF without verdict: empty SSE. Error only if an error was seen,
		// else stream the (empty) prefix through for the terminal chunk.
		if seenError {
			return decision{verdict: "error", status: inferOr500(lastErr, lastStatus), errObj: lastErr, prefix: buf.Bytes()}
		}
		return decision{verdict: "stream", prefix: buf.Bytes()}
	}
	d := decide()
	if d.verdict == "error" {
		// Decision consumed only the prefix: the live upstream body is
		// still open and never becomes the response body — close it now.
		// Missing this leaked one conn + attempt ctx per mapped 403/429.
		upstream.Body.Close()
		b := openAIErrorPayload(d.errObj, "", d.status)
		upstream.StatusCode = d.status
		upstream.Header.Set("Content-Type", "application/json")
		upstream.Header.Del("Content-Length")
		upstream.Body = io.NopCloser(bytes.NewReader(b))
		upstream.ContentLength = int64(len(b))
		if d.status == http.StatusForbidden && isAccountGateBody(b) {
			upstream.Header.Set(gateTerminalHeader, "1")
		}
		return upstream, nil
	}
	// Content verdict: transcode the FULL stream (prefix replay + live tail)
	// into chat-completions SSE, emitting each chunk as its delta arrives.
	pr, pw := newPipeReader()
	origBody := upstream.Body
	upstream.StatusCode = http.StatusOK
	upstream.Header.Set("Content-Type", "text/event-stream")
	upstream.Header.Del("Content-Length")
	upstream.Body = pr
	streamSrc := io.MultiReader(bytes.NewReader(d.prefix), restored)
	go func() {
		transcodeResponsesSSE(pw, streamSrc, model, clientWantsStream)
		// Transcode drained the UPSTREAM stream — close it for conn reuse.
		// upstream.Body is pr now (owned by the caller via leaseBody):
		// closing it here raced the consumer (closed pipe, 2026-09-18).
		origBody.Close()
	}()
	return upstream, nil
}

// responsesEvent is the shared shape of upstream /responses SSE payloads,
// used by both the decision scan and the transcoder.
type responsesEvent struct {
	Type   string `json:"type"`
	Delta  string `json:"delta"`
	ItemID string `json:"item_id"`
	Item   *struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		Name      string `json:"name"`
		CallID    string `json:"call_id"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response *struct {
		Status string          `json:"status"`
		Error  *responsesError `json:"error"`
	} `json:"response"`
}

// responsesTypedError is the inline {"type":"error","error":{...}} envelope.
type responsesTypedError struct {
	Type  string          `json:"type"`
	Error *responsesError `json:"error"`
}

// inferOr500 maps an error envelope to HTTP status, defaulting to 500.
func inferOr500(err *responsesError, status string) int {
	if st := inferResponsesStatus(err, status); st != 0 {
		return st
	}
	return http.StatusInternalServerError
}

// wrapResponsesJSON maps a whole-object JSON /responses body (stream:false
// upstream) to chat.completion. Unchanged success/error semantics; split
// out so the SSE path stays streaming.
func wrapResponsesJSON(upstream *http.Response, body io.Reader, model string) (*http.Response, error) {
	rawAll, rerr := io.ReadAll(body)
	upstream.Body.Close()
	if rerr != nil {
		slog.Debug("[responses] body read err", "err", rerr)
	}
	var resp struct {
		Status string          `json:"status"`
		Error  *responsesError `json:"error"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			TotalTokens        int64 `json:"total_tokens"`
			InputTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(rawAll, &resp)
	inferred := inferResponsesStatus(resp.Error, resp.Status)
	hasContent := false
	for _, o := range resp.Output {
		if o.Type == "message" {
			for _, c := range o.Content {
				if strings.TrimSpace(c.Text) != "" {
					hasContent = true
					break
				}
			}
		}
		if o.Type == "function_call" {
			hasContent = true
		}
		if hasContent {
			break
		}
	}
	// failed without content → honest JSON error, not a 200 success.
	if inferred != 0 && !hasContent {
		b := openAIErrorPayload(resp.Error, resp.Status, inferred)
		upstream.StatusCode = inferred
		upstream.Header.Set("Content-Type", "application/json")
		upstream.Header.Del("Content-Length")
		upstream.Body = io.NopCloser(bytes.NewReader(b))
		if inferred == http.StatusForbidden && isAccountGateBody(b) {
			upstream.Header.Set(gateTerminalHeader, "1")
		}
		return upstream, nil
	}
	// Success path: map to chat.completion.
	var text strings.Builder
	var tcs []map[string]any
	for _, o := range resp.Output {
		switch o.Type {
		case "function_call":
			tcs = append(tcs, map[string]any{
				"id": o.CallID, "type": "function",
				"function": map[string]any{"name": o.Name, "arguments": o.Arguments},
			})
		case "message":
			for _, c := range o.Content {
				text.WriteString(c.Text)
			}
		}
	}
	finReason := "stop"
	msg := map[string]any{"role": "assistant"}
	if len(tcs) > 0 {
		msg["tool_calls"] = tcs
		finReason = "tool_calls"
	} else {
		msg["content"] = text.String()
	}
	out := map[string]any{
		"id": "chatcmpl-resp", "object": "chat.completion", "created": timeNowUnix(), "model": model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finReason}},
	}
	if resp.Usage != nil {
		out["usage"] = map[string]any{
			"prompt_tokens":     resp.Usage.InputTokens,
			"completion_tokens": resp.Usage.OutputTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		}
	}
	b, _ := json.Marshal(out)
	upstream.StatusCode = http.StatusOK
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Del("Content-Length")
	upstream.Body = io.NopCloser(bytes.NewReader(b))
	return upstream, nil
}

// transcodeResponsesSSE re-encodes an upstream /responses SSE stream into
// chat-completions SSE, emitting each chunk the moment its delta arrives.
// This is the vLLM-style hot path: scan → convert → send, no accumulation.
// The input MUST start at the stream head (decision prefix replay + live
// tail); the decision scan already committed to 200, so errors from here
// are relayed verbatim as in-stream events (OpenAI standard: after content
// starts, failures arrive in-band, not as HTTP errors).
func transcodeResponsesSSE(pw *io.PipeWriter, src io.Reader, model string, clientWantsStream bool) {
	defer pw.Close()
	// Non-stream callers are de-streamed client-side into one JSON object:
	// emitting SSE chunks here corrupts the body (live 2026-09-18: "data:"
	// prefix + trailing chat.completion glued into one non-stream reply).
	send := func(v map[string]any) {
		if !clientWantsStream {
			return
		}
		b, _ := json.Marshal(v)
		_, _ = pw.Write([]byte("data: " + string(b) + "\n\n"))
	}
	id := "chatcmpl-resp"
	created := timeNowUnix()
	var usage map[string]any
	finish := ""
	roleSent := false
	var lastCompleted *struct {
		Status string          `json:"status"`
		Error  *responsesError `json:"error"`
		Usage  *struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			TotalTokens        int64 `json:"total_tokens"`
			InputTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	type openCall struct {
		index    int
		id, name string
		sawArgs  bool
	}
	var lastOpen *openCall
	openCalls := map[string]*openCall{}
	callIdx := 0
	textOut := strings.Builder{}
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var ev struct {
			Type   string `json:"type"`
			Delta  string `json:"delta"`
			ItemID string `json:"item_id"`
			Item   *struct {
				ID        string `json:"id"`
				Type      string `json:"type"`
				Name      string `json:"name"`
				CallID    string `json:"call_id"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Response *struct {
				Status string          `json:"status"`
				Error  *responsesError `json:"error"`
				Usage  *struct {
					InputTokens        int64 `json:"input_tokens"`
					OutputTokens       int64 `json:"output_tokens"`
					TotalTokens        int64 `json:"total_tokens"`
					InputTokensDetails *struct {
						CachedTokens int64 `json:"cached_tokens"`
					} `json:"input_tokens_details"`
					OutputTokensDetails *struct {
						ReasoningTokens int64 `json:"reasoning_tokens"`
					} `json:"output_tokens_details"`
				} `json:"usage"`
				Output []struct {
					Type      string `json:"type"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
					Content   []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.reasoning_summary_text.delta":
			if !clientWantsStream {
				textOut.WriteString(ev.Delta)
				continue
			}
			send(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"reasoning_content": ev.Delta}}},
			})
		case "response.output_text.delta":
			if !clientWantsStream {
				textOut.WriteString(ev.Delta)
				continue
			}
			delta := map[string]any{"content": ev.Delta}
			if !roleSent {
				delta["role"] = "assistant"
				roleSent = true
			}
			send(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []map[string]any{{"index": 0, "delta": delta}},
			})
		case "response.completed", "response.failed", "response.incomplete":
			lastCompleted = ev.Response
			if u := ev.Response; u != nil && u.Usage != nil {
				cached := int64(0)
				if u.Usage.InputTokensDetails != nil {
					cached = u.Usage.InputTokensDetails.CachedTokens
				}
				reasoning := int64(0)
				if u.Usage.OutputTokensDetails != nil {
					reasoning = u.Usage.OutputTokensDetails.ReasoningTokens
				}
				usage = map[string]any{
					"prompt_tokens":             u.Usage.InputTokens,
					"completion_tokens":         u.Usage.OutputTokens,
					"total_tokens":              u.Usage.TotalTokens,
					"prompt_tokens_details":     map[string]any{"cached_tokens": cached},
					"completion_tokens_details": map[string]any{"reasoning_tokens": reasoning},
				}
			}
			for _, o := range ev.Response.Output {
				if o.Type == "function_call" {
					finish = "tool_calls"
					break
				}
			}
			if finish == "" {
				finish = "stop"
			}
		case "response.output_item.added":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				oc := &openCall{index: callIdx, id: ev.Item.CallID, name: ev.Item.Name}
				openCalls[ev.Item.ID] = oc
				lastOpen = oc
				send(map[string]any{
					"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": callIdx, "id": ev.Item.CallID, "type": "function",
							"function": map[string]any{"name": ev.Item.Name, "arguments": ""},
						}},
					}}},
				})
				callIdx++
			}
		case "response.function_call_arguments.delta":
			// Привязка delta к вызову: сначала ev.item_id (точный id item),
			// иначе последний открытый вызов. Старый код брал ПЕРВЫЙ элемент
			// map (случайный порядок Go) — при 2+ параллельных вызовах
			idx := -1
			var hit *openCall
			if ev.ItemID != "" {
				if oc, ok := openCalls[ev.ItemID]; ok {
					idx = oc.index
					hit = oc
				}
			}
			if idx < 0 && lastOpen != nil {
				idx = lastOpen.index
				hit = lastOpen
			}
			if idx < 0 {
				continue
			}
			if hit != nil {
				hit.sawArgs = true
			}
			send(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{
					"tool_calls": []map[string]any{{"index": idx, "function": map[string]any{"arguments": ev.Delta}}},
				}}},
			})
		case "response.output_item.done":
			// zen's done-event carries the FULL arguments blob
			// (added-event only sets name+""). Forward it so the
			// client doesn't see an empty path/content (Write: "path
			// must be file path (was missing)"). Bind by item_id like
			// the delta case — lastOpen fallback when absent.
			if ev.Item != nil && ev.Item.Type == "function_call" {
				finish = "tool_calls"
				if ev.Item.Arguments != "" {
					idx := -1
					var hit *openCall
					if ev.Item.ID != "" {
						if oc, ok := openCalls[ev.Item.ID]; ok {
							idx = oc.index
							hit = oc
						}
					}
					if idx < 0 && lastOpen != nil {
						idx = lastOpen.index
						hit = lastOpen
					}
					// deltas уже несли куски — done-blob это их конкатенация,
					// слать его вторым разом = дубль. Только если deltas не было.
					if idx >= 0 && (hit == nil || !hit.sawArgs) {
						send(map[string]any{
							"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
							"choices": []map[string]any{{"index": 0, "delta": map[string]any{
								"tool_calls": []map[string]any{{"index": idx, "function": map[string]any{"arguments": ev.Item.Arguments}}},
							}}},
						})
					}
				}
			}
		case "response.error":
			// Typed error verbatim: preserve type/code/message, not just message.
			errObj := map[string]any{}
			if ev.Response != nil && ev.Response.Error != nil {
				e := ev.Response.Error
				if e.Message != "" {
					errObj["message"] = e.Message
				}
				if e.Type != "" {
					errObj["type"] = e.Type
				}
				if e.Code != "" {
					errObj["code"] = e.Code
				}
				if len(errObj) == 0 {
					errObj["message"] = "upstream error"
				}
			} else {
				errObj["message"] = "upstream error"
			}
			send(map[string]any{"error": errObj})
		default:
			// Inline {"type":"error","error":{...}} (no response envelope):
			// after content it is an in-band event per OpenAI standard.
			var typed responsesTypedError
			if json.Unmarshal([]byte(payload), &typed) == nil && typed.Type == "error" && typed.Error != nil {
				errObj := map[string]any{}
				if typed.Error.Message != "" {
					errObj["message"] = typed.Error.Message
				}
				if typed.Error.Type != "" {
					errObj["type"] = typed.Error.Type
				}
				if typed.Error.Code != "" {
					errObj["code"] = typed.Error.Code
				}
				if len(errObj) == 0 {
					errObj["message"] = "upstream error"
				}
				send(map[string]any{"error": errObj})
			}
		}
	}
	if sc.Err() != nil {
		slog.Debug("[responses] scan err", "err", sc.Err())
	}
	if !clientWantsStream {
		msg := map[string]any{"role": "assistant", "content": textOut.String()}
		finish2 := "stop"
		var tcs []map[string]any
		if lastCompleted != nil {
			for _, o := range lastCompleted.Output {
				if o.Type == "function_call" {
					tcs = append(tcs, map[string]any{
						"id": o.CallID, "type": "function",
						"function": map[string]any{"name": o.Name, "arguments": o.Arguments},
					})
				}
			}
		}
		if len(tcs) > 0 {
			msg["tool_calls"] = tcs
			delete(msg, "content")
			finish2 = "tool_calls"
		}
		out := map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": model,
			"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish2}},
		}
		if usage != nil {
			out["usage"] = usage
		}
		b, _ := json.Marshal(out)
		_, _ = pw.Write(b)
		slog.Debug("[responses] non-stream converted", "chars", textOut.Len())
		return
	}
	fin := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
	}
	if usage != nil {
		fin["usage"] = usage
	}
	send(fin)
	_, _ = pw.Write([]byte(`data: {"choices":[],"cost":"0"}` + "\n\n"))
	_, _ = pw.Write([]byte("data: [DONE]\n\n"))
	slog.Debug("[responses] stream done")
}

// newPipeReader returns an io.ReadCloser wired to pw.
func newPipeReader() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

// timeNowUnix is a tiny indirection for readability.
func timeNowUnix() int64 { return time.Now().Unix() }

// sseDecoder iterates "data: ..." payloads of an SSE stream.
type sseDecoder struct {
	sc   *bufio.Scanner
	data string
}

func newSSEDecoder(r io.Reader) *sseDecoder {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &sseDecoder{sc: sc}
}

// next advances to the next data payload; false at stream end.
func (d *sseDecoder) next() bool {
	for d.sc.Scan() {
		line := d.sc.Text()
		if strings.HasPrefix(line, "data: ") {
			d.data = strings.TrimPrefix(line, "data: ")
			if d.data == "[DONE]" {
				continue
			}
			return true
		}
	}
	return false
}
