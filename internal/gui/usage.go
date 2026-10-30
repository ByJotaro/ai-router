package gui

// UsageView — итоги по ключам.
type UsageView struct {
	Keys      int   `json:"keys"`
	Revoked   int   `json:"revoked"`
	Reqs      int64 `json:"reqs"`
	Tokens    int64 `json:"tokens"`
	USD       int64 `json:"usd"` // центы
	BudgetUSD int64 `json:"budget_usd"`
}

func (a *App) Usage() (*UsageView, error) {
	u := &UsageView{}
	for _, v := range a.ks.List() {
		u.Keys++
		if v.Revoked {
			u.Revoked++
		}
		u.Reqs += v.UsageReqs
		u.Tokens += v.UsageTokens
		u.USD += v.UsageUSD
		u.BudgetUSD += v.BudgetUSD
	}
	return u, nil
}

// UsageModelRow — топ модели за окно для usage-аналитики.
type UsageModelRow struct {
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Requests   int64  `json:"requests"`
	Prompt     int64  `json:"prompt"`
	Completion int64  `json:"completion"`
	Total      int64  `json:"total"`
	Errors     int64  `json:"errors"`
	LastUsed   string `json:"last_used"`
	CostUSD    int64  `json:"cost_usd"`
}

// UsageProviderRow — топ провайдеров за окно.
type UsageProviderRow struct {
	Provider string `json:"provider"`
	Requests int64  `json:"requests"`
	Prompt   int64  `json:"prompt"`
	Complete int64  `json:"complete"`
	Total    int64  `json:"total"`
	Errors   int64  `json:"errors"`
	CostUSD  int64  `json:"cost_usd"`
}

// UsageAnalyticsView — календарь + топы + streak (все дни, не только окно).
type UsageAnalyticsView struct {
	Days       int                `json:"days"`
	Requests   int64              `json:"requests"`
	Prompt     int64              `json:"prompt"`
	Completion int64              `json:"completion"`
	CacheRead  int64              `json:"cache_read"`
	CacheWrite int64              `json:"cache_write"`
	Reasoning  int64              `json:"reasoning"`
	Compressed int64              `json:"compressed"`
	CostUSD    int64              `json:"cost_usd"`
	Streak     int                `json:"streak"`
	ByModel    []UsageModelRow    `json:"by_model"`
	ByProvider []UsageProviderRow `json:"by_provider"`
	// Series — ряд по дням (формат DailyPointView) для heatmap-календаря:
	// фронт строит calFuel() из series, туда нужны ВСЕ дни окна.
	Series []DailyPointView `json:"series"`
	// ActivityMap — день->токены за 365 дней (github-style heatmap-источник,
	// Omni activityMap parity: DATE(ts)->SUM(in+out)).
	ActivityMap map[string]int64 `json:"activity_map"`
	// DailyTrend — ряд день/запросы/in/out/total/cost для графика тренда
	// (Omni dailyTrend parity: date/requests/promptTokens/completionTokens/totalTokens/cost).
	DailyTrend []DailyTrendPoint `json:"daily_trend"`
	// Weekly — агрегат по дням недели 0=вс..6=сб (Omni weeklyPattern parity).
	Weekly []WeeklyPoint `json:"weekly"`
}

// DailyTrendPoint — один день графика тренда.
type DailyTrendPoint struct {
	Date       string `json:"date"`
	Requests   int64  `json:"requests"`
	Prompt     int64  `json:"prompt"`
	Completion int64  `json:"completion"`
	Total      int64  `json:"total"`
	CostUSD    int64  `json:"cost_usd"`
	CacheRead  int64  `json:"cache_read"`
	CacheWrite int64  `json:"cache_write"`
}

// WeeklyPoint — агрегат одного дня недели.
type WeeklyPoint struct {
	DayOfWeek int64 `json:"day_of_week"`
	Days      int64 `json:"days"`
	Requests  int64 `json:"requests"`
	Total     int64 `json:"total"`
}

// uncachedPlus считает честный total без двойного счёта кэша: prompt_tokens
// от апстрима уже включают cached часть, поэтому total = completion +
// cache_read + cache_write + (prompt - min(cache_read, prompt)).
func uncachedPlus(prompt, completion, cacheRead, cacheWrite int64) int64 {
	cached := cacheRead
	if cached > prompt {
		cached = prompt
	}
	return completion + cacheRead + cacheWrite + (prompt - cached)
}

