package routing

import (
	"reflect"
	"testing"
)

func cands() []Candidate {
	return []Candidate{
		{Name: "zeta", Tier: "free", Weight: 1},
		{Name: "alpha", Tier: "subscription", Weight: 1},
		{Name: "mid", Tier: "api", Weight: 1},
	}
}

func names(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func TestPriorityDeterministic(t *testing.T) {
	cs := SortByPriority(cands())
	want := []string{"alpha", "mid", "zeta"} // config order (priority), then name
	if !reflect.DeepEqual(names(cs), want) {
		t.Fatalf("priority order = %v, want %v", names(cs), want)
	}
}

func TestCascadeTiers(t *testing.T) {
	tiers := []string{"subscription", "api", "free"}
	cs := Cascade(cands(), tiers)
	want := []string{"alpha", "mid", "zeta"}
	if !reflect.DeepEqual(names(cs), want) {
		t.Fatalf("cascade order = %v, want %v", names(cs), want)
	}
}

func TestCostCheapestFirst(t *testing.T) {
	cs := []Candidate{
		{Name: "expensive", CostIn: 8, CostOut: 8},
		{Name: "cheap", CostIn: 0.2, CostOut: 0.4},
		{Name: "mid", CostIn: 2, CostOut: 2},
	}
	got := names(Cost{}.Choose(cs))
	want := []string{"cheap", "mid", "expensive"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cost order = %v, want %v", got, want)
	}
}

func TestLKGPPromotesFewerKeys(t *testing.T) {
	cs := []Candidate{
		{Name: "many", Keys: 9},
		{Name: "few", Keys: 1},
		{Name: "mid", Keys: 5},
	}
	got := names(LKGP{}.Choose(cs))
	want := []string{"few", "mid", "many"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lkgp order = %v, want %v", got, want)
	}
}

func TestRoundRobinRotates(t *testing.T) {
	r := &RoundRobin{}
	a := names(r.Choose(cands()))
	b := names(r.Choose(cands()))
	if reflect.DeepEqual(a, b) {
		t.Fatalf("round-robin did not rotate: %v", a)
	}
}

func TestWeightedKeepsAll(t *testing.T) {
	cs := Weighted{}.Choose(cands())
	got := map[string]bool{}
	for _, c := range cs {
		got[c.Name] = true
	}
	if len(got) != 3 {
		t.Fatalf("weighted lost candidates: %v", names(cs))
	}
}

func TestForUnknownStrategyDefaultsToPriority(t *testing.T) {
	if _, ok := For("bogus").(priorityStrategy); !ok {
		t.Fatal("unknown strategy must fall back to priority")
	}
}
