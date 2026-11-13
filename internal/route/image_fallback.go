package route

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const (
	// imageUnavailableMarker is the upstream's wording when the engine behind
	// the free pool has no multimodal support. Measured 2026-11-05 on
	// opencode/deepseek-v4.1-flash-free: fresh one- and five-image stream:true
	// probes read the secret PNG with 200, while a long session carrying five
	// history images got this 400 from a text-only engine instance.
	imageUnavailableMarker = "image input is not available"
	// imageOmittedNote keeps the model honest: the attachment was dropped by
	// the gateway, not by the client.
	imageOmittedNote       = "[an image was attached but this engine has no image input; it was omitted]"
	upstreamErrorReadLimit = 64 << 10
)

// bodyHasImageParts is a cheap pre-check: most requests never carry images, so
// the error-body read and retry stay off that hot path.
func bodyHasImageParts(body []byte) bool {
	return bytes.Contains(body, []byte(`"image_url"`)) || bytes.Contains(body, []byte(`"input_image"`))
}

// isImageInputUnavailable reports whether the upstream rejected an image input.
func isImageInputUnavailable(raw []byte) bool {
	return bytes.Contains(bytes.ToLower(raw), []byte(imageUnavailableMarker))
}

// readSmallBody drains an error response body (bounded) so the caller can
// inspect it or put it back. The original body is closed.
func readSmallBody(res *http.Response) ([]byte, bool) {
	if res == nil || res.Body == nil {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, upstreamErrorReadLimit))
	_ = res.Body.Close()
	if err != nil {
		return nil, false
	}
	return raw, true
}

// stripImageParts removes image parts from an OpenAI chat body. A content array
// that becomes empty gets a one-line note: a silently empty message makes the
// model answer "no image attached" with no trace of what happened.
func stripImageParts(body []byte) ([]byte, bool) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	msgs, ok := m["messages"].([]any)
	if !ok {
		return nil, false
	}
	changed := false
	for _, msg := range msgs {
		mm, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := mm["content"].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(parts))
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				kept = append(kept, p)
				continue
			}
			t, _ := pm["type"].(string)
			if t == "image_url" || t == "input_image" {
				changed = true
				continue
			}
			kept = append(kept, p)
		}
		if len(kept) == 0 {
			kept = append(kept, map[string]any{"type": "text", "text": imageOmittedNote})
		}
		mm["content"] = kept
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

// IsImageUnavailableBody reports whether an upstream error body is the
// "engine has no image input" rejection. The gateway uses it to shape the
// final answer as retryable when even the stripped retry was refused: a 400
// is terminal for clients that retry only 5xx/429, while another egress may
// reach a multimodal instance.
func IsImageUnavailableBody(raw []byte) bool { return isImageInputUnavailable(raw) }

// errorExcerpt keeps the upstream's own reason for a 400 (bounded, one line):
// the image-input refusal is actionable — another egress may be multimodal —
// and the gateway turns it into a retryable 503 instead of a fatal 400.
func errorExcerpt(b []byte) string {
	s := strings.TrimSpace(strings.ToValidUTF8(string(b), ""))
	if i := strings.Index(s, `"message":"`); i >= 0 {
		rest := s[i+11:]
		if j := strings.Index(rest, `"`); j > 0 && j < 300 {
			return rest[:j]
		}
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return strings.ReplaceAll(s, "\n", " ")
}
