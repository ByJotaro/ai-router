package route

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Per-egress free-window accounting and diagnostics.
//
// The upstream free lane closes an egress' window without any warning: the
// only signal is a 429 whose Retry-After names the re-open time. Everything
// the pool knows about a window is therefore learned from its own traffic:
//
//   - ipOK[key] counts successful requests in the CURRENT window and is
//     reset when that window is banned (markBad429), so it is exactly
//     "requests served by this egress since its window opened";
//   - ok_before at ban time is the size of the window that just closed.
//
// noteWindowSample feeds those observations back into softBudget: the
// smallest window ever observed, less a safety margin. Once an egress has
// served that many requests its budget is nearly spent, so lease selection
// prefers a fresher egress and the pool rotates BEFORE the next request
// turns into a client-visible 429 — the "predict and rotate ahead" policy.

const (
	windowSampleCap = 64
	// softBudgetEnvVar overrides the learned request budget per egress
	// window. 0 = learn from observed windows, negative = disable the
	// soft-budget tier entirely.
	softBudgetEnvVar = "AI_ROUTER_POOL_SOFT_BUDGET"
	// rotateHoursEnvVar enables periodic identity rotation (hours, float).
	// Empty / 0 disables it. Rotation is skipped while the pool is healthy
	// (no hard bans and plenty of fresh-budget egresses) so a calm pool is
	// never respawned for nothing.
	rotateHoursEnvVar = "AI_ROUTER_POOL_ROTATE_HOURS"
)

// learnedSoftBudget derives the per-window request budget from observed
// window sizes. min(samples) is the safe bound: every larger window is
// covered by it. A margin of 25% leaves room for in-flight requests.
func learnedSoftBudget(samples []int) int {
	min := 0
	for _, s := range samples {
		if s <= 0 {
			continue
		}
		if min == 0 || s < min {
			min = s
		}
	}
	if min == 0 {
		return 0
	}
	soft := min * 3 / 4
	if soft < 10 {
		soft = 10
	}
	return soft
}

// noteWindowSample records the size of a window that just closed
// (ok_before at ban time) and re-learns the soft budget.
func (p *PoolTransport) noteWindowSample(n int) {
	if n <= 0 {
		return
	}
	p.muMu.Lock()
	p.winSamples = append(p.winSamples, n)
	if len(p.winSamples) > windowSampleCap {
		p.winSamples = p.winSamples[len(p.winSamples)-windowSampleCap:]
	}
	p.softBudget = learnedSoftBudget(p.winSamples)
	soft := p.softBudget
	pool := len(p.winSamples)
	p.muMu.Unlock()
	p.log().Info("pool: window sample", "ok_before", n, "soft_budget", soft, "samples", pool)
}

// softBudgetLimit returns the request budget per egress window in effect:
// env override wins, otherwise the learned value (0 = no prediction yet).
func (p *PoolTransport) softBudgetLimit() int {
	if raw := strings.TrimSpace(os.Getenv(softBudgetEnvVar)); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			if v < 0 {
				return 0
			}
			return v
		}
	}
	p.muMu.Lock()
	v := p.softBudget
	p.muMu.Unlock()
	return v
}

// budgetLow reports whether an egress has (nearly) spent its predicted
// window budget and should be avoided while fresher egresses exist.
func (p *PoolTransport) budgetLow(key string, soft int) bool {
	if key == "" || soft <= 0 {
		return false
	}
	p.muMu.Lock()
	ok := p.ipOK[key]
	p.muMu.Unlock()
	return ok >= soft
}

// EgressDiag is one egress row of the diagnostics endpoint.
type EgressDiag struct {
	Key       string `json:"ip"`
	WindowOK  int    `json:"window_ok"`
	BudgetLow bool   `json:"budget_low"`
	HardBad   bool   `json:"hard_bad"`
	BadUntil  string `json:"bad_until,omitempty"`
	Parked    bool   `json:"parked"`
	Heat      int    `json:"heat"`
	LastOK    string `json:"last_ok,omitempty"`
}

