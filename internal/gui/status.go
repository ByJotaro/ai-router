package gui

import (
	"net/http"
	"time"
)

// hubOnline проверяет доступность хаба по публичному /v1/health (без токена).
func (a *App) hubOnline() bool {
	if a.cfg.Hub.URL == "" {
		return false
	}
	cli := &http.Client{Timeout: 3 * time.Second}
	resp, err := cli.Get(a.cfg.Hub.URL + "/v1/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// StatusView is a secret-free GUI status summary.
type StatusView struct {
	Version         string `json:"version"`
	UptimeS         int64  `json:"uptime_s"`
	Gateway         string `json:"gateway"`
	Config          string `json:"config"`
	HubConfigured   bool   `json:"hub_configured"`
	HubOnline       bool   `json:"hub_online"`
	AdminConfigured bool   `json:"admin_configured"`
	Providers       int    `json:"providers"`
	Models          int    `json:"models"`
	Keys            int    `json:"keys"`
}

func (a *App) Status() (*StatusView, error) {
	return &StatusView{
		Version:         a.version,
		UptimeS:         int64(time.Since(a.started).Seconds()),
		Gateway:         a.cfg.Addr(),
		Config:          a.cfgPath,
		HubConfigured:   a.cfg.Hub.URL != "",
		HubOnline:       a.hubOnline(),
		AdminConfigured: a.cfg.AdminToken != "",
		Providers:       len(a.router.Providers()),
		Models:          len(a.router.Models()),
		Keys:            len(a.ks.List()),
	}, nil
}
