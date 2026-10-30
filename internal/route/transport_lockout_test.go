package route

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/lockout"
	"github.com/ByJotaro/ai-router/internal/provider"
)

// TestTransportFailuresDoNotLockModel pins the stand failure of 2026-10-22:
// during a pool-wide outage (burned egresses / empty lease batches) every
// request fails with a TRANSPORT error, i.e. a verdict about the pool. Feeding
// those into the per-model lockout locked a healthy model for the full
// 15-minute window: after the upstream recovered, 277 of 1000 requests at
// 1000 rpm were answered `model locked out (repeated failures)` in 2 ms while
// the pool was serving 200s again.
func TestTransportFailuresDoNotLockModel(t *testing.T) {
	r := New(map[string]*provider.Provider{},
		WithLockout(lockout.Config{MaxFailures: 3, Window: time.Minute, Lockout: 15 * time.Minute}))
	for range 50 {
		r.recordModelFailure("m", fmt.Errorf("%w: pool hot, %d seen", ErrTransportUnavailable, 6))
	}
	if !r.lock.Allowed("m", time.Now()) {
		t.Fatal("транспортные ошибки залочили модель: пул виноват, а 429 получает клиент")
	}
}

// TestModelFailuresStillLock: the exemption must be narrow — a real upstream
// verdict about the model keeps the breaker working.
func TestModelFailuresStillLock(t *testing.T) {
	r := New(map[string]*provider.Provider{},
		WithLockout(lockout.Config{MaxFailures: 3, Window: time.Minute, Lockout: 15 * time.Minute}))
	for range 3 {
		r.recordModelFailure("m", errors.New("upstream 500: model exploded"))
	}
	if r.lock.Allowed("m", time.Now()) {
		t.Fatal("вердикт модели обязан лочить модель после MaxFailures")
	}
}

// TestQuotaVerdictsDoNotLockModel: an upstream 429 (spent window) or a pool 503
// is a capacity verdict. The pool bans the egress for the Retry-After window;
// the model is fine, so locking it only converts honest 503s into 15 minutes
// of 429 (live 2026-10-22: 2687 `model locked out` in half an hour).
func TestQuotaVerdictsDoNotLockModel(t *testing.T) {
	r := New(map[string]*provider.Provider{},
		WithLockout(lockout.Config{MaxFailures: 2, Window: time.Minute, Lockout: 15 * time.Minute}))
	for range 10 {
		r.recordModelFailure("m", &UpstreamStatusError{Status: 429, Err: errors.New("429 quota"), RetryAfter: "5"})
		r.recordModelFailure("m", &UpstreamStatusError{Status: 503, Err: errors.New("no ready egress")})
	}
	if !r.lock.Allowed("m", time.Now()) {
		t.Fatal("квотные вердикты (429/503) залочили модель")
	}
}
