// Package stream implements SSE passthrough for streaming chat completions:
// upstream SSE chunks are re-emitted as-is, non-SSE upstream responses are
// converted into a single SSE chunk, and the stream is always terminated
// with [DONE]. Final-chunk usage is captured for accounting.
package stream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ByJotaro/ai-router/internal/config"
)

// reasoningAsAnswer restores the legacy promotion of a reasoning-only final
// stream into the answer slot. Off by default (see the promotion site).
var reasoningAsAnswer = os.Getenv("AI_ROUTER_REASONING_AS_ANSWER") == "1"

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
// allowedTools is the set of function names the client declared in its
// request body. When non-empty, a tool_calls delta naming anything else is
// dropped: the model invents names for capabilities it was not given
// (measured 5/6 on inferway qwen), and relaying them turns a recoverable turn
// into the client's "requested unavailable tool" failure. nil = no filtering
// (the client declared no tools, so every call would be foreign).
func Passthrough(w http.ResponseWriter, upstream io.Reader, contentType string, flush, final bool, allowedTools map[string]bool, suppressReasoning bool) (Usage, bool) {
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
		defer func() {
			// Why does a turn read as truncated? This line names the upstream
			// verdict (length = the model hit its output budget, stop = it chose
			// to stop) and the bytes actually relayed, so "text is cut" can be
			// attributed to the upstream, the client's max_tokens or the relay
			// instead of guessed.
			slog.Default().Info("stream: sse done",
				"finish", st.finishValue,
				"content_bytes", st.contentBytes,
				"reasoning_bytes", st.reasonText.Len(),
				"tool_calls", st.toolCalls)
		}()
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
		// callBuf holds tool-call frames until the turn closes. The client
		// executes calls only after the stream, so holding them costs no UX;
		// it makes a transport cut retryable (a partial call never reached the
		// client) and stops a reset from being reported as the model's output
		// limit. Past callBufferLimit the buffer is flushed live: a multi-MB
		// argument document is more important to deliver than to retry.
		var callBuf [][]byte
		callBufBytes := 0
		callsFlushed := false
		writeCalls := func() {
			for _, b := range callBuf {
				writePayload(string(b))
			}
			if len(callBuf) > 0 {
				callsFlushed = true
			}
			callBuf, callBufBytes = nil, 0
		}
		// backfillEmptyArgs completes a call whose arguments never arrived
		// before the finish chunk: clients JSON.parse the accumulated string and
		// "" is not JSON (measured on the opencode lane: finish=tool_calls with
		// zero argument bytes). "{}" is the only honest completion.
		backfillEmptyArgs := func() {
			for _, key := range st.callOrder {
				if st.callNames[key] == "" || len(st.callArgs[key]) > 0 || st.blockedTool[key] {
					continue
				}
				// Attribution for the rare "call arrived with empty arguments":
				// the upstream sent a name and no argument bytes at all (or the
				// bytes were dropped before this layer). Without this line the
				// client-side symptom has no router-side trace.
				slog.Default().Warn("stream: backfilled empty tool arguments",
					"choice", key.choice, "tool", key.tool, "name", st.callNames[key])
				payload := fmt.Sprintf(
					`{"choices":[{"index":%d,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":"{}"}}]}}]}`,
					key.choice, key.tool)
				callBuf = append(callBuf, []byte(payload))
				callBufBytes += len(payload)
			}
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
				if allowedTools != nil {
					// Every frame of an agent turn, unfiltered: a call lost
					// between upstream and client must be visible here.
					dumpToolFrame("raw=" + truncDump(string(payload), 700))
				}
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
				if allowedTools != nil && hasToolCalls(&ch) {
					filtered, keep := filterToolCalls(string(payload), allowedTools, &st)
					dumpToolFrame("in="+string(payload), "out="+filtered, "keep="+boolStr(keep))
					if !keep {
						continue
					}
					if filtered != string(payload) {
						out = []byte(filtered)
						if json.Unmarshal(out, &ch) != nil {
							continue
						}
					}
				}
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
				// A retry after a think-only stream must not replay the
				// reasoning the client already received from the first
				// attempt: only the answer slot is repeated. The state still
				// sees the reasoning so a final attempt can salvage it.
				if suppressReasoning && st.reasoning {
					out = stripReasoningBytes(out)
					if len(out) == 0 {
						continue
					}
				}
				if firstContentMs == 0 && (st.content || st.toolCalls) {
					firstContentMs = time.Since(streamStart).Milliseconds()
					slog.Default().Info("stream: first content",
						"first_byte_ms", firstByteMs, "first_content_ms", firstContentMs)
				}
				if hasFinish(&ch) {
					backfillEmptyArgs()
				}
				if hasToolCallFinish(&ch) {
					dumpToolFrame("finish=tool_calls", "relayed="+boolStr(st.toolCalls), "held="+itoa(len(st.heldArgs)), "blocked="+itoa(len(st.blockedTool)), "names="+allowedList(allowedTools))
					openHeld := 0
					for key := range st.heldArgs {
						if !st.blockedTool[key] {
							openHeld++
						}
					}
					if openHeld > 0 {
						slog.Default().Warn("stream: held tool arguments never got a name", "held", openHeld)
					}
				}
				if !st.toolCalls && hasToolCallFinish(&ch) {
					// The upstream announced a call the client never declared:
					// the filtered deltas carried nothing, so the turn is a
					// plain stop, not an agent step with an empty call list.
					// Say WHICH call was refused: an empty stop makes the agent
					// retry the same impossible tool until its retry cap.
					notice := ""
					if final && len(allowedTools) > 0 {
						// A tool-less chat client owes nobody an explanation;
						// an AGENT whose call was refused must be told why.
						// Only on the attempt whose stream the client keeps:
						// a non-final attempt is discarded by the think-only
						// retry, so its notice would be dead text (or, worse,
						// a second generation of it prepended to the retry).
						notice = blockedNotice(st.blockedNames, allowedTools)
						st.blockedNames = nil
					}
					if notice != "" {
						writePayload(fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%s}}]}`, jsonString(notice)))
					}
					out = rewriteFinishBytes(out, "stop")
				}
				if isErrorFinish(&ch) {
					// Some upstreams (opencode included) can emit a final
					// finish_reason of "error"; OpenAI clients abort the
					// whole turn with "Provider returned error
					// finish_reason". Downgrade it to "stop".
					// `out` is the filtered payload: sanitizing `payload` would
					// resurrect every call the declared-name filter just removed.
					out = sanitizeFinishBytes(out)
				}
				// Reasoning is data for the client, not a preamble to hide:
				// holding it made a 20-90 s thinking phase arrive as one
				// silent gap (only keepalives) and then a burst — and it kept
				// the stream retryable, so a late 429 replaced the whole
				// answer. Reasoning flows live; only truly empty frames are
				// held.
				//
				// A TERMINAL chunk is the exception: while the turn has no
				// content and no call, it is held even when reasoning already
				// flowed. Relaying it live let the upstream verdict precede
				// the answer (finish -> salvaged content -> synthesized
				// finish), and a retried attempt then appended a second
				// generation after an already-flushed finish_reason.
				// Hold tool-call frames until the turn closes. A reset in the
				// middle of the arguments used to leave the client a partial
				// call and force a synthetic "length" (which OMP reports as
				// "the assistant hit its output token limit"); now the client
				// sees no call and the turn can be retried on a fresh lease.
				if hasToolCalls(&ch) && !hasFinish(&ch) && !chunkHasText(&ch) {
					callBuf = append(callBuf, append([]byte(nil), out...))
					callBufBytes += len(out)
					if callBufBytes > callBufferLimit {
						writeCalls()
					}
					if time.Since(keepaliveStart) >= 15*time.Second {
						_, _ = io.WriteString(w, ": keepalive\n\n")
						if f, ok := w.(http.Flusher); ok && flush {
							f.Flush()
						}
						keepaliveStart = time.Now()
					}
					continue
				}
				if hasToolCalls(&ch) {
					// A mixed (text/call) or terminal call chunk is relayed
					// live: flush the held fragments first so their order is
					// preserved, then treat the call as client-visible.
					callsFlushed = true
					writeCalls()
				}
				if hasFinish(&ch) {
					// The turn is closing: every held call (and any "{}"
					// backfill) must reach the client before the verdict.
					writeCalls()
				}
				if !st.content && !st.toolCalls && (!st.reasoning || hasFinish(&ch)) && heldBytes+len(out) <= thinkBufferLimit {
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
		if scanErr != nil && !st.toolArgsComplete() {
			// A transport cut left the call incomplete: the client must not
			// receive a call it cannot run, with or without visible text. On
			// the final pass this used to leak the partial call + "length" and
			// reproduced the false output-limit message. A real upstream
			// finish=length (no scanErr) still delivers the partial call and
			// its honest verdict.
			callBuf, callBufBytes = nil, 0
		}
		if scanErr != nil && !final {
			if !st.content && !callsFlushed {
				// Held calls never reached the client: nothing executable was
				// shown, so the turn is retryable even if reasoning streamed
				// (the caller suppresses it on the retry). This is the path
				// that used to synthesize "length" after a WARP lane reset.
				slog.Default().Warn("stream: sse truncated before a complete call, retrying on fresh lease",
					"err", scanErr.Error(), "tool_calls", st.toolCalls)
				return usage, true
			}
			if flushedAny {
				slog.Default().Warn("stream: sse truncated after content, closing out partial",
					"err", scanErr.Error(), "tool_calls", st.toolCalls)
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
			// Reasoning is relayed live, so the first attempt already showed
			// the preamble. The retry suppresses reasoning (see
			// suppressReasoning above), so a fresh lease can still produce
			// the answer without duplicating what the client saw.
			// Upstream "thought" but never emitted content: nothing was
			// flushed to the client, so the caller can retry on a fresh
			// lease. On the FINAL attempt a bare empty stop is fatal for the
			// client: OMP aborts the turn with "Assistant returned an empty
			// stop after retry cap; provider reported N output tokens, but no
			// text or tool calls were delivered" (measured 2026-11-20 on the
			// opencode free lane, where reasoning-only stops are the norm).
			// Everything the model actually said is its reasoning. Promoting it
			// into the answer slot made the user read the model's deliberations
			// as the reply (report 2026-11-22). The promotion is therefore
			// opt-in: AI_ROUTER_REASONING_AS_ANSWER=1 restores the legacy
			// behavior; by default the client gets the honest empty stop and
			// can retry.
			if final {
				if reasoningAsAnswer {
					if text := st.reasonAnswer(); text != "" {
						writeEvent(w, flush, reasonContentChunk(text))
					}
				}
				// The upstream terminal chunk was held by the passthrough
				// loop (a finish must never precede the answer it
				// terminates). Flush it now — it carries the honest verdict
				// ("length" when the model was cut mid-thought) — and
				// synthesize "stop" only when the upstream sent none.
				if st.finishReason {
					flushHeld()
				} else {
					writeEvent(w, flush, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
				}
				writeEvent(w, flush, "[DONE]")
				return usage, false
			}
			return usage, true
		}
		if !st.content && !st.toolCalls && !st.reasoning && !final {
			// A turn with nothing in it is not a success: the client sees an
			// empty assistant message and no tool call (measured 1/6 on
			// inferway qwen). Retry once on a fresh lease. The final pass falls
			// through to the shared tail — held already may carry the upstream
			// finish chunk ("length", "stop"), and a second synthesized finish
			// would let "last wins" overwrite that verdict.
			return usage, true
		}
		if !st.content && !st.toolCalls && !st.reasoning && !st.finishReason && len(st.errorEvent) == 0 {
			// Truly empty final stream: no content, no calls, no reasoning.
			// A bare "stop" here is the "Assistant returned an empty stop
			// after retry cap" turn OMP aborts anyway, only with a misleading
			// hint. Name the real condition so the client can retry or pick
			// another lane instead of blaming its own context.
			flushHeld()
			slog.Default().Warn("stream: empty final stream, relaying upstream_empty", "dur_ms", time.Since(streamStart).Milliseconds())
			writeEvent(w, flush, `{"error":{"type":"upstream_empty","message":"upstream returned an empty stream (no content, tool calls or reasoning) on the final attempt"}}`)
			writeEvent(w, flush, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
			writeEvent(w, flush, "[DONE]")
			return usage, false
		}
		if st.content && !st.finishReason {
			slog.Default().Warn("stream: upstream closed without finish_reason after content; closing as length",
				"content", true, "tool_calls", st.toolCalls, "error_event", len(st.errorEvent) > 0)
		}
		flushHeld()
		if !st.finishReason {
			backfillEmptyArgs()
			writeCalls()
			writeEvent(w, flush, st.synthesizeFinal())
		}
		// Some free-tier upstreams end the SSE stream without a usage chunk.
		// OMP then reads completion_tokens=0 and reports "output limit (0
		// tokens)", truncating the stored answer. Synthesize an honest usage
		// from the observed bytes (~4 bytes/token) when none was sent.
		if usage.TotalTokens == 0 && (st.contentBytes > 0 || st.reasonText.Len() > 0) {
			ct := (st.contentBytes + st.reasonText.Len() + 3) / 4
			writeEvent(w, flush, fmt.Sprintf(`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":%d,"total_tokens":%d}}`, ct, ct))
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
		for _, chunk := range nonSSEToDeltaChunks(body, allowedTools) {
			writeEvent(w, flush, chunk)
		}
	}
	writeEvent(w, flush, "[DONE]")
	return usage, false
}

