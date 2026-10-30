package gui

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/keys"
	"github.com/ByJotaro/ai-router/internal/modmgmt"
	"github.com/ByJotaro/ai-router/internal/proxypool"
	"github.com/ByJotaro/ai-router/internal/route"
)

// Launch attaches the tray + Wails window to the LIVE daemon instances and
// blocks until the GUI closes. Called from cmd/ai-router serve after the
// HTTP gateway is up; runs on the main goroutine (wails requirement).
// minimized=true — свёрнутый старт (autostart); ручной запуск — видимое окно.
func Launch(router *route.Router, ks *keys.Store, cfg *config.Config, cfgPath, version string, hubPool *proxypool.Client, show bool, minimized bool) error {
	logger := newLogger(cfg.Server.LogLevel)
	app := NewApp(router, ks, cfg, cfgPath, version, logger)

	mg := modmgmt.New(logger)
	mg.Upsert(&modmgmt.Module{
		Name:   "egress",
		Desc:   "прокси-пул для роутинга запросов (требуется для egress/прокси)",
		Health: "http://127.0.0.1:8801/health",
		Bin:    cfg.Modules.Egress.Bin,
		Args:   cfg.Modules.Egress.Args,
		Auto:   cfg.Modules.Egress.Auto,
	})
	mg.Upsert(&modmgmt.Module{
		Name:   "warp-router",
		Desc:   "VPN-ядро (socks :1080 / http :8118) — используется egress",
		Health: "http://127.0.0.1:8787/api/v1/status",
		Bin:    cfg.Modules.Warp.Bin,
		Args:   cfg.Modules.Warp.Args,
		Auto:   cfg.Modules.Warp.Auto,
	})
	mg.Upsert(&modmgmt.Module{
		Name:   "hub",
		Desc:   "реестр модулей и событий (портал :8700)",
		Health: "http://127.0.0.1:8700/v1/health",
		Bin:    cfg.Modules.Hub.Bin,
		Args:   cfg.Modules.Hub.Args,
		Auto:   cfg.Modules.Hub.Auto,
	})
	app.withMods(mg)
	if hubPool != nil {
		app.withProxies(hubPool)
	}
	if err := RunApp(app, show, minimized); err != nil {
		return fmt.Errorf("gui: %w", err)
	}
	return nil
}

func newLogger(level string) *log.Logger {
	lvl := "INFO"
	switch strings.ToLower(level) {
	case "debug":
		lvl = "DEBUG"
	case "warn", "error":
		lvl = "WARN"
	}
	return log.New(os.Stderr, fmt.Sprintf("[ai-router %s] ", lvl), log.LstdFlags)
}
