// Package routing implements provider-selection strategies and the tier
// cascade (subscription\u2192api\u2192cheap\u2192free). The default "priority"
// strategy keeps the deterministic config-order behaviour.
package routing

import (
	"math/rand"
	"sort"
	"sync"
)

// Candidate is a provider eligible for a model.
type Candidate struct {
	Name       string
	Tier       string
	Weight     float64
	CostIn     float64
	CostOut    float64
	Keys       int
	Priority   int  // config declaration order; lower wins for "priority"
	IsWildcard bool // true when Models: [] — serves any model, must rank last within tier
}

// Strategy orders candidates for a request.
type Strategy interface {
	Choose([]Candidate) []Candidate
}

// SortByPriority is the deterministic default: config order, then name.
func SortByPriority(cs []Candidate) []Candidate {
	out := append([]Candidate(nil), cs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Weighted shuffles candidates by weight (higher weight = more likely first).
type Weighted struct{}

func (Weighted) Choose(cs []Candidate) []Candidate {
	out := append([]Candidate(nil), cs...)
	// Weighted random sort: each candidate gets weight^U(0,1) score.
	scores := make([]float64, len(out))
	total := 0.0
	for i, c := range out {
		w := c.Weight
		if w <= 0 {
			w = 1
		}
		u := rand.Float64()
		// Avoid pow(0,0): exp form keeps ordering by weight with a random tiebreak.
		scores[i] = w * (1 + u)
		total += scores[i]
	}
	sort.SliceStable(out, func(i, j int) bool { return scores[i] > scores[j] })
	return out
}

// RoundRobin rotates the candidate order per call.
type RoundRobin struct {
	mu     sync.Mutex
	cursor int
}

func (r *RoundRobin) Choose(cs []Candidate) []Candidate {
	out := SortByPriority(cs)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(out) == 0 {
		return out
	}
	start := r.cursor % len(out)
	r.cursor++
	rot := append(append([]Candidate(nil), out[start:]...), out[:start]...)
	return rot
}

// P2C is power-of-two-choices: sample two candidates, prefer the higher
// weight (proxy for health/availability). O(2) per request.
type P2C struct{}

func (P2C) Choose(cs []Candidate) []Candidate {
	if len(cs) <= 2 {
		return SortByPriority(cs)
	}
	a := cs[rand.Intn(len(cs))]
	b := cs[rand.Intn(len(cs))]
	first := a
	if b.Weight > a.Weight {
		first = b
	}
	out := make([]Candidate, 0, len(cs))
	out = append(out, first)
	for _, c := range cs {
		if c.Name != first.Name {
			out = append(out, c)
		}
	}
	return out
}

// Cost sorts candidates cheapest-first by (input+output) price.
type Cost struct{}

func (Cost) Choose(cs []Candidate) []Candidate {
	out := append([]Candidate(nil), cs...)
	sort.SliceStable(out, func(i, j int) bool {
		ci := out[i].CostIn + out[i].CostOut
		cj := out[j].CostIn + out[j].CostOut
		if ci != cj {
			return ci < cj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// LKGP promotes providers with the fewest keys ("least key gets promotion"),
// balancing load across accounts.
type LKGP struct{}

func (LKGP) Choose(cs []Candidate) []Candidate {
	out := append([]Candidate(nil), cs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Keys != out[j].Keys {
			return out[i].Keys < out[j].Keys
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// For returns the strategy named by cfg (default: priority).
func For(name string) Strategy {
	switch name {
	case "weighted":
		return Weighted{}
	case "round-robin":
		return &RoundRobin{}
	case "p2c":
		return P2C{}
	case "cost":
		return Cost{}
	case "lkgp":
		return LKGP{}
	default:
		return priorityStrategy{}
	}
}

type priorityStrategy struct{}

func (priorityStrategy) Choose(cs []Candidate) []Candidate { return SortByPriority(cs) }

// CascadeChoose buckets candidates by tier (in tiers order), sorts
// non-wildcard vs wildcard within each bucket, then delegates intra-bucket
// ordering to Strategy.Choose. Unlisted tiers come last in declaration
// order. This replaces the old Cascade+Choose two-pass which sorted by name
// globally and used Priority+=100000 to push wildcards behind typed peers.
func CascadeChoose(cs []Candidate, tiers []string, strat Strategy) []Candidate {
	if len(cs) == 0 {
		return nil
	}
	rank := map[string]int{}
	for i, t := range tiers {
		rank[t] = i
	}
	type bucket struct {
		tierIdx  int
		inTier   bool
		members  []Candidate
	}
	buckets := map[int]*bucket{}
	order := []int{}
	for _, c := range cs {
		r, ok := rank[c.Tier]
		if !ok {
			r = len(tiers) // unlisted — after all tiers, preserves grouping
		}
		b, seen := buckets[r]
		if !seen {
			b = &bucket{tierIdx: r, inTier: ok}
			buckets[r] = b
			order = append(order, r)
		}
		b.members = append(b.members, c)
	}
	sort.Ints(order)
	out := make([]Candidate, 0, len(cs))
	for _, idx := range order {
		b := buckets[idx]
		// Wildcard last honestly within the tier: split before delegating.
		var typed, wild []Candidate
		for _, c := range b.members {
			if c.IsWildcard {
				wild = append(wild, c)
			} else {
				typed = append(typed, c)
			}
		}
		if len(typed) > 0 && strat != nil {
			typed = strat.Choose(typed)
		}
		if len(wild) > 0 && strat != nil {
			wild = strat.Choose(wild)
		}
		out = append(out, typed...)
		out = append(out, wild...)
	}
	return out
}

// Cascade groups candidates by tier order and flattens: all candidates of the
// first tier, then the second, etc. Providers without a listed tier land last.
// Deprecated: use CascadeChoose which applies Strategy per-bucket. Kept for
// tests and callers that only need tier grouping.
func Cascade(cs []Candidate, tiers []string) []Candidate {
	rank := map[string]int{}
	for i, t := range tiers {
		rank[t] = i
	}
	out := append([]Candidate(nil), cs...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, iok := rank[out[i].Tier]
		rj, jok := rank[out[j].Tier]
		if iok && jok {
			if ri != rj {
				return ri < rj
			}
			// Preserve declaration order inside same tier; name only as
			// secondary tie-breaker when priorities coincide.
			if out[i].Priority != out[j].Priority {
				return out[i].Priority < out[j].Priority
			}
			return out[i].Name < out[j].Name
		}
		if iok != jok {
			return iok // listed tiers come before unlisted
		}
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}
