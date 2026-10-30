package route

import (
	"testing"
	"time"
)

func leaseOn(id, proxy, ip string) *poolLease {
	return &poolLease{leaseID: id, proxy: proxy, publicIP: ip}
}

// Мягкий per-IP cap: занятый сверх предела egress уступает свободному, даже
// если он холоднее по LRU. Выключенный предел (0) сохраняет прежний выбор.
func TestPickSpreadSkipsEgressAboveInflightCap(t *testing.T) {
	hot := leaseOn("l-hot", "http://127.0.0.1:9620", "10.7.1.5")   // горел давно -> LRU-победитель
	cold := leaseOn("l-cold", "http://127.0.0.1:9621", "10.7.1.6") // использован только что
	pt := &PoolTransport{MaxEgressInflight: 1}
	key := pt.ipKey(hot.proxy, hot.publicIP)
	pt.muMu.Lock()
	pt.usedAt = map[string]time.Time{
		key:                                  time.Now().Add(-time.Hour),
		pt.ipKey(cold.proxy, cold.publicIP): time.Now(),
	}
	pt.muMu.Unlock()
	t.Cleanup(func() { pt.noteLeaseEnd(hot.leaseID) })

	if got := pt.pickSpread([]*poolLease{hot, cold}); got != 0 {
		t.Fatalf("без нагрузки выбран %d, want 0 (холодный LRU)", got)
	}
	pt.noteLeaseStart(hot) // 10.7.1.5 занят под пределом
	if n := pt.inflightLocked(key); n != 1 {
		t.Fatalf("inflight(%s) = %d, want 1", key, n)
	}
	if got := pt.pickSpread([]*poolLease{hot, cold}); got != 1 {
		t.Fatalf("занятый egress выбран (%d), want 1 (свободный)", got)
	}
	pt.noteLeaseEnd(hot.leaseID)
	if n := pt.inflightLocked(key); n != 0 {
		t.Fatalf("inflight после release = %d, want 0", n)
	}
	// Предел выключен: занятость не влияет на выбор.
	pt.noteLeaseStart(hot)
	pt.MaxEgressInflight = 0
	if got := pt.pickSpread([]*poolLease{hot, cold}); got != 0 {
		t.Fatalf("при выключенном пределе выбран %d, want 0 (LRU)", got)
	}
}

// Если заняты все кандидаты, выбор не падает и не блокирует запрос: берётся
// лучший по LRU из занятых.
func TestPickSpreadAllBusyStillPicks(t *testing.T) {
	a := leaseOn("l-busy-a", "http://127.0.0.1:9622", "10.7.2.5")
	b := leaseOn("l-busy-b", "http://127.0.0.1:9623", "10.7.2.6")
	pt := &PoolTransport{MaxEgressInflight: 1}
	pt.noteLeaseStart(a)
	pt.noteLeaseStart(b)
	t.Cleanup(func() {
		pt.noteLeaseEnd(a.leaseID)
		pt.noteLeaseEnd(b.leaseID)
	})
	i := pt.pickSpread([]*poolLease{a, b})
	if i < 0 || i > 1 {
		t.Fatalf("pickSpread вернул %d вне диапазона кандидатов", i)
	}
}

// Учёт идемпотентен: повторный release того же лиза не уводит счётчик в минус,
// неизвестный id — no-op (release зовут и standby/prewarm, которые не считались).
func TestEgressInflightAccountingIdempotent(t *testing.T) {
	pt := &PoolTransport{}
	l := leaseOn("l-idem", "http://127.0.0.1:9624", "10.7.3.5")
	key := pt.ipKey(l.proxy, l.publicIP)
	pt.noteLeaseStart(l)
	pt.noteLeaseStart(l)
	if n := pt.inflightLocked(key); n != 1 {
		t.Fatalf("двойной старт дал inflight=%d, want 1", n)
	}
	pt.noteLeaseEnd("l-idem")
	pt.noteLeaseEnd("l-idem")
	pt.noteLeaseEnd("l-unknown")
	if n := pt.inflightLocked(key); n != 0 {
		t.Fatalf("inflight после release = %d, want 0 (без ухода в минус)", n)
	}
	pt.noteLeaseStart(nil)
	if n := pt.inflightLocked(""); n != 0 {
		t.Fatalf("inflight пустого ключа = %d, want 0", n)
	}
}

// Счётчик общий на процесс: egress одного warp-пула делят все keyless-лейны
// (kilocode/opencode/opencode-zen), иначе каждый транспорт пропускал бы свой
// предел и один IP собрал бы N×cap параллельных.
func TestEgressInflightSharedAcrossTransports(t *testing.T) {
	ptA := &PoolTransport{Provider: "opencode", MaxEgressInflight: 1}
	ptB := &PoolTransport{Provider: "kilocode", MaxEgressInflight: 1}
	l := leaseOn("l-shared", "http://127.0.0.1:9625", "10.7.4.5")
	ptA.noteLeaseStart(l)
	t.Cleanup(func() { ptA.noteLeaseEnd(l.leaseID) })
	key := ptA.ipKey(l.proxy, l.publicIP)
	if n := ptB.inflightLocked(key); n != 1 {
		t.Fatalf("второй транспорт видит inflight=%d, want 1 (общий счётчик)", n)
	}
	// Тот же занятый egress для второго транспорта: берётся свободный.
	free := leaseOn("l-free", "http://127.0.0.1:9626", "10.7.4.6")
	ptB.muMu.Lock()
	ptB.usedAt = map[string]time.Time{key: time.Now().Add(-time.Hour), ptB.ipKey(free.proxy, free.publicIP): time.Now()}
	ptB.muMu.Unlock()
	if got := ptB.pickSpread([]*poolLease{l, free}); got != 1 {
		t.Fatalf("второй транспорт выбрал занятый egress (%d), want 1 (свободный)", got)
	}
}
