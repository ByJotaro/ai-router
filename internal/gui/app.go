package gui

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/keys"
	"github.com/ByJotaro/ai-router/internal/modmgmt"
	"github.com/ByJotaro/ai-router/internal/proxypool"
	"github.com/ByJotaro/ai-router/internal/route"
	"github.com/ByJotaro/ai-router/internal/store"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App — набор Wails-биндингов поверх живых инстансов ядра. Биндинги
// возвращают максимум (T, error); в JSON уходят только безопасные для
// отображения поля (ключи всегда маскированные).
type App struct {
	router *route.Router
	ks     *keys.Store
	cfg    *config.Config
	// proxies — клиент egress-пула, когда cfg.Egress.Enabled; nil = выключен.
	proxies *proxypool.Client
	cfgPath string
	version string
	started time.Time
	log     *log.Logger

	// mods — менеджер зависимых модулей (egress/warp/hub); инициализируется
	// в main.go из cfg.Modules, nil в тестах.
	mods *modmgmt.Manager

	ctx context.Context

	// st — SQLite-хендл для журналов/использования. main.go не передаёт
	// store в App, поэтому открываем свой (тот же state.db, чтение по WAL
	// безопасно в параллель с демоном). Тесты инжектят хендл через withStore.
	// Ошибку НЕ кэшируем: transient open/busy (два процесса на одном state.db)
	// должен чиниться следующим тиком, а не висеть до рестарта GUI.
	storeMu sync.Mutex
	st      *store.Store
}

func NewApp(router *route.Router, ks *keys.Store, cfg *config.Config, cfgPath, version string, log *log.Logger) *App {
	return &App{router: router, ks: ks, cfg: cfg, cfgPath: cfgPath, version: version, started: time.Now(), log: log}
}

// withStore подставляет готовый SQLite-хендл (для тестов).
func (a *App) withStore(s *store.Store) *App {
	a.st = s
	return a
}

// withMods подставляет менеджер зависимых модулей (из main.go).
func (a *App) withMods(m *modmgmt.Manager) *App {
	a.mods = m
	return a
}

// withProxies attaches the egress pool client and wires its proxies_log sink
// to the App's lazy SQLite handle.
func (a *App) withProxies(pc *proxypool.Client) *App {
	a.proxies = pc
	if pc != nil {
		pc.SetLog(func(proxy, action string, ok bool, ms int64, errText string) {
			st, err := a.openStore()
			if err != nil {
				return
			}
			_ = st.InsertProxiesLog(proxy, action, ok, ms, errText)
		})
	}
	return a
}

// openStore открывает (лениво) свой SQLite-хендл на state.db. Успех кэшируется,
// ошибка — нет: следующий тик пробует снова.
func (a *App) openStore() (*store.Store, error) {
	a.storeMu.Lock()
	defer a.storeMu.Unlock()
	if a.st != nil {
		return a.st, nil
	}
	st, err := store.Open(config.DBPath())
	if err != nil {
		return nil, err
	}
	a.st = st
	return a.st, nil
}

func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
}

// Shutdown releases GUI-owned resources before Wails exits.
func (a *App) Shutdown(_ context.Context) {
	a.storeMu.Lock()
	defer a.storeMu.Unlock()
	if a.st != nil {
		_ = a.st.Close()
		a.st = nil
	}
}

func (a *App) quit() {
	if a.ctx != nil {
		runtime.Quit(a.ctx)
	}
}

func (a *App) showWindow() {
	if a.ctx != nil {
		runtime.WindowShow(a.ctx)
	}
}

// writeFileAtomic пишет data в path через temp+rename (как config.Save).
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
