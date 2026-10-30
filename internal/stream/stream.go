// Package stream implements SSE passthrough for streaming chat completions:
// upstream SSE chunks are re-emitted as-is, non-SSE upstream responses are
// converted into a single SSE chunk, and the stream is always terminated
// with [DONE]. Final-chunk usage is captured for accounting.
package stream

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Usage captures the final chunk's token usage. CacheRead/CacheWrite/
// Reasoning/Compressed are populated from provider-specific usage details
// (see UnmarshalJSON); they are zero when the upstream does not report them.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CacheRead        int64 `json:"cache_read,omitempty"`
	CacheWrite       int64 `json:"cache_write,omitempty"`
	Reasoning        int64 `json:"reasoning,omitempty"`
	Compressed       int64 `json:"compressed,omitempty"`
}

// UnmarshalJSON extends the flat OpenAI counters with the provider-specific
// usage details: prompt_tokens_details.cached_tokens (OpenAI cache read),
// completion_tokens_details.reasoning_tokens (DeepSeek-style reasoning),
// cache_read/cache_creation_input_tokens (Anthropic) and compressed_tokens.
func (u *Usage) UnmarshalJSON(data []byte) error {
	type flat Usage
	var f struct {
		flat
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		ReasoningTokens          int64 `json:"reasoning_tokens"`
		CompressedTokens         int64 `json:"compressed_tokens"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	*u = Usage(f.flat)
	if f.PromptTokensDetails != nil {
		u.CacheRead = f.PromptTokensDetails.CachedTokens
	}
	if f.CompletionTokensDetails != nil {
		u.Reasoning = f.CompletionTokensDetails.ReasoningTokens
	}
	if f.CacheReadInputTokens > 0 {
		u.CacheRead = f.CacheReadInputTokens
	}
	if f.CacheCreationInputTokens > 0 {
		u.CacheWrite = f.CacheCreationInputTokens
	}
	if f.ReasoningTokens > 0 {
		u.Reasoning = f.ReasoningTokens
	}
	if f.CompressedTokens > 0 {
		u.Compressed = f.CompressedTokens
	}
	return nil
}

// Passthrough copies an upstream response to w as SSE. When the upstream
// returned SSE (content-type text/event-stream) its data events are relayed
// verbatim; a trailing [DONE] is appended if missing. When the upstream
// returned a plain JSON body (non-streaming provider despite stream=true)
// the body is emitted as a single data event plus a final usage event.
// The returned usage is the last usage seen in the stream.
func Passthrough(w http.ResponseWriter, upstream io.Reader, contentType string, flush, final bool) (Usage, bool) {
	var usage Usage
	streamStart := time.Now()
	var firstByteMs, firstContentMs int64
	defer func() {
		slog.Default().Info("stream: passthrough done",
			"first_byte_ms", firstByteMs, "first_content_ms", firstContentMs,
			"total_ms", time.Since(streamStart).Milliseconds(), "sse", isSSE(contentType))
	}()
	if isSSE(contentType) {
		// Cut only a fully silent stream (wedged egress): reasoning and
		// content chunks slide the deadline, so thinking forever is fine.
		tcr := &streamIdleReader{r: upstream, deadline: time.Now().Add(streamIdleTimeout)}
		sc := bufio.NewScanner(tcr)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		var st streamState
		// held buffers reasoning-only chunks so a stream that turns out to
		// be think-only (upstream emitted reasoning then closed without
		// content) can be retried on a fresh lease without the client
		// ever seeing a half-answer. First content chunk flushes it.
		var held [][]byte
		var heldBytes int
		flushedAny := false
		flushHeld := func() {
			for _, h := range held {
				writeEvent(w, flush, string(h))
			}
			if len(held) > 0 {
				flushedAny = true
			}
			held = nil
			heldBytes = 0
		}
		// writePayload marks the client as "has seen a payload": any direct
		// (non-held) frame counts, so a later truncation can tell a retry
		// (nothing shown) from a duplicated stream (review 2026-10-17 r09 F1).
		// Keepalive comments deliberately do NOT count — a think-only stream
		// must stay retryable.
		writePayload := func(payload string) {
			writeEvent(w, flush, payload)
			flushedAny = true
		}
		// Reasoning-stall guard (90s <= idle 100s): a stream emitting ONLY empty data: frames
		// (no reasoning, no content, no tool_calls — muse free-tier does
		// exactly this while "thinking") is indistinguishable from a wedged
		// egress and would otherwise pin the request forever because every
		// byte slides the idle deadline. Cut it after reasoningStallTimeout
		// of emptiness; the caller retries on a fresh lease. Streams that
		// actually emit reasoning deltas are NOT cut (they are healthy).
		stallStart := time.Now()
		// Early keepalive while the preamble is held: clients/proxies cut
		// idle SSE after ~15-30s of zero bytes, but long-thinking models
		// legitimately emit 20-90s of reasoning before first content. Ping
		// every 15s until content flows (SSE comment, ignored by OpenAI
		// clients, resets proxy idle timers).
		keepaliveStart := time.Now()
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			if len(line) > 6 && string(line[:6]) == "data: " {
				payload := line[6:]
				if len(payload) == 6 && string(payload) == "[DONE]" {
					// Defer [DONE] until the end: when the upstream stream
					// closes without a finish_reason we must emit a final
					// chunk first, or OpenAI clients fail with "stream
					// closed before a finish_reason was received".
					continue
				}
				// Single typed decode per chunk: state observation, usage
				// capture and finish_reason sanitization all read the same
				// parse. The map-based rewrites below run only on the rare
				// chunks that actually need rewriting.
				var ch chunkView
				if json.Unmarshal(payload, &ch) != nil {
					continue
				}
				if firstByteMs == 0 {
					firstByteMs = time.Since(streamStart).Milliseconds()
				}
				out := payload
				// Typed upstream error event ("type":"error" with a nested
				// error object): some opencode clusters (muse) answer model
				// or region problems with HTTP 200 + this event. Before any
				// content it is an upstream failure: hold it, retry on a
				// fresh lease, and surface the event verbatim only on the
				// final attempt. After content it is relayed live like any
				// other chunk (the client already saw the answer).
				if ch.Type == "error" || ch.Error.Type != "" || ch.Error.Message != "" {
					st.errorEvent = append(st.errorEvent[:0], out...)
					if st.content || st.toolCalls || st.reasoning {
						flushHeld()
						writePayload(string(out))
					}
					continue
				}
				st.observeView(&ch)
				if ch.Usage != nil {
					usage = *ch.Usage
				}
				if firstContentMs == 0 && (st.content || st.toolCalls) {
					firstContentMs = time.Since(streamStart).Milliseconds()
					slog.Default().Info("stream: first content",
						"first_byte_ms", firstByteMs, "first_content_ms", firstContentMs)
				}
				if isErrorFinish(&ch) {
					// Some upstreams (opencode included) can emit a final
					// finish_reason of "error"; OpenAI clients abort the
					// whole turn with "Provider returned error
					// finish_reason". Downgrade it to "stop".
					out = sanitizeFinishBytes(payload)
				}
				if !st.content && !st.toolCalls && heldBytes+len(out) <= thinkBufferLimit {
					held = append(held, append([]byte(nil), out...))
					heldBytes += len(out)
					if time.Since(keepaliveStart) >= 15*time.Second {
						_, _ = io.WriteString(w, ": keepalive\n\n")
						if f, ok := w.(http.Flusher); ok && flush {
							f.Flush()
						}
						keepaliveStart = time.Now()
					}
					continue
				}
				flushHeld()
				writePayload(string(out))
				// A no-progress stream: only empty frames so far. Slide the
				// stall clock on anything that advances the answer (content,
				// tool calls, reasoning, usage, errors); a purely silent
				// stream (every payload empty choices/usage-less) that
				// exceeds the budget is cut so the caller can retry.
				if st.content || st.toolCalls || st.reasoning || len(st.errorEvent) > 0 {
					stallStart = time.Now()
				} else if time.Since(stallStart) > reasoningStallTimeout {
					if !final {
						return usage, true // held reasoning only -> retry
					}
					break
				}
			}
		}
		// Scanner died mid-stream (token >16MB cap, upstream read error):
		// a truncated turn is NOT a clean stop — on non-final pass ask the
		// caller to retry on a fresh lease; on final pass fall through to
		// the honest close-out below (client sees partial + stop).
		//
		// Retry only while the client has seen NOTHING: a truncation after
		// flushed content would replay the answer into the same response
		// writer (duplicated SSE stream + double upstream spend — review
		// 2026-10-17, r09 F1). With flushed bytes we close out honestly
		// instead, like the reasoning-only branch below does.
		scanErr := sc.Err()
		if scanErr != nil && !final {
			if flushedAny {
				slog.Default().Warn("stream: sse truncated after content, closing out partial",
					"err", scanErr.Error())
			} else {
				slog.Default().Warn("stream: sse truncated, retrying on fresh lease", "err", scanErr.Error())
				return usage, true
			}
		}
		if len(st.errorEvent) > 0 && !st.content && !st.toolCalls && !st.reasoning {
			// The upstream delivered only a typed error event (e.g. muse
			// RegionError in a 200 response). Retryable pass: nothing was
			// flushed, so the caller retries on a fresh lease. Final pass:
			// emit the error event verbatim + [DONE] — the client sees the
			// real upstream error instead of a "successful" empty turn.
			if final {
				flushHeld()
				writeEvent(w, flush, string(st.errorEvent))
				writeEvent(w, flush, "[DONE]")
				return usage, false
			}
			return usage, true
		}
		if st.reasoning && !st.content && !st.toolCalls {
			// Live relay past thinkBufferLimit already showed preamble:
			// retry would duplicate it — close out partial instead.
			if flushedAny {
				flushHeld()
				if !st.finishReason {
					writeEvent(w, flush, st.synthesizeFinal())
				}
				writeEvent(w, flush, "[DONE]")
				return usage, false
			}
			// Upstream "thought" but never emitted content: nothing was
			// flushed to the client, so the caller can retry on a fresh
			// lease. On the final attempt emit a plain empty terminal chunk
			// (no synthesized "think only" text — clients should treat it
			// as a silent turn, not as words the model never said).
			if final {
				writeEvent(w, flush, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
				writeEvent(w, flush, "[DONE]")
				return usage, false
			}
			return usage, true
		}
		flushHeld()
		if !st.finishReason {
			writeEvent(w, flush, st.synthesizeFinal())
		}
		writeEvent(w, flush, "[DONE]")
		return usage, false
	}
	// Non-SSE upstream: convert to one content chunk, then a usage chunk.
	// Read errors are NOT empty success: retry on fresh lease (non-final),
	// honest partial on final.
	body, err := io.ReadAll(io.LimitReader(upstream, 64<<20))
	if err != nil {
		if !final {
			slog.Default().Warn("stream: non-sse read failed, retrying", "err", err.Error())
			return usage, true
		}
		writeEvent(w, flush, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		writeEvent(w, flush, "[DONE]")
		return usage, false
	}
	if len(body) > 0 {
		var full struct {
			Usage *Usage `json:"usage"`
		}
		if json.Unmarshal(body, &full) == nil && full.Usage != nil {
			usage = *full.Usage
		}
		writeEvent(w, flush, string(body))
	}
	writeEvent(w, flush, "[DONE]")
	return usage, false
}
// thinkBufferLimit bounds how much reasoning is held before the first
// content chunk. Past it the stream is relayed live (a retry would need to
// replay a huge preamble); think-only streams longer than this are rare.
const thinkBufferLimit = 2 << 20

// reasoningStallTimeout is how long a stream may emit nothing but empty
// frames (no reasoning, no content, no tool calls) before it is cut and
// retried. Every empty muse data: frame slides the idle deadline, so a
// wedged "thinking forever" stream would otherwise pin the request for
// minutes; this budget bounds it while leaving real thinking untouched.
const reasoningStallTimeout = 45 * time.Second

// streamIdleTimeout is how long an SSE stream may be completely silent
// (no bytes at all) before it is cut. Reasoning chunks ARE the first
// (TTFB) tokens — a stream that keeps emitting reasoning or content is
// healthy and never cut here, no matter how long it runs.
const streamIdleTimeout = 100 * time.Second

// streamIdleReader returns EOF once the stream has produced no data for
// streamIdleTimeout. Every successful read slides the deadline, so long
// thinking/phrasing survives; only a truly wedged egress (nothing at all)
// is cut so the caller can rotate/retry instead of pinning the client.
type streamIdleReader struct {
	r        io.Reader
	deadline time.Time
}

func (t *streamIdleReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.deadline = time.Now().Add(streamIdleTimeout)
	}
	if err == nil && n < len(p) && time.Now().After(t.deadline) {
		return n, io.EOF
	}
	return n, err
}

// chunkView is the single typed decode of one SSE data payload. It feeds all
// three consumers (state observation, usage capture, finish sanitization)
// so the hot path parses every chunk exactly once.
type chunkView struct {
	Choices []struct {
		Delta struct {
			Content       string            `json:"content"`
			Reasoning     string            `json:"reasoning_content"`
			ReasoningAlt  string            `json:"reasoning"`
			ToolCalls     []json.RawMessage `json:"tool_calls"`
			ContentBlocks []json.RawMessage `json:"content_blocks"`
		} `json:"delta"`
		FinishReason any `json:"finish_reason"`
	} `json:"choices"`
	Reasoning string `json:"reasoning_content"`
	Usage     *Usage `json:"usage"`
	// Type + Error capture a typed upstream error event of the form
	// {"type":"error","error":{"type":...,"message":...}}. Some opencode
	// clusters (muse included) answer model/region problems with HTTP 200
	// + an SSE error event, which clients would otherwise see as a
	// successful empty turn. The passthrough treats such events as an
	// upstream failure: retried on a fresh lease, and surfaced verbatim
	// on the final attempt.
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// streamState tracks what the upstream stream actually delivered so the
// passthrough can close an incomplete stream with a valid final chunk.
type streamState struct {
	content      bool   // any non-empty delta.content
	reasoning    bool   // any delta.reasoning_content / top-level reasoning
	toolCalls    bool   // any delta.tool_calls
	finishReason bool   // any choices[0].finish_reason
	errorEvent   []byte // raw typed upstream error event ({"type":"error",...})
}

// isErrorFinish reports a first-choice finish_reason of "error".
func isErrorFinish(ch *chunkView) bool {
	for _, c := range ch.Choices {
		if fr, ok := c.FinishReason.(string); ok && fr == "error" {
			return true
		}
	}
	return false
}

// observeView folds the pre-parsed chunk into the stream state.
func (s *streamState) observeView(ch *chunkView) {
	if ch.Reasoning != "" {
		s.reasoning = true
	}
	for _, c := range ch.Choices {
		if c.FinishReason != nil && c.FinishReason != "" {
			s.finishReason = true
		}
		if c.Delta.Content != "" {
			s.content = true
		}
		if c.Delta.Reasoning != "" || c.Delta.ReasoningAlt != "" {
			s.reasoning = true
		}
		if len(c.Delta.ToolCalls) > 0 || len(c.Delta.ContentBlocks) > 0 {
			s.toolCalls = true
		}
	}
}

// observe inspects one SSE data payload. Malformed chunks are ignored; a
// chunk with usage only still updates nothing but the usage handled by the
// caller.
func (s *streamState) observe(payload string) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content       string            `json:"content"`
				Reasoning     string            `json:"reasoning_content"`
				ReasoningAlt  string            `json:"reasoning"`
				ToolCalls     []json.RawMessage `json:"tool_calls"`
				ContentBlocks []json.RawMessage `json:"content_blocks"`
			} `json:"delta"`
			FinishReason any `json:"finish_reason"`
		} `json:"choices"`
		Reasoning string `json:"reasoning_content"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}
	if chunk.Reasoning != "" {
		s.reasoning = true
	}
	for _, c := range chunk.Choices {
		if c.FinishReason != nil && c.FinishReason != "" {
			s.finishReason = true
		}
		if c.Delta.Content != "" {
			s.content = true
		}
		if c.Delta.Reasoning != "" || c.Delta.ReasoningAlt != "" {
			s.reasoning = true
		}
		if len(c.Delta.ToolCalls) > 0 || len(c.Delta.ContentBlocks) > 0 {
			s.toolCalls = true
		}
	}
}

