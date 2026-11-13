package gui

import "testing"

// Язык панели управляет только нативными подписями (трей); фронт присылает
// его биндингом. Тест фиксирует контракт: неизвестное значение = ru,
// "en" переключает подписи, пустой трей не паникует при relabel.
func TestUILanguageSwitch(t *testing.T) {
	t.Cleanup(func() { uiLang.Store("ru"); trayMu.Lock(); trayMenu = nil; trayMu.Unlock() })

	if got := uiTr("Открыть", "Open"); got != "Открыть" {
		t.Fatalf("default uiTr = %q, want ru", got)
	}
	a := &App{}
	a.SetUILanguage("en")
	if got := uiTr("Открыть", "Open"); got != "Open" {
		t.Fatalf("en uiTr = %q, want Open", got)
	}
	a.SetUILanguage("de")
	if got := uiTr("Открыть", "Open"); got != "Открыть" {
		t.Fatalf("unknown language uiTr = %q, want ru", got)
	}
	relabelTray() // трей не запущен — не должно паниковать
}
