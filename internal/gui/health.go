package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/proxypool"
	"github.com/ByJotaro/ai-router/internal/stream"
)

// TestResult — результат живого теста (chat.complete через роутер).
type TestResult struct {
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Content   string `json:"content"`
	Usage     string `json:"usage"`
	Error     string `json:"error"`
	ComboName string `json:"combo_name,omitempty"`
	ComboStep string `json:"combo_step,omitempty"`
}

// TestProvider выполняет короткий живой запрос СТРОГО через указанный провайдер
// (provider/model pin, без fallback-ухода): честный тест карточки X.
func (a *App) TestProvider(providerName, model string) (*TestResult, error) {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return nil, fmt.Errorf("пустое имя провайдера")
	}
	if a.router.Provider(providerName) == nil {
		return nil, fmt.Errorf("провайдер %q не найден", providerName)
	}
	if model == "" {
		if p := a.router.Provider(providerName); p != nil {
			for _, m := range p.Models {
				if !hasStr(p.DisabledModels, m) && !hasStr(p.HiddenModels, m) {
					model = m
					break
				}
			}
			if model == "" && len(p.Models) > 0 {
				model = p.Models[0]
			}
		}
	}
	if model == "" {
		return nil, fmt.Errorf("нет моделей для теста")
	}
	return a.Test(providerName + "/" + model)
}

func hasStr(ss []string, v string) bool {
	for _, x := range ss {
		if x == v {
			return true
		}
	}
	return false
}

