package gui

import (
	"strings"

	"github.com/ByJotaro/ai-router/internal/store"
)

// RequestLogView — строка журнала запросов для UI (все поля request_log).
type RequestLogView struct {
	ID             int64    `json:"id"`
	Ts             string   `json:"ts"`
	RID            string   `json:"rid"`
	Model          string   `json:"model"`
	RequestedModel string   `json:"requested_model"`
	Provider       string   `json:"provider"`
	Account        string   `json:"account"`
	DurationMS     int64    `json:"duration_ms"`
	Prompt         int64    `json:"prompt"`
	Completion     int64    `json:"completion"`
	CacheRead      int64    `json:"cache_read"`
	CacheWrite     int64    `json:"cache_write"`
	Reasoning      int64    `json:"reasoning"`
	Status         string   `json:"status"`
	HTTPStatus     int      `json:"http_status"`
	Error          string   `json:"error"`
	Proxy          string   `json:"proxy"`
	Method         string   `json:"method"`
	Path           string   `json:"path"`
	ComboName      string   `json:"combo_name"`
	ComboStepID    string   `json:"combo_step_id"`
	ComboPath      []string `json:"combo_path"`
	// WaitedMS is the raw acquire wait (tens of ms of ordinary lease latency
	// included); ≥1s is the "served after a real wait" mark. Kept raw because
	// the exact number is what tells a wait apart from a slow upstream: the
	// pool held the request instead of answering 503 in two milliseconds.
	WaitedMS int64 `json:"waited_ms"`
}

func logView(r store.RequestLogRow) RequestLogView {
	return RequestLogView{
		ID: r.ID, Ts: r.Ts, RID: r.RID, Model: r.Model, RequestedModel: r.RequestedModel,
		Provider: r.Provider, Account: r.Account, DurationMS: r.DurationMS,
		Prompt: r.PromptTokens, Completion: r.CompletionTokens,
		CacheRead: r.CacheRead, CacheWrite: r.CacheWrite, Reasoning: r.Reasoning,
		Status: r.Status, HTTPStatus: r.HTTPStatus, Error: redactLogText(r.Error, 512),
		Proxy: redactProxy(r.Proxy), Method: r.Method, Path: r.Path,
		ComboName: r.ComboName, ComboStepID: r.ComboStepID,
		ComboPath: store.ParseComboPath(r.ComboPath),
		WaitedMS:  r.WaitedMS,
	}
}

func redactProxy(v string) string {
	if v == "" {
		return ""
	}
	if i := strings.Index(v, "@"); i >= 0 {
		if scheme := strings.Index(v, "://"); scheme >= 0 && scheme < i {
			return v[:scheme+3] + "…@" + v[i+1:]
		}
	}
	return v
}

func redactLogText(v string, max int) string {
	v = strings.TrimSpace(v)
	for _, marker := range []string{"Bearer ", "api_key=", "api_key\":", "apikey=", "apikey:", "api-key=", "x-api-key:", "Authorization:", "token=", "token\":", "key=", "key\":", "password=", "secret="} {
		for {
			i := strings.Index(strings.ToLower(v), strings.ToLower(marker))
			if i < 0 {
				break
			}
			end := i + len(marker)
			for end < len(v) && v[end] != ' ' && v[end] != ',' && v[end] != ';' && v[end] != '}' && v[end] != '"' {
				end++
			}
			v = v[:i] + marker + "…" + v[end:]
		}
	}
	return truncate(v, max)
}

// Logs возвращает последние N запросов из request_log (новые сверху).
func (a *App) Logs(limit int) ([]RequestLogView, error) {
	st, err := a.openStore()
	if err != nil {
		return nil, err
	}
	rows, err := st.RequestLogs(limit)
	if err != nil {
		return nil, err
	}
	out := make([]RequestLogView, 0, len(rows))
	for _, r := range rows {
		out = append(out, logView(r))
	}
	return out, nil
}

// LogsByQuery ищет по request_log (LIKE по model/provider/rid/error).
func (a *App) LogsByQuery(query string, limit int) ([]RequestLogView, error) {
	st, err := a.openStore()
	if err != nil {
		return nil, err
	}
	rows, err := st.RequestLogsByQuery(query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]RequestLogView, 0, len(rows))
	for _, r := range rows {
		out = append(out, logView(r))
	}
	return out, nil
}

// LogByID — одна полная строка request_log для инспектора (вход/выход/доп).
func (a *App) LogByID(id int64) (RequestLogView, error) {
	st, err := a.openStore()
	if err != nil {
		return RequestLogView{}, err
	}
	r, err := st.LogByID(id)
	if err != nil {
		return RequestLogView{}, err
	}
	return logView(r), nil
}

// ProxiesLogView — строка журнала egress-прокси.
type ProxiesLogView struct {
	ID        int64  `json:"id"`
	Ts        string `json:"ts"`
	Proxy     string `json:"proxy"`
	Action    string `json:"action"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error"`
}

// ProxiesLogs возвращает последние N событий egress-прокси из proxies_log.
func (a *App) ProxiesLogs(limit int) ([]ProxiesLogView, error) {
	st, err := a.openStore()
	if err != nil {
		return nil, err
	}
	rows, err := st.ProxiesLogs(limit)
	if err != nil {
		return nil, err
	}
	out := make([]ProxiesLogView, 0, len(rows))
	for _, r := range rows {
		out = append(out, ProxiesLogView{
			ID: r.ID, Ts: r.Ts, Proxy: r.Proxy, Action: r.Action, OK: r.OK,
			LatencyMS: r.LatencyMS, Error: r.Error,
		})
	}
	return out, nil
}
