package route

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// safeOut renders a request/probe body for failure output with the canonical
// marker broken everywhere, including the system head: test output is read by
// agents, and a verbatim head in a log becomes the head in their next request,
// which the gate then 429s. Never print a raw body from this package.
func safeOut(b []byte) string { return string(redactCanonMarkerForOutput(b)) }

func TestRenameForbiddenPropsKit(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"edit","parameters":{"type":"object","properties":{"filePath":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["filePath","old_string","new_string"]}}}]}`)

	out, changed := renameForbiddenPropsWith(body, kitText)
	if !changed {
		t.Fatal("kit text must rename the schema")
	}
	if bytes.Contains(out, []byte(`"old_string"`)) || bytes.Contains(out, []byte(`"new_string"`)) {
		t.Fatalf("forbidden names survived: %s", safeOut(out))
	}
	for _, want := range []string{`"oldText"`, `"newText"`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("missing %s in %s", want, safeOut(out))
		}
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(out, &doc) != nil {
		t.Fatalf("renamed body is not JSON: %s", safeOut(out))
	}

	// The negative control must leave the snake names intact: the sweep needs
	// a variant that is expected to 429 on a fresh IP.
	snake, changed := renameForbiddenPropsWith(body, kitSnake)
	if changed || !bytes.Equal(snake, body) {
		t.Fatalf("snake control must be a no-op")
	}
}

func TestProbeBodyCarriesShapeAndKit(t *testing.T) {
	for _, kit := range []*toolKit{kitCamel, kitText, kitStr} {
		b := probeModelBody("deepseek-v4.1-flash-free", kit, nil)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: probe is not JSON: %v", kit.ID, err)
		}
		if stream, _ := m["stream"].(bool); !stream {
			t.Fatalf("%s: probe is not streaming", kit.ID)
		}
		msgs, _ := m["messages"].([]any)
		if len(msgs) == 0 {
			t.Fatalf("%s: probe has no messages", kit.ID)
		}
		first, _ := msgs[0].(map[string]any)
		if got, _ := first["content"].(string); got != canonSystemPrefix() {
			t.Fatalf("%s: probe lost the canonical system prefix", kit.ID)
		}
		if bytes.Contains(b, []byte(`"old_string"`)) || bytes.Contains(b, []byte(`"new_string"`)) {
			t.Fatalf("%s: probe still carries forbidden snake names", kit.ID)
		}
		for wire := range kit.Reverse {
			if !bytes.Contains(b, []byte(`"`+wire+`"`)) {
				t.Fatalf("%s: missing renamed property %q", kit.ID, wire)
			}
		}
	}
}

func TestBypassCatalogAxes(t *testing.T) {
	cat := BypassCatalog()
	if len(cat) < 8 {
		t.Fatalf("catalog too small: %d", len(cat))
	}
	seen := map[string]bool{}
	for _, v := range cat {
		if seen[v.ID] {
			t.Fatalf("duplicate variant %s", v.ID)
		}
		seen[v.ID] = true
		switch v.Axis {
		case "base", "ua", "kit":
		default:
			t.Fatalf("unknown axis %q", v.Axis)
		}
	}
	if !seen["kit:snake"] || !seen["ua:1.20.0"] {
		t.Fatalf("catalog misses expected variants: %v", seen)
	}
}

func TestBypassApplyReset(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Cleanup(func() { _, _ = ResetBypassOverrides() })
	bypassMu.Lock()
	bypassCur = BypassActive{}
	bypassLoaded = true
	bypassMu.Unlock()

	a, err := ApplyBypassVariant("ua:1.19.5")
	if err != nil {
		t.Fatal(err)
	}
	if a.UA != "opencode/1.19.5 "+bypassUASuffix || a.Kit != "" {
		t.Fatalf("ua apply: %+v", a)
	}
	if got := activeBypassUA(); got != a.UA {
		t.Fatalf("active UA = %q", got)
	}

	a, err = ApplyBypassVariant("kit:str")
	if err != nil {
		t.Fatal(err)
	}
	if a.Kit != "str" || a.UA == "" {
		t.Fatalf("kit apply must keep the UA axis: %+v", a)
	}
	if got := activeToolKit(); got.ID != "str" {
		t.Fatalf("active kit = %q", got.ID)
	}

	if _, err := ApplyBypassVariant("kit:nope"); err == nil {
		t.Fatal("unknown kit must be rejected")
	}

	a, err = ResetBypassOverrides()
	if err != nil {
		t.Fatal(err)
	}
	if a.UA != "" || a.Kit != "" || len(a.KitMap) != 0 {
		t.Fatalf("reset: %+v", a)
	}
	if got := activeToolKit(); got.ID != "camel" {
		t.Fatalf("default kit = %q", got.ID)
	}
	if got := activeBypassUA(); got != cliUserAgent {
		t.Fatalf("default UA = %q", got)
	}
}

// A full manual sweep against a fake gate: 429 for snake parameter names and
// for UA 1.19.4, 200 for the rest. Proves the state machine, the variant
// application and that probes never park the lane.
func TestBypassSweepAgainstFakeGate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		ua := r.Header.Get("User-Agent")
		if bytes.Contains(body, []byte(`"old_string"`)) || strings.Contains(ua, "opencode/1.19.4") {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"FreeUsageLimitError"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	tr := newSweepTransport(t, srv.URL)
	tr.mu.Lock()
	tr.lanes = []lane{{proxy: "", ip: "10.0.0.1"}}
	tr.fetched = time.Now()
	tr.mu.Unlock()

	if _, err := tr.StartBypass(BypassOptions{Fresh: -1, Model: "deepseek-v4.1-flash-free"}); err != nil {
		t.Fatal(err)
	}
	var st *BypassSweepState
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st = tr.BypassState()
		if !st.Running {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st == nil || st.Running {
		t.Fatal("sweep did not finish")
	}
	if st.Error != "" {
		t.Fatalf("sweep error: %s", st.Error)
	}
	if len(st.Lanes) != 1 || st.Lanes[0].IP != "10.0.0.1" {
		t.Fatalf("lanes: %+v", st.Lanes)
	}
	byID := map[string]BypassRow{}
	for _, r := range st.Rows {
		byID[r.Variant] = r
	}
	if r := byID["current"]; r.Pass != 1 {
		t.Fatalf("baseline must pass: %+v", r)
	}
	if r := byID["kit:text"]; r.Pass != 1 || r.Drift != 1 || r.Unlocked != 0 {
		t.Fatalf("kit:text pass on a serving lane must be drift, not unlock: %+v", r)
	}
	if r := byID["kit:snake"]; r.Fail != 1 || r.Results[0].Status != 429 {
		t.Fatalf("snake control must 429: %+v", r)
	}
	if r := byID["ua:1.19.4"]; r.Fail != 1 || r.Results[0].Status != 429 {
		t.Fatalf("ua 1.19.4 must 429: %+v", r)
	}
	// Diagnostic probes must not poison the live book.
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.book) != 0 {
		t.Fatalf("probe parked a lane: %+v", tr.book)
	}
}

// ABA verdict: a lane where the baseline 429s and the variant answers 200 on
// BOTH sides of the variant is an unlock; the same pass on a serving lane is
// drift and must not be applyable.
func TestBypassABAVerdicts(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var doc struct {
			Tools json.RawMessage `json:"tools"`
		}
		_ = json.Unmarshal(body, &doc)
		if bytes.Contains(doc.Tools, []byte(`"oldString"`)) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"FreeUsageLimitError"}}`))
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	tr := newSweepTransport(t, srv.URL)
	tr.mu.Lock()
	tr.lanes = []lane{{proxy: "", ip: "10.0.0.2"}}
	tr.fetched = time.Now()
	tr.mu.Unlock()

	if _, err := tr.StartBypass(BypassOptions{Fresh: -1, Model: "deepseek-v4.1-flash-free"}); err != nil {
		t.Fatal(err)
	}
	var st *BypassSweepState
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st = tr.BypassState()
		if !st.Running {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st == nil || st.Running {
		t.Fatal("sweep did not finish")
	}
	byID := map[string]BypassRow{}
	for _, r := range st.Rows {
		byID[r.Variant] = r
	}
	r := byID["kit:text"]
	if r.Unlocked != 1 || r.Pass != 1 {
		t.Fatalf("baseline 429 + variant 200 + baseline 429 must unlock: %+v", r)
	}
	if rr := r.Results[0]; rr.BaseBefore != 429 || rr.BaseAfter != 429 || rr.Status != 200 {
		t.Fatalf("ABA statuses: %+v", rr)
	}
	// The same sweep's base row proves the baseline really was refused.
	if b := byID["current"]; b.Pass != 0 {
		t.Fatalf("baseline unexpectedly served: %+v", b)
	}
}

