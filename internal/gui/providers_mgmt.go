package gui

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/provider"
	"gopkg.in/yaml.v3"
)

// ProviderInput — данные нового/редактируемого провайдера из GUI.
type ProviderInput struct {
	Name              string            `json:"name"`
	BaseURL           string            `json:"base_url"`
	Type              string            `json:"type"`
	APIKeys           []string          `json:"api_keys"`
	Models            []string          `json:"models"`
	DisabledModels    []string          `json:"disabled_models,omitempty"`
	HiddenModels      []string          `json:"hidden_models,omitempty"`
	QuotaMode         string            `json:"quota_mode,omitempty"`
	TimeoutS          int               `json:"timeout_s"`
	Egress            string            `json:"egress,omitempty"`
	KeySwap           string            `json:"key_swap,omitempty"`
	ErrorPolicy       map[string]string `json:"error_policy,omitempty"`
	AuthMode          string            `json:"auth_mode,omitempty"`
	UserAgent         string            `json:"user_agent,omitempty"`
	ResponsesModels   []string          `json:"responses_models,omitempty"`
	TransportMode     string            `json:"transport_mode,omitempty"`
	TransportFanout   int               `json:"transport_fanout,omitempty"`
	TransportRounds   int               `json:"transport_rounds,omitempty"`
	TransportCooldown int               `json:"transport_cooldown,omitempty"`
	ModelReasoning    map[string]string `json:"model_reasoning,omitempty"`
	DisabledKeys      []string          `json:"disabled_keys,omitempty"`
	KeyProxies        map[string]string `json:"key_proxies,omitempty"`
	DefaultKey        string            `json:"default_key,omitempty"`
}

// AddProvider добавляет openai-совместимый провайдер: валидирует, пишет в
// конфиг-файл и регистрирует в живом роутере (route.Add), без рестарта.
func (a *App) AddProvider(in ProviderInput) (*SaveResult, error) {
	name := strings.TrimSpace(in.Name)
	base := strings.TrimSpace(in.BaseURL)
	if name == "" || base == "" {
		return &SaveResult{OK: false, Message: "имя и base_url обязательны"}, nil
	}
	typ := in.Type
	if typ == "" {
		typ = "openai-compatible"
	}
	prov, err := provider.Resolve(name, config.ProviderConf{Type: typ, BaseURL: base, APIKeys: in.APIKeys, Models: in.Models, TimeoutS: in.TimeoutS, Egress: in.Egress})
	if err != nil {
		return &SaveResult{OK: false, Message: err.Error()}, nil
	}
	// Записать в конфиг-файл (карта, как SaveConfig).
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		return &SaveResult{OK: false, Message: "конфиг: " + err.Error()}, nil
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.ProviderConf{}
	}
	cfg.Providers[name] = config.ProviderConf{
		Type: typ, BaseURL: base, APIKeys: in.APIKeys, Models: in.Models,
		TimeoutS: in.TimeoutS, Egress: in.Egress,
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return &SaveResult{OK: false, Message: "yaml: " + err.Error()}, nil
	}
	if err := writeFileAtomic(a.cfgPath, data); err != nil {
		return &SaveResult{OK: false, Message: "запись: " + err.Error()}, nil
	}
	a.router.Add(prov)
	a.log.Printf("provider added: %s (%s)", name, base)
	return &SaveResult{OK: true, Message: "провайдер «" + name + "» добавлен и подключён"}, nil
}

// AddProviderKeys добавляет bulk-ключи провайдеру (значения дописываются в
// ProviderConf.api_keys, пул пересоздаётся через route.Add с живым decrypt).
func (a *App) AddProviderKeys(providerName string, values []string) (*SaveResult, error) {
	providerName = strings.TrimSpace(providerName)
	clean := []string{}
	seen := map[string]bool{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		clean = append(clean, v)
	}
	if providerName == "" || len(clean) == 0 {
		return &SaveResult{OK: false, Message: "нет ключей для добавления"}, nil
	}
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		return &SaveResult{OK: false, Message: "конфиг: " + err.Error()}, nil
	}
	pc, ok := cfg.Providers[providerName]
	if !ok {
		return &SaveResult{OK: false, Message: "провайдер «" + providerName + "» не найден"}, nil
	}
	have := map[string]bool{}
	for _, v := range pc.APIKeys {
		have[strings.TrimSpace(v)] = true
	}
	added := 0
	for _, v := range clean {
		if !have[v] {
			pc.APIKeys = append(pc.APIKeys, v)
			added++
		}
	}
	cfg.Providers[providerName] = pc
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return &SaveResult{OK: false, Message: "yaml: " + err.Error()}, nil
	}
	if err := writeFileAtomic(a.cfgPath, data); err != nil {
		return &SaveResult{OK: false, Message: "запись: " + err.Error()}, nil
	}
	prov, err := provider.Resolve(providerName, pc)
	if err != nil {
		return &SaveResult{OK: false, Message: "валидация: " + err.Error()}, nil
	}
	a.router.Add(prov)
	a.log.Printf("provider keys added: %s +%d", providerName, added)
	return &SaveResult{OK: true, Message: "добавлено ключей: " + itoa(added)}, nil
}

