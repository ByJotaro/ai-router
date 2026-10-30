package crypto

import (
	"net/http"
	"regexp"
	"strings"
)

// DefaultStripHeaders lists headers never forwarded from a client request to
// an upstream provider (auth-bearing headers are re-set by the router).
var DefaultStripHeaders = []string{
	"Authorization",
	"Cookie",
	"X-Api-Key",
	"Proxy-Authorization",
	"X-Goog-Api-Key",
	"X-Amz-Security-Token",
}

// StripUpstreamHeaders removes denylisted headers from h in place.
func StripUpstreamHeaders(h http.Header, strip []string) {
	if h == nil {
		return
	}
	for _, name := range strip {
		h.Del(name)
	}
}

// credentialRe matches likely secret values so SafeError can mask them.
var credentialRe = regexp.MustCompile(`(?i)(Bearer\s+[A-Za-z0-9._~+/=-]{8,}|sk_ent_[A-Za-z0-9._~+/=-]{8,}|sk-[A-Za-z0-9._~+/=-]{8,}|sp-[A-Za-z0-9._~+/=-]{8,}|enc:v1:[A-Za-z0-9:]{16,}|[A-Za-z0-9]{40,})`)

// SafeError sanitizes an upstream error message for exposure to clients:
// credential-like tokens are masked and the string is length-capped, so raw
// upstream internals never leak. Blank output falls back to a generic message.
func SafeError(msg string) string {
	if msg == "" {
		return "upstream error"
	}
	s := credentialRe.ReplaceAllString(msg, "[redacted]")
	s = strings.TrimSpace(s)
	// Rune-safe truncation at 300.
	runes := []rune(s)
	if len(runes) > 300 {
		s = string(runes[:300]) + "…"
	}
	if s == "" {
		return "upstream error"
	}
	return s
}

// SanitizeForLog masks credentials in a log message and caps it at 300 runes,
// rune-safe (won't cut a multi-byte rune). Used for slog/ring fields.
func SanitizeForLog(msg string) string {
	if msg == "" {
		return ""
	}
	s := credentialRe.ReplaceAllString(msg, "[redacted]")
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) > 300 {
		s = string(runes[:300]) + "…"
	}
	return s
}