// UsageAnalyticsLight — легкий режим для календаря/тренда: только роллап
// daily_usage (DailyTrend+Series из него же), без 4 full-scan агрегатов
// request_log (ByModel/ByProvider/ActivityMap/Weekly).
func (a *App) UsageAnalyticsLight(days int) (*UsageAnalyticsView, error) {
	if days <= 0 {
		days = 365
	}
	if days > 730 {
		days = 730
	}
	st, err := a.openStore()
	if err != nil {
		return nil, err
	}
	rows, err := st.DailyUsageRange(days)
	if err != nil {
		return nil, err
	}
	v := &UsageAnalyticsView{Series: []DailyPointView{}}
	for _, r := range rows {
		v.Days++
		v.Requests += r.Requests
		v.Prompt += r.Prompt
		v.Completion += r.Completion
		v.CacheRead += r.CacheRead
		v.CacheWrite += r.CacheWrite
		v.Reasoning += r.Reasoning
		v.Compressed += r.Compressed
		v.CostUSD += r.CostUSD
		v.Series = append(v.Series, DailyPointView{
			Day: r.Day, Requests: r.Requests, Prompt: r.Prompt,
			Completion: r.Completion, CacheRead: r.CacheRead,
			CacheWrite: r.CacheWrite, Reasoning: r.Reasoning, CostUSD: r.CostUSD,
		})
		v.DailyTrend = append(v.DailyTrend, DailyTrendPoint{
			Date: r.Day, Requests: r.Requests, Prompt: r.Prompt,
			Completion: r.Completion, Total: uncachedPlus(r.Prompt, r.Completion, r.CacheRead, r.CacheWrite), CostUSD: r.CostUSD,
			CacheRead: r.CacheRead, CacheWrite: r.CacheWrite,
		})
	}
	return v, nil
}

// UsageAnalytics — полный режим (4 full-scan агрегата request_log).
// Фронт его не вызывает (только UsageAnalyticsLight); для ручного разбора,
// не дёргать с автотика. После idx_request_log_day сканы идут по range SEARCH.
func (a *App) UsageAnalytics(days int) (*UsageAnalyticsView, error) {
	if days <= 0 {
		days = 365
	}
	if days > 730 {
		days = 730
	}
	st, err := a.openStore()
	if err != nil {
		return nil, err
	}
	rows, err := st.DailyUsageRange(days)
	if err != nil {
		return nil, err
	}
	v := &UsageAnalyticsView{Series: []DailyPointView{}}
	for _, r := range rows {
		v.Days++
		v.Requests += r.Requests
		v.Prompt += r.Prompt
		v.Completion += r.Completion
		v.CacheRead += r.CacheRead
		v.CacheWrite += r.CacheWrite
		v.Reasoning += r.Reasoning
		v.Compressed += r.Compressed
		v.CostUSD += r.CostUSD
		v.Series = append(v.Series, DailyPointView{
			Day: r.Day, Requests: r.Requests, Prompt: r.Prompt,
			Completion: r.Completion, CacheRead: r.CacheRead,
			CacheWrite: r.CacheWrite, Reasoning: r.Reasoning, CostUSD: r.CostUSD,
		})
	}
	if ms, err := st.UsageByModel(days); err == nil {
		for _, m := range ms {
			v.ByModel = append(v.ByModel, UsageModelRow{
				Model: m.Model, Provider: m.Provider, Requests: m.Requests,
				Prompt: m.Prompt, Completion: m.Completion, Total: m.Total,
				Errors: m.Errors, LastUsed: m.LastUsed, CostUSD: m.CostUSD,
			})
		}
	}
	if ps, err := st.UsageByProvider(days); err == nil {
		for _, p := range ps {
			v.ByProvider = append(v.ByProvider, UsageProviderRow{
				Provider: p.Provider, Requests: p.Requests, Prompt: p.Prompt,
				Complete: p.Complete, Total: p.Total, Errors: p.Errors, CostUSD: p.CostUSD,
			})
		}
	}
	if s, err := st.UsageStreak(); err == nil {
		v.Streak = s
	}
	if am, err := st.ActivityMap(days); err == nil {
		v.ActivityMap = am
	}
	if dt, err := st.DailyTrend(days); err == nil {
		for _, d := range dt {
			v.DailyTrend = append(v.DailyTrend, DailyTrendPoint{
				Date: d.Day, Requests: d.Requests, Prompt: d.Prompt,
				Completion: d.Completion, Total: d.Total, CostUSD: d.CostUSD,
			})
		}
	}
	if w, err := st.WeeklyPattern(days); err == nil {
		for _, r := range w {
			v.Weekly = append(v.Weekly, WeeklyPoint{
				DayOfWeek: r.DayOfWeek, Days: r.Days, Requests: r.Requests, Total: r.Total,
			})
		}
	}
	return v, nil
}