// The newest MITM capture is the probe template: real tool properties are
// enumerated and an applied property kit persists its concrete map.
func TestBypassTemplatePropsAndApply(t *testing.T) {
	dir := t.TempDir()
	body := `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"edit","description":"Edit","parameters":{"type":"object","properties":{"filePath":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["filePath","old_string","new_string"]}}},{"type":"function","function":{"name":"custom","parameters":{"type":"object","properties":{"custom_param":{"type":"string"},"another_one":{"type":"integer"}}}}}]}`
	raw := "POST /inference/v1/chat/completions HTTP/1.1\r\nhost: x\r\n\r\n" + body
	if err := os.WriteFile(filepath.Join(dir, "20261123-120000-msg_x.req"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_ROUTER_MITM_DIR", dir)
	tplMu.Lock()
	tplCache = nil
	tplMu.Unlock()
	t.Cleanup(func() {
		tplMu.Lock()
		tplCache = nil
		tplMu.Unlock()
	})

	tpl := loadProbeTemplate()
	if tpl == nil || len(tpl.props) < 3 {
		t.Fatalf("template not parsed: %+v", tpl)
	}
	if !tpl.hasProp["custom_param"] || !tpl.hasProp["old_string"] {
		t.Fatalf("missing props: %+v", tpl.props)
	}
	vars := sweepVariants(true)
	found := false
	for _, v := range vars {
		if v.ID == "prop:custom_param" {
			found = true
		}
	}
	if !found {
		t.Fatal("all-params sweep misses the real property")
	}

	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Cleanup(func() { _, _ = ResetBypassOverrides() })
	bypassMu.Lock()
	bypassCur = BypassActive{}
	bypassLoaded = true
	bypassMu.Unlock()
	a, err := ApplyBypassVariant("prop:custom_param")
	if err != nil {
		t.Fatal(err)
	}
	if a.KitMap["custom_param"] != "customParam" {
		t.Fatalf("applied map: %+v", a.KitMap)
	}
	kit := activeToolKit()
	if kit.Schema["custom_param"] != "customParam" {
		t.Fatalf("active schema: %+v", kit.Schema)
	}
	kb := probeModelBody("deepseek-v4.1-flash-free", kit, tpl)
	if !bytes.Contains(kb, []byte(`"customParam"`)) {
		t.Fatalf("probe missing renamed custom property")
	}

	// An all-* kit must keep the forbidden edit names renamed even when the
	// capture already carried the live kit's camelCase wire names.
	a, err = ApplyBypassVariant("kit:all-camel")
	if err != nil {
		t.Fatal(err)
	}
	if a.KitMap["old_string"] != "oldString" {
		t.Fatalf("all-camel lost the forbidden-name mapping: %+v", a.KitMap)
	}
	all := probeModelBody("deepseek-v4.1-flash-free", activeToolKit(), tpl)
	if bytes.Contains(all, []byte(`"old_string"`)) || bytes.Contains(all, []byte(`"new_string"`)) {
		t.Fatalf("all-camel probe left a forbidden snake name: %s", safeOut(all))
	}
	if !bytes.Contains(all, []byte(`"oldString"`)) || !bytes.Contains(all, []byte(`"newString"`)) {
		t.Fatalf("all-camel probe lost the forbidden rename: %s", safeOut(all))
	}
}

func TestProxyReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if !proxyReachable("http://" + ln.Addr().String()) {
		t.Fatal("listening proxy must be reachable")
	}
	if proxyReachable("http://" + ln.Addr().String() + "x") {
		t.Fatal("bogus address must not be reachable")
	}
	if !proxyReachable("") {
		t.Fatal("direct leg must be reachable")
	}
}

// A probe must reuse the lane's warm session: the zen allowance binds to
// (session, egress), and a freshly minted ses_* on a lane whose
// new-conversation budget is spent answers 429 even while warm traffic serves.
func TestProbeSessionPrefersPin(t *testing.T) {
	tp := &CLITransport{pins: map[string][]sidPin{
		"conv": {{SID: "ses_pinned", Proxy: "http://probe-lane-a", IP: "2a09:bac1::a", At: 5}},
	}}
	sid, src := tp.probeSession(BypassLane{Proxy: "http://probe-lane-a", IP: "2a09:bac1::a"})
	if sid != "ses_pinned" || src != "pin" {
		t.Fatalf("pin not preferred: sid=%q source=%q", sid, src)
	}
	// Newest pin wins, and an IP match is enough (the lane list may expose a
	// different proxy spelling than the one the pin was created with).
	tp.pins["conv2"] = []sidPin{
		{SID: "ses_old", Proxy: "http://probe-lane-b", IP: "2a09:bac1::b", At: 1},
		{SID: "ses_new", Proxy: "http://probe-lane-c", IP: "2a09:bac1::b", At: 9},
	}
	sid, src = tp.probeSession(BypassLane{Proxy: "http://probe-lane-z", IP: "2a09:bac1::b"})
	if sid != "ses_new" || src != "pin" {
		t.Fatalf("newest IP-matched pin not preferred: sid=%q source=%q", sid, src)
	}
	// No pin: the process-stable fallback is reused, not minted per probe.
	a, asrc := tp.probeSession(BypassLane{Proxy: "http://probe-lane-x"})
	b, bsrc := tp.probeSession(BypassLane{Proxy: "http://probe-lane-x"})
	if a == "" || !strings.HasPrefix(a, "ses_") {
		t.Fatalf("fallback is not a session id: %q", a)
	}
	if a != b || asrc != "fallback" || bsrc != "fallback" {
		t.Fatalf("fallback not stable across probes: %q/%q %q/%q", a, asrc, b, bsrc)
	}
}
