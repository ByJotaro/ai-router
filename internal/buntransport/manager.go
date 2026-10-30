package buntransport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Enabled reports whether the Bun sidecar hop is active. Set
// OPENCODE_TRANSPORT=off to disable without a rebuild (mirrors 9router).
func Enabled() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv("OPENCODE_TRANSPORT"))) != "off"
}

// manager owns the lazily-started Bun sidecar. The zero value is usable.
type manager struct {
	mu      sync.Mutex
	started bool
	port    int
	token   string
	cmd     *exec.Cmd
	err     error
}

var global manager

// sidecarScriptPath resolves the transport script location.
func sidecarScriptPath() string {
	if p := strings.TrimSpace(os.Getenv("OPENCODE_TRANSPORT_SCRIPT")); p != "" {
		return p
	}
	candidates := []string{
		"scripts/opencode-transport.mjs",
		"../scripts/opencode-transport.mjs",
		"../../scripts/opencode-transport.mjs",
		"../../../scripts/opencode-transport.mjs",
	}
	// A release unzips the exe next to scripts/: resolve from the binary too,
	// not only from the working directory.
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "scripts", "opencode-transport.mjs"))
	}
	// walk up from cwd
	wd, _ := os.Getwd()
	for dir := wd; dir != "" && dir != "."; {
		candidates = append(candidates, filepath.Join(dir, "scripts", "opencode-transport.mjs"))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "scripts/opencode-transport.mjs"
}

// bunBin resolves the Bun executable path.
func bunBin() string {
	if p := strings.TrimSpace(os.Getenv("OPENCODE_BUN_BIN")); p != "" {
		return p
	}
	return "bun"
}

// ensure starts the sidecar once and returns its port+token. On failure
// err is non-nil and the caller must fall back to the normal client.
func (m *manager) ensure(ctx context.Context) (int, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		if m.err != nil {
			return 0, "", m.err
		}
		return m.port, m.token, nil
	}
	m.started = true
	if !Enabled() {
		m.err = fmt.Errorf("bun transport disabled (OPENCODE_TRANSPORT=off)")
		return 0, "", m.err
	}
	token := randomToken()
	port := 0
	if v := strings.TrimSpace(os.Getenv("OPENCODE_TRANSPORT_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	script := sidecarScriptPath()
	cmd := exec.Command(bunBin(), "run", script)
	cmd.Env = append(os.Environ(),
		"OPENCODE_TRANSPORT_TOKEN="+token,
		"OPENCODE_TRANSPORT_PORT="+strconv.Itoa(port),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.err = err
		return 0, "", err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		m.err = err
		return 0, "", err
	}
	m.cmd = cmd
	m.token = token

	// Read READY line with a deadline.
	readyCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "READY ") {
				readyCh <- line
				// keep draining
				go func() {
					for sc.Scan() {
					}
				}()
				return
			}
		}
		readyCh <- ""
	}()
	select {
	case line := <-readyCh:
		if line == "" {
			m.err = fmt.Errorf("bun sidecar did not report READY")
			_ = cmd.Process.Kill()
			return 0, "", m.err
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			m.err = fmt.Errorf("bad READY line: %q", line)
			_ = cmd.Process.Kill()
			return 0, "", m.err
		}
		p, err := strconv.Atoi(fields[1])
		if err != nil {
			m.err = fmt.Errorf("bad port in READY line: %q", line)
			_ = cmd.Process.Kill()
			return 0, "", m.err
		}
		m.port = p
	case <-time.After(10 * time.Second):
		m.err = fmt.Errorf("bun sidecar startup timeout")
		_ = cmd.Process.Kill()
		return 0, "", m.err
	case <-ctx.Done():
		m.err = ctx.Err()
		_ = cmd.Process.Kill()
		return 0, "", m.err
	}
	return m.port, m.token, nil
}

func randomToken() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 32)
	for i := range b {
		b[i] = letters[rand.IntN(len(letters))]
	}
	return string(b)
}

// Forward sends one upstream request through the Bun sidecar and returns the
// raw response (status, headers, body stream). On any sidecar failure it
// returns an error so the caller can fall back to the normal client.
func Forward(ctx context.Context, url, method string, headers http.Header, body []byte, proxy string) (*http.Response, error) {
	port, token, err := global.ensure(ctx)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"url":    url,
		"method": method,
		"headers": func() map[string]string {
			out := map[string]string{}
			for k, v := range headers {
				if len(v) > 0 {
					out[k] = v[0]
				}
			}
			return out
		}(),
		"body": string(body),
	}
	if proxy != "" {
		payload["proxy"] = proxy
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/forward", port), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Transport-Token", token)
	}
	cl := &http.Client{Timeout: 120 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	// Wrap: status/headers already on resp; body streams through.
	return resp, nil
}

// Do is a convenience that mirrors opencodeTransport.js: try Bun first, fall
// back to the provided client on any failure.
func Do(ctx context.Context, client *http.Client, req *http.Request, proxy string) (*http.Response, error) {
	if !Enabled() {
		return client.Do(req)
	}
	// buffer body
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return client.Do(req)
		}
		body = b
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	resp, err := Forward(ctx, req.URL.String(), req.Method, req.Header, body, proxy)
	if err != nil {
		return client.Do(req)
	}
	return resp, nil
}

// Shutdown kills the sidecar if running.
func Shutdown() {
	global.mu.Lock()
	defer global.mu.Unlock()
	if global.cmd != nil && global.cmd.Process != nil {
		_ = global.cmd.Process.Kill()
		global.cmd = nil
	}
}