// Test выполняет короткий живой запрос через роутер (короткий промпт,
// max_tokens=16). model пустой → первый доступный.
func (a *App) Test(model string) (*TestResult, error) {
	if model == "" {
		for _, candidate := range a.router.Models() {
			if candidate != "auto" {
				model = candidate
				break
			}
		}
		if model == "" {
			return nil, fmt.Errorf("нет настроенных моделей — добавьте провайдеров с моделями")
		}
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"max_tokens": 16,
		"stream":     false,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	res, err := a.router.Call(ctx, model, body, false)
	tr := &TestResult{Model: model, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		tr.Error = err.Error()
		return tr, nil
	}
	defer res.Resp.Body.Close()
	tr.Provider = res.Provider
	tr.Status = res.Status
	data, _ := io.ReadAll(io.LimitReader(res.Resp.Body, 1<<20))
	if res.Status >= 400 {
		tr.Error = strings.TrimSpace(truncate(string(data), 200))
		return tr, nil
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage any `json:"usage"`
	}
	_ = json.Unmarshal(data, &out)
	if len(out.Choices) > 0 {
		tr.Content, _ = out.Choices[0].Message.Content.(string)
	}
	tr.Content = truncate(tr.Content, 200)
	if u, err := json.Marshal(out.Usage); err == nil {
		tr.Usage = string(u)
	}
	return tr, nil
}

// TestKey выполняет короткий живой chat.complete через роутер под конкретным
// виртуальным ключом: модель берётся из allowlist ключа (или первая доступная),
// по завершении расход списывается с ключа через keys.Store (RecordUsageByKey).
// Возвращает ok/model/длительность/tokens — формы из TestResult.
func (a *App) TestKey(keyName string) (*TestResult, error) {
	if a.ks == nil {
		return nil, fmt.Errorf("хранилище ключей недоступно")
	}
	k := a.ks.FindByName(keyName)
	if k == nil {
		return nil, fmt.Errorf("ключ %q не найден", keyName)
	}
	model := ""
	if len(k.Models) > 0 {
		model = k.Models[0]
	}
	if model == "" {
		models := a.router.Models()
		if len(models) == 0 {
			return nil, fmt.Errorf("нет настроенных моделей для ключа")
		}
		model = models[0]
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"max_tokens": 8,
		"stream":     false,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	res, err := a.router.Call(ctx, model, body, false)
	tr := &TestResult{Model: model, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		tr.Error = err.Error()
		a.chargeTestKey(keyName, model, 0, 0)
		return tr, nil
	}
	defer res.Resp.Body.Close()
	tr.Provider = res.Provider
	tr.Status = res.Status
	data, _ := io.ReadAll(io.LimitReader(res.Resp.Body, 1<<20))
	if res.Status >= 400 {
		tr.Error = strings.TrimSpace(truncate(string(data), 200))
		a.chargeTestKey(keyName, model, 0, 0)
		return tr, nil
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *stream.Usage `json:"usage"`
	}
	_ = json.Unmarshal(data, &out)
	if len(out.Choices) > 0 {
		tr.Content, _ = out.Choices[0].Message.Content.(string)
	}
	tr.Content = truncate(tr.Content, 200)
	if u, err := json.Marshal(out.Usage); err == nil {
		tr.Usage = string(u)
	}
	if out.Usage != nil {
		a.chargeTestKey(keyName, model, out.Usage.PromptTokens, out.Usage.CompletionTokens)
	} else {
		a.chargeTestKey(keyName, model, 0, 0)
	}
	return tr, nil
}

// chargeTestKey списывает токены с виртуального ключа по имени (best-effort).
func (a *App) chargeTestKey(name, model string, prompt, completion int64) {
	if a.ks == nil {
		return
	}
	k := a.ks.FindByName(name)
	if k == nil {
		return
	}
	_ = a.ks.RecordUsageByKey(k.Key, model, prompt, completion)
}

// RiskView — строка риск-центра: приоритет + сообщение + детали.
type RiskView struct {
	Level  string `json:"level"` // high | warn | info
	Msg    string `json:"msg"`
	Detail string `json:"detail"`
}

// Risks вычисляет алерты из реальных данных ядра: близкие к исчерпанию
// квоты ключей (<10% остатка), доля ошибок >5% в request_log, открытые
// брейкеры, активный lockout, деградировавший прокси-пул, офлайновый хаб.
func (a *App) Risks() ([]RiskView, error) {
	var out []RiskView

	// 1) Квоты ключей < 10% остатка (использовано >= 90%).
	if a.ks != nil {
		for _, k := range a.ks.List() {
			dim := []struct {
				label string
				pct   float64
				used  int64
				quota int64
			}{
				{"токенам", pct(k.UsageTokens, k.QuotaTokens), k.UsageTokens, k.QuotaTokens},
				{"запросам", pct(k.UsageReqs, k.QuotaReqs), k.UsageReqs, k.QuotaReqs},
				{"бюджету", pct(k.UsageUSD, k.BudgetUSD), k.UsageUSD, k.BudgetUSD},
			}
			for _, d := range dim {
				if d.pct < 0 || d.pct < 0.9 {
					continue
				}
				out = append(out, RiskView{
					Level:  "high",
					Msg:    fmt.Sprintf("Ключ %q: квота по %s почти исчерпана (%d/%d)", k.Name, d.label, d.used, d.quota),
					Detail: fmt.Sprintf("остаток < 10%% · использовано %.0f%% квоты", d.pct*100),
				})
			}
		}
	}

	// 2) Доля ошибок > 5% за последние запросы.
	if st, err := a.openStore(); err == nil {
		if total, errs, rerr := st.RequestLogStats(300); rerr == nil && total >= 10 {
			rate := float64(errs) / float64(total)
			if rate > 0.05 {
				out = append(out, RiskView{
					Level:  "warn",
					Msg:    fmt.Sprintf("Доля ошибок %d/%d (%.1f%%) за последние %d запросов", errs, total, rate*100, total),
					Detail: "в request_log больше 5% строк со status=error — возможна деградация upstream",
				})
			}
		}
	}

	// 3) Открытые/полуоткрытые брейкеры.
	for name, b := range a.router.BreakerState() {
		if b.State != "closed" {
			out = append(out, RiskView{
				Level:  "warn",
				Msg:    fmt.Sprintf("Брейкер %q открыт (%s, failures=%d)", name, b.State, b.Failures),
				Detail: b.LastError,
			})
		}
	}

	// 4) Lockout активен.
	for model, l := range a.router.LockoutState() {
		if !l.LockedUntil.IsZero() {
			out = append(out, RiskView{
				Level:  "warn",
				Msg:    fmt.Sprintf("Модель %q заблокирована (lockout, failures=%d)", model, l.Failures),
				Detail: l.LastErr,
			})
		}
	}

	// 5) Прокси-пул деградирован / недоступен.
	if a.proxies != nil {
		if degraded, detail := a.poolDegraded(); degraded {
			out = append(out, RiskView{Level: "warn", Msg: "Прокси-пул (egress) деградирован или недоступен", Detail: detail})
		}
	}

	// 6) удалён: хаб не участвует в маршруте запроса; его состояние показывает только sidebar-dot из Status().

	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) > rank(out[j]) })
	return out, nil
}

