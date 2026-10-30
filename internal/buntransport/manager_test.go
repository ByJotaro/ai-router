package buntransport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
)

func TestSidecarForward200(t *testing.T) {
	if os.Getenv("OPENCODE_TRANSPORT") == "off" {
		t.Skip("transport disabled")
	}
	// The sidecar runs scripts/opencode-transport.mjs under bun; a runner
	// without bun (CI) can only test the fallback path.
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not installed")
	}
	// local upstream that returns 200 with a marker
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Marker", "upstream-ok")
		w.WriteHeader(200)
		w.Write([]byte("hello-from-upstream"))
	}))
	defer up.Close()

	resp, err := Forward(t.Context(), up.URL+"/foo", "GET", http.Header{"X-Test": []string{"1"}}, nil, "")
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Marker") != "upstream-ok" {
		t.Fatalf("marker header = %q", resp.Header.Get("X-Marker"))
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "hello-from-upstream" {
		t.Fatalf("body = %q", string(b))
	}
	t.Logf("sidecar forwarded OK: status=%d", resp.StatusCode)
}

func TestDoFallback(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("direct"))
	}))
	defer up.Close()
	req, _ := http.NewRequest("GET", up.URL, nil)
	resp, err := Do(t.Context(), http.DefaultClient, req, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}
