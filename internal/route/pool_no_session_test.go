package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// TestAcquireRefusesLeaseWithoutSession: when the harvest is exhausted the pool
// lane must NOT hand out a lease carrying a fabricated or shared session — both
// are visible to the free tier (generated ids 403 on every egress; a shared id
// links two IPs). The egress is refused instead, and the caller sees a clear
// "no unique session" verdict rather than a silent identity leak.
func TestAcquireRefusesLeaseWithoutSession(t *testing.T) {
	resetSessionPool()
	t.Cleanup(resetSessionPool)
	p := getSessionPool()
	p.Fetch = func(string, time.Duration, time.Time) []sessionEntry { return nil } // empty harvest

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	api := &fakePoolAPI{leases: []string{up.URL}}
	pa := httptest.NewServer(http.HandlerFunc(api.handler))
	defer pa.Close()

	pt := &PoolTransport{APIURL: pa.URL, Fanout: 1, Rounds: 1, Cooldown: time.Nanosecond}
	blockBackgroundWorkers(pt)
	prov := &provider.Provider{Name: "opencode", Type: "openai-compatible", BaseURL: "http://upstream/v1", AuthMode: "none"}

	lease, err := pt.acquireLease(context.Background(), pt.reqSeq.Add(1), "deepseek-v4.1-flash-free", stampFor(prov))
	if err == nil {
		if lease != nil {
			t.Fatalf("пул выдал лиз с сессией %q при пустой поставке", lease.session)
		}
		t.Fatal("acquire без сессии обязан вернуть ошибку, а не пустой лиз")
	}
	if !strings.Contains(err.Error(), "no unique session") {
		t.Fatalf("ошибка без явной причины: %v", err)
	}
}
