package crypto

import (
	"net/http"
	"strings"
	"testing"
)

func TestPassthroughWithoutEnv(t *testing.T) {
	t.Setenv("STORAGE_ENCRYPTION_KEY", "")
	c := New()
	if c.Enabled() {
		t.Fatal("expected passthrough mode")
	}
	v := "sk-test-123"
	if got := c.Encrypt(v); got != v {
		t.Errorf("Encrypt = %q, want passthrough %q", got, v)
	}
	if got := c.Decrypt(v); got != v {
		t.Errorf("Decrypt = %q, want %q", got, v)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Setenv("STORAGE_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	c := New()
	if !c.Enabled() {
		t.Fatal("expected encryption enabled")
	}
	for _, v := range []string{"sk-abc", "sk-very-long-key-abcdefghijklmnop", ""} {
		enc := c.Encrypt(v)
		if v == "" {
			if enc != "" {
				t.Errorf("empty Encrypt = %q", enc)
			}
			continue
		}
		if !LooksEncrypted(enc) {
			t.Fatalf("Encrypt(%q) = %q, missing prefix", v, enc)
		}
		if got := c.Decrypt(enc); got != v {
			t.Errorf("Decrypt = %q, want %q", got, v)
		}
	}
}

func TestNoDoubleEncrypt(t *testing.T) {
	t.Setenv("STORAGE_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	c := New()
	enc := c.Encrypt("sk-abc")
	if got := c.Encrypt(enc); got != enc {
		t.Errorf("double Encrypt changed value: %q -> %q", enc, got)
	}
}

func TestWrongKeyFails(t *testing.T) {
	t.Setenv("STORAGE_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	c := New()
	enc := c.Encrypt("sk-secret")
	t.Setenv("STORAGE_ENCRYPTION_KEY", "ffffffffffffffffffffffffffffffff")
	c2 := New()
	if got := c2.Decrypt(enc); got != "" {
		t.Errorf("Decrypt with wrong key = %q, want empty", got)
	}
}

func TestMalformed(t *testing.T) {
	t.Setenv("STORAGE_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	c := New()
	for _, v := range []string{"enc:v1:short", "enc:v1:a:b:c:d", "plain"} {
		if got := c.Decrypt(v); got != v && got != "" {
			t.Errorf("Decrypt(%q) = %q, want %q or empty", v, got, v)
		}
	}
}

func TestStripUpstreamHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Cookie", "a=b")
	h.Set("X-Api-Key", "secret")
	h.Set("Authorization", "Bearer keep-me")
	h.Set("X-Custom", "ok")
	StripUpstreamHeaders(h, DefaultStripHeaders)
	if h.Get("Cookie") != "" || h.Get("X-Api-Key") != "" || h.Get("Authorization") != "" {
		t.Error("denylisted headers not stripped")
	}
	if h.Get("X-Custom") != "ok" {
		t.Error("allowed headers lost")
	}
}

func TestSafeError(t *testing.T) {
	if got := SafeError(""); got != "upstream error" {
		t.Errorf("SafeError empty = %q", got)
	}
	want := "auth failed: [redacted] invalid"
	if got := SafeError("auth failed: Bearer sk-abcdef1234567890 invalid"); got != want {
		t.Errorf("SafeError = %q, want %q", got, want)
	}
	long := SafeError(strings.Repeat("a", 400))
	if len(long) > 301 {
		t.Errorf("SafeError long = %d chars, want <=301", len(long))
	}
}
