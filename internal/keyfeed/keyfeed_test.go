package keyfeed

import (
	"testing"

	"github.com/ByJotaro/ai-router/internal/config"
)

func TestParseLines(t *testing.T) {
	in := "# farm feed\n" +
		"key-a\t\t\t\t\t\t\t\thttp://127.0.0.1:8750\r\n" +
		"key-b http://127.0.0.1:8751\n" +
		"key-c\n" +
		"\n" +
		"   \n" +
		"bad key with spaces\n"
	got := ParseLines(in)
	if len(got) != 3 {
		t.Fatalf("entries=%d want 3: %#v", len(got), got)
	}
	if got[0].Key != "key-a" || got[0].Proxy != "http://127.0.0.1:8750" {
		t.Fatalf("tab line parsed wrong: %#v", got[0])
	}
	if got[1].Key != "key-b" || got[1].Proxy != "http://127.0.0.1:8751" {
		t.Fatalf("space line parsed wrong: %#v", got[1])
	}
	if got[2].Key != "key-c" || got[2].Proxy != "" {
		t.Fatalf("bare key parsed wrong: %#v", got[2])
	}
}

func TestParseLinesCRLF(t *testing.T) {
	// Windows text-mode appends turn "key\tproxy\n" into "key\tproxy\r\n"
	// and a hand-built line can carry \r INSIDE the key (before the tab);
	// a control char in the key poisons the Authorization header.
	got := ParseLines("key-a\r\thttp://127.0.0.1:8750\r\nkey-b\thttp://127.0.0.1:8751\r\n")
	if len(got) != 2 {
		t.Fatalf("entries=%d want 2: %#v", len(got), got)
	}
	if got[0].Key != "key-a" || got[0].Proxy != "http://127.0.0.1:8750" {
		t.Fatalf("cr-inside line parsed wrong: %#v", got[0])
	}
	if got[1].Key != "key-b" || got[1].Proxy != "http://127.0.0.1:8751" {
		t.Fatalf("crlf line parsed wrong: %#v", got[1])
	}
}

func TestMergeAddsKeysAndProxies(t *testing.T) {
	pc := &config.ProviderConf{APIKeys: []string{"old-1"}}
	if Merge(pc, []Entry{{Key: "old-1", Proxy: "http://p:1"}}) != true {
		t.Fatal("binding a proxy to an existing key is a change")
	}
	if changed := Merge(pc, []Entry{
		{Key: "old-1", Proxy: "http://p:1"},
		{Key: "new-2", Proxy: "http://p:2"},
	}); !changed {
		t.Fatal("new key must be a change")
	}
	if len(pc.APIKeys) != 2 || pc.APIKeys[1] != "new-2" {
		t.Fatalf("keys=%v", pc.APIKeys)
	}
	if pc.KeyProxies[KeyHash("new-2")] != "http://p:2" {
		t.Fatalf("proxy hash mismatch: %v", pc.KeyProxies)
	}
	if Merge(pc, []Entry{{Key: "old-1"}, {Key: "new-2", Proxy: "http://p:2"}}) {
		t.Fatal("idempotent merge must report no change")
	}
	// Feed is append-only: a drop from the file must not remove a live key.
	Merge(pc, nil)
	if len(pc.APIKeys) != 2 {
		t.Fatalf("keys dropped: %v", pc.APIKeys)
	}
}
