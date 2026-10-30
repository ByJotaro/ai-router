package gui

import (
	"fmt"

	"github.com/energye/systray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

// RunApp стартует Wails-окно (статический фронт, без npm) вместе с треем.
// show=true показывает окно сразу (ручной запуск двойным кликом ждёт панель);
// minimized=true (в связке с show) стартует СВЁРНУТЫМ — режим autostart,
// панель не всплывает поверх работы на логине; show=false стартует вообще без
// окна, только трей; HideWindowOnClose — приложение живёт в трее;
// «Выйти» в трее завершает процесс (runtime.Quit).
// Блокируется до закрытия приложения.
func RunApp(app *App, show bool, minimized bool) error {
	trayDone := startTray(app)
	opts := &options.App{
		Title:             "ai-router — панель ядра",
		Width:             1080,
		Height:            700,
		MinWidth:          760,
		MinHeight:         520,
		StartHidden:       !show,
		HideWindowOnClose: true,
		BackgroundColour:  &options.RGBA{R: 11, G: 14, B: 20, A: 255},
		Assets:            frontendAssets,
		OnStartup:         app.Startup,
		Bind:              []interface{}{app},
		OnShutdown:        app.Shutdown,
		Windows: &windows.Options{
			Theme: windows.Dark,
		},
	}
	if show && minimized {
		opts.WindowStartState = options.Minimised
	}
	err := wails.Run(opts)
	systray.Quit()
	<-trayDone
	if err != nil {
		return fmt.Errorf("gui: %w", err)
	}
	return nil
}
