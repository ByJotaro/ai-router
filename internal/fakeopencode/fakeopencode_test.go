package fakeopencode

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func post(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func postZen(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Post(url+"/zen/v1/responses", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestPerIP(t *testing.T) {
	s := New(Config{Strategy: PerIP, RASecs: 60, Limit: 2, Window: time.Minute})
	defer s.Close()
	for i := 0; i < 2; i++ {
		if c := post(t, s.URL); c != 200 {
			t.Fatalf("pre-limit = %d, want 200", c)
		}
	}
	resp, err := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("over-limit = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q, want 60", resp.Header.Get("Retry-After"))
	}
	if len(s.HitsByIP()) != 1 {
		t.Fatalf("HitsByIP keys = %v", s.HitsByIP())
	}
}

func TestSharedDeadline(t *testing.T) {
	s := New(Config{Strategy: SharedDeadline, Limit: 2, Window: time.Minute, RASecs: 120})
	defer s.Close()
	for i := 0; i < 2; i++ {
		if c := post(t, s.URL); c != 200 {
			t.Fatalf("pre-limit = %d, want 200", c)
		}
	}
	if c := post(t, s.URL); c != 429 {
		t.Fatalf("over-limit = %d, want 429", c)
	}
}

func TestPer48(t *testing.T) {
	s := New(Config{Strategy: Per48, Limit: 1, Window: time.Minute})
	defer s.Close()
	if c := post(t, s.URL); c != 200 {
		t.Fatalf("first = %d, want 200", c)
	}
	if c := post(t, s.URL); c != 429 {
		t.Fatalf("second = %d, want 429", c)
	}
}

func TestNoRASawtooth(t *testing.T) {
	s := New(Config{Strategy: NoRASawtooth, Limit: 3})
	defer s.Close()
	var sawRA string
	for i := 0; i < 3; i++ {
		resp, err := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		ra := resp.Header.Get("Retry-After")
		code := resp.StatusCode
		resp.Body.Close()
		if i < 2 && code != 200 {
			t.Fatalf("hit %d = %d, want 200", i, code)
		}
		if i == 2 {
			if code != 429 {
				t.Fatalf("saw tooth = %d, want 429", code)
			}
			sawRA = ra
		}
	}
	if sawRA != "" {
		t.Fatalf("sawtooth Retry-After = %q, want empty", sawRA)
	}
	if c := postZen(t, s.URL); c != 200 {
		t.Fatalf("zen = %d, want 200", c)
	}
}

func TestMixed(t *testing.T) {
	s := New(Config{Strategy: Mixed, Limit: 100, Window: time.Minute})
	defer s.Close()
	if c := post(t, s.URL); c != 200 {
		t.Fatalf("first = %d, want 200", c)
	}
	var got429 bool
	for i := 0; i < 6; i++ {
		resp, err := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code == 429 {
			got429 = true
		}
	}
	if !got429 {
		t.Fatal("mixed sawtooth never banned")
	}
	s.Reset()
	if len(s.Hits()) != 0 || len(s.HitsByIP()) != 0 {
		t.Fatal("Reset must clear counters")
	}
}
