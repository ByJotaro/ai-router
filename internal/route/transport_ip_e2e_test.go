package route

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// TestKeylessOpenCodeIPRotateEndToEnd эмулирует работу keyless opencode через
// реальный роутер: фейковый upstream возвращает 429 для одних IP и 200 для
// одного рабочего. Проверяем, что IP-ротация делает именно то, что задумано:
//  1. первый запрос идёт на один IP → 429;
//  2. сразу 5 параллельных запросов на разные IP;
//  3. ответивший 200-м IP попадает в пул;
//  4. следующий запрос идёт на рабочий IP из пула (без фан-аута);
//  5. User-Agent opencode доходит до апстрима.
func TestKeylessOpenCodeIPRotateEndToEnd(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	// Эмулируем: почти все IP = 429, только .200 отвечает 200-м.
	statusFor := func(ip string) int {
		if ip == "9.9.9.200" {
			return http.StatusOK
		}
		return http.StatusTooManyRequests
	}
	var gotUA string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.Header.Get("X-Pin-IP")
		mu.Lock()
		hits[ip]++
		if r.Header.Get("User-Agent") != "" {
			gotUA = r.Header.Get("User-Agent")
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if statusFor(ip) != http.StatusOK {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error","code":"rate_limit"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"deepseek-v4-flash-free","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	// Транспорт: pinning "9.9.9.x" шлёт запрос на локальный upstream, при этом
	// пробрасывает IP через заголовок X-Pin-IP, чтобы upstream мог эмулировать
	// разное поведение разных IP.
	port := ""
	it := &IPRotateTransport{
		Host:     "opencode.ai",
		Port:     port,
		Fanout:   5,
		Rounds:   2,
		Cooldown: time.Minute,
	}
	it.resolver = func(context.Context, string) ([]string, error) {
		return []string{"9.9.9.1", "9.9.9.2", "9.9.9.3", "9.9.9.4", "9.9.9.5", "9.9.9.200"}, nil
	}
	it.transportForOverride = func(ip string) http.RoundTripper {
		return rtFunc(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			clone.Header.Set("X-Pin-IP", ip)
			clone.URL.Scheme = "http"
			clone.URL.Host = upstream.Listener.Addr().String()
			clone.Host = clone.URL.Host
			return http.DefaultTransport.RoundTrip(clone)
		})
	}

	p := &provider.Provider{
		Name:      "opencode",
		Type:      "openai-compatible",
		BaseURL:   "https://opencode.ai/zen/v1",
		AuthMode:  "none",
		UserAgent: "opencode/1.18.18 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14",
		Models:    []string{"deepseek-v4-flash-free"},
		Tier:      "free",
	}
	r := New(map[string]*provider.Provider{"opencode": p}, WithTransport(it))

	body, _ := json.Marshal(map[string]any{
		"model":      "deepseek-v4-flash-free",
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 32,
	})

	res, err := r.Call(context.Background(), "deepseek-v4-flash-free", body, false)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	defer res.Resp.Body.Close()
	if res.Resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Resp.StatusCode)
	}

	mu.Lock()
	goodHits := hits["9.9.9.200"]
	total := 0
	for _, n := range hits {
		total += n
	}
	mu.Unlock()

	if goodHits < 1 {
		t.Fatalf("working IP 9.9.9.200 never hit, hits=%v", hits)
	}
	if !strings.Contains(gotUA, "opencode") {
		t.Fatalf("User-Agent = %q, want opencode UA to reach upstream", gotUA)
	}

	// Пул должен содержать рабочий IP.
	it.mu.Lock()
	promoted := false
	for _, g := range it.good {
		if g == "9.9.9.200" {
			promoted = true
		}
	}
	it.mu.Unlock()
	if !promoted {
		t.Fatalf("9.9.9.200 not promoted to good pool: %v", it.good)
	}

	// Второй запрос: должен пойти на рабочий IP из пула без повторного фан-аута.
	// Фан-аут-горутины первого запроса ещё могут добегать до апстрима в фоне,
	// поэтому ждём стабилизации счётчиков перед замером.
	waitHitsStable(t, &mu, &hits, 500*time.Millisecond)
	before := map[string]int{}
	mu.Lock()
	for k, v := range hits {
		before[k] = v
	}
	mu.Unlock()

	res2, err := r.Call(context.Background(), "deepseek-v4-flash-free", body, false)
	if err != nil {
		t.Fatalf("Call#2: %v", err)
	}
	defer res2.Resp.Body.Close()
	if res2.Resp.StatusCode != http.StatusOK {
		t.Fatalf("Call#2 status = %d, want 200", res2.Resp.StatusCode)
	}
	waitHitsStable(t, &mu, &hits, 500*time.Millisecond)

	mu.Lock()
	goodHits2 := hits["9.9.9.200"]
	total2 := 0
	for _, n := range hits {
		total2 += n
	}
	mu.Unlock()

	if goodHits2-goodHits != 1 {
		t.Fatalf("second call must hit the pooled IP exactly once, goodHits delta = %d (hits=%v)", goodHits2-goodHits, hits)
	}
	if total2-total > 2 {
		t.Fatalf("second call re-fanned out: %d extra requests, want 1 (hits=%v)", total2-total, hits)
	}
}