func itoa(n int) string { return strconv.Itoa(n) }

// SetKeyEnabled включает/выключает использование ключа в пуле провайдера.
// Выключенный ключ пропускается при Pick (ротация идёт по остальным).
func (a *App) SetKeyEnabled(providerName, keyHash string, enabled bool) (*SaveResult, error) {
	if pl := a.router.Pool(providerName); pl != nil {
		if err := pl.SetEnabled(keyHash, enabled); err != nil {
			return &SaveResult{OK: false, Message: err.Error()}, nil
		}
		// Персист ручного тумблера: хэш в ProviderConf.disabled_keys (значения ключей не пишем).
		if cfg, err := config.Load(a.cfgPath); err == nil {
			if pc, ok := cfg.Providers[providerName]; ok {
				set := map[string]bool{}
				for _, h := range pc.DisabledKeys {
					set[h] = true
				}
				if enabled {
					delete(set, keyHash)
				} else {
					set[keyHash] = true
				}
				pc.DisabledKeys = pc.DisabledKeys[:0]
				for h := range set {
					pc.DisabledKeys = append(pc.DisabledKeys, h)
				}
				sort.Strings(pc.DisabledKeys)
				cfg.Providers[providerName] = pc
				if data, err := yaml.Marshal(cfg); err == nil {
					_ = writeFileAtomic(a.cfgPath, data)
				}
			}
		}
		return &SaveResult{OK: true, Message: "ключ " + condStr(enabled, "включён", "выключен")}, nil
	}
	return &SaveResult{OK: false, Message: "пул провайдера не найден"}, nil
}

// PoolKeyView — API-ключ пула провайдера с состоянием (маскированный хэш).
type PoolKeyView struct {
	Hash        string `json:"hash"` // KeyHash (префикс, не сам ключ)
	Masked      string `json:"masked"`
	Enabled     bool   `json:"enabled"`
	BackoffSecs int64  `json:"backoff_secs"` // 0 = не в бэкоффе
	Failures    int    `json:"failures"`
	Default     bool   `json:"default"`
	Proxy       string `json:"proxy"`
}

// PoolKeys возвращает список API-ключей пула провайдера (для детали).
func (a *App) PoolKeys(providerName string) ([]PoolKeyView, error) {
	pl := a.router.Pool(providerName)
	if pl == nil {
		return nil, nil
	}
	rows := pl.SnapshotKeys()
	out := make([]PoolKeyView, 0, len(rows))
	for _, r := range rows {
		out = append(out, PoolKeyView{Hash: r.Hash, Masked: r.Masked, Enabled: r.Enabled, BackoffSecs: r.BackoffSecs, Failures: r.Failures, Default: r.Default, Proxy: r.Proxy})
	}
	return out, nil
}

// SetKeyDefault выставляет ключ пула провайдера «по дефолту» (приоритетный).
func (a *App) SetKeyDefault(providerName, keyHash string) (*SaveResult, error) {
	if pl := a.router.Pool(providerName); pl != nil {
		if err := pl.SetDefault(keyHash); err != nil {
			return &SaveResult{OK: false, Message: err.Error()}, nil
		}
		if cfg, err := config.Load(a.cfgPath); err == nil {
			if pc, ok := cfg.Providers[providerName]; ok {
				pc.DefaultKey = keyHash
				cfg.Providers[providerName] = pc
				if data, err := yaml.Marshal(cfg); err == nil {
					_ = writeFileAtomic(a.cfgPath, data)
				}
			}
		}
		return &SaveResult{OK: true, Message: "ключ выбран по дефолту"}, nil
	}
	return &SaveResult{OK: false, Message: "пул провайдера не найден"}, nil
}