// thinkBufferLimit bounds how much reasoning is held before the first
// content chunk. Past it the stream is relayed live (a retry would need to
// replay a huge preamble); think-only streams longer than this are rare.
const thinkBufferLimit = 2 << 20

// callBufferLimit bounds held tool-call frames. A larger argument document is
// flushed live: delivering it matters more than keeping the retry option.
const callBufferLimit = 8 << 20

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
			Content      string          `json:"content"`
			Reasoning    string          `json:"reasoning_content"`
			ReasoningAlt string          `json:"reasoning"`
			ToolCalls    []toolCallDelta `json:"tool_calls"`
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

// toolCallDelta is one streamed call fragment: the name arrives once, later
// deltas carry only arguments.
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// streamState tracks what the upstream stream actually delivered so the
// passthrough can close an incomplete stream with a valid final chunk.
type streamState struct {
	content bool // any non-empty delta.content
	// contentBytes and finishValue exist only for the "stream: sse done" log:
	// a turn that reads as truncated must be attributable to the upstream's
	// verdict (length = output budget hit) rather than guessed.
	contentBytes int
	finishValue  string
	reasoning    bool // any delta.reasoning_content / top-level reasoning
	// reasonText accumulates the reasoning deltas themselves: when a stream
	// ends reasoning-only on the final attempt, this text is relayed as the
	// answer instead of an empty stop (see the final branch above).
	reasonText   strings.Builder
	toolCalls    bool   // any delta.tool_calls that reached the client
	finishReason bool   // any choices[0].finish_reason
	errorEvent   []byte // raw typed upstream error event ({"type":"error",...})
	// toolNames/blockedTool/toolArgs key the per-call state by tool_call
	// index: the name arrives once, later deltas carry only arguments.
	toolNames   map[callKey]string
	blockedTool map[callKey]bool
	// blockedNames remembers WHICH undeclared tools the agent asked for: a turn
	// whose every call is blocked used to reach the client as finish=tool_calls
	// with no calls, which OMP reports as "Assistant returned empty stop after
	// retry cap" (measured 2026-11-20: scout agents told to run Playwright).
	blockedNames map[string]bool
	toolArgs     map[callKey][]byte
	// callOrder/callNames/callArgs remember every call that reached the wire,
	// so a call whose arguments never arrived can be completed with "{}" before
	// the finish chunk instead of being handed to the client as an empty string
	// (measured: finish=tool_calls with zero argument bytes, JSON.parse("")).
	callOrder []callKey
	callNames map[callKey]string
	callArgs  map[callKey][]byte
	// heldArgs buffers argument fragments that arrive before the call's name:
	// a nameless call reaches the client as "Tool  not found" (measured
	// 2026-11-16), so the fragments wait for the name and are dropped with it
	// when the name never comes. The id/type ride on that same first fragment,
	// so they are held too — an identity-less call cannot be executed.
	heldArgs map[callKey][]byte
	heldID   map[callKey]string
	heldType map[callKey]string
}

