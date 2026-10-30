package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ByJotaro/ai-router/internal/crypto"
	"github.com/ByJotaro/ai-router/internal/lockout"
	"github.com/ByJotaro/ai-router/internal/route"
	"github.com/ByJotaro/ai-router/internal/stream"
)

//	Request: model, input (string | input-item array), instructions,
//	  reasoning ({effort, summary} passthrough), max_output_tokens,
//	  tools (flat {type,name,description,parameters}), tool_choice,
//	  stream, previous_response_id, metadata, temperature, top_p,
//	  parallel_tool_calls.
//	Response object: id resp_*, object=response, created_at, model,
//	  status (completed/incomplete/failed), output[] (message items with
//	  output_text parts, function_call items), usage
//	  (input/output/total + input_tokens_details.cached_tokens +
//	  output_tokens_details.reasoning_tokens), error / incomplete_details.
//	SSE: response.created / response.in_progress /
//	  response.output_item.added / response.content_part.added /
//	  response.output_text.delta / response.output_text.done /
//	  response.content_part.done / response.output_item.done /
//	  response.completed (resp.failed only on error), each framed with an
//	  explicit `event:` line per SSE standard — not bare data: lines.
//
// Execution reuses the chat path: the Responses request is converted to a
// chat body (responsesInToChat) and served by router.Call, so routing,
// retries, think-only recovery and accounting behave identically. The chat
// result is mapped back: non-stream → Response object JSON; stream →
// Responses SSE transcoded from the chat-SSE delta stream.
//
// previous_response_id / instructions / metadata / store are accepted and
// echoed (instructions selects the leading developer/system message);
// server-side conversation chaining is stateless — previous_response_id is
// NOT resolved to stored items (store:false semantics): the caller sends
// the full input each turn.

// responsesIn is the subset of the /responses request we consume.
type responsesIn struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       json.RawMessage `json:"instructions"`
	Reasoning          json.RawMessage `json:"reasoning"`
	MaxOutputTokens    *int            `json:"max_output_tokens"`
	Tools              json.RawMessage `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	Stream             bool            `json:"stream"`
	PreviousResponseID string          `json:"previous_response_id"`
	Metadata           json.RawMessage `json:"metadata"`
	Temperature        *float64        `json:"temperature"`
	TopP               *float64        `json:"top_p"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls"`
	Store              *bool           `json:"store"`
}

