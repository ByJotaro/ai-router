package gui

import (
	"errors"

	"github.com/ByJotaro/ai-router/internal/route"
)

// Manual emergency enumeration ("аварийный перебор") bindings. Everything is
// operator-driven: BypassRun starts one sweep, BypassApply adopts a winner.

func (a *App) bypassTransport() (*route.CLITransport, error) {
	if a.router == nil {
		return nil, errors.New("роутер не инициализирован")
	}
	tr := a.router.BypassTransport()
	if tr == nil {
		return nil, errors.New("CLI-транспорт не активен (провайдер не в режиме cli)")
	}
	return tr, nil
}

// BypassCatalog lists the probe variants and the active identity tuple.
func (a *App) BypassCatalog() map[string]any {
	return map[string]any{
		"catalog":       route.BypassCatalog(),
		"active":        route.CurrentBypass(),
		"template":      route.TemplateInfo(),
		"default_model": "deepseek-v4.1-flash-free",
	}
}

// BypassState returns the current (or last) sweep snapshot.
func (a *App) BypassState() (*route.BypassSweepState, error) {
	tr, err := a.bypassTransport()
	if err != nil {
		return nil, err
	}
	return tr.BypassState(), nil
}

// BypassRun starts one sweep on fresh egresses.
func (a *App) BypassRun(fresh int, model string, allParams bool) (*route.BypassSweepState, error) {
	tr, err := a.bypassTransport()
	if err != nil {
		return nil, err
	}
	return tr.StartBypass(route.BypassOptions{Fresh: fresh, Model: model, AllParams: allParams})
}

// BypassApply adopts one catalog variant (single axis: ua or kit).
func (a *App) BypassApply(id string) (route.BypassActive, error) {
	return route.ApplyBypassVariant(id)
}

// BypassReset returns the lane to the built-in identity tuple.
func (a *App) BypassReset() (route.BypassActive, error) {
	return route.ResetBypassOverrides()
}