func rank(r RiskView) int {
	switch r.Level {
	case "high":
		return 3
	case "warn":
		return 2
	default:
		return 1
	}
}

// poolDegraded проверяет статус egress-пула коротким таймаутом (не блокируя
// авторитальный refresh GUI надолго).
func (a *App) poolDegraded() (bool, string) {
	if a.cfg == nil || a.cfg.Egress.BaseURL == "" {
		return false, ""
	}
	// Переиспользуем живой клиент пула (a.proxies из withProxies),
	// а не New на каждый тик 25s.
	var ps *proxypool.PoolStatus
	var err error
	if a.proxies != nil {
		var ctx context.Context
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		ps, err = a.proxies.Status(ctx)
	} else {
		pc := proxypool.New(proxypool.Config{
			BaseURL:    a.cfg.Egress.BaseURL,
			Token:      a.cfg.Egress.Token,
			HTTPClient: &http.Client{Timeout: 5 * time.Second},
		})
		var ctx context.Context
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		ps, err = pc.Status(ctx)
	}
	if err != nil {
		return true, "egress недоступен: " + err.Error()
	}
	if ps.Degraded {
		return true, fmt.Sprintf("alive=%d dead=%d failures_24h=%d", ps.Alive, ps.Dead, ps.Failures24h)
	}
	return false, ""
}

// ChannelHealth is a health result plus the effective route metadata.
type ChannelHealth struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Endpoint       string `json:"endpoint"`
	Route          string `json:"route"`
	TransportReady bool   `json:"transport_ready"`
	LatencyMs      int64  `json:"latency_ms"`
	HTTP           int    `json:"http"`
	OK             bool   `json:"ok"`
	LastErr        string `json:"last_err"`
}

// healthCache — свежий HealthCheck переиспользуется 30с: вкладка провайдеров
// открывается мгновенно, живые GET /models не дёргают сеть на каждый показ.
var healthCache struct {
	sync.Mutex
	at   time.Time
	data []ChannelHealth
}

// healthInflight — single-flight: второй конкурент ждёт первого,
// а не дублирует N живых GET /models мимо кэша 30s.
var healthInflight struct {
	sync.Mutex
	done chan struct{}
}

// HealthCheck измеряет RTT до всех провайдеров (лёгкий GET на base_url/models)
// и, при включённом egress, отражает латентность пула. Провайдеры проверяются
// параллельно (≤6 в раунде). Результат кэшируется на 30с.
func (a *App) HealthCheck() ([]ChannelHealth, error) {
	healthCache.Lock()
	if time.Since(healthCache.at) < 30*time.Second && healthCache.data != nil {
		out := make([]ChannelHealth, len(healthCache.data))
		copy(out, healthCache.data)
		healthCache.Unlock()
		return out, nil
	}
	healthCache.Unlock()
	healthInflight.Lock()
	if healthInflight.done != nil {
		ch := healthInflight.done
		healthInflight.Unlock()
		<-ch
		healthCache.Lock()
		out := make([]ChannelHealth, len(healthCache.data))
		copy(out, healthCache.data)
		healthCache.Unlock()
		return out, nil
	}
	ch := make(chan struct{})
	healthInflight.done = ch
	healthInflight.Unlock()
	defer func() {
		healthInflight.Lock()
		healthInflight.done = nil
		close(ch)
		healthInflight.Unlock()
	}()
	names := a.router.Providers()
	provs := make([]*provider.Provider, 0, len(names))
	for _, n := range names {
		if p := a.router.Provider(n); p != nil {
			provs = append(provs, p)
		}
	}
	out := make([]ChannelHealth, 0, len(provs)+8)
	if len(provs) == 0 {
		out = append(out, ChannelHealth{Name: "нет провайдеров", Type: "provider", OK: false, LastErr: "—"})
	}
	var mu sync.Mutex
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for _, p := range provs {
		mu.Lock()
		route := "direct"
		ready := true
		if a.cfg != nil && a.cfg.Egress.Enabled && a.cfg.Egress.BaseURL != "" {
			route = "pool"
			ready = a.proxies != nil
		} else if p.Egress != "" {
			route = "static"
		}
		out = append(out, ChannelHealth{Name: p.Name, Type: "provider", Endpoint: p.BaseURL, Route: route, TransportReady: ready})
		idx := len(out) - 1
		mu.Unlock()
		wg.Add(1)
		go func(p *provider.Provider, idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			probeProviderHealth(p, &out[idx])
		}(p, idx)
	}
	wg.Wait()

	// Прокси-каналы из статуса пула (если egress настроен).
	// Короткий таймаут 3s: висящий egress не тормозит провайдерные строки
	// (полный 15s оставить вкладке proxies, там пользователь явно ждёт пул).
	ps, err := a.proxyPoolStatusTimeout(3 * time.Second)
	if err == nil {
		for _, ac := range ps.Accounts {
			ch := ChannelHealth{
				Name:      accountLabel(ac),
				Type:      "proxy",
				Endpoint:  ac.VirtualIP,
				LatencyMs: ac.LatencyMs,
				OK:        ac.Healthy,
			}
			if !ac.Healthy {
				ch.LastErr = fmt.Sprintf("fail_count=%d", ac.FailCount)
			}
			out = append(out, ch)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type+out[i].Name < out[j].Type+out[j].Name })
	healthCache.Lock()
	healthCache.at = time.Now()
	healthCache.data = make([]ChannelHealth, len(out))
	copy(healthCache.data, out)
	healthCache.Unlock()
	return out, nil
}

