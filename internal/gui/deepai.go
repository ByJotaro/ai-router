package gui

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/provider"
	"gopkg.in/yaml.v3"
)

// DeepAI Code lane: browserless key mint from a Google/Apple sign-in.
//
// The lane refuses email-only accounts (403 social_login_required), so the GUI
// button walks the desktop OAuth page with a local callback and exchanges the
// code for an api-key:
//
//	GET  api.deepai.org/deepai_code/desktop_auth/?state=..&port=..
//	     -> 302 http://127.0.0.1:<port>/auth-callback?code=..&state=..
//	POST api.deepai.org/deepai_code/desktop_auth/exchange/  {"code": ".."}
//	     -> {"api_key","username","is_pro"}
//
// The key is appended to the provider's api_keys (created on first use) and the
// provider is hot-added to the live router, so the next request uses it.
const deepAIProvider = "deepai-code"

// deepAIModels are the 11 unmetered (0 Theos) ids of the lane.
var deepAIModels = []string{
	"glm-5.3-flash", "deepseek-v4.1-flash", "mimo-2.6-flash", "qwen-3.8-flash",
	"minimax-m3.1-flash", "muse-ember", "solar-mini-4", "gpt-6-luna", "mimo-2.6-pro",
	"glyph-cluster", "deepai-cyber",
}

// deepAIMachineID mirrors the app: sha256 of the SMBIOS UUID, lowercased.
func deepAIMachineID() string {
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		"(Get-CimInstance Win32_ComputerSystemProduct).UUID").Output()
	if err != nil {
		return ""
	}
	u := strings.ToLower(strings.TrimSpace(string(out)))
	if u == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(u))
	return hex.EncodeToString(sum[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n*2, time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func openBrowser(u string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}

func deepAIExchange(code string) (map[string]any, error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequest(http.MethodPost,
		"https://api.deepai.org/deepai_code/desktop_auth/exchange", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/131.0.0.0 Safari/537.36")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("exchange %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange %d: %v", resp.StatusCode, out["error"])
	}
	return out, nil
}

// ensureDeepAIProvider writes the provider block when it is missing (headers +
// direct transport are not part of ProviderInput), then resolves it for the router.
func (a *App) ensureDeepAIProvider() (*SaveResult, error) {
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		return nil, err
	}
	if _, ok := cfg.Providers[deepAIProvider]; ok {
		return &SaveResult{OK: true, Message: "провайдер уже есть"}, nil
	}
	headers := map[string]string{
		"X-DeepAI-Code-Theos":    "7",
		"X-DeepAI-Code-Instance": randHex(16),
	}
	if m := deepAIMachineID(); m != "" {
		headers["X-DeepAI-Code-Machine"] = m
	}
	cfg.Providers[deepAIProvider] = config.ProviderConf{
		Type:      "openai-compatible",
		BaseURL:   "https://api.deepai.org/deepai_code",
		APIKeys:   nil,
		AuthMode:  "api-key",
		Headers:   headers,
		Models:    append([]string(nil), deepAIModels...),
		Transport: config.TransportConf{Mode: "direct"},
		TimeoutS:  300,
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(a.cfgPath, data); err != nil {
		return nil, err
	}
	prov, err := provider.Resolve(deepAIProvider, cfg.Providers[deepAIProvider])
	if err != nil {
		return nil, err
	}
	a.router.Add(prov)
	return &SaveResult{OK: true, Message: "провайдер создан"}, nil
}

// DeepAILogin opens the DeepAI sign-in page, captures the OAuth callback on a
// loopback port, exchanges the code for an api-key and adds it to the provider.
func (a *App) DeepAILogin(providerName string) (map[string]any, error) {
	name := strings.TrimSpace(providerName)
	if name == "" {
		name = deepAIProvider
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	state := randHex(32)
	codeCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth-callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "no code", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "<h2>DeepAI: вход выполнен — окно можно закрыть.</h2>")
		select {
		case codeCh <- code:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	authURL := fmt.Sprintf("https://api.deepai.org/deepai_code/desktop_auth/?state=%s&port=%d",
		url.QueryEscape(state), port)
	if err := openBrowser(authURL); err != nil {
		return map[string]any{"ok": false, "message": "браузер не открылся: " + err.Error(), "url": authURL}, nil
	}
	a.log.Printf("deepai: sign-in window opened on 127.0.0.1:%d", port)

	var code string
	select {
	case code = <-codeCh:
	case <-time.After(5 * time.Minute):
		return map[string]any{"ok": false, "message": "вход не завершён за 5 минут", "url": authURL}, nil
	}
	cred, err := deepAIExchange(code)
	if err != nil {
		return map[string]any{"ok": false, "message": "обмен кода: " + err.Error()}, nil
	}
	apiKey, _ := cred["api_key"].(string)
	if apiKey == "" {
		return map[string]any{"ok": false, "message": "exchange не вернул api_key"}, nil
	}
	if _, err := a.ensureDeepAIProvider(); err != nil {
		return map[string]any{"ok": false, "message": "провайдер: " + err.Error()}, nil
	}
	save, err := a.AddProviderKeys(name, []string{apiKey})
	if err != nil {
		return map[string]any{"ok": false, "message": "ключ: " + err.Error()}, nil
	}
	user, _ := cred["username"].(string)
	masked := apiKey
	if len(masked) > 12 {
		masked = masked[:8] + "…" + masked[len(masked)-4:]
	}
	a.log.Printf("deepai: key minted for %s (%s)", user, masked)
	return map[string]any{
		"ok":       save != nil && save.OK,
		"message":  "ключ " + masked + " добавлен" + func() string {
			if user != "" {
				return " (" + user + ")"
			}
			return ""
		}(),
		"username": user,
		"key":      masked,
		"provider": name,
	}, nil
}
