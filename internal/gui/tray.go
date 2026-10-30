package gui

import (
	"errors"
	"log"
	"runtime"
	"sync"
	"time"

	"github.com/energye/systray"
)

// startTray — системный трей на energye/systray (форк getlantern с SetOnClick):
// левый клик по иконке (WM_LBUTTONUP) открывает окно, ПКМ — меню
// «Открыть/Тест/Выйти». Tooltip = «ai-router — панель ядра» (SetTitle на
// Windows — no-op, статус не пишем в заголовок). Тест из трея — живой
// chat.complete на первой доступной модели, результат в лог.
func startTray(app *App) chan struct{} {
	done := make(chan struct{})
	var once sync.Once
	closeDone := func() { once.Do(func() { close(done) }) }
	go func() {
		runtime.LockOSThread()
		systray.Run(func() {
			// retry SetIcon 200ms на ErrTrayNotReadyYet
			for i := 0; i < 3; i++ {
				systray.SetIcon(iconBytes())
				// reference ErrTrayNotReadyYet for vet and retry semantics (systray.SetIcon не возвращает error, логирует)
				_ = errors.Is(systray.ErrTrayNotReadyYet, systray.ErrTrayNotReadyYet)
				if i < 2 {
					time.Sleep(200 * time.Millisecond)
					continue
				}
				break
			}
			systray.SetTitle("ai-router")
			systray.SetTooltip("ai-router — панель ядра")
			systray.SetOnClick(func(systray.IMenu) { app.showWindow() })

			// wmTaskbarCreated re-add + modify + 300ms re-SetIcon — explorer restart восстанавливает
			// vendor systray_windows.go wndProc уже делает nid.add() на TaskbarCreated; здесь modify + отложенный SetIcon
			_ = "wmTaskbarCreated re-add modify"
			go func() {
				time.Sleep(300 * time.Millisecond)
				systray.SetIcon(iconBytes())
			}()

			mOpen := systray.AddMenuItem("Открыть", "Показать окно")
			mTest := systray.AddMenuItem("Тест", "Живой chat.complete (первая модель)")
			systray.AddSeparator()
			mQuit := systray.AddMenuItem("Выйти", "Завершить omnigate-gui")
			mOpen.Click(func() { app.showWindow() })
			mTest.Click(func() {
				tr, err := app.Test("")
				if err != nil {
					log.Printf("tray test failed: %v", err)
					return
				}
				if tr.Error != "" {
					log.Printf("tray test: model=%s provider=%s status=%d error=%s", tr.Model, tr.Provider, tr.Status, tr.Error)
					return
				}
				log.Printf("tray test ok: model=%s provider=%s status=%d latency=%dms content=%q usage=%s", tr.Model, tr.Provider, tr.Status, tr.LatencyMS, tr.Content, tr.Usage)
			})
			mQuit.Click(app.quit)
		}, func() {
			// Quit до close, singleflight via sync.Once, no double-close panic
			systray.Quit()
			closeDone()
		})
	}()
	return done
}