// deltaCarriesPayload reports whether a delta has anything worth relaying on
// its own (content, reasoning or a role preamble).
func deltaCarriesPayload(delta map[string]any) bool {
	for _, k := range []string{"content", "reasoning_content", "reasoning", "role"} {
		if v, ok := delta[k]; ok && v != nil && v != "" {
			return true
		}
	}
	return false
}

// callKey identifies one call: streams with n>1 repeat tool indexes per
// choice, so the index alone would cross-link a foreign call in choice 0 with
// a declared call in choice 1.
type callKey struct {
	choice int
	tool   int
}

// nonSSEToDeltaChunks converts a plain chat.completion body (an upstream that
// ignored stream:true) into OpenAI delta chunks. Relaying such a body verbatim
// as one data event hands the client message.tool_calls it never parses, so the
// turn looks empty — and the declared-name filter and the empty-args backfill
// never run on that path (measured 2026-11-16: one live probe, byte-exact
// replay into OMP: 0 calls, 3 retries, "Assistant returned empty stop").
func nonSSEToDeltaChunks(body []byte, allowed map[string]bool) []string {
	var doc struct {
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   any    `json:"content"`
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
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &doc) != nil || len(doc.Choices) == 0 {
		return []string{string(body)}
	}
	var out []string
	for ci, c := range doc.Choices {
		delta := map[string]any{}
		if c.Message.Role != "" {
			delta["role"] = c.Message.Role
		}
		if text, ok := c.Message.Content.(string); ok && text != "" {
			delta["content"] = text
		}
		if len(delta) > 0 {
			if b, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": ci, "delta": delta}}}); err == nil {
				out = append(out, string(b))
			}
		}
		blocked := map[string]bool{}
		emitted := 0
		for i, tc := range c.Message.ToolCalls {
			if allowed != nil && !allowed[tc.Function.Name] {
				blocked[tc.Function.Name] = true
				continue
			}
			args := tc.Function.Arguments
			if args == "" {
				args = "{}"
			}
			call := map[string]any{"index": i, "function": map[string]any{"name": tc.Function.Name, "arguments": args}}
			if tc.ID != "" {
				call["id"] = tc.ID
			}
			if tc.Type != "" {
				call["type"] = tc.Type
			}
			if b, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": ci, "delta": map[string]any{"tool_calls": []any{call}}}}}); err == nil {
				out = append(out, string(b))
				emitted++
			}
		}
		reason := c.FinishReason
		if reason == "" {
			if emitted > 0 {
				reason = "tool_calls"
			} else {
				reason = "stop"
			}
		}
		if reason == "tool_calls" && emitted == 0 {
			// Every announced call was filtered out: a tool_calls verdict with
			// zero call frames is the non-stream twin of the empty stop the
			// incident was about. Say which call was refused, like the SSE
			// path does, and close as a plain stop.
			if notice := blockedNotice(blocked, allowed); notice != "" {
				if b, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": ci, "delta": map[string]any{"content": notice}}}}); err == nil {
					out = append(out, string(b))
				}
			}
			reason = "stop"
		}
		if b, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": ci, "delta": map[string]any{}, "finish_reason": reason}}}); err == nil {
			out = append(out, string(b))
		}
	}
	if len(doc.Usage) > 0 {
		if b, err := json.Marshal(map[string]any{"choices": []any{}, "usage": doc.Usage}); err == nil {
			out = append(out, string(b))
		}
	}
	return out
}

