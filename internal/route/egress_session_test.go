package route

import (
	"net/http"
	"testing"
)

// TestSetEgressSessionKeepsTripleConsistent is the 2026-10-23 header regression:
// the hot path overwrote only x-opencode-session, so x-opencode-session-id kept
// the process-wide pool default (identical on every egress = cross-IP link) and
// x-opencode-request stayed paired with that other session.
func TestSetEgressSessionKeepsTripleConsistent(t *testing.T) {
	h := http.Header{}
	h.Set("x-opencode-session", "ses_defaultshared00000000000000")
	h.Set("x-opencode-session-id", "ses_defaultshared00000000000000")
	h.Set("x-opencode-request", "msg_fromthedefaultsession0000")

	const egress = "ses_egressown000000000000000000"
	setEgressSession(h, egress)

	if got := h.Get("x-opencode-session"); got != egress {
		t.Fatalf("x-opencode-session=%q, хочу %q", got, egress)
	}
	if got := h.Get("x-opencode-session-id"); got != egress {
		t.Fatalf("x-opencode-session-id=%q — остался общий id, егресс утёк связкой", got)
	}
	if h.Get("x-opencode-session") != h.Get("x-opencode-session-id") {
		t.Fatal("session-хедеры противоречат друг другу")
	}
	if got := h.Get("x-opencode-request"); got == "msg_fromthedefaultsession0000" {
		t.Fatalf("x-opencode-request остался от чужой сессии: %q", got)
	}
}

// TestSetEgressSessionNoopOnEmpty: без сессии хедеры не трогаем (иначе
// setOpencodeHeaders перестанет доливать дефолт).
func TestSetEgressSessionNoopOnEmpty(t *testing.T) {
	h := http.Header{}
	h.Set("x-opencode-session", "ses_defaultshared00000000000000")
	setEgressSession(h, "")
	if got := h.Get("x-opencode-session"); got != "ses_defaultshared00000000000000" {
		t.Fatalf("пустая сессия перетёрла хедер: %q", got)
	}
}
