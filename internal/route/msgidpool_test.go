package route

import (
	"testing"
	"time"
)

// TestMsgIDPoolNeverSharesAcrossSessions pins the identity-isolation rule for
// request ids: an id the TUI minted inside session A must never be handed to
// session B. The old selector fell back to cached[0] (the newest id in the DB)
// for every session, which made all egresses carry ONE identical
// x-opencode-request value — the strongest possible cross-IP signature.
func TestMsgIDPoolNeverSharesAcrossSessions(t *testing.T) {
	now := time.Now().UnixMilli()
	m := &MsgIDPool{
		Fetch: func(string, time.Duration, time.Time) []msgID {
			return []msgID{
				{id: "msg_a1", session: "ses_a", created: now - 3000},
				{id: "msg_b1", session: "ses_b", created: now - 2000},
				{id: "msg_b2", session: "ses_b", created: now - 1000},
			}
		},
	}
	a := m.NextFor("ses_a")
	if a != "msg_a1" {
		t.Fatalf("NextFor(ses_a) = %q, want the id minted in that session", a)
	}
	b := m.NextFor("ses_b")
	if b != "msg_b1" && b != "msg_b2" {
		t.Fatalf("NextFor(ses_b) = %q, want a ses_b id", b)
	}
	if b == a {
		t.Fatalf("same request id %q handed to two sessions", b)
	}
	// Unknown session: no fallback to somebody else's id.
	if got := m.NextFor("ses_unknown"); got != "" {
		t.Fatalf("NextFor(unknown) = %q, want empty (never a foreign id)", got)
	}
	if got := m.NextFor(""); got != "" {
		t.Fatalf("NextFor(\"\") = %q, want empty", got)
	}
	// Rotation inside one session must walk its own ids before repeating.
	seen := map[string]bool{a: true}
	for i := 0; i < 4; i++ {
		seen[m.NextFor("ses_a")] = true
	}
	if len(seen) != 1 {
		t.Fatalf("ses_a received foreign ids: %v", seen)
	}
}
