package provider

import "testing"

func TestCatalogIncludesOpenCodeNoAuth(t *testing.T) {
	v, ok := CatalogEntryByID("opencode")
	if !ok || v.AuthKind != "no-auth" || v.BaseURL == "" {
		t.Fatalf("opencode catalog entry = %+v, ok=%v", v, ok)
	}
}

func TestCatalogRemovedOpenCodeZen(t *testing.T) {
	if _, ok := CatalogEntryByID("opencode-zen"); ok {
		t.Fatal("opencode-zen catalog entry must be removed; only keyless opencode remains")
	}
}