// SetKeyProxy привязывает прокси к конкретному ключу пула ("" — снять).
func (a *App) SetKeyProxy(providerName, keyHash, proxy string) (*SaveResult, error) {
	if pl := a.router.Pool(providerName); pl != nil {
		if err := pl.SetProxy(keyHash, proxy); err != nil {
			return &SaveResult{OK: false, Message: err.Error()}, nil
		}
		if cfg, err := config.Load(a.cfgPath); err == nil {
			if pc, ok := cfg.Providers[providerName]; ok {
				if pc.KeyProxies == nil {
					pc.KeyProxies = map[string]string{}
				}
				if proxy == "" {
					delete(pc.KeyProxies, keyHash)
				} else {
					pc.KeyProxies[keyHash] = proxy
				}
				cfg.Providers[providerName] = pc
				if data, err := yaml.Marshal(cfg); err == nil {
					_ = writeFileAtomic(a.cfgPath, data)
				}
			}
		}
		return &SaveResult{OK: true, Message: condStr(proxy == "", "прокси снят с ключа", "прокси привязан к ключу")}, nil
	}
	return &SaveResult{OK: false, Message: "пул провайдера не найден"}, nil
}

// Modules возвращает статус зависимых модулей (egress/warp/hub).
func (a *App) Modules() []map[string]any {
	if a.mods == nil {
		return []map[string]any{}
	}
	return a.mods.Status()
}

// ModuleStart запускает модуль (идемпотентно) и поднимает флаг авто-старта.
func (a *App) ModuleStart(name string) (*SaveResult, error) {
	if a.mods == nil {
		return &SaveResult{OK: false, Message: "менеджер модулей не инициализирован"}, nil
	}
	if err := a.mods.Start(name); err != nil {
		return &SaveResult{OK: false, Message: err.Error()}, nil
	}
	if err := a.mods.SetAuto(name, true); err != nil {
		return &SaveResult{OK: false, Message: err.Error()}, nil
	}
	return &SaveResult{OK: true, Message: "модуль «" + name + "» запущен"}, nil
}

// ModuleStop останавливает модуль, если его запускал этот менеджер.
// Внешние/ранее запущенные процессы не трогаются (политика юзера).
func (a *App) ModuleStop(name string) (*SaveResult, error) {
	if a.mods == nil {
		return &SaveResult{OK: false, Message: "менеджер модулей не инициализирован"}, nil
	}
	if err := a.mods.Stop(name); err != nil {
		return &SaveResult{OK: false, Message: err.Error()}, nil
	}
	if err := a.mods.SetAuto(name, false); err != nil {
		return &SaveResult{OK: false, Message: err.Error()}, nil
	}
	return &SaveResult{OK: true, Message: "модуль «" + name + "» остановлен (авто-старт выключен)"}, nil
}

func (a *App) ModuleAuto(name string, auto bool) (*SaveResult, error) {
	if a.mods == nil {
		return &SaveResult{OK: false, Message: "менеджер модулей не инициализирован"}, nil
	}
	if err := a.mods.SetAuto(name, auto); err != nil {
		return &SaveResult{OK: false, Message: err.Error()}, nil
	}
	return &SaveResult{OK: true, Message: condStr(auto, "авто-старт включён", "авто-старт выключен")}, nil
}

// SyncModels обновляет список моделей провайдера из GET {base_url}/models,
// пишет в конфиг-файл и перерегистрирует провайдера в живом роутере.
func (a *App) SyncModels(providerName string) (*SaveResult, error) {
	p := a.router.Provider(providerName)
	if p == nil {
		return &SaveResult{OK: false, Message: "провайдер не найден"}, nil
	}
	// /models needs the same fingerprint as chat: provider UA + key when
	// the upstream gates anonymous calls (zen returns a richer list with
	// a valid key).
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	if err != nil {
		return &SaveResult{OK: false, Message: "запрос: " + err.Error()}, nil
	}
	if p.UserAgent != "" {
		req.Header.Set("User-Agent", p.UserAgent)
	}
	if len(p.APIKeys) > 0 && p.AuthMode != "none" {
		req.Header.Set("Authorization", "Bearer "+p.APIKeys[0])
	}
	res, err := client.Do(req)
	if err != nil {
		return &SaveResult{OK: false, Message: "модели недоступны: " + err.Error()}, nil
	}
	defer res.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return &SaveResult{OK: false, Message: "невалидный ответ /models: " + err.Error()}, nil
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		return &SaveResult{OK: false, Message: "в /models нет моделей"}, nil
	}
	// Одна запись через union-путь: syncModelsPruned сам грузит/пишет конфиг.
	// Прямая запись pc.Models=ids здесь затирала бы ручные модели до union.
	return a.syncModelsPruned(providerName, ids, false, nil)
}

