package gui

import (
	"sync/atomic"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// uiLang — язык интерфейса, который выбрала панель ("ru" | "en"). Фронт
// присылает его биндингом SetUILanguage при старте и при смене в настройках;
// трей (нативное меню Windows) живёт вне DOM и читает язык отсюда.
var uiLang atomic.Value

func init() { uiLang.Store("ru") }

// uiTr выбирает подпись по текущему языку панели.
func uiTr(ru, en string) string {
	if v, _ := uiLang.Load().(string); v == "en" {
		return en
	}
	return ru
}

// SetUILanguage переключает нативные подписи (трей) на язык панели.
// Неизвестное значение трактуется как ru.
func (a *App) SetUILanguage(lang string) {
	if lang != "en" {
		lang = "ru"
	}
	uiLang.Store(lang)
	relabelTray()
	// Заголовок окна задаётся при старте по-русски (язык панели живёт в
	// localStorage и на момент запуска неизвестен), поэтому переписываем его
	// из того же источника, что и трей.
	if a != nil && a.ctx != nil {
		runtime.WindowSetTitle(a.ctx, uiTitle())
	}
}

// uiTitle — заголовок окна и подсказка иконки трея: одна пара строк на
// оба нативных места, чтобы они не разъезжались.
func uiTitle() string { return uiTr("ai-router — панель ядра", "ai-router — core panel") }
