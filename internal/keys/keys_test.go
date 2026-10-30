package keys

import (
	"testing"

	"github.com/ByJotaro/ai-router/internal/config"
)

// TestSetConfigKeepsRevokeAndRuntimeQuota: config cannot express key revoke
// (KeyConf has no such field) and the GUI raises quotas in the store, not in
// the YAML. Re-applying the config on boot used to clear Revoked and roll the
// quotas back to stale YAML values (review 2026-10-17, r13/r18 F1).
func TestSetConfigKeepsRevokeAndRuntimeQuota(t *testing.T) {
	s := New(nil, nil)
	cfg := []config.KeyConf{{Name: "team", Key: "sk-virtual-1", Models: []string{"gpt-4o"}}}
	if err := s.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	if err := s.Revoke("sk-virtual-1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := s.SetQuota("team", 500000, 0, 0); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}

	// Restart: hydrate config again exactly as main.go does.
	if err := s.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig reload: %v", err)
	}

	k := s.FindByName("team")
	if k == nil {
		t.Fatal("key lost across SetConfig")
	}
	if !k.Revoked {
		t.Fatal("revoked key was re-enabled by SetConfig")
	}
	if _, err := s.Authorize("sk-virtual-1", "gpt-4o"); err != ErrRevoked {
		t.Fatalf("Authorize = %v, want ErrRevoked", err)
	}
	if k.QuotaTokens != 500000 {
		t.Fatalf("runtime quota rolled back: got %d, want 500000", k.QuotaTokens)
	}

	// A config that DECLARES a quota still wins (config is authoritative for
	// what it states).
	cfg[0].QuotaTokens = 42
	if err := s.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig with quota: %v", err)
	}
	if k := s.FindByName("team"); k == nil || k.QuotaTokens != 42 {
		t.Fatalf("declared quota not applied: %+v", k)
	}
}