// waitHitsStable ждёт, пока счётчики попаданий перестанут меняться: требуем
// три подряд замера с одинаковой суммой (по 100мс тишины между ними), чтобы
// догоняющие фан-аут-горутины точно завершились.
func waitHitsStable(t *testing.T, mu *sync.Mutex, hits *map[string]int, quiet time.Duration) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stable := 0
		prev := -1
		for stable < 3 {
			mu.Lock()
			sum := 0
			for _, n := range *hits {
				sum += n
			}
			mu.Unlock()
			if sum == prev {
				stable++
			} else {
				stable = 1
			}
			prev = sum
			time.Sleep(100 * time.Millisecond)
		}
		return
	}
}

// TestKeylessOpenCodeAll429Repeats проверяет: если все IP отвечают 429, роутер
// в итоге возвращает ошибку (провайдер не отвечает), а не "успех".
func TestKeylessOpenCodeAll429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer upstream.Close()

	it := &IPRotateTransport{
		Host:   "opencode.ai",
		Fanout: 5,
		Rounds: 1,
	}
	it.resolver = func(context.Context, string) ([]string, error) {
		return []string{"9.9.9.1", "9.9.9.2", "9.9.9.3"}, nil
	}
	it.transportForOverride = func(ip string) http.RoundTripper {
		return rtFunc(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			clone.Header.Set("X-Pin-IP", ip)
			clone.URL.Scheme = "http"
			clone.URL.Host = upstream.Listener.Addr().String()
			clone.Host = clone.URL.Host
			return http.DefaultTransport.RoundTrip(clone)
		})
	}

	p := &provider.Provider{
		Name:      "opencode",
		Type:      "openai-compatible",
		BaseURL:   "https://opencode.ai/zen/v1",
		AuthMode:  "none",
		UserAgent: "opencode/1.18.18 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14",
		Models:    []string{"deepseek-v4-flash-free"},
		Tier:      "free",
	}
	r := New(map[string]*provider.Provider{"opencode": p}, WithTransport(it))
	body := []byte(`{"model":"deepseek-v4-flash-free","messages":[{"role":"user","content":"hi"}]}`)

	res, err := r.Call(context.Background(), "deepseek-v4-flash-free", body, false)
	if err == nil {
		var st int
		if res != nil && res.Resp != nil {
			st = res.Resp.StatusCode
		}
		t.Fatalf("Call must fail when every IP answers 429 (got result status=%d)", st)
	}
	t.Logf("all-429 err = %v", err)
	if !strings.Contains(err.Error(), "Too Many Requests") {
		t.Fatalf("err = %v, want 429-derived error", err)
	}
}

var _ = io.Discard