// synthesizeFinal builds a valid closing chunk for a stream that ended
// without a finish_reason. Tool-call streams keep their agent semantics;
// everything else closes with a plain empty stop — no synthesized "think
// only" text: clients must never see words the model never said.

// sanitizeFinishBytes rewrites finish_reason "error" to "stop" in a raw
// SSE payload (map-based rebuild keeps all other fields).
func sanitizeFinishBytes(payload []byte) []byte {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return payload
	}
	choices, ok := m["choices"].([]any)
	if !ok || len(choices) == 0 {
		return payload
	}
	c0, ok := choices[0].(map[string]any)
	if !ok {
		return payload
	}
	if fr, _ := c0["finish_reason"].(string); fr == "error" {
		c0["finish_reason"] = "stop"
		if out, err := json.Marshal(m); err == nil {
			return out
		}
	}
	return payload
}

func (s *streamState) synthesizeFinal() string {
	reason := "stop"
	if s.toolCalls {
		reason = "tool_calls"
	}
	return `{"choices":[{"delta":{},"finish_reason":"` + reason + `"}]}`
}

func isSSE(contentType string) bool {
	for _, part := range split(contentType, ";") {
		if trim(part) == "text/event-stream" {
			return true
		}
	}
	return false
}

func writeEvent(w http.ResponseWriter, flush bool, payload string) {
	_, _ = io.WriteString(w, "data: "+payload+"\n\n")
	if f, ok := w.(http.Flusher); ok && flush {
		f.Flush()
	}
}
