// Package keyfeed adds provider API keys from append-only files without
// restarting the daemon. A provider with `keys_file: <path>` gets every line
// of that file merged into its key pool: "key" or "key<TAB>proxy" (proxy is
// bound through KeyProxies by sha256 hash, so one key keeps one egress).
//
// The watcher re-reads the config and the file whenever the file signature
// (size+mtime) changes and rebuilds only that provider through Router.Add,
// which carries live key health (cooldowns/backoff) into the new pool.
package keyfeed

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/route"
)

// Entry is one line of a feed file.
type Entry struct {
	Key   string
	Proxy string
}

// ParseLines parses feed lines: "key" or "key<TAB|whitespace>proxy".
// Blank lines, comments (#) and malformed keys (whitespace inside) are
// skipped: a key containing spaces would hash to something no client sends.
func ParseLines(text string) []Entry {
	var out []Entry
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key := line
		proxy := ""
		if i := strings.IndexAny(line, "\t "); i > 0 {
			key = line[:i]
			proxy = strings.TrimSpace(line[i+1:])
		}
		// CRLF feeds and copy-paste artifacts leave stray \r around fields;
		// a control char inside a key would leak into the Authorization
		// header and net/http rejects the whole request (measured 2026-11-15).
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsFunc(key, unicode.IsSpace) || strings.ContainsFunc(key, unicode.IsControl) {
			continue
		}
		if proxy != "" && !validProxy(proxy) {
			// A key never contains spaces: "key extra words" is a malformed
			// line, not a key with a proxy.
			continue
		}
		out = append(out, Entry{Key: key, Proxy: proxy})
	}
	return out
}

// KeyHash is the pool key identity: sha256 hex of the plaintext key, the same
// hash KeyProxies and DisabledKeys use.
func KeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Merge folds entries into pc. Returns true when the provider gained a key or
// a proxy binding. Existing keys are never removed: the feed is append-only.
func Merge(pc *config.ProviderConf, entries []Entry) bool {
	changed := false
	have := make(map[string]bool, len(pc.APIKeys))
	for _, k := range pc.APIKeys {
		have[k] = true
	}
	for _, e := range entries {
		if !have[e.Key] {
			pc.APIKeys = append(pc.APIKeys, e.Key)
			have[e.Key] = true
			changed = true
		}
		if e.Proxy != "" {
			h := KeyHash(e.Key)
			if pc.KeyProxies == nil {
				pc.KeyProxies = map[string]string{}
			}
			if pc.KeyProxies[h] != e.Proxy {
				pc.KeyProxies[h] = e.Proxy
				changed = true
			}
		}
	}
	return changed
}

// Watcher polls feed files and hot-adds keys to the live router.
type Watcher struct {
	cfgPath string
	router  *route.Router
	log     *slog.Logger
	mu      sync.Mutex
	seen    map[string]string // provider -> file signature
}

// Start launches the watcher; it returns immediately. Polling (not filesystem
// notification) keeps it portable and immune to editor-replace semantics.
func Start(cfgPath string, r *route.Router, log *slog.Logger) *Watcher {
	w := &Watcher{cfgPath: cfgPath, router: r, log: log, seen: map[string]string{}}
	go w.loop()
	return w
}

func (w *Watcher) loop() {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for range tick.C {
		w.poll()
	}
}

func signature(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return st.ModTime().UTC().Format(time.RFC3339Nano) + ":" + strconv.FormatInt(st.Size(), 10)
}

func (w *Watcher) poll() {
	cfg, err := config.Load(w.cfgPath)
	if err != nil {
		return
	}
	for name, pc := range cfg.Providers {
		path := strings.TrimSpace(pc.KeysFile)
		if path == "" {
			continue
		}
		sig := signature(path)
		w.mu.Lock()
		prev, known := w.seen[name]
		w.mu.Unlock()
		if known && prev == sig {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			w.mu.Lock()
			w.seen[name] = sig
			w.mu.Unlock()
			continue
		}
		entries := ParseLines(string(data))
		if !Merge(&pc, entries) {
			w.mu.Lock()
			w.seen[name] = sig
			w.mu.Unlock()
			continue
		}
		p, err := provider.Resolve(name, pc)
		if err != nil {
			w.log.Warn("keyfeed: provider resolve failed", "provider", name, "err", err)
			w.mu.Lock()
			w.seen[name] = sig
			w.mu.Unlock()
			continue
		}
		w.router.Add(p)
		w.mu.Lock()
		w.seen[name] = sig
		w.mu.Unlock()
		w.log.Info("keyfeed: keys merged", "provider", name, "file", path,
			"keys", len(p.APIKeys), "feed_entries", len(entries))
	}
}

func validProxy(p string) bool {
	for _, scheme := range []string{"http://", "https://", "socks5://", "socks5h://"} {
		if strings.HasPrefix(p, scheme) {
			return true
		}
	}
	return false
}