// hasToolCalls reports whether any choice delta carries a tool_calls key.
func hasToolCalls(ch *chunkView) bool {
	for _, c := range ch.Choices {
		if len(c.Delta.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// chunkHasText reports whether a tool-call chunk also carries visible text or
// reasoning; such a mixed chunk is relayed live instead of held, because the
// text must not wait for the turn to close.
func chunkHasText(ch *chunkView) bool {
	for _, c := range ch.Choices {
		if c.Delta.Content != "" || c.Delta.Reasoning != "" || c.Delta.ReasoningAlt != "" {
			return true
		}
	}
	return false
}

// hasFinish reports whether any choice of the chunk carries a finish_reason.
func hasFinish(ch *chunkView) bool {
	for _, c := range ch.Choices {
		if c.FinishReason != nil && c.FinishReason != "" {
			return true
		}
	}
	return false
}

// hasToolCallFinish reports a first-choice finish_reason of "tool_calls".
func hasToolCallFinish(ch *chunkView) bool {
	for _, c := range ch.Choices {
		if fr, ok := c.FinishReason.(string); ok && fr == "tool_calls" {
			return true
		}
	}
	return false
}

// rewriteFinishBytes replaces the first choice's finish_reason in a raw SSE
// payload (map rebuild keeps every other field).
func rewriteFinishBytes(payload []byte, reason string) []byte {
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
	c0["finish_reason"] = reason
	out, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return out
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
		s.reasonText.WriteString(ch.Reasoning)
	}
	if s.callNames == nil {
		s.callNames = map[callKey]string{}
		s.callArgs = map[callKey][]byte{}
	}
	for ci, c := range ch.Choices {
		if c.FinishReason != nil && c.FinishReason != "" {
			s.finishReason = true
			if fr, ok := c.FinishReason.(string); ok {
				s.finishValue = fr
			}
		}
		if c.Delta.Content != "" {
			s.content = true
			s.contentBytes += len(c.Delta.Content)
		}
		if c.Delta.Reasoning != "" || c.Delta.ReasoningAlt != "" {
			s.reasoning = true
			s.reasonText.WriteString(c.Delta.Reasoning)
			s.reasonText.WriteString(c.Delta.ReasoningAlt)
		}
		if len(c.Delta.ToolCalls) > 0 {
			// content_blocks used to count as a call here, but no provider in
			// the repo emits it (grep over 803 raw SSE dumps: zero) and the
			// passthrough relays such chunks verbatim anyway. Counting it made
			// synthesizeFinal claim tool_calls for a shape whose arguments we
			// never validate.
			s.toolCalls = true
		}
		for _, tc := range c.Delta.ToolCalls {
			key := callKey{choice: ci, tool: tc.Index}
			if _, seen := s.callNames[key]; !seen {
				s.callNames[key] = tc.Function.Name
				s.callOrder = append(s.callOrder, key)
			} else if tc.Function.Name != "" {
				s.callNames[key] = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				s.callArgs[key] = append(s.callArgs[key], tc.Function.Arguments...)
			}
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
				Content      string            `json:"content"`
				Reasoning    string            `json:"reasoning_content"`
				ReasoningAlt string            `json:"reasoning"`
				ToolCalls    []json.RawMessage `json:"tool_calls"`
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
		s.reasonText.WriteString(chunk.Reasoning)
	}
	for _, c := range chunk.Choices {
		if c.FinishReason != nil && c.FinishReason != "" {
			s.finishReason = true
			if fr, ok := c.FinishReason.(string); ok {
				s.finishValue = fr
			}
		}
		if c.Delta.Content != "" {
			s.content = true
			s.contentBytes += len(c.Delta.Content)
		}
		if c.Delta.Reasoning != "" || c.Delta.ReasoningAlt != "" {
			s.reasoning = true
			s.reasonText.WriteString(c.Delta.Reasoning)
			s.reasonText.WriteString(c.Delta.ReasoningAlt)
		}
		if len(c.Delta.ToolCalls) > 0 {
			// content_blocks used to count as a call here, but no provider in
			// the repo emits it (grep over 803 raw SSE dumps: zero) and the
			// passthrough relays such chunks verbatim anyway. Counting it made
			// synthesizeFinal claim tool_calls for a shape whose arguments we
			// never validate.
			s.toolCalls = true
		}
	}
}

// reasonAnswerLimit caps how much reasoning is relayed back as the answer on
// a final think-only close: a free lane can "think" for megabytes and the
// client would then get that whole preamble as one content chunk.
const reasonAnswerLimit = 200_000

// reasonAnswer returns the reasoning text a reasoning-only stream produced,
// trimmed and capped, ready to be relayed in the answer slot.
func (s *streamState) reasonAnswer() string {
	text := strings.TrimSpace(s.reasonText.String())
	if len(text) > reasonAnswerLimit {
		// Cut on a rune boundary so the JSON stays valid UTF-8.
		cut := reasonAnswerLimit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = strings.TrimSpace(text[:cut])
	}
	return text
}

// reasonContentChunk builds one SSE payload that carries text as a normal
// delta.content, so the client sees a finished answer rather than an empty
// stop.
func reasonContentChunk(text string) string {
	payload, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": text}}},
	})
	if err != nil {
		return `{"choices":[{"delta":{},"finish_reason":"stop"}]}`
	}
	return string(payload)
}

// synthesizeFinal builds a valid closing chunk for a stream that ended
// without a finish_reason. Tool-call streams keep their agent semantics;
// a text stream that already reached the client is a truncation, not a
// natural stop, so it closes with "length". Clients must never see a cut
// answer presented as successfully finished, and no synthesized "think
// only" text is ever emitted: clients must never see words the model never
// said.

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
		// A cut mid-arguments must not be announced as a completed call: the
		// client would parse unterminated JSON and lose the whole turn. An
		// incomplete argument document is a length verdict (measured: the
		// cut path used to synthesize tool_calls with invalid args).
		if !s.toolArgsComplete() {
			reason = "length"
		}
	} else if s.content {
		// The upstream closed after visible text but before a terminal
		// finish_reason. Calling that "stop" tells the client the answer is
		// complete when it was cut mid-sentence (the 2026-11-19 OMP cut:
		// 47 s, 3733 output tokens, no finish, logged as ok/200). "length"
		// is the honest OpenAI verdict: the turn was truncated, so clients
		// can continue or surface it instead of silently degrading.
		reason = "length"
	}
	return `{"choices":[{"delta":{},"finish_reason":"` + reason + `"}]}`
}

