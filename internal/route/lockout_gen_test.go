package route

import (
	"context"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/lockout"
	"github.com/ByJotaro/ai-router/internal/provider"
)

// genTransport is a stub pool whose identity generation the test advances, the
// way a completed heal rotate bumps PoolTransport.identGen.
type genTransport struct{ gen uint64 }

func (g *genTransport) Name() string { return "gen" }
func (g *genTransport) Acquire(context.Context, RequestContext, *provider.Provider) (*Lease, error) {
	return &Lease{}, nil
}
func (g *genTransport) IdentityGen() uint64 { return g.gen }

// TestIdentityGenerationResetsModelLockout pins the "healed pool still answers
// 429" failure: a 1000 rpm storm trips the per-model lockout (10 failures in
// 5 minutes) in about a second, and before this reset the lock kept the model
// out of service for the full 15-minute window even after the pool rotate had
// minted fresh identities. A new generation must clear those stale verdicts.
func TestIdentityGenerationResetsModelLockout(t *testing.T) {
	tp := &genTransport{gen: 1}
	r := New(map[string]*provider.Provider{}, WithTransport(tp),
		WithLockout(lockout.Config{MaxFailures: 2, Window: time.Minute, Lockout: time.Hour}))
	r.syncIdentityGen() // first sighting: records baseline, no reset
	for i := 0; i < 2; i++ {
		r.lock.RecordFailure("m", "boom", time.Now())
	}
	if r.lock.Allowed("m", time.Now()) {
		t.Fatal("предусловие: модель должна быть залочена после 2 фейлов")
	}
	tp.gen = 2 // rotate completed
	r.syncIdentityGen()
	if !r.lock.Allowed("m", time.Now()) {
		t.Fatal("lockout не сброшен на новой генерации идентичностей — пул вылечен, а клиент получает 429")
	}
}