// handleResponses serves POST /v1/responses and POST /responses.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	// Auth FIRST, same as chat: reject unknown callers before 64MB reads.
	if _, _, aerr := s.authenticate(r); aerr != nil {
		writeResponsesErr(w, aerr.Status, aerr.Code, aerr.Message)
		return
	}
	body, rerr := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if rerr != nil {
		writeResponsesErr(w, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}
	if len(body) == 64<<20 {
		writeResponsesErr(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 64MB cap")
		return
	}

	// Request-shape diagnostics (reqdump.go), same contract as handleChat.
	dw := &dumpWriter{ResponseWriter: w}
	w = dw
	reqStart := time.Now()
	defer finishRequest(r, body, dw, reqStart)
	var in responsesIn
	if err := json.Unmarshal(body, &in); err != nil {
		writeResponsesErr(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Model) == "" {
		writeResponsesErr(w, http.StatusBadRequest, "invalid_request", "field 'model' is required")
		return
	}
	if len(bytes.TrimSpace(in.Input)) == 0 || string(bytes.TrimSpace(in.Input)) == "null" {
		if strings.TrimSpace(in.PreviousResponseID) == "" {
			writeResponsesErr(w, http.StatusBadRequest, "invalid_request", "field 'input' is required (or 'previous_response_id' with appended input)")
			return
		}
		// previous_response_id alone: stateless server has no stored
		// items — report honestly, never hallucinate a turn.
		writeResponsesErr(w, http.StatusBadRequest, "invalid_request", "previous_response_id chaining is stateless: send full 'input' each turn")
		return
	}

	chatBody, convErr := responsesInToChat(&in)
	if convErr != nil {
		writeResponsesErr(w, http.StatusBadRequest, "invalid_request", convErr.Error())
		return
	}
	// Reasoning default mirrors chat: family auto-max (xhigh for
	// muse-spark/mimo with 32768 budget) when the client set nothing.
	// The chat body already carries reasoning when the client sent it —
	// ensureReasoningMax passthroughs explicit params untouched.
	chatBody = ensureReasoningMax(chatBody)

	var hint struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(chatBody, &hint)
	slog.Default().Info("req", "remote", r.RemoteAddr, "model", hint.Model, "stream", in.Stream, "path", r.URL.Path, "rid", strings.TrimSpace(r.Header.Get("X-Request-ID")))
	admin, k, aerr := s.authenticate(r)
	if aerr != nil {
		writeResponsesErr(w, aerr.Status, aerr.Code, aerr.Message)
		return
	}
	if !admin {
		if _, err := s.keys.Authorize(k, in.Model); err != nil {
			status, code, _, msg := keyError(err)
			writeResponsesErr(w, status, code, msg)
			return
		}
	}

	// Idempotency for non-stream, same policy as chat.
	idemHash := ""
	if s.idemOn && !in.Stream && s.db != nil {
		if rid := strings.TrimSpace(r.Header.Get("X-Request-ID")); rid != "" {
			idemHash = "rid:" + rid
		} else {
			idemHash = "responses:" + in.Model + ":" + string(bodyHash(body))
		}
		if status, cached, ok := s.idemGet(idemHash); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-AiRouter-Idempotent", "replay")
			w.WriteHeader(status)
			_, _ = w.Write(cached)
			return
		}
	}

	start := time.Now()
	callCtx := route.ContextWithClientHeaders(r.Context(), r.Header)
	res, err := s.router.Call(callCtx, in.Model, chatBody, in.Stream)
	if err != nil && isUpstream500(err) && !hasExplicitReasoning(chatBody) {
		// Same deep-reasoning 500 recovery as chat (2026-09-19:
		// killer-prompt 500s on xhigh/high/medium, 200 on low).
		if s.ring != nil {
			s.ring.Add("warn", fmt.Sprintf("responses upstream 500, retrying low model=%s", in.Model))
		}
		res, err = s.router.Call(callCtx, in.Model, forceLowReasoning(forcedAnswerBody(chatBody)), in.Stream)
	}
	if err != nil {
		// Отмена клиента: соединение уже мёртво — не пишем ошибку в сокет и в лог.
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		status := writeResponsesRouteError(w, err, s.sanitize)
		ev := s.requestEvent(in.Model, nil, nil, k, status, start, r)
		ev.Error = crypto.SafeError(err.Error())
		var use *route.UpstreamStatusError
		if errors.As(err, &use) && use.RetryAfter != "" {
			ev.Error += " retry_after=" + use.RetryAfter
		}
		s.enqueueRecord(ev, r.URL.Path)
		slog.Default().Info("resp", "path", r.URL.Path, "status", status, "model", in.Model, "error", crypto.SanitizeForLog(err.Error()), "dur_ms", time.Since(start).Milliseconds())
		return
	}
	s.setTelemetry(w, k, in.Model, res.Provider, time.Since(start))
	if s.ring != nil {
		s.ring.Add("info", fmt.Sprintf("responses model=%s provider=%s (%s)", in.Model, res.Provider, time.Since(start).Round(time.Millisecond)))
	}

	if in.Stream {
		s.serveResponsesStream(w, r, res, &in, k, admin, start)
		return
	}
	s.serveResponsesJSON(w, r, res, &in, chatBody, k, admin, callCtx, idemHash, start)
}

// serveResponsesJSON maps one chat.completion body to a Response object.
// Think-only/empty answers retry once on a fresh lease with an explicit
// answer instruction — same policy as the chat non-stream path.
func (s *Server) serveResponsesJSON(w http.ResponseWriter, r *http.Request, res *route.Result, in *responsesIn, chatBody []byte, k string, admin bool, callCtx context.Context, idemHash string, start time.Time) {
	const thinkOnlyRetries = 1
	for attempt := 0; ; attempt++ {
		data, rerr := io.ReadAll(io.LimitReader(res.Resp.Body, 64<<20))
		res.Resp.Body.Close()
		final := attempt >= thinkOnlyRetries
		if rerr == nil && !final && thinkOnlyBody(data) {
			if s.ring != nil {
				s.ring.Add("warn", fmt.Sprintf("responses empty non-stream, retrying model=%s provider=%s attempt=%d", in.Model, res.Provider, attempt+1))
			}
			var err error
			res, err = s.router.Call(callCtx, in.Model, forcedAnswerBody(chatBody), false)
			if err != nil {
				st := writeResponsesRouteError(w, err, s.sanitize)
				slog.Default().Info("resp", "path", r.URL.Path, "status", st, "provider", "", "dur_ms", time.Since(start).Milliseconds())
				return
			}
			continue
		}
		obj, usage := chatCompletionToResponse(data, in)
		out, _ := json.Marshal(obj)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.Status)
		_, _ = w.Write(out)
		if usage != nil && !admin {
			_ = s.keys.RecordUsageByKey(k, actualRespModel(res, in.Model), usage.PromptTokens, usage.CompletionTokens)
		}
		if idemHash != "" && res.Status >= 200 && res.Status < 400 {
			s.idemPut(idemHash, k, res.Status, out)
		}
		s.enqueueRecord(s.requestEvent(in.Model, res, usage, k, res.Status, start, r), r.URL.Path)
		slog.Default().Info("resp", "path", r.URL.Path, "status", res.Status, "provider", res.Provider, "stream", false, "dur_ms", time.Since(start).Milliseconds())
		return
	}
}

