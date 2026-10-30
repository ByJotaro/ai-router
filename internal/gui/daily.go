package gui

// DailyPointView — один день дневного роллапа.
type DailyPointView struct {
	Day        string `json:"day"`
	Requests   int64  `json:"requests"`
	Prompt     int64  `json:"prompt"`
	Completion int64  `json:"completion"`
	CacheRead  int64  `json:"cache_read"`
	CacheWrite int64  `json:"cache_write"`
	Reasoning  int64  `json:"reasoning"`
	CostUSD    int64  `json:"cost_usd"` // центы
}

// DailyUsageView — итоги и ряд по дням для диаграммы «Использование».
type DailyUsageView struct {
	Days       int              `json:"days"` // активные дни в окне
	Requests   int64            `json:"requests"`
	Prompt     int64            `json:"prompt"`
	Completion int64            `json:"completion"`
	CacheRead  int64            `json:"cache_read"`
	CacheWrite int64            `json:"cache_write"`
	CostUSD    int64            `json:"cost_usd"` // центы
	Series     []DailyPointView `json:"series"`
}

// DailyUsage возвращает роллап за последние days дней.
func (a *App) DailyUsage(days int) (*DailyUsageView, error) {
	st, err := a.openStore()
	if err != nil {
		return nil, err
	}
	rows, err := st.DailyUsageRange(days)
	if err != nil {
		return nil, err
	}
	v := &DailyUsageView{Days: len(rows), Series: []DailyPointView{}}
	for _, r := range rows {
		p := DailyPointView{
			Day: r.Day, Requests: r.Requests, Prompt: r.Prompt,
			Completion: r.Completion, CacheRead: r.CacheRead,
			CacheWrite: r.CacheWrite, Reasoning: r.Reasoning, CostUSD: r.CostUSD,
		}
		v.Requests += p.Requests
		v.Prompt += p.Prompt
		v.Completion += p.Completion
		v.CacheRead += p.CacheRead
		v.CacheWrite += p.CacheWrite
		v.CostUSD += p.CostUSD
		v.Series = append(v.Series, p)
	}
	return v, nil
}
