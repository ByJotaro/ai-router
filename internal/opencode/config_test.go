package opencode

import (
	"encoding/json"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	for _, in := range []string{"http://localhost:9001", "http://localhost:9001/", "http://localhost:9001/v1///"} {
		if got := NormalizeBaseURL(in); got != "http://localhost:9001/v1" {
			t.Fatalf("%q => %q", in, got)
		}
	}
}

func TestMergeConfigPreservesProvidersAndAddsAuto(t *testing.T) {
	out, err := MergeConfig([]byte(`{"provider":{"openai":{"name":"keep"}}}`), "http://localhost:9001", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	p := doc["provider"].(map[string]any)
	if p["openai"] == nil || p["ai-router"] == nil {
		t.Fatalf("providers = %+v", p)
	}
}
