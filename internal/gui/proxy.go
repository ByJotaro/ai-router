package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ByJotaro/ai-router/internal/proxypool"
	"gopkg.in/yaml.v3"
)

// proxyCtx — короткий таймаут для GUI-вызовов к egress-модулю хаба.
func proxyCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// ProviderPoolEntry — провайдер с собственным транспортом pool/warp/iprotate
// (warp-router, не hub-egress). Источник правды — cfg.Providers[].Transport.
type ProviderPoolEntry struct {
	Name   string `json:"name"`
	Mode   string `json:"mode"` // pool | warp | iprotate
	APIURL string `json:"api_url,omitempty"`
}

// ProxyStatusView — честный статус: egress-сводка + инвентарь provider-pool.
// Hub-поля (enabled/total/alive/...) как раньше; новые поля не ломают фронт.
type ProxyStatusView struct {
	*proxypool.PoolStatus
	EgressEnabled bool                `json:"egress_enabled"`
	HubEnabled    bool                `json:"hub_enabled"` // legacy-алиас EgressEnabled
	ProviderPool  []ProviderPoolEntry `json:"provider_pool"`
	WarpPool      *WarpPoolStats      `json:"warp_pool,omitempty"`
}

// WarpBackendStat — per-backend live warp-пула (GET /api/v1/pool/stats).
type WarpBackendStat struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	PublicIP string `json:"public_ip,omitempty"`
	Country  string `json:"country,omitempty"`
	Socks    string `json:"socks_addr,omitempty"`
	HTTP     string `json:"http_addr,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	UpAt     int64  `json:"up_at,omitempty"`
	Requests int64  `json:"requests"`
	Errors   int64  `json:"errors"`
	Fails    int    `json:"fails"`
}

// WarpPoolStats — live-телеметрия warp-router пула: состояние, порты
// подключения (api/front), счётчики acquire/release/rotate, per-backend.
type WarpPoolStats struct {
	APIURL         string            `json:"api_url"`
	Enabled        bool              `json:"enabled"`
	State          string            `json:"state"`
	Size           int               `json:"size"`
	Up             int               `json:"up"`
	Starting       int               `json:"starting"`
	Down           int               `json:"down"`
	UniqueEgress   int               `json:"unique_egresses"`
	Anycast        int               `json:"anycast_egresses"`
	AcquireTotal   int64             `json:"acquire_total"`
	AcquireOK      int64             `json:"acquire_ok"`
	AcquireFail    int64             `json:"acquire_fail"`
	ReleaseTotal   int64             `json:"release_total"`
	RotateTotal    int64             `json:"rotate_total"`
	Restarts       int               `json:"restarts"`
	LastAcquireAt  int64             `json:"last_acquire_at,omitempty"`
	LastAcquireErr string            `json:"last_acquire_err,omitempty"`
	LastRotateAt   int64             `json:"last_rotate_at,omitempty"`
	StartedAt      int64             `json:"started_at,omitempty"`
	UptimeS        int64             `json:"uptime_s,omitempty"`
	FrontHTTP      string            `json:"front_http,omitempty"`
	FrontSocks     string            `json:"front_socks,omitempty"`
	Backends       []WarpBackendStat `json:"backends,omitempty"`
	Err            string            `json:"err,omitempty"`
}

// warpTokenFromDisk читает api.auth_token из warp-router config.yaml
// (тот же источник, что PoolTransport.effectiveToken).
func warpTokenFromDisk() string {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(local, "warp-router", "config.yaml"))
	if err != nil {
		return ""
	}
	var cfg struct {
		API struct {
			AuthToken string `yaml:"auth_token"`
		} `yaml:"api"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	return cfg.API.AuthToken
}