// PoolDiag snapshots everything the pool knows per egress, for the admin
// endpoint and for post-mortems of a 429 storm.
func (p *PoolTransport) PoolDiag() map[string]any {
	now := time.Now()
	soft := p.softBudgetLimit()
	p.muMu.Lock()
	defer p.muMu.Unlock()
	keys := map[string]bool{}
	for k := range p.ipOK {
		keys[k] = true
	}
	for k := range p.bad {
		keys[k] = true
	}
	for k := range p.hardBad {
		keys[k] = true
	}
	for k := range p.heat {
		keys[k] = true
	}
	for k := range p.okAt {
		keys[k] = true
	}
	rows := make([]EgressDiag, 0, len(keys))
	hard, low := 0, 0
	for k := range keys {
		row := EgressDiag{Key: k, WindowOK: p.ipOK[k], HardBad: p.hardBad[k]}
		row.BudgetLow = soft > 0 && row.WindowOK >= soft
		if row.BudgetLow {
			low++
		}
		if row.HardBad {
			hard++
		}
		if until, ok := p.bad[k]; ok && until.After(now) {
			row.BadUntil = until.Format(time.RFC3339)
		}
		if until, ok := p.park[k]; ok && until.After(now) {
			row.Parked = true
		}
		row.Heat = len(p.heat[k])
		if at := p.okAt[k]; at > 0 {
			row.LastOK = time.Unix(0, at).Format(time.RFC3339)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	samples := append([]int(nil), p.winSamples...)
	globalUntil := ""
	if p.globalBurnUntil.After(now) {
		globalUntil = p.globalBurnUntil.Format(time.RFC3339)
	}
	out := map[string]any{
		"provider":          p.Provider,
		"egresses":          len(rows),
		"hard_bad":          hard,
		"budget_low":        low,
		"soft_budget":       soft,
		"window_samples":    samples,
		"global_burn_until": globalUntil,
		"rows":              rows,
	}
	// PoolV2 speaks with warp-router; v1 pools keep the legacy contract.
	if g := p.Providers; len(g) > 0 {
		names := make([]string, 0, len(g))
		for n := range g {
			names = append(names, n)
		}
		sort.Strings(names)
		out["providers"] = names
	}
	return out
}

// ForceRotate asks the warp-router pool for a fresh identity generation
// (throttled by maybeRotatePoolForce). Returns true when the request was
// dispatched.
func (p *PoolTransport) ForceRotate() bool {
	before := p.lastRotate.Load()
	p.maybeRotatePoolForce()
	return p.lastRotate.Load() != before
}

// startRotatePolicy runs the pre-emptive identity rotation: when the pool is
// visibly degrading (hard bans share of the fleet, or nearly every egress out
// of predicted budget) the fresh-identity rotate goes out BEFORE clients see
// 429s. A healthy pool is left alone — each rotate respawns all workers and
// costs 1-2 minutes of degraded service.
func (p *PoolTransport) startRotatePolicy() {
	raw := strings.TrimSpace(os.Getenv(rotateHoursEnvVar))
	if raw == "" {
		return
	}
	hours, err := strconv.ParseFloat(raw, 64)
	if err != nil || hours <= 0 {
		return
	}
	every := time.Duration(hours * float64(time.Hour))
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			soft := p.softBudgetLimit()
			p.muMu.Lock()
			total := len(p.ipOK)
			hard := len(p.hardBad)
			low := 0
			for k, n := range p.ipOK {
				if soft > 0 && n >= soft {
					low++
				}
				_ = k
			}
			last := p.okAt
			recent := int64(0)
			for _, at := range last {
				if at > recent {
					recent = at
				}
			}
			p.muMu.Unlock()
			if total == 0 || recent == 0 {
				continue
			}
			// Traffic must be flowing: don't rotate (and disturb) an idle
			// pool's windows.
			if time.Since(time.Unix(0, recent)) > every {
				continue
			}
			stressed := hard*100 >= total*20 || (soft > 0 && low*10 >= total*8)
			if !stressed {
				continue
			}
			p.log().Warn("pool: pre-emptive rotate (degraded pool)",
				"egresses", total, "hard_bad", hard, "budget_low", low, "soft_budget", soft)
			p.maybeRotatePoolForce()
		}
	}()
}

// PoolDiag aggregates per-provider pool diagnostics for the admin endpoint.
func (g *poolGroup) PoolDiag() map[string]any {
	out := map[string]any{}
	for name, t := range g.byProv {
		if pt, ok := t.(*PoolTransport); ok {
			out[name] = pt.PoolDiag()
		}
	}
	return out
}

// ForceRotate asks one provider's pool (or every pool when provider is "")
// for a fresh identity generation.
func (g *poolGroup) ForceRotate(provider string) bool {
	ok := false
	for name, t := range g.byProv {
		if provider != "" && name != provider {
			continue
		}
		if pt, isPool := t.(*PoolTransport); isPool {
			if pt.ForceRotate() {
				ok = true
			}
		}
	}
	return ok
}
