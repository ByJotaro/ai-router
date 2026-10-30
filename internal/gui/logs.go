package gui

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
)

// ExportLogs сериализует request_log или proxies_log в строку JSON или CSV
// для скачивания через blob из GUI.
func (a *App) ExportLogs(kind, format string) (string, error) {
	st, err := a.openStore()
	if err != nil {
		return "", err
	}
	switch kind {
	case "proxies":
		rows, err := st.ProxiesLogsAll()
		if err != nil {
			return "", err
		}
		vs := make([]ProxiesLogView, 0, len(rows))
		for _, r := range rows {
			vs = append(vs, ProxiesLogView{
				ID: r.ID, Ts: r.Ts, Proxy: r.Proxy, Action: r.Action, OK: r.OK,
				LatencyMS: r.LatencyMS, Error: r.Error,
			})
		}
		return exportRows(vs, func(v ProxiesLogView) []string {
			return []string{strconv.FormatInt(v.ID, 10), v.Ts, v.Proxy, v.Action, fmt.Sprintf("%t", v.OK), strconv.FormatInt(v.LatencyMS, 10), v.Error}
		}, []string{"id", "ts", "proxy", "action", "ok", "latency_ms", "error"}, format)
	default: // request
		rows, err := st.RequestLogsAll()
		if err != nil {
			return "", err
		}
		vs := make([]RequestLogView, 0, len(rows))
		for _, r := range rows {
			vs = append(vs, logView(r))
		}
		return exportRows(vs, func(v RequestLogView) []string {
			return []string{
				strconv.FormatInt(v.ID, 10), v.Ts, v.RID, v.Model, v.RequestedModel, v.Provider,
				v.Account, strconv.FormatInt(v.DurationMS, 10),
				strconv.FormatInt(v.Prompt, 10), strconv.FormatInt(v.Completion, 10),
				strconv.FormatInt(v.CacheRead, 10), v.Status, strconv.Itoa(v.HTTPStatus),
				v.Error, v.ComboName, v.ComboStepID,
			}
		}, []string{"id", "ts", "rid", "model", "requested_model", "provider", "account", "duration_ms", "prompt_tokens", "completion_tokens", "cache_read", "status", "http_status", "error", "combo_name", "combo_step_id"}, format)
	}
}

// exportRows рендер rows в заданный формат (json|csv).
func exportRows[T any](rows []T, toCSV func(T) []string, header []string, format string) (string, error) {
	if format == "csv" {
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		_ = w.Write(header)
		for _, r := range rows {
			_ = w.Write(toCSV(r))
		}
		w.Flush()
		return buf.String(), w.Error()
	}
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ClearLogs очищает журнал (request | proxies). Размерная операция, вызов с
// подтверждением из GUI.
func (a *App) ClearLogs(kind string) error {
	st, err := a.openStore()
	if err != nil {
		return err
	}
	switch kind {
	case "proxies":
		return st.ClearProxiesLogs()
	case "request":
		return st.ClearRequestLogs()
	default:
		return fmt.Errorf("неизвестный журнал: %s", kind)
	}
}
