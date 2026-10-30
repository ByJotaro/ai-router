package route

// TestDetectorMatrix — измеритель, не гейт: для каждой BanStrategy из
// internal/fakeopencode поднимает эмулятор апстрима + fakePoolAPI с лизами
// на него, прогоняет N=20 запросов через PoolTransport{Fanout:5,Rounds:2} и
// ПЕЧАТАЕТ матрицу стратегия x детектор. Тест всегда PASS, если стенд
// отработал (fail — только при ошибке стенда, а не при слепоте детектора).
import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/fakeopencode"
	"github.com/ByJotaro/ai-router/internal/provider"
)

var matrixThrottledRe = regexp.MustCompile(`(\d+) throttled`)

func TestDetectorMatrix(t *testing.T) {
	oldDeadline := poolTryDeadline
	poolTryDeadline = 3 * time.Second
	defer func() { poolTryDeadline = oldDeadline }()

	strategies := []struct {
		name string
		cfg  fakeopencode.Config
	}{
		// Лимиты под N=20/6 серверов (~3-4 хита/сервер): Limit=2 включает
		// бан с 3-го хита — смесь 200/429, детекторам есть что ловить.
		// RASecs=3600 (hours-класс): проверяет и global-арм.
		{"PerIP", fakeopencode.Config{Strategy: fakeopencode.PerIP, RASecs: 3600, Limit: 2, Window: time.Hour}},
		{"SharedDeadline", fakeopencode.Config{Strategy: fakeopencode.SharedDeadline, RASecs: 3600, Limit: 2, Window: time.Hour}},
		{"Per48", fakeopencode.Config{Strategy: fakeopencode.Per48, RASecs: 3600, Limit: 2, Window: time.Hour}},
		{"NoRASawtooth", fakeopencode.Config{Strategy: fakeopencode.NoRASawtooth, RASecs: 60, Limit: 4, Window: time.Hour}},
		{"Mixed", fakeopencode.Config{Strategy: fakeopencode.Mixed, RASecs: 3600, Limit: 2, Window: time.Hour}},
	}
	const N = 20

	t.Logf("detector matrix: N=%d req/strategy, PoolTransport{Fanout:5,Rounds:2}, poolTryDeadline=3s", N)
	t.Logf("%-14s | %-5s %-6s %-5s %-6s | %-7s | %-14s | %s",
		"strategy", "storm", "global", "heat", "single", "ok200", "throttledAvg", "upHits")
	for _, s := range strategies {
		// Multi-egress: 6 upstream-серверов (6 разных портов = 6 разных
		// egress IP 10.1.0.x от fakePoolAPI), лизы раздаются round-robin —
		// иначе все warmups идут в один IP и storm/global/single слепы по
		// построению стенда, а не по вине детекторов (первый прогон).
		const nUp = 6
		srvs := make([]*fakeopencode.Server, nUp)
		urls := make([]string, nUp)
		for k := range nUp {
			srvs[k] = fakeopencode.New(s.cfg)
			urls[k] = srvs[k].URL
		}
		leases := make([]string, 500)
		for i := range leases {
			leases[i] = urls[i%nUp]
		}
		api := &fakePoolAPI{leases: leases}
		pa := httptest.NewServer(http.HandlerFunc(api.handler))
		pt := &PoolTransport{APIURL: pa.URL, Fanout: 5, Rounds: 2, Cooldown: time.Nanosecond}
		prov := &provider.Provider{Name: "x", Type: "openai-compatible", BaseURL: "http://upstream/v1"}
		ok200, done := 0, 0
		thrSum, thrN := 0, 0
		noteErr := func(text string) {
			if m := matrixThrottledRe.FindStringSubmatch(text); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil {
					thrSum += n
					thrN++
				}
			}
		}
		for i := 0; i < N; i++ {
			lease, err := pt.Acquire(context.Background(), RequestContext{Provider: "x"}, prov)
			if err != nil {
				noteErr(err.Error())
				done++
				continue
			}
			req, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions",
				strings.NewReader(`{"model":"m"}`))
			resp, err := lease.Client.Do(req)
			if err != nil {
				noteErr(err.Error())
			} else {
				if resp.StatusCode == http.StatusOK {
					ok200++
				}
				resp.Body.Close()
			}
			_ = lease.Release(context.Background())
			done++
		}

		storm := pt.stormActive()
		until, _ := pt.globalStatus()
		global := !until.IsZero() && time.Now().Before(until)
		_, single := pt.stormSingleIP(time.Now())
		heat := false
		pt.muMu.RLock()
		for k := range pt.heat {
			if pt.heatCountLocked(k, time.Now()) > 0 {
				heat = true
				break
			}
		}
		pt.muMu.RUnlock()
		upHits := 0
		for _, sv := range srvs {
			for _, n := range sv.HitsByIP() {
				upHits += n
			}
		}
		thrAvg := 0.0
		if thrN > 0 {
			thrAvg = float64(thrSum) / float64(thrN)
		}
		t.Logf("%-14s | %-5v %-6v %-5v %-6v | %3d/%-3d | %5.1f (n=%-2d) | %d",
			s.name, storm, global, heat, single, ok200, done, thrAvg, thrN, upHits)

		pa.Close()
		for _, sv := range srvs {
			sv.Close()
		}
		if done != N {
			t.Fatalf("strategy %s: стенд отработал %d/%d запросов", s.name, done, N)
		}
	}
}