// warpPoolStats опрашивает warp-router GET {api}/api/v1/pool/stats напрямую:
// APIURL из первого провайдера с transport.mode pool/warp/iprotate
// (дефолт :8787), токен из transport.token или warp config с диска.
// Возвращает stats + apiURL; при недоступности — stats с Err (фронт рисует
// «warp-router не ответил», а не нули).
func (a *App) warpPoolStats() (*WarpPoolStats, string) {
	apiURL := "http://127.0.0.1:8787"
	token := ""
	if a.cfg != nil {
		for _, pc := range a.cfg.Providers {
			m := pc.Transport.Mode
			if m != "pool" && m != "warp" && m != "iprotate" {
				continue
			}
			if pc.Transport.APIURL != "" {
				apiURL = strings.TrimRight(pc.Transport.APIURL, "/")
			}
			token = pc.Transport.Token
			break
		}
	}
	if token == "" {
		token = warpTokenFromDisk()
	}
	out := &WarpPoolStats{APIURL: apiURL}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/api/v1/pool/stats", nil)
	if err != nil {
		out.Err = err.Error()
		return out, apiURL
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		out.Err = err.Error()
		return out, apiURL
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out.Err = "HTTP " + strconv.Itoa(resp.StatusCode)
		return out, apiURL
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		out.Err = err.Error()
		return out, apiURL
	}
	out.APIURL = apiURL
	return out, apiURL
}

// ProxyStatus возвращает сводку egress-пула + инвентарь provider-pool.
// При выключенном egress (a.proxies == nil) больше не врёт «всё выключено»:
// отдаёт пустую сводку с EgressEnabled=false ПЛЮС список pool-провайдеров
// из конфига, чтобы фронт рисовал provider-pool вместо «выключен».
func (a *App) ProxyStatus() (*ProxyStatusView, error) {
	out := &ProxyStatusView{
		PoolStatus:   &proxypool.PoolStatus{Accounts: []proxypool.Account{}, Servers: []proxypool.Server{}},
		ProviderPool: a.providerPools(),
	}
	if a.proxies == nil {
		out.WarpPool, _ = a.warpPoolStats()
		return out, nil
	}
	ctx, cancel := proxyCtx()
	defer cancel()
	ps, err := a.proxies.Status(ctx)
	if ps == nil {
		ps = out.PoolStatus
	}
	ps.Enabled = true
	out.PoolStatus = ps
	out.EgressEnabled = true
	out.HubEnabled = true // legacy-алиас
	out.WarpPool, _ = a.warpPoolStats()
	return out, err
}

// providerPools сканирует только cfg (без сети): провайдеры с Transport.Mode
// pool/warp/iprotate. Вынесено отдельно чтобы переиспользовать в Endpoints.
func (a *App) providerPools() []ProviderPoolEntry {
	if a.cfg == nil {
		return nil
	}
	var res []ProviderPoolEntry
	for name, pc := range a.cfg.Providers {
		m := pc.Transport.Mode
		if m != "pool" && m != "warp" && m != "iprotate" {
			continue
		}
		res = append(res, ProviderPoolEntry{Name: name, Mode: m, APIURL: pc.Transport.APIURL})
	}
	return res
}

// ProxyConnect подключает egress к указанному серверу (server может быть
// пустым — egress выберет сам).
func (a *App) ProxyConnect(server string) error {
	if a.proxies == nil {
		return fmt.Errorf("egress выключен в конфиге (enabled: false)")
	}
	ctx, cancel := proxyCtx()
	defer cancel()
	return a.proxies.Connect(ctx, server)
}

// ProxyDisconnect отключает активный выход.
func (a *App) ProxyDisconnect() error {
	if a.proxies == nil {
		return fmt.Errorf("egress выключен в конфиге")
	}
	ctx, cancel := proxyCtx()
	defer cancel()
	return a.proxies.Disconnect(ctx)
}

// ProxyRotate ротирует активный канал. server/mode передаётся как подсказка
// режима ротации (пусто — по стратегии).
func (a *App) ProxyRotate(server string) error {
	if a.proxies == nil {
		return fmt.Errorf("egress выключен в конфиге")
	}
	ctx, cancel := proxyCtx()
	defer cancel()
	return a.proxies.Rotate(ctx, server)
}
