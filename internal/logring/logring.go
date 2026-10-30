// Package logring implements a fixed-size ring buffer of recent log lines.
package logring

import (
	"sync"
	"time"
)

// Entry is a single log line.
type Entry struct {
	TS    time.Time `json:"ts"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

// Ring is a concurrency-safe fixed-size ring buffer.
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	next int
	full bool
}

// New creates a ring with the given capacity.
func New(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 1
	}
	return &Ring{buf: make([]Entry, capacity)}
}

// Add appends an entry, evicting the oldest when full.
func (r *Ring) Add(level, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = Entry{TS: time.Now(), Level: level, Msg: msg}
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// Last returns the most recent n entries in chronological order.
func (r *Ring) Last(n int) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.buf) {
		n = len(r.buf)
	}
	if !r.full && r.next < n {
		n = r.next
	}
	start := (r.next - n + len(r.buf)) % len(r.buf)
	out := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}
