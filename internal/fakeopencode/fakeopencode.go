// Package fakeopencode emulates an opencode upstream with ban modes.
package fakeopencode

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BanStrategy selects how the fake upstream emits 429s.
type BanStrategy int

const (
	PerIP BanStrategy = iota
	SharedDeadline
	Per48
	NoRASawtooth
	Mixed
)

// Config tunes the fake upstream.
type Config struct {
	Strategy BanStrategy
	RASecs   int           // Retry-After seconds on 429s that carry it
	Limit    int           // requests per Window (PerIP/SharedDeadline/Per48/Mixed); saw period for NoRASawtooth
	Window   time.Duration // counting window; <=0 means no expiry
}

// Server is a fake opencode upstream (httptest based, no external deps).
type Server struct {
	URL  string
	Hits func() map[string]int // snapshot of full hit keys -> count

	mu      sync.Mutex
	srv     *httptest.Server
	cfg     Config
	all     map[string]int
	byIP    map[string]int
	buckets map[string]*bucket
	saw     int64 // monotonic counter for sawtooth modes
}

type bucket struct {
	n     int
	start time.Time
}

// New starts the fake upstream.
func New(cfg Config) *Server {
	if cfg.Limit <= 0 {
		cfg.Limit = 5
	}
	if cfg.RASecs <= 0 {
		cfg.RASecs = 60
	}
	s := &Server{
		cfg:     cfg,
		all:     map[string]int{},
		byIP:    map[string]int{},
		buckets: map[string]*bucket{},
	}
	s.Hits = func() map[string]int { return s.snapshot() }
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	return s
}

// Close stops the fake upstream.
func (s *Server) Close() { s.srv.Close() }

// HitsByIP returns per-client-IP hit counts.
func (s *Server) HitsByIP() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.byIP))
	for k, v := range s.byIP {
		out[k] = v
	}
	return out
}

// Reset clears all counters.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all = map[string]int{}
	s.byIP = map[string]int{}
	s.buckets = map[string]*bucket{}
	s.saw = 0
}

func (s *Server) snapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.all))
	for k, v := range s.all {
		out[k] = v
	}
	return out
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost ||
		(r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/zen/v1/responses") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	model := ""
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	model = req.Model

	ip := clientIP(r)
	ua := r.Header.Get("User-Agent")
	auth := r.Header.Get("Authorization")
	sess := r.Header.Get("x-opencode-session")

	s.mu.Lock()
	key := strings.Join([]string{ip, ua, auth, sess, model}, "|")
	s.all[key]++
	s.byIP[ip]++
	banned, withRA := s.banLocked(ip)
	raSecs := s.cfg.RASecs
	s.mu.Unlock()

	if banned {
		if withRA {
			w.Header().Set("Retry-After", strconv.Itoa(raSecs))
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit"}}`))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`))
}

// banLocked decides ban; caller must hold s.mu.
func (s *Server) banLocked(ip string) (banned, withRA bool) {
	now := time.Now()
	touch := func(k string) int {
		b := s.buckets[k]
		if b == nil {
			b = &bucket{start: now}
			s.buckets[k] = b
		}
		if s.cfg.Window > 0 && now.Sub(b.start) > s.cfg.Window {
			b.n = 0
			b.start = now
		}
		b.n++
		return b.n
	}
	switch s.cfg.Strategy {
	case SharedDeadline:
		return touch("shared") > s.cfg.Limit, true
	case Per48:
		return touch("p48:"+prefix48(ip)) > s.cfg.Limit, true
	case NoRASawtooth:
		s.saw++
		return s.saw%int64(s.cfg.Limit) == 0, false
	case Mixed:
		s.saw++
		if s.saw%7 == 0 {
			return true, false // sawtooth overlay without RA
		}
		return touch("ip:"+ip) > s.cfg.Limit, true
	default: // PerIP
		return touch("ip:"+ip) > s.cfg.Limit, true
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// prefix48 groups IPs: first 3 hextets for IPv6, first 3 octets for IPv4.
func prefix48(ip string) string {
	if strings.Contains(ip, ":") {
		parts := strings.Split(ip, ":")
		if len(parts) >= 3 {
			return strings.Join(parts[:3], ":")
		}
		return ip
	}
	parts := strings.Split(ip, ".")
	if len(parts) == 4 {
		return strings.Join(parts[:3], ".")
	}
	return ip
}