// toolArgsComplete reports whether every emitted call carries a full JSON
// argument document. A call without arguments yet (the model was cut before
// writing any) is incomplete by definition.
func (s *streamState) toolArgsComplete() bool {
	// No arguments at all is a legal call (the model passes none); a NON-empty
	// argument document that is not valid JSON means the stream died mid-call.
	for _, key := range s.callOrder {
		if a := s.callArgs[key]; len(a) > 0 && !json.Valid(a) {
			return false
		}
	}
	return true
}

// filterToolCalls drops tool_calls entries whose function name the client
// never declared. It returns the rewritten payload and whether anything is
// left to send; a chunk whose only payload was a foreign call is dropped
// whole. Arguments of a blocked index are discarded with it, so a foreign
// call cannot leak a half-argument fragment later.
func filterToolCalls(payload string, allowed map[string]bool, st *streamState) (string, bool) {
	var probe struct {
		Choices []struct {
			Delta map[string]json.RawMessage `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(payload), &probe) != nil {
		return payload, true
	}
	hasCalls := false
	for _, c := range probe.Choices {
		if _, ok := c.Delta["tool_calls"]; ok {
			hasCalls = true
			break
		}
	}
	if !hasCalls {
		// Usage-only chunks ({"choices":[],"usage":…}) and role preambles carry
		// no calls: they must pass byte-for-byte, or usage accounting silently
		// loses the final chunk of every request that declares tools.
		return payload, true
	}
	var m map[string]any
	if json.Unmarshal([]byte(payload), &m) != nil {
		return payload, true
	}
	if st.toolNames == nil {
		st.toolNames = map[callKey]string{}
		st.blockedTool = map[callKey]bool{}
		st.toolArgs = map[callKey][]byte{}
		st.heldArgs = map[callKey][]byte{}
		st.heldID = map[callKey]string{}
		st.heldType = map[callKey]string{}
	}
	choices, _ := m["choices"].([]any)
	keptAny, hadSomething := false, false
	for ci, c := range choices {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		delta, _ := cm["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if _, ok := delta["tool_calls"]; !ok {
			if delta["content"] != nil || delta["reasoning_content"] != nil || delta["reasoning"] != nil || cm["finish_reason"] != nil {
				hadSomething = true
			}
			continue
		}
		calls, _ := delta["tool_calls"].([]any)
		kept := make([]any, 0, len(calls))
		for _, tc := range calls {
			tm, _ := tc.(map[string]any)
			if tm == nil {
				continue
			}
			// Some OpenAI-compatible relays omit index on continuation frames.
			// With exactly one open call on this choice the fragment belongs to
			// it; a default of 0 keeps a nameless first frame consistent with
			// its own argument frames. Before this, a name at key 0 and its
			// arguments at key -1 were never joined, and the client got a call
			// with the "{}" backfill instead of the model's arguments.
			idx := 0
			haveIdx := false
			if f, ok := tm["index"].(float64); ok {
				idx = int(f)
				haveIdx = true
			}
			if !haveIdx {
				only, open := 0, 0
				for k, nm := range st.toolNames {
					if k.choice == ci && nm != "" {
						only, open = k.tool, open+1
					}
				}
				if open == 1 {
					idx = only
				}
			}
			key := callKey{choice: ci, tool: idx}
			fn, _ := tm["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if name == "" && st.toolNames[key] != "" {
				// The name already arrived in an earlier chunk (the sidecar emits
				// name and arguments as separate frames): this is the ordinary
				// argument fragment, not a nameless call.
				kept = append(kept, tm)
				keptAny = true
				continue
			}
			if name == "" {
				// Name not seen yet: hold the arguments instead of emitting a
				// nameless call. The name may arrive in a later chunk, in which
				// case it is released with the arguments there.
				if a, ok := fn["arguments"].(string); ok && a != "" {
					st.heldArgs[key] = append(st.heldArgs[key], a...)
					delete(fn, "arguments")
				}
				if id, ok := tm["id"].(string); ok && id != "" {
					st.heldID[key] = id
				}
				if typ, ok := tm["type"].(string); ok && typ != "" {
					st.heldType[key] = typ
				}
				continue
			}
			if !allowed[name] {
				st.blockedTool[key] = true
				if st.blockedNames == nil {
					st.blockedNames = map[string]bool{}
				}
				st.blockedNames[name] = true
				delete(st.heldArgs, key)
				delete(st.heldID, key)
				delete(st.heldType, key)
				continue
			}
			st.toolNames[key] = name
			if id := st.heldID[key]; id != "" {
				if cur, _ := tm["id"].(string); cur == "" {
					tm["id"] = id
				}
				delete(st.heldID, key)
			}
			if typ := st.heldType[key]; typ != "" {
				if cur, _ := tm["type"].(string); cur == "" {
					tm["type"] = typ
				}
				delete(st.heldType, key)
			}
			if held := st.heldArgs[key]; len(held) > 0 {
				cur, _ := fn["arguments"].(string)
				fn["arguments"] = string(held) + cur
				delete(st.heldArgs, key)
			}
			kept = append(kept, tm)
			keptAny = true
		}
		if len(kept) == 0 {
			// The calls are gone, but the same delta may carry prose, a role
			// preamble or the upstream finish_reason. Dropping the whole frame
			// there silently truncated answers (reviewer: content+"It is " with
			// an undeclared call in one delta vanished).
			delete(delta, "tool_calls")
			if deltaCarriesPayload(delta) || cm["finish_reason"] != nil {
				hadSomething = true
			}
			continue
		}
		delta["tool_calls"] = kept
	}
	if _, ok := m["usage"]; ok {
		hadSomething = true
	}
	if !keptAny && !hadSomething {
		return "", false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return payload, true
	}
	return string(out), true
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

// stripReasoningBytes removes reasoning deltas from one SSE payload. An empty
// payload (reasoning was the only field) returns nil so the caller can skip it.
func stripReasoningBytes(payload []byte) []byte {
	var v map[string]any
	if json.Unmarshal(payload, &v) != nil {
		return payload
	}
	changed := false
	if _, ok := v["reasoning_content"]; ok {
		delete(v, "reasoning_content")
		changed = true
	}
	if chs, ok := v["choices"].([]any); ok {
		for _, c := range chs {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			dl, ok := cm["delta"].(map[string]any)
			if !ok {
				continue
			}
			for _, k := range []string{"reasoning_content", "reasoning"} {
				if _, ok := dl[k]; ok {
					delete(dl, k)
					changed = true
				}
			}
		}
	}
	if !changed {
		return payload
	}
	if len(v) == 1 {
		if chs, ok := v["choices"].([]any); ok {
			empty := true
			for _, c := range chs {
				cm, _ := c.(map[string]any)
				// A chunk that also carries the finish verdict is never
				// droppable: dropping it left the client with content and
				// [DONE] but no finish_reason on the suppressed retry.
				if fr, ok := cm["finish_reason"]; ok && fr != nil && fr != "" {
					empty = false
					break
				}
				dl, _ := cm["delta"].(map[string]any)
				if len(dl) > 0 {
					empty = false
					break
				}
			}
			if empty {
				return nil
			}
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return payload
	}
	return b
}

// dumpToolFrame writes tool-call diagnosis lines when AI_ROUTER_TOOL_DUMP names
// a directory. Off by default: a per-frame dump is only for chasing lost tool
// calls (2026-11-20: subagents recorded finish=tool_calls with no call).
var toolDumpMu sync.Mutex

func dumpToolFrame(parts ...string) {
	// Marker file, not env: the daemon is also started by the watchdog and via
	// task/WMI, which do not inherit the shell that promoted the binary.
	base := config.BaseDir()
	dir := filepath.Join(base, "tool_dump")
	if _, err := os.Stat(filepath.Join(base, "tool_dump.on")); err != nil {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	toolDumpMu.Lock()
	defer toolDumpMu.Unlock()
	f, err := os.OpenFile(filepath.Join(dir, "tool_frames.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(time.Now().Format("15:04:05.000") + " " + strings.Join(parts, " | ") + "\n")
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func itoa(n int) string { return strconv.Itoa(n) }

func allowedList(m map[string]bool) string {
	if m == nil {
		return "nil"
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func truncDump(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// blockedNotice is the one line an agent needs to recover: which tool it asked
// for and what it may use instead.
func blockedNotice(blocked map[string]bool, allowed map[string]bool) string {
	if len(blocked) == 0 {
		return ""
	}
	names := make([]string, 0, len(blocked))
	for n := range blocked {
		names = append(names, n)
	}
	sort.Strings(names)
	ok := make([]string, 0, len(allowed))
	for n := range allowed {
		ok = append(ok, n)
	}
	sort.Strings(ok)
	if len(ok) > 24 {
		ok = ok[:24]
	}
	return fmt.Sprintf("[router] Tool call refused: %s is not declared by this client. Available tools: %s. Call one of those (or answer in text).",
		strings.Join(names, ", "), strings.Join(ok, ", "))
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"` + s + `"`
	}
	return string(b)
}