func (a *App) proxyPoolStatus() (*proxypool.PoolStatus, error) {
	return a.proxyPoolStatusTimeout(0)
}

// proxyPoolStatusTimeout — статус пула с заданным таймаутом (0 = общий proxyCtx 15s).
func (a *App) proxyPoolStatusTimeout(d time.Duration) (*proxypool.PoolStatus, error) {
	if a.proxies == nil {
		return nil, fmt.Errorf("egress выключен")
	}
	if d <= 0 {
		ctx, cancel := proxyCtx()
		defer cancel()
		return a.proxies.Status(ctx)
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	type res struct {
		ps  *proxypool.PoolStatus
		err error
	}
	done := make(chan res, 1)
	go func() {
		ps, err := a.proxies.Status(ctx)
		done <- res{ps, err}
	}()
	select {
	case r := <-done:
		return r.ps, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// probeProviderHealth делает лёгкий GET на base_url + "/models" и кладёт
// RTT/статус/ошибку в поставленную строку.
// healthClient — один на процесс: keep-alive вместо N новых TCP/TLS-handshake
// каждые 30s, явный User-Agent для честных /models.
var healthClient = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{MaxIdleConnsPerHost: 6, IdleConnTimeout: 30 * time.Second},
}

func probeProviderHealth(p *provider.Provider, out *ChannelHealth) {
	base := strings.TrimRight(p.BaseURL, "/")
	url := base + "/models"
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "ai-router-gui/health")
	start := time.Now()
	resp, err := healthClient.Do(req)
	out.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		// fallback на голый base — только при сетевой ошибке (refused/timeout),
		// не при HTTP-ответе /models: 403/404 там не значит OK.
		if ne, ok := err.(interface{ Timeout() bool }); !ok || ne.Timeout() {
			if !strings.Contains(err.Error(), "connection refused") && !strings.Contains(err.Error(), "no such host") && !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
				out.OK = false
				out.LastErr = err.Error()
				return
			}
		}
		req2, _ := http.NewRequest("GET", base, nil)
		req2.Header.Set("User-Agent", "ai-router-gui/health")
		resp2, err2 := healthClient.Do(req2)
		if err2 == nil {
			resp2.Body.Close()
			out.OK = true
			out.HTTP = resp2.StatusCode
			return
		}
		out.OK = false
		out.LastErr = err.Error()
		return
	}
	defer resp.Body.Close()
	out.OK = resp.StatusCode < 400
	out.HTTP = resp.StatusCode
	if !out.OK {
		out.LastErr = "HTTP " + strconv.Itoa(resp.StatusCode)
	}
}

func accountLabel(ac proxypool.Account) string {
	if ac.ID != "" {
		return "proxy/" + ac.ID
	}
	if ac.VirtualIP != "" {
		return "proxy/" + ac.VirtualIP
	}
	return "proxy"
}
