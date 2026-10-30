package gui

import (
	"fmt"
	"sort"
)

// KeyView — виртуальный ключ с квотами (значения в «USD-центах» для budget).
type KeyView struct {
	Name    string   `json:"name"`
	Key     string   `json:"key"` // маскированный
	Models  []string `json:"models,omitempty"`
	Revoked bool     `json:"revoked"`
	NoLog   bool     `json:"no_log"`

	QuotaTokens int64 `json:"quota_tokens"`
	QuotaReqs   int64 `json:"quota_reqs"`
	BudgetUSD   int64 `json:"budget_usd"`
	UsageTokens int64 `json:"usage_tokens"`
	UsageReqs   int64 `json:"usage_reqs"`
	UsageUSD    int64 `json:"usage_usd"`

	TokenPct float64 `json:"token_pct"` // 0..1, -1 = безлимит
	ReqPct   float64 `json:"req_pct"`
	UsdPct   float64 `json:"usd_pct"`
}

func (a *App) Keys() ([]KeyView, error) {
	out := make([]KeyView, 0, len(a.ks.List()))
	for _, v := range a.ks.List() {
		kv := KeyView{
			Name: v.Name, Key: v.Key, Models: v.Models, Revoked: v.Revoked, NoLog: v.NoLog,
			QuotaTokens: v.QuotaTokens, QuotaReqs: v.QuotaReqs, BudgetUSD: v.BudgetUSD,
			UsageTokens: v.UsageTokens, UsageReqs: v.UsageReqs, UsageUSD: v.UsageUSD,
		}
		kv.TokenPct = pct(v.UsageTokens, v.QuotaTokens)
		kv.ReqPct = pct(v.UsageReqs, v.QuotaReqs)
		kv.UsdPct = pct(v.UsageUSD, v.BudgetUSD)
		out = append(out, kv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// KeysByProvider — группировка виртуальных ключей по провайдеру: ключ попадает
// к провайдеру, обслуживающему большинство моделей из его allowlist. Ключи без
// моделей (wildcard) и те, чьи модели не нашлись ни у одного провайдера,
// собираются в бакет "глобальные". Ключи провайдерского пула (здоровье API-ключей)
// при этом уже отражены в EndpointView (keys/keys_healthy/keys_backoff).
func (a *App) KeysByProvider() (map[string][]KeyView, error) {
	out := map[string][]KeyView{}
	unassigned := "глобальные"
	provModels := map[string]map[string]bool{}
	for _, name := range a.router.Providers() {
		pm := map[string]bool{}
		if p := a.router.Provider(name); p != nil {
			for _, m := range p.Models {
				pm[m] = true
			}
		}
		provModels[name] = pm
	}
	kvs, err := a.Keys()
	if err != nil {
		return nil, err
	}
	for _, kv := range kvs {
		target := ""
		if len(kv.Models) > 0 {
			best := 0
			for name, pm := range provModels {
				score := 0
				for _, m := range kv.Models {
					if pm[m] {
						score++
					}
				}
				if score > best {
					best = score
					target = name
				}
			}
		}
		if target == "" {
			target = unassigned
		}
		out[target] = append(out[target], kv)
	}
	// каждый провайдер присутствует даже с пустым списком ключей
	for _, name := range a.router.Providers() {
		if _, ok := out[name]; !ok {
			out[name] = nil
		}
	}
	return out, nil
}

// SetKeyQuota обновляет квоты виртуального ключа по имени (additive к GUI:
// живые квоты меняются в keys.Store и персистятся).
func (a *App) SetKeyQuota(name string, tokens, reqs, budgetUSD int64) error {
	if a.ks == nil {
		return fmt.Errorf("хранилище ключей недоступно")
	}
	return a.ks.SetQuota(name, tokens, reqs, budgetUSD)
}

// RevokeKey отзывает виртуальный ключ по имени (additive).
func (a *App) RevokeKey(name string) error {
	if a.ks == nil {
		return fmt.Errorf("хранилище ключей недоступно")
	}
	k := a.ks.FindByName(name)
	if k == nil {
		return fmt.Errorf("ключ %q не найден", name)
	}
	return a.ks.Revoke(k.Key)
}

// pct возвращает использованную долю квоты (0..1), -1 при безлимите.
func pct(used, quota int64) float64 {
	if quota <= 0 {
		return -1
	}
	if used >= quota {
		return 1
	}
	return float64(used) / float64(quota)
}