// serveResponsesStream transcodes the chat-SSE result into Responses SSE.
// The upstream may answer non-SSE (plain JSON despite stream=true): that
// single body is emitted as synthesized deltas + completed, so the client
// always sees a well-formed Responses stream.
func (s *Server) serveResponsesStream(w http.ResponseWriter, r *http.Request, res *route.Result, in *responsesIn, k string, admin bool, start time.Time) {
	// Honest upstream error before stream start: JSON error, never fake SSE.
	if res.Status >= 400 {
		data, _ := io.ReadAll(io.LimitReader(res.Resp.Body, 64<<20))
		res.Resp.Body.Close()
		st := res.Status
		if st < 400 || st >= 600 {
			st = http.StatusBadGateway
		}
		msg := responsesErrMessage(data, http.StatusText(st))
		writeResponsesErr(w, st, responsesErrCode(res.Status), msg)
		s.enqueueRecord(s.requestEvent(in.Model, res, nil, k, st, start, r), r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	usage := transcodeChatSSEToResponses(w, res.Resp.Body, in)
	res.Resp.Body.Close()
	if usage != nil && !admin {
		_ = s.keys.RecordUsageByKey(k, actualRespModel(res, in.Model), usage.PromptTokens, usage.CompletionTokens)
	}
	s.enqueueRecord(s.requestEvent(in.Model, res, usage, k, http.StatusOK, start, r), r.URL.Path)
	slog.Default().Info("resp", "path", r.URL.Path, "status", 200, "stream", true, "dur_ms", time.Since(start).Milliseconds())
}

func actualRespModel(res *route.Result, req string) string {
	if res.Model != "" {
		return res.Model
	}
	return req
}

// responsesInToChat converts a Responses request into a chat body for
// router.Call. instructions becomes the leading system message (spec:
// inserted into the model's context); string input becomes one user
// message; input-item arrays map per type (message / function_call /
// function_call_output); reasoning/effort+summary, max_output_tokens→
// max_completion_tokens, temperature/top_p/parallel_tool_calls/tools pass
// through in chat names. metadata/previous_response_id/store/user shape
// the Response envelope, not the chat body.
func responsesInToChat(in *responsesIn) ([]byte, error) {
	msgs, err := responsesInputToMessages(in.Input, in.Instructions)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"model":    in.Model,
		"messages": msgs,
		"stream":   in.Stream,
	}
	if in.MaxOutputTokens != nil {
		out["max_completion_tokens"] = *in.MaxOutputTokens
	}
	if len(bytes.TrimSpace(in.Tools)) > 0 && string(bytes.TrimSpace(in.Tools)) != "null" {
		out["tools"] = responsesToolsToChat(in.Tools)
	}
	if len(bytes.TrimSpace(in.ToolChoice)) > 0 && string(bytes.TrimSpace(in.ToolChoice)) != "null" {
		if tc := responsesToolChoiceToChat(in.ToolChoice); len(tc) > 0 {
			out["tool_choice"] = tc
		}
	}
	if len(bytes.TrimSpace(in.Reasoning)) > 0 && string(bytes.TrimSpace(in.Reasoning)) != "null" {
		var rs struct {
			Effort  string `json:"effort"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(in.Reasoning, &rs); err == nil {
			robj := map[string]any{}
			if rs.Effort != "" {
				out["reasoning_effort"] = rs.Effort
				robj["effort"] = rs.Effort
			}
			robj["summary"] = rs.Summary
			out["reasoning"] = robj
		} else {
			var s string
			if json.Unmarshal(in.Reasoning, &s) == nil && s != "" {
				out["reasoning_effort"] = s
				out["reasoning"] = map[string]any{"effort": s}
			}
		}
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if in.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *in.ParallelToolCalls
	}
	return json.Marshal(out)
}

// responsesInputToMessages maps Responses input to chat messages.
// String input → single user message (spec: equivalent to user-role text).
// Array items: {role,content} messages (content string or input_text /
// input_image / input_file parts → chat text/image_url parts), assistant
// function_call items → assistant tool_calls, function_call_output items
// → tool messages with tool_call_id.
func responsesInputToMessages(raw, instructions json.RawMessage) ([]any, error) {
	msgs := []any{}
	if len(bytes.TrimSpace(instructions)) > 0 && string(bytes.TrimSpace(instructions)) != "null" {
		var s string
		if err := json.Unmarshal(instructions, &s); err == nil {
			if strings.TrimSpace(s) != "" {
				msgs = append(msgs, map[string]any{"role": "system", "content": s})
			}
		} else {
			// Array-form instructions: flatten to text, same as input.
			if txt := responsesContentToText(instructions); strings.TrimSpace(txt) != "" {
				msgs = append(msgs, map[string]any{"role": "system", "content": txt})
			}
		}
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return msgs, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return nil, errors.New("field 'input' must not be empty")
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": s})
		return msgs, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("field 'input' must be a string or an array of input items")
	}
	pendingCallID := ""
	pendingName := ""
	for i, it := range items {
		typ, _ := it["type"].(string)
		switch typ {
		case "function_call":
			name, _ := it["name"].(string)
			args, _ := it["arguments"].(string)
			cid, _ := it["call_id"].(string)
			if cid == "" {
				cid, _ = it["id"].(string)
			}
			if cid == "" {
				cid = fmt.Sprintf("call_in_%d", i)
			}
			if name == "" {
				name = "tool"
			}
			msgs = append(msgs, map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id":   cid,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args,
					},
				}},
			})
			pendingCallID = cid
			pendingName = name
		case "function_call_output":
			cid, _ := it["call_id"].(string)
			if cid == "" {
				cid = pendingCallID
			}
			if cid == "" {
				cid = fmt.Sprintf("call_in_%d", i)
			}
			text := responsesOutputToText(it["output"])
			msg := map[string]any{"role": "tool", "content": text, "tool_call_id": cid}
			if pendingName != "" {
				msg["name"] = pendingName
				pendingName = ""
			}
			msgs = append(msgs, msg)
			pendingCallID = ""
		case "reasoning":
			// Reasoning items are model-internal; not replayed as chat
			// turns (chatToResponses drops them the same way outbound).
			continue
		default:
			// message / easy input message / item with role+content.
			role, _ := it["role"].(string)
			if role == "" {
				role = "user"
			}
			switch role {
			case "system", "developer":
				role = "system"
			case "assistant", "user", "tool":
			default:
				role = "user"
			}
			content, images := responsesItemContent(it["content"])
			if role == "tool" {
				msgs = append(msgs, map[string]any{"role": "tool", "content": content, "tool_call_id": pendingCallID})
				pendingCallID = ""
				continue
			}
			if strings.TrimSpace(content) == "" && len(images) == 0 {
				// Spec mirrors the outbound guard: empty-content items
				// are rejected upstream — skip, never fail the turn.
				continue
			}
			if len(images) == 0 {
				msgs = append(msgs, map[string]any{"role": role, "content": content})
				continue
			}
			parts := []any{}
			if strings.TrimSpace(content) != "" {
				parts = append(parts, map[string]any{"type": "text", "text": content})
			}
			for _, img := range images {
				parts = append(parts, img)
			}
			msgs = append(msgs, map[string]any{"role": role, "content": parts})
		}
	}
	if len(msgs) == 0 {
		return nil, errors.New("field 'input' must contain at least one non-empty message")
	}
	return msgs, nil
}

// responsesItemContent renders Responses content (string | parts array
// with input_text/input_image/input_file) into chat text + image parts.
func responsesItemContent(raw any) (string, []any) {
	switch c := raw.(type) {
	case nil:
		return "", nil
	case string:
		return c, nil
	case []any:
		var b strings.Builder
		var imgs []any
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			pt, _ := m["type"].(string)
			switch pt {
			case "input_text":
				if t, _ := m["text"].(string); t != "" {
					b.WriteString(t)
				}
			case "input_image":
				url, _ := m["image_url"].(string)
				if url != "" {
					imgs = append(imgs, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			case "input_file":
				if fd, _ := m["file_data"].(string); fd != "" {
					b.WriteString(fd)
				} else if fu, _ := m["file_url"].(string); fu != "" {
					b.WriteString(fu)
				}
			case "output_text":
				if t, _ := m["text"].(string); t != "" {
					b.WriteString(t)
				}
			default:
				if t, _ := m["text"].(string); t != "" {
					b.WriteString(t)
				}
			}
		}
		return b.String(), imgs
	default:
		return "", nil
	}
}

// responsesContentToText flattens instructions arrays / unknown shapes.
func responsesContentToText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var items []any
	if json.Unmarshal(raw, &items) == nil {
		txt, _ := responsesItemContent(items)
		return txt
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		txt, _ := responsesItemContent(m["content"])
		return txt
	}
	return ""
}

// responsesOutputToText renders function_call_output output (string |
// input_text parts array) as plain text.
func responsesOutputToText(raw any) string {
	if s, ok := raw.(string); ok {
		return s
	}
	txt, _ := responsesItemContent(raw)
	return txt
}

// responsesToolsToChat maps flat Responses tools
// ({type:function,name,description,parameters[,strict]}) to chat tools
// ({type:function,function:{...}}). Non-function tools (web_search etc.)
// pass through untouched — the upstream decides.
func responsesToolsToChat(raw json.RawMessage) json.RawMessage {
	var tools []map[string]any
	if err := json.Unmarshal(raw, &tools); err != nil {
		return raw
	}
	chat := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		typ, _ := t["type"].(string)
		if typ == "" || typ == "function" {
			name, _ := t["name"].(string)
			if name == "" {
				if fn, ok := t["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
			}
			if name == "" {
				continue
			}
			fn := map[string]any{"name": name}
			if d, ok := t["description"].(string); ok && d != "" {
				fn["description"] = d
			} else {
				fn["description"] = name
			}
			if p, ok := t["parameters"]; ok && p != nil {
				fn["parameters"] = p
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			if st, ok := t["strict"]; ok && st != nil {
				fn["strict"] = st
			}
			entry := map[string]any{"type": "function", "function": fn}
			chat = append(chat, entry)
			continue
		}
		chat = append(chat, t)
	}
	out, err := json.Marshal(chat)
	if err != nil {
		return raw
	}
	return out
}

// responsesToolChoiceToChat maps Responses tool_choice to chat shape.
// "none"/"auto"/"required" pass verbatim; {"type":"function",...} and
// {"type":"allowed_tools",...} become chat {"type","function"}.
func responsesToolChoiceToChat(raw json.RawMessage) json.RawMessage {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "none", "auto", "required":
			return raw
		default:
			return nil
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	typ, _ := m["type"].(string)
	switch typ {
	case "function":
		name, _ := m["name"].(string)
		if name == "" {
			return nil
		}
		out, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": name}})
		return out
	case "allowed_tools":
		out, _ := json.Marshal(map[string]any{"type": "allowed_tools", "allowed_tools": m["allowed_tools"]})
		return out
	default:
		return nil
	}
}

// chatCompletionToResponse maps a chat.completion body to a Response
// object + stream.Usage. Text answers become one message item with a
// single output_text part; tool calls become function_call items (with
// matching call_id/name/arguments). finish_reason length → incomplete
// with reason max_output_tokens; error finish → failed with error object.
// usage maps 1:1 including reasoning_tokens passthrough; metadata echoes
// the request metadata; previous_response_id chains into the new id
// lineage is NOT fabricated — echo stays null unless the client sent one
// (kept on the envelope for correlation).
func chatCompletionToResponse(data []byte, in *responsesIn) (map[string]any, *stream.Usage) {
	var v struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *stream.Usage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &v)
	created := v.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	respID := "resp_" + newRespHex(12)
	msgID := "msg_" + newRespHex(12)
	if idemID := chatCompletionID(v.ID); idemID != "" {
		respID = idemID
	}
	model := v.Model
	if model == "" {
		model = in.Model
	}
	status := "completed"
	var errObj any
	var incomplete any
	output := []any{}
	if v.Error != nil {
		status = "failed"
		errObj = map[string]any{"code": orStr(v.Error.Code, "server_error"), "message": orStr(v.Error.Message, "upstream error")}
	} else if len(v.Choices) > 0 {
		c := v.Choices[0]
		if c.FinishReason == "length" {
			status = "incomplete"
			incomplete = map[string]any{"reason": "max_output_tokens"}
		} else if c.FinishReason == "content_filter" {
			status = "incomplete"
			incomplete = map[string]any{"reason": "content_filter"}
		} else if c.FinishReason == "error" {
			status = "failed"
			errObj = map[string]any{"code": "server_error", "message": "upstream error"}
		}
		for _, tc := range c.Message.ToolCalls {
			cid := tc.ID
			if cid == "" {
				cid = "call_" + newRespHex(12)
			}
			output = append(output, map[string]any{
				"id": cid, "type": "function_call", "status": "completed",
				"call_id": cid, "name": tc.Function.Name, "arguments": tc.Function.Arguments,
			})
		}
		if strings.TrimSpace(c.Message.Content) != "" {
			output = append(output, map[string]any{
				"id": msgID, "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{
					"type": "output_text", "text": c.Message.Content, "annotations": []any{},
				}},
			})
		}
	}
	if output == nil {
		output = []any{}
	}
	var usage any
	if v.Usage != nil {
		usage = map[string]any{
			"input_tokens":  v.Usage.PromptTokens,
			"output_tokens": v.Usage.CompletionTokens,
			"total_tokens":  v.Usage.TotalTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens": v.Usage.CacheRead,
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": v.Usage.Reasoning,
			},
		}
	}
	var meta any
	if len(bytes.TrimSpace(in.Metadata)) > 0 && string(bytes.TrimSpace(in.Metadata)) != "null" {
		var m map[string]any
		if json.Unmarshal(in.Metadata, &m) == nil {
			meta = m
		}
	}
	if meta == nil {
		meta = map[string]any{}
	}
	var prevID any
	if strings.TrimSpace(in.PreviousResponseID) != "" {
		prevID = in.PreviousResponseID
	}
	obj := map[string]any{
		"id": respID, "object": "response", "created_at": created,
		"status": status, "error": errObj, "incomplete_details": incomplete,
		"instructions":         responsesInstructionsEcho(in.Instructions),
		"max_output_tokens":    in.MaxOutputTokens,
		"model":                model,
		"output":               output,
		"parallel_tool_calls":  responsesParallelEcho(in.ParallelToolCalls),
		"previous_response_id": prevID,
		"reasoning":            responsesReasoningEcho(in.Reasoning),
		"store":                responsesStoreEcho(in.Store),
		"temperature":          responsesFloatEcho(in.Temperature, 1.0),
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          responsesToolChoiceEcho(in.ToolChoice),
		"tools":                responsesToolsEcho(in.Tools),
		"top_p":                responsesFloatEcho(in.TopP, 1.0),
		"truncation":           "disabled",
		"usage":                usage,
		"user":                 nil,
		"metadata":             meta,
	}
	return obj, v.Usage
}

// responsesInstructionsEcho renders instructions back onto the envelope
// (string stays string; array flattens to text; absent → nil).
func responsesInstructionsEcho(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return responsesContentToText(raw)
}

func responsesReasoningEcho(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return map[string]any{"effort": nil, "summary": nil}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		eff, _ := m["effort"].(string)
		sum, _ := m["summary"].(string)
		var e any
		if eff != "" {
			e = eff
		}
		var sm any
		if sum != "" {
			sm = sum
		}
		return map[string]any{"effort": e, "summary": sm}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return map[string]any{"effort": s, "summary": nil}
	}
	return map[string]any{"effort": nil, "summary": nil}
}

func responsesParallelEcho(b *bool) any {
	if b == nil {
		return true
	}
	return *b
}

func responsesStoreEcho(b *bool) any {
	if b == nil {
		return true
	}
	return *b
}

func responsesFloatEcho(f *float64, def float64) any {
	if f == nil {
		return def
	}
	return *f
}

func responsesToolChoiceEcho(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "auto"
	}
	var v any
	if json.Unmarshal(raw, &v) == nil {
		return v
	}
	return "auto"
}

func responsesToolsEcho(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return []any{}
	}
	var v any
	if json.Unmarshal(raw, &v) == nil {
		return v
	}
	return []any{}
}

// transcodeChatSSEToResponses relays a chat-completions SSE stream as
// Responses SSE: response.created + response.in_progress, one
// output_item/content_part pair, output_text.delta per content delta,
// function_call_arguments deltas for tool calls, output_text.done +
// content_part.done + output_item.done, then response.completed carrying
// the full Response object (output + usage with reasoning_tokens).
// Non-SSE bodies (plain chat.completion JSON) are synthesized into the
// same event sequence. Every event carries an explicit `event:` line per
// the SSE standard, as in the official example.
func transcodeChatSSEToResponses(w http.ResponseWriter, upstream io.Reader, in *responsesIn) *stream.Usage {
	respID := "resp_" + newRespHex(12)
	msgID := "msg_" + newRespHex(12)
	created := time.Now().Unix()
	emit := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		_, _ = io.WriteString(w, "event: "+event+"\n")
		_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	base := responseEnvelope(in, respID, created, "in_progress", nil, nil)
	emit("response.created", map[string]any{"type": "response.created", "response": base})
	emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": base})

	// Peek: JSON object (non-SSE upstream) vs SSE stream.
	peek := make([]byte, 2048)
	n, _ := io.ReadFull(upstream, peek)
	peek = peek[:n]
	trimmed := bytes.TrimSpace(peek)
	isSSE := bytes.HasPrefix(trimmed, []byte("data: ")) ||
		bytes.HasPrefix(trimmed, []byte("event: ")) ||
		bytes.Contains(trimmed, []byte("\ndata: "))
	restored := io.MultiReader(bytes.NewReader(peek), upstream)

	var text strings.Builder
	var calls []respCall
	callByIdx := map[int]*respCall{}
	var usage *stream.Usage
	status := "completed"
	var failedErr any

	if !isSSE {
		rawAll, _ := io.ReadAll(restored)
		var v struct {
			Choices []struct {
				Message struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *stream.Usage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rawAll, &v)
		usage = v.Usage
		if v.Error != nil {
			status = "failed"
			failedErr = map[string]any{"code": orStr(v.Error.Code, "server_error"), "message": orStr(v.Error.Message, "upstream error")}
		} else if len(v.Choices) > 0 {
			c := v.Choices[0]
			text.WriteString(c.Message.Content)
			for _, tc := range c.Message.ToolCalls {
				calls = append(calls, respCall{ID: orStr(tc.ID, "call_"+newRespHex(12)), Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
			switch c.FinishReason {
			case "length":
				status = "incomplete"
			case "error":
				status = "failed"
				failedErr = map[string]any{"code": "server_error", "message": "upstream error"}
			}
		}
	} else {
		itemAdded := false
		partAdded := false
		sc := bufio.NewScanner(restored)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		emitDelta := func(delta string) {
			if !itemAdded {
				emit("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": 0,
					"item": map[string]any{"id": msgID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
				})
				itemAdded = true
			}
			if !partAdded {
				emit("response.content_part.added", map[string]any{
					"type": "response.content_part.added", "item_id": msgID, "output_index": 0, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				})
				partAdded = true
			}
			emit("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": msgID, "output_index": 0, "content_index": 0, "delta": delta,
			})
		}
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				continue
			}
			var ch struct {
				Choices []struct {
					Delta struct {
						Content   string `json:"content"`
						ToolCalls []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
				Usage *stream.Usage `json:"usage"`
				Error *struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
				Type string `json:"type"`
			}
			if json.Unmarshal([]byte(payload), &ch) != nil {
				continue
			}
			if ch.Usage != nil {
				usage = ch.Usage
			}
			if ch.Error != nil && (ch.Error.Message != "" || ch.Error.Type != "" || ch.Error.Code != "") {
				status = "failed"
				failedErr = map[string]any{"code": orStr(ch.Error.Code, "server_error"), "message": orStr(ch.Error.Message, "upstream error")}
				continue
			}
			for _, c := range ch.Choices {
				if c.Delta.Content != "" {
					text.WriteString(c.Delta.Content)
					emitDelta(c.Delta.Content)
				}
				for _, tc := range c.Delta.ToolCalls {
					cp := callByIdx[tc.Index]
					if cp == nil {
						calls = append(calls, respCall{ID: tc.ID, Index: tc.Index, Name: tc.Function.Name})
						cp = &calls[len(calls)-1]
						callByIdx[tc.Index] = cp
					}
					if tc.ID != "" {
						cp.ID = tc.ID
					}
					if tc.Function.Name != "" {
						cp.Name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						cp.Arguments += tc.Function.Arguments
						emit("response.function_call_arguments.delta", map[string]any{
							"type":    "response.function_call_arguments.delta",
							"item_id": orStr(cp.ID, msgID), "output_index": len(calls) - 1,
							"delta": tc.Function.Arguments,
						})
					}
				}
				switch c.FinishReason {
				case "length":
					status = "incomplete"
				case "content_filter":
					status = "incomplete"
				case "error":
					status = "failed"
					failedErr = map[string]any{"code": "server_error", "message": "upstream error"}
				}
			}
		}
		if sc.Err() != nil {
			slog.Debug("[responses-in] chat SSE scan err", "err", sc.Err())
		}
	}

	final := text.String()
	output := []any{}
	// Close out per spec: output_text.done + content_part.done +
	// output_item.done for text; output_item.added/done per function call.
	if strings.TrimSpace(final) != "" || len(calls) == 0 {
		streamed := isSSE && text.Len() > 0
		if !streamed && strings.TrimSpace(final) != "" {
			emit("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"id": msgID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
			})
			emit("response.content_part.added", map[string]any{
				"type": "response.content_part.added", "item_id": msgID, "output_index": 0, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			})
			emit("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": msgID, "output_index": 0, "content_index": 0, "delta": final,
			})
		}
		emit("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": msgID, "output_index": 0, "content_index": 0, "text": final,
		})
		emit("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": msgID, "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": final, "annotations": []any{}},
		})
		emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0,
			"item": map[string]any{"id": msgID, "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": final, "annotations": []any{}}}},
		})
		output = append(output, map[string]any{
			"id": msgID, "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": final, "annotations": []any{}}},
		})
	}
	for i, c := range calls {
		cid := c.ID
		if cid == "" {
			cid = "call_" + newRespHex(12)
			calls[i].ID = cid
		}
		name := c.Name
		if name == "" {
			name = "tool"
		}
		emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": len(output),
			"item": map[string]any{"id": cid, "type": "function_call", "status": "in_progress", "call_id": cid, "name": name, "arguments": ""},
		})
		emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": len(output),
			"item": map[string]any{"id": cid, "type": "function_call", "status": "completed", "call_id": cid, "name": name, "arguments": c.Arguments},
		})
		output = append(output, map[string]any{
			"id": cid, "type": "function_call", "status": "completed",
			"call_id": cid, "name": name, "arguments": c.Arguments,
		})
	}
	if output == nil {
		output = []any{}
	}
	if status == "failed" {
		if failedErr == nil {
			failedErr = map[string]any{"code": "server_error", "message": "upstream error"}
		}
		done := responseEnvelope(in, respID, created, "failed", output, usage)
		done["error"] = failedErr
		emit("response.failed", map[string]any{"type": "response.failed", "response": done})
	} else if status == "incomplete" {
		done := responseEnvelope(in, respID, created, "incomplete", output, usage)
		done["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		emit("response.incomplete", map[string]any{"type": "response.incomplete", "response": done})
	} else {
		done := responseEnvelope(in, respID, created, "completed", output, usage)
		emit("response.completed", map[string]any{"type": "response.completed", "response": done})
	}
	return usage
}

// respCall accumulates one function call across chat-SSE tool deltas.
type respCall struct {
	ID        string
	Index     int
	Name      string
	Arguments string
}

// responseEnvelope builds the Response object carried by created /
// in_progress / completed events: id resp_*, object=response, created_at,
// model, status, output, usage (with reasoning_tokens + cached_tokens),
// plus echoed request fields (instructions, tools, tool_choice,
// temperature, top_p, parallel_tool_calls, reasoning, store, metadata).
func responseEnvelope(in *responsesIn, id string, created int64, status string, output []any, usage *stream.Usage) map[string]any {
	if output == nil {
		output = []any{}
	}
	var u any
	if usage != nil {
		u = map[string]any{
			"input_tokens":  usage.PromptTokens,
			"output_tokens": usage.CompletionTokens,
			"total_tokens":  usage.TotalTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens": usage.CacheRead,
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": usage.Reasoning,
			},
		}
	}
	var meta any
	if len(bytes.TrimSpace(in.Metadata)) > 0 && string(bytes.TrimSpace(in.Metadata)) != "null" {
		var m map[string]any
		if json.Unmarshal(in.Metadata, &m) == nil {
			meta = m
		}
	}
	if meta == nil {
		meta = map[string]any{}
	}
	var prevID any
	if strings.TrimSpace(in.PreviousResponseID) != "" {
		prevID = in.PreviousResponseID
	}
	return map[string]any{
		"id": id, "object": "response", "created_at": created,
		"status": status, "error": nil, "incomplete_details": nil,
		"instructions":         responsesInstructionsEcho(in.Instructions),
		"max_output_tokens":    in.MaxOutputTokens,
		"model":                in.Model,
		"output":               output,
		"parallel_tool_calls":  responsesParallelEcho(in.ParallelToolCalls),
		"previous_response_id": prevID,
		"reasoning":            responsesReasoningEcho(in.Reasoning),
		"store":                responsesStoreEcho(in.Store),
		"temperature":          responsesFloatEcho(in.Temperature, 1.0),
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          responsesToolChoiceEcho(in.ToolChoice),
		"tools":                responsesToolsEcho(in.Tools),
		"top_p":                responsesFloatEcho(in.TopP, 1.0),
		"truncation":           "disabled",
		"usage":                u,
		"user":                 nil,
		"metadata":             meta,
	}
}

// writeResponsesErr writes a Responses-style JSON error:
// {"error":{"code","message","type"}} with the honest HTTP status
// (4xx passthrough, 5xx upstream). Type mirrors OpenAI's envelope
// (invalid_request_error for 4xx, server_error for 5xx).
func writeResponsesErr(w http.ResponseWriter, status int, code, msg string) {
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "server_error"
	} else if status == http.StatusTooManyRequests {
		typ = "rate_limit_error"
	}
	if code == "" {
		code = http.StatusText(status)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg, "type": typ},
	})
}

// writeResponsesRouteError maps router errors to honest Responses errors:
// model_not_found 404, lockout 429, UpstreamStatusError keeps its status
// (403 region/account-gate, 429 rate_limit, 408/524 → 504 timeout);
// everything else is 502 upstream_error. Never collapses 4xx into 5xx.
func writeResponsesRouteError(w http.ResponseWriter, err error, sanitize bool) int {
	msg := err.Error()
	if sanitize {
		msg = crypto.SafeError(msg)
	}
	if errors.Is(err, route.ErrNoCandidates) {
		writeResponsesErr(w, http.StatusNotFound, "model_not_found", msg)
		return http.StatusNotFound
	}
	if errors.Is(err, lockout.ErrLocked) {
		writeResponsesErr(w, http.StatusTooManyRequests, "model_locked", msg)
		return http.StatusTooManyRequests
	}
	var upstream *route.UpstreamStatusError
	if errors.As(err, &upstream) {
		st := upstream.Status
		switch st {
		case http.StatusForbidden:
			if strings.Contains(msg, "account gate:") {
				writeResponsesErr(w, http.StatusForbidden, "account_gate", msg)
				return http.StatusForbidden
			}
			writeResponsesErr(w, http.StatusForbidden, "region_error", msg)
			return http.StatusForbidden
		case http.StatusTooManyRequests:
			writeResponsesErr(w, http.StatusTooManyRequests, "rate_limit", msg)
			return http.StatusTooManyRequests
		case http.StatusRequestTimeout, 524:
			writeResponsesErr(w, http.StatusGatewayTimeout, "gateway_timeout", msg)
			return http.StatusGatewayTimeout
		default:
			if st >= 500 && st < 600 {
				writeResponsesErr(w, st, "upstream_error", msg)
				return st
			}
			if st >= 400 && st < 500 {
				code := "upstream_error"
				if st == http.StatusUnauthorized {
					code = "unauthorized"
				} else if st == http.StatusNotFound {
					code = "model_not_found"
				}
				writeResponsesErr(w, st, code, msg)
				return st
			}
			writeResponsesErr(w, http.StatusBadGateway, "upstream_error", msg)
			return http.StatusBadGateway
		}
	}
	writeResponsesErr(w, http.StatusBadGateway, "upstream_error", msg)
	return http.StatusBadGateway
}

// responsesErrCode picks the Responses error code for a relayed upstream
// HTTP status on the stream path.
func responsesErrCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "region_error"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusTooManyRequests:
		return "rate_limit"
	case http.StatusRequestTimeout, 524:
		return "gateway_timeout"
	default:
		return "upstream_error"
	}
}

// responsesErrMessage extracts a message from a relayed upstream JSON body.
func responsesErrMessage(data []byte, fallback string) string {
	var v struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &v) == nil && v.Error != nil && v.Error.Message != "" {
		return v.Error.Message
	}
	if strings.TrimSpace(string(data)) != "" && len(data) < 500 {
		return strings.TrimSpace(string(data))
	}
	return fallback
}

func bodyHash(b []byte) []byte {
	h := 0
	for _, c := range b {
		h = h*31 + int(c)
	}
	return []byte(fmt.Sprintf("%x", h))
}

func chatCompletionID(id string) string {
	// Preserve resp_* ids end to end when the chat layer already minted
	// one (converted zen bodies); chatcmpl-* ids stay chat-scoped.
	if strings.HasPrefix(id, "resp_") {
		return id
	}
	return ""
}

func newRespHex(n int) string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strings.Repeat("0", 2*n)[:2*n]
	}
	return hex.EncodeToString(b[:n])
}

func orStr(s, def string) string {
	if s != "" {
		return s
	}
	return def
}