// SyncModelsPruned — синк с опциональной чисткой: prune=false = union
// (локальные ручные + upstream, дедуп, L-первым); prune=true = только
// upstream минус retired, disabled/hidden пересечения сохраняются.
func (a *App) SyncModelsPruned(providerName string, prune bool) (*SaveResult, error) {
	p := a.router.Provider(providerName)
	if p == nil {
		return &SaveResult{OK: false, Message: "провайдер не найден"}, nil
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	if err != nil {
		return &SaveResult{OK: false, Message: "запрос: " + err.Error()}, nil
	}
	if p.UserAgent != "" {
		req.Header.Set("User-Agent", p.UserAgent)
	}
	if len(p.APIKeys) > 0 && p.AuthMode != "none" {
		req.Header.Set("Authorization", "Bearer "+p.APIKeys[0])
	}
	res, err := client.Do(req)
	if err != nil {
		return &SaveResult{OK: false, Message: "модели недоступны: " + err.Error()}, nil
	}
	defer res.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return &SaveResult{OK: false, Message: "невалидный ответ /models: " + err.Error()}, nil
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		return &SaveResult{OK: false, Message: "в /models нет моделей"}, nil
	}
	return a.syncModelsPruned(providerName, ids, prune, nil)
}

func (a *App) syncModelsPruned(providerName string, ids []string, prune bool, retired map[string]bool) (*SaveResult, error) {
	p := a.router.Provider(providerName)
	if p == nil {
		return &SaveResult{OK: false, Message: "провайдер не найден"}, nil
	}
	keepDis := map[string]bool{}
	for _, m := range p.DisabledModels {
		keepDis[m] = true
	}
	keepHid := map[string]bool{}
	for _, m := range p.HiddenModels {
		keepHid[m] = true
	}
	var final []string
	var added, removed int
	if !prune {
		seen := map[string]bool{}
		for _, m := range p.Models {
			if !seen[m] {
				seen[m] = true
				final = append(final, m)
			}
		}
		for _, m := range ids {
			if retired != nil && retired[m] {
				continue
			}
			if !seen[m] {
				seen[m] = true
				final = append(final, m)
				added++
			}
		}
	} else {
		inU := map[string]bool{}
		for _, m := range ids {
			if retired != nil && retired[m] {
				continue
			}
			inU[m] = true
		}
		for _, m := range p.Models {
			if !inU[m] && !keepDis[m] && !keepHid[m] {
				removed++
				continue
			}
			final = append(final, m)
		}
		for _, m := range ids {
			found := false
			for _, f := range final {
				if f == m {
					found = true
					break
				}
			}
			if !found {
				final = append(final, m)
				added++
			}
		}
		var kept int
		for m := range keepDis {
			if !inU[m] {
				kept++
			}
		}
		for m := range keepHid {
			if !inU[m] && !keepDis[m] {
				kept++
			}
		}
		return a.syncModelsSave(providerName, final, added, removed, kept)
	}
	return a.syncModelsSave(providerName, final, added, 0, 0)
}

func (a *App) syncModelsSave(providerName string, final []string, added, removed, kept int) (*SaveResult, error) {
	p := a.router.Provider(providerName)
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		return &SaveResult{OK: false, Message: "конфиг: " + err.Error()}, nil
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.ProviderConf{}
	}
	pc := cfg.Providers[providerName]
	pc.Models = final
	cfg.Providers[providerName] = pc
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return &SaveResult{OK: false, Message: "yaml: " + err.Error()}, nil
	}
	if err := writeFileAtomic(a.cfgPath, data); err != nil {
		return &SaveResult{OK: false, Message: "запись: " + err.Error()}, nil
	}
	p.Models = final
	a.router.Add(p)
	a.log.Printf("sync models: %s -> %d моделей (added %d removed %d kept_dis %d)", providerName, len(final), added, removed, kept)
	msg := "моделей: " + strconv.Itoa(len(final))
	if added > 0 || removed > 0 || kept > 0 {
		msg += " (добавлено: " + strconv.Itoa(added) + ", удалено: " + strconv.Itoa(removed) + ", сохранено disabled/hidden: " + strconv.Itoa(kept) + ")"
	}
	return &SaveResult{OK: true, Message: msg}, nil
}

// UpdateProvider обновляет настройки существующего провайдера (стратегия
// свапа ключей, политика ошибок, таймаут, egress, модели) в конфиге и живом
// роутере. Пустые slices не затирают существующие модели/ключи.
func (a *App) UpdateProvider(name string, in ProviderInput) (*SaveResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return &SaveResult{OK: false, Message: "имя провайдера пустое"}, nil
	}
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		return &SaveResult{OK: false, Message: "конфиг: " + err.Error()}, nil
	}
	pc, ok := cfg.Providers[name]
	if !ok {
		return &SaveResult{OK: false, Message: "провайдер «" + name + "» не найден в конфиге"}, nil
	}
	if in.BaseURL != "" {
		pc.BaseURL = strings.TrimRight(in.BaseURL, "/")
	}
	if in.Type != "" {
		pc.Type = in.Type
	}
	if in.APIKeys != nil {
		pc.APIKeys = in.APIKeys
	}
	if in.Models != nil {
		pc.Models = in.Models
	}
	if in.DisabledModels != nil {
		pc.DisabledModels = in.DisabledModels
	}
	if in.HiddenModels != nil {
		pc.HiddenModels = in.HiddenModels
	}
	if in.QuotaMode == "auto" || in.QuotaMode == "usd" || in.QuotaMode == "tokens" {
		pc.QuotaMode = in.QuotaMode
	}
	if in.TimeoutS > 0 {
		pc.TimeoutS = in.TimeoutS
	}
	if in.KeySwap != "" {
		pc.KeySwap = in.KeySwap
	}
	if in.AuthMode != "" {
		pc.AuthMode = in.AuthMode
	}
	if in.UserAgent != "" {
		pc.UserAgent = in.UserAgent
	}
	if in.ResponsesModels != nil {
		pc.ResponsesModels = in.ResponsesModels
	}
	if in.TransportMode != "" {
		pc.Transport.Mode = in.TransportMode
	}
	if in.TransportFanout > 0 {
		pc.Transport.Fanout = in.TransportFanout
	}
	if in.TransportRounds > 0 {
		pc.Transport.Rounds = in.TransportRounds
	}
	if in.TransportCooldown > 0 {
		pc.Transport.CooldownS = in.TransportCooldown
	}
	if in.ModelReasoning != nil {
		pc.ModelReasoning = in.ModelReasoning
	}
	if in.ErrorPolicy != nil {
		if pc.ErrorPolicy == nil {
			pc.ErrorPolicy = map[string]string{}
		}
		for k, v := range in.ErrorPolicy {
			pc.ErrorPolicy[k] = v
		}
	}
	if in.Egress != "" {
		pc.Egress = in.Egress
	}
	if in.DisabledKeys != nil {
		pc.DisabledKeys = in.DisabledKeys
	}
	if in.KeyProxies != nil {
		pc.KeyProxies = in.KeyProxies
	}
	if in.DefaultKey != "" {
		pc.DefaultKey = in.DefaultKey
	}
	cfg.Providers[name] = pc
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return &SaveResult{OK: false, Message: "yaml: " + err.Error()}, nil
	}
	if err := writeFileAtomic(a.cfgPath, data); err != nil {
		return &SaveResult{OK: false, Message: "запись: " + err.Error()}, nil
	}
	prov, err := provider.Resolve(name, pc)
	if err != nil {
		return &SaveResult{OK: false, Message: "валидация: " + err.Error()}, nil
	}
	a.router.Add(prov)
	a.log.Printf("provider updated: %s", name)
	return &SaveResult{OK: true, Message: "провайдер «" + name + "» обновлён"}, nil
}

// ResetProviderDefaults сбрасывает переопределения провайдера (свап ключей,
// политика ошибок, таймаут, egress) к дефолтам: поля выставляются в нулевые,
// и при следующей сборке компонентов наследуются Config.Defaults.
func (a *App) ResetProviderDefaults(name string) (*SaveResult, error) {
	name = strings.TrimSpace(name)
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		return &SaveResult{OK: false, Message: "конфиг: " + err.Error()}, nil
	}
	pc, ok := cfg.Providers[name]
	if !ok {
		return &SaveResult{OK: false, Message: "провайдер «" + name + "» не найден"}, nil
	}
	pc.KeySwap = ""
	pc.ErrorPolicy = nil
	pc.TimeoutS = 0
	pc.Egress = ""
	cfg.Providers[name] = pc
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return &SaveResult{OK: false, Message: "yaml: " + err.Error()}, nil
	}
	if err := writeFileAtomic(a.cfgPath, data); err != nil {
		return &SaveResult{OK: false, Message: "запись: " + err.Error()}, nil
	}
	prov, err := provider.Resolve(name, pc)
	if err != nil {
		return &SaveResult{OK: false, Message: "валидация: " + err.Error()}, nil
	}
	a.router.Add(prov)
	a.log.Printf("provider defaults reset: %s", name)
	return &SaveResult{OK: true, Message: "настройки «" + name + "» сброшены к дефолтам"}, nil
}

func condStr(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}
