package route

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ByJotaro/ai-router/internal/buntransport"
	"github.com/ByJotaro/ai-router/internal/crypto"
	"github.com/ByJotaro/ai-router/internal/provider"
)

// PoolTransport adapts the standalone warp-router /api/v1/pool contract.
// Each upstream request is pinned to one pool lease (one WARP backend =
// one real egress IP). On a 429/5xx/transport error the lease is released
// and the next lease is acquired and tried, up to Fanout leases per round.
// If every lease in a round 429s, the next round acquires Fanout fresh
// leases again (up to Rounds) — the same "5 IPs, then the next 5 IPs"
// semantics as iprotate, but with real egress diversity (different WARP
// accounts/backends) instead of the 4 anycast addresses opencode.ai has.
//
// Only the provider named in Provider uses the pool; every other provider
// gets a direct lease, so one pool-enabled endpoint never hijacks the rest
// of the traffic.
type PoolTransport struct {
	APIURL string
	Token  string
	// Keys are provider API keys (e.g. opencode zen) attached to upstream
	// requests via Authorization: Bearer. Rotated on 429/401 alongside the
	// IP-pool rotation: opencode free caps by IP, not key, but several keys
	// hedge against per-key fingerprinting.
	Keys        []string
	keyIdx      atomic.Int64
	keyIdxBySet map[string]*atomic.Int64
	// ProviderKeys maps provider name -> that provider's own API keys.
	// Auth is per-provider: opencode free tier is anonymous (a Bearer key
	// on its routes earns 401), while opencode-zen requires its key. A
	// single shared Keys list leaked one provider's credentials into
	// another's requests ("provider opencode: Unauthorized").
	ProviderKeys   map[string][]string
	HTTPClient     *http.Client
	DirectFallback bool // explicit opt-in; false is fail-closed
	// AllowHomeIPDirect — второй ключ: прямой выход с нашего IP разрешён
	// только если и DirectFallback, и этот флаг выставлены. Иначе пул при
	// отказе отдаёт честный 503, а домашний IP не светится в апстриме
	// (2026-10-19: opencode пометил наш IP «abusive traffic» после серии
	// direct-запросов, 49 × 403, ноль через пул).
	AllowHomeIPDirect bool
	homeDirectWarn    sync.Once
	// MaxEgressInflight — мягкий предел одновременных запросов через один
	// egress (0 = выключено). Не блокирует и не 503-ит: при выборе среди
	// кандидатов пула занятые сверх предела egress уступают свободным, если
	// такие есть. Цель — не давать одному IP всплеск параллельных запросов,
	// который апстрим читает как фермерский трафик. Счётчик общий на процесс
	// (egressInflightReg): egresses одного warp-пула делят между собой все
	// keyless-транспорты (kilocode/opencode/opencode-zen), и per-транспортный
	// счётчик пропускал бы N×предел на один IP.
	MaxEgressInflight int
	Country           string
	Provider          string // pool applies only to this provider ("" = all)
	// Providers is the set of provider names the pool applies to. When
	// non-empty it wins over Provider and lets several pool-enabled
	// endpoints share one transport (e.g. opencode + opencode-zen),
	// instead of the first pool-mode provider in map order hijacking the
	// pool and every other one falling through to the home IP.
	Providers map[string]bool // provider name -> pool applies
	Fanout    int             // leases tried per round; default 5
	Rounds    int             // fresh lease rounds on persistent 429; default 2
	// NoStormCollapse opts this transport out of the storm fail-fast
	// collapse (config transport.storm_collapse=false). Kilo keyless:
	// its 429s are 5s short-parks with no hours-class Retry-After, so a
	// 1x1 collapse turns a reopening pool into premature 503s — full
	// fanout×rounds must stay during the storm.
	NoStormCollapse bool
	Cooldown        time.Duration // how long a 429-marked egress is skipped; default 60s
	// OnEvent mirrors proxypool.LogFunc: pool-path acquire/rotate outcomes
	// go to proxies_log (the tab feed), one row per logical request —
	// not per spin. Nil = no sink (tests, hub-only installs).
	OnEvent func(proxy, action string, ok bool, latencyMS int64, errText string)
	// PoolV2 speaks the warp-router /api/v2/pool dialect (TTL leases,
	// epoch on every response, idempotent batch release). Default false =
	// v1 contract. Flipped per provider by transport.pool_api: v2.
	PoolV2 bool
	// epochSeen is the last warp-router generation observed on v2
	// responses. A change means the pool was rebuilt (daemon restart or
	// rotate-with-respawn): identity-keyed bans/pins from the old
	// generation are stale and get cleared (notePoolEpoch).
	epochSeen  uint64
	lastRotate atomic.Int64 // unixnano of the last pool-rotate request
	// lastPoolOK is the unixnano of the last 200 served THROUGH a pool lease;
	// satTailStreak counts back-to-back "pool saturated" tail verdicts and is
	// reset by that 200. Together they expose "pool alive but serving
	// nothing" — the 2026-10-17 27-minute outage, where warp kept handing out
	// throttled egresses: every request probed exactly one, got a 429 and
	// returned 503, while the home-IP leg answered 200 immediately.
	lastPoolOK    atomic.Int64
	satTailStreak atomic.Int32
	lastSatRotate atomic.Int64 // unixnano of the last saturation-triggered rotate
	// Identity-recycle (heal) policy — replaces the manual "restart
	// warp-router" that revived the pool on 2026-10-22. Guarded by healMu so
	// concurrent exhausted requests cannot stack rotates on top of a worker
	// respawn that already runs 30-90s.
	healMu      sync.Mutex
	healAt      time.Time // last heal rotate
	healFails   int       // consecutive heals that did not restore service
	healRunning bool      // a rotate POST is in flight
	// healPreGroups is the /48 set known before the rotate and healExpectRenewal
	// marks a rotate whose renewal is not yet PROVEN: warp may hand back the
	// same prefixes (5 accounts, same endpoint), in which case the shared
	// upstream budget is unchanged and claiming "new generation" would be a
	// lie. Renewal is confirmed only by serving a 200 through a group that was
	// not in healPreGroups.
	healPreGroups     map[string]bool
	healExpectRenewal bool
	// WaitForLease: how long a request may WAIT for one egress before the
	// honest 503. Zero keeps the legacy fail-fast path (tests, direct-mode
	// stands); production sets it from config (default 30s).
	WaitForLease time.Duration
	// identGen counts completed identity recycles. The Router compares it
	// against its last seen value to know that every per-model failure verdict
	// was recorded against a generation that no longer exists (lockout reset).
	identGen  atomic.Uint64
	reqSeq    atomic.Uint64 // per-request id for log correlation (pool: req=N)
	muMu      sync.RWMutex
	fileMu    sync.Mutex           // serializes StateFile tmp+rename (concurrent saveState callers would race on the same .tmp)
	stateSeq  atomic.Uint64        // monotonic save generation; stale async snapshots drop
	stateDone atomic.Uint64        // max generation already persisted
	bad       map[string]time.Time // egress key -> hard/long cooldown until
	park      map[string]time.Time // egress key -> transient short-park until (memory-only, never persisted)
	heat      map[string][]int64   // egress key -> unixnano ring of recent 429 hits (memory-only, sliding window)
	heatGood  map[string]int64     // egress key -> unixnano of last 200 (cools heat faster than decay)
	// spent counts how many times an egress proved its budget gone (RA-less
	// 429 with zero successes in the window, or a clustered hours-class RA).
	// The ban ladder 10m -> 30m -> 2h keeps a provably burned address out of
	// the draw without stranding it for the full 12h window: measured
	// 2026-10-22, 3292 hours-429s landed on ~478 /128s (~7 re-probes each)
	// and 29888 transient 429s re-drew the same already-spent addresses,
	// because a 5s park forgets them between sweeps.
	spent map[string]int
	// hardBad marks egresses with an hours-class ban (Retry-After >=
	// minHoursBan): the window is burned for exactly the named RA
	// seconds, so the IP stays out until its deadline regardless of
	// later 200s from other backends.
	// A 200 on the banned IP itself still lifts it (markGood) — that
	// proves the window reopened early.
	hardBad     map[string]bool      // egress key -> hard class-ban set
	ipOK        map[string]int       // egress key -> successful requests before its first 429
	okAt        map[string]int64     // egress key -> unixnano of the LAST 200 (recency, unlike ipOK count)
	logThrottle map[string]int64     // log key -> unixnano of the last emission (anti-flood)
	ctrlProbed  map[string]bool      // egress key -> real-chat control probe already done
	verifyAt    map[string]time.Time // egress key -> next allowed standby verification of a soft ban
	// globalBurn is the shared-budget detector: when distinct egresses report
	// the SAME hours-class Retry-After within globalClusterWindow, the burn
	// is global (one deadline for all), not per-IP — banning shadows of
	// rotated identities is pure loss. While globalBurnUntil is in the
	// future: no per-IP markBad429 from 429s (short park only), no pool
	// rotate (fresh identities inherit the same deadline), fanout collapses
	// to 1 (probing one egress proves the class); 429s still leave as 503
	// WITHOUT Retry-After (swap mode — syntheticVerdict/tailVerdict never
	// forward RA outward). A 200 on any egress clears it
	// immediately (markGoodKey) — the window reopened. Guarded by muMu like
	// bad. Memory-only: after a restart the detector re-learns within two
	// 429s instead of trusting a stale global flag.
	globalBurnUntil time.Time // global backoff deadline (zero = inactive)
	// globalGen is the identity generation the burn was observed against. A
	// rotate re-registers the WARP devices, so a burn armed on the retired
	// generation must not keep collapsing the pool for hours: live 2026-10-22
	// stand run armed a 17.3h flag 4s AFTER a rotate, from an old lease that
	// was still draining.
	globalGen     uint64
	globalArmedAt time.Time     // when the current flag was armed
	globalRA      int           // earliest observed hours-class Retry-After secs
	globalObs     []globalRAObs // recent per-IP hours-class observations
	globalOKAt    time.Time     // first 200 inside global (probation for clear)
	globalOKGroup string        // /48 group of that first 200
	storm429      []int64       // unixnano ring of recent pool 429 hits (all egress, memory-only)
	stormOKs      []int64       // unixnano ring of recent pool 200s (storm lift needs 2+ ok, wave1 heat P1)
	stormOKKeys   []stormOKHit  // те же 200 с привязкой к egress: per-egress лифт
	// winSamples/softBudget — pre-emptive per-egress budget accounting
	// (pool_diag.go): sizes of recently closed windows and the learned
	// request budget after which an egress is treated as nearly spent.
	winSamples []int
	softBudget int
	stormKeys     []stormHit    // те же хиты с привязкой к egress: single-IP завал vs shared шторм
	// standby — очередь верифицированных egress (+1 поток verify): 429-хит
	// ставит IP в очередь, воркер гоняет max_tokens=1 мимо горячего пути,
	// 200 возвращает IP в ротацию СРАЗУ (notePrewarmLive + park/bad lift),
	// остальные запросы тем временем идут в живые. Дешёвый: last-used модель.
	standbyLive    map[string]time.Time // egress key -> last verified 200 (verify-поток)
	standbyQueued  map[string]time.Time // egress key -> когда поставлен в очередь
	standbyRunning atomic.Bool          // verify-поток уже запущен (один на транспорт)
	standbyCh      chan string          // очередь IP на проверку (non-blocking send)
	standbyProv    string               // провайдер для verify-запросов
	standbyBaseURL string               // baseURL для verify-запросов
	// prewarm — фоновая проверка живых egress на 1 резервном прокси:
	// верифицированный egress (200 < prewarmTTL) ходит первым в acquire,
	// под нагрузкой есть упор вместо слепого свипа. Дешёвый: last-used модель
	// провайдера + max_tokens=1 (~десятки токенов на тик, не сотни тысяч).
	prewarmLive map[string]time.Time // egress key -> last verified 200
	// lastProbe bounds qualification probes per egress (quota guard): the
	// free tier bills every probe against the /64 daily budget.
	lastProbe      map[string]time.Time
	prewarmModel   string      // last-used модель (что реально гоняют)
	prewarmRunning atomic.Bool // тикер уже запущен (один на транспорт)
	// usedAt tracks the last hand-out time per egress key for spread
	// rotation: a new request avoids backends handed out within
	// spreadWindow and otherwise picks the least-recently-used one, so
	// burst traffic spreads across the pool instead of stacking on one
	// backend. Guarded by muMu like bad. Memory-only, never persisted.
	usedAt map[string]time.Time // egress key -> last acquire time
	// regionGood remembers the egress that last served a model after a
	// region swap (memory-only): the next request for the same model goes
	// straight there and skips the 1-3 probe discovery sweep (~2-5s on
	// muse-1.3). regionBad remembers per-model region-rejected egresses
	// with TTL so acquire deprioritizes them for THAT model only; other
	// models are unaffected. Guarded by muMu like bad. Never persisted:
	// after a restart discovery re-learns within one swap.
	regionGood map[string]string
	regionBad  map[string]map[string]time.Time
	// Egress identity (sticky per-IP session): upstream keys the free-tier
	// window by (IP, full-UA, session). UA is the pinned 1.18.99 for every
	// egress; the session is pinned per egress key inside SessionPool
	// (NextFor), spread across harvested TUI ids. Memory-only.
	tokenFromFile      string    // warp-router api.auth_token read from its own config
	tokenFileMtime     time.Time // mtime of the file when it was last read
	tokenFileCheckedAt time.Time // wall time of last successful stat
	// StateFile persists 429/Retry-After cooldowns and hard class-bans to
	// disk so a restart (promote, crash) does not forget burned exact-IP windows
	// and re-storm them from scratch. Empty disables persistence.
	StateFile string
	// Log receives rotation events: egress 429/5xx -> cooldown, NOT_FOUND
	// blacklist, direct home-IP fallback. Nil defaults to slog.Default().
	Log *slog.Logger
	// apiClient is the shared keep-alive client for the local pool API
	// (:8787 acquire/release/rotate). Per-call http.Clients share
	// DefaultTransport pooling, but an explicit client documents the
	// intent and survives HTTPClient rewiring. Initialized once.
	apiOnce   sync.Once
	apiClient *http.Client
	// apiVerOnce logs which pool API shape the live transport uses. Без этой
	// строки нельзя доказать, работает ли batch-выбор лизов (v2) или код
	// инертен (v1) — ревью 2026-10-21 читало только конфиг.
	apiVerOnce sync.Once
	// pickLogN samples the batch-pick diagnostic (1 of 8) so the choice logic
	// is auditable in production without flooding the log.
	pickLogN atomic.Uint64
	// apiFails counts consecutive pool-API hard failures (transport error,
	// pool-not-enabled, auth). At apiFailThreshold the breaker opens for a
	// cooldown: Acquire fails fast with no TCP at all instead of spinning
	// 24 attempts x 10s against a dead API (2026-09-11: pool off after
	// reboot, every request burned 10s + ~12 sockets, TIME_WAIT pileup
	// starved the box until warp's :8118 couldn't bind). Any success
	// resets the counter immediately.
	apiFails     atomic.Int64
	apiDownUntil atomic.Int64
	apiProbeAt   atomic.Int64 // unixnano fail-fast deadline, 0 = closed
	// pxClients caches one HTTP transport per pool egress proxy URL.
	// clientFor used to Clone DefaultTransport per attempt; clones are
	// never closed, so every attempt leaked idle connections + timers
	// into socket exhaustion. The pool is small; the map is capped.
	pxMu      sync.Mutex
	pxClients map[string]*http.Client
}

// prewarmTTL: верифицированный egress считается живым это время. 60с:
// короче heatWindow 3м (heat успевает остыть раньше, чем prewarm протухнет),
// длиннее тика 15с (несколько тиков на один IP без перепроверки).
// В шторме TTL тянется до 120с (stormPrewarmTTL): метки должны жить весь
// burn, иначе acquire слепнет ровно когда упор нужнее (волна 2026-09-20).
const prewarmTTL = 60 * time.Second
const stormPrewarmTTL = 120 * time.Second
const prewarmTick = 30 * time.Second

// standbyTTL: верифицированный standby-IP считается живым это время.
// Короче heatWindow, длиннее verify-интервала: метка живёт весь парк.
const standbyTTL = 60 * time.Second
const standbyQueueCap = 64

// noteStandbyLive: verify-поток вернул IP в строй — метка + снятие
// парка/бана. Остальные запросы тем временем шли в живые.
func (p *PoolTransport) noteStandbyLive(key string, now time.Time) {
	if key == "" {
		return
	}
	p.muMu.Lock()
	if p.standbyLive == nil {
		p.standbyLive = map[string]time.Time{}
	}
	p.standbyLive[key] = now
	delete(p.standbyQueued, key)
	delete(p.park, key)
	delete(p.verifyAt, key)
	if !p.hardBad[key] {
		delete(p.bad, key)
	}
	// A verified 200 is the proof the ladder ban was wrong: reset its stage.
	if p.spent != nil {
		delete(p.spent, key)
	}
	p.muMu.Unlock()
	p.notePrewarmLive(key, now)
}

// standbyFresh: IP проверен verify-потоком и не в бане — упор для acquire.
func (p *PoolTransport) standbyFresh(key string, now time.Time) bool {
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	t, ok := p.standbyLive[key]
	if !ok || now.Sub(t) > standbyTTL {
		return false
	}
	if until, bad := p.bad[key]; bad && now.Before(until) {
		return false
	}
	if until, park := p.park[key]; park && now.Before(until) {
		return false
	}
	return p.heatCountLocked(key, now) < heatMaxHits
}

// enqueueStandby ставит IP в очередь verify-потока (non-blocking, dedup
// поQueued-метке): 429-хит уходит на проверку мимо горячего пути.
func (p *PoolTransport) enqueueStandby(key string) {
	if key == "" {
		return
	}
	p.muMu.Lock()
	if p.standbyQueued == nil {
		p.standbyQueued = map[string]time.Time{}
	}
	if _, dup := p.standbyQueued[key]; dup {
		p.muMu.Unlock()
		return
	}
	p.standbyQueued[key] = time.Now()
	ch := p.standbyCh
	p.muMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- key:
	default:
	}
}

// startStandbyVerify запускает +1 поток верификации (один на транспорт):
// очередь 429-битых IP, проверка max_tokens=1 вне горячего пути, 200 —
// IP сразу в ротацию. Останавливается вместе с процессом.
func (p *PoolTransport) startStandbyVerify(prov string, baseURL string, provProvider *provider.Provider) {
	p.muMu.Lock()
	p.standbyProv = prov
	p.standbyBaseURL = baseURL
	if p.standbyCh == nil {
		p.standbyCh = make(chan string, standbyQueueCap)
	}
	p.muMu.Unlock()
	if p.standbyRunning.Swap(true) {
		return
	}
	go func() {
		for key := range p.standbyCh {
			p.standbyOnce(key, prov, baseURL, provProvider)
		}
	}()
}

// standbyOnce: одна проверка одного IP из очереди.
// verifyAnyway reports whether a non-hard (ladder/soft) ban may be falsified by
// the standby verifier: our own guess must be testable once per half-window,
// while the plant-level hours ban (hardBad) stays authoritative.
func (p *PoolTransport) verifyAnyway(key string) bool {
	now := time.Now()
	p.muMu.Lock()
	defer p.muMu.Unlock()
	if p.hardBad[key] {
		return false
	}
	until, bad := p.bad[key]
	if !bad || !until.After(now) {
		return true
	}
	if p.verifyAt == nil {
		p.verifyAt = map[string]time.Time{}
	}
	if next, seen := p.verifyAt[key]; seen && now.Before(next) {
		return false
	}
	half := until.Sub(now) / 2
	if half < 30*time.Second {
		half = 30 * time.Second
	}
	p.verifyAt[key] = now.Add(half)
	return true
}

func (p *PoolTransport) standbyOnce(key, prov, baseURL string, provProvider *provider.Provider) {
	if key == "" || baseURL == "" {
		return
	}
	p.muMu.RLock()
	model := p.prewarmModel
	base := p.standbyBaseURL
	if base == "" {
		base = baseURL
	}
	p.muMu.RUnlock()
	if model == "" {
		// Never default to muse-spark: the free tier answers 403 on it 24/24
		// (route.go:1784-1787), so an empty default would condemn every egress
		// the probe touches. deepseek-v4.1-flash-free is the model the pool
		// actually serves (200s measured 2026-10-23).
		model = "deepseek-v4.1-flash-free"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	body := gateProbeBody(model)
	var l *poolLease
	// До 5 acquire в поисках целевого IP: single-random промахивается
	// мимо цели (~1/20). Промах после 5 — чистим очередь, следующий 429
	// перепоставит IP в очередь (иначе never-clear заклинивает requeue).
	for i := 0; i < 5; i++ {
		var aerr error
		l, aerr = p.acquireLease(ctx, p.reqSeq.Add(1), model, stampFor(provProvider))
		if aerr != nil || l == nil {
			p.muMu.Lock()
			delete(p.standbyQueued, key)
			p.muMu.Unlock()
			return
		}
		if p.ipKey(l.proxy, l.publicIP) == key {
			break
		}
		_ = p.release(context.Background(), l.leaseID)
		l = nil
	}
	if l == nil {
		p.muMu.Lock()
		delete(p.standbyQueued, key)
		p.muMu.Unlock()
		return
	}
	defer func() { _ = p.release(context.Background(), l.leaseID) }()
	if p.isBad(key, time.Now()) && !p.verifyAnyway(key) {
		p.muMu.Lock()
		delete(p.standbyQueued, key)
		p.muMu.Unlock()
		return
	}
	cl := p.clientFor(l.proxy)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return
	}
	r.Header.Set("Content-Type", "application/json")
	if l.ua != "" {
		r.Header.Set("User-Agent", l.ua)
	} else {
		r.Header.Set("User-Agent", pinnedUA())
	}
	if k := p.currentKeyFor(prov); k != "" {
		r.Header.Set("Authorization", "Bearer "+k)
	}
	if l.session != "" {
		// Same triple the hot path ships: probe must verify that exact shape.
		setEgressSession(r.Header, l.session)
	}
	if provProvider != nil {
		setOpencodeHeadersAt(r, context.Background(), provProvider, key)
	}
	var resp *http.Response
	if provProvider != nil && isOpencodeUpstream(provProvider) && buntransport.Enabled() && l.proxy != "" {
		resp, err = buntransport.Do(ctx, cl, r, l.proxy)
	} else {
		resp, err = cl.Do(r)
	}
	if err != nil {
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		p.noteStandbyLive(key, time.Now())
		p.noteStormOKKey(key, time.Now())
		p.log().Info("pool: standby verified, back in rotation", "ip", key)
	}
}

// notePrewarmLive помечает egress верифицированным (фоновый 200).
func (p *PoolTransport) notePrewarmLive(key string, now time.Time) {
	if key == "" {
		return
	}
	p.muMu.Lock()
	if p.prewarmLive == nil {
		p.prewarmLive = map[string]time.Time{}
	}
	p.prewarmLive[key] = now
	p.muMu.Unlock()
}

// prewarmFresh: egress верифицирован и не в бане — упор для acquire.
// Свежие 429 бьют несвежий 200: heat>=cap отключает fast-path (иначе
// verify-then-throttle внутри TTL возвращал битый egress первым в обход
// hot-очереди). heatCountLocked — та же логика под уже взятым RLock
// (RWMutex R-рекурсия = дедлок, поэтому не зовём heatCount отсюда).
func (p *PoolTransport) heatCountLocked(key string, now time.Time) int {
	cutoff := now.Add(-heatWindow).UnixNano()
	good, hasGood := p.heatGood[key]
	n := 0
	for _, h := range p.heat[key] {
		if h < cutoff {
			continue
		}
		// F2: 200 прощает только хиты ДО него; пост-200 429 видны сразу.
		// Старое good>=oldest → 0 маскировало новые 429 до expiry среза.
		if hasGood && h <= good {
			continue
		}
		n++
	}
	return n
}

func (p *PoolTransport) prewarmFresh(key string, now time.Time) bool {
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	t, ok := p.prewarmLive[key]
	if !ok {
		return false
	}
	// Шторм: метки живут stormPrewarmTTL (упор на весь burn).
	// review_1 F1: IsZero истинен и ПОСЛЕ expiry (сброс только по двум 200),
	// len(storm429) не прунится без хитов — TTL залипал на 120s. Живой флаг +
	// cutoff вместо len.
	ttl := prewarmTTL
	cutoff := now.Add(-stormWindow).UnixNano()
	n := 0
	for _, h := range p.storm429 {
		if h >= cutoff {
			n++
		}
	}
	if now.Before(p.globalBurnUntil) || n >= stormMax429 {
		ttl = stormPrewarmTTL
	}
	if now.Sub(t) > ttl {
		return false
	}
	if until, bad := p.bad[key]; bad && now.Before(until) {
		return false
	}
	if until, park := p.park[key]; park && now.Before(until) {
		return false
	}
	if p.heatCountLocked(key, now) >= heatMaxHits {
		return false
	}
	return true
}

// rememberModel: last-used модель — prewarm гоняет то, что реально используют.
func (p *PoolTransport) rememberModel(model string) {
	if model == "" {
		return
	}
	p.muMu.Lock()
	p.prewarmModel = model
	p.muMu.Unlock()
}

// startPrewarm запускает фоновый тикер (один на транспорт): берёт 1 резервный
// лиз (не из горячего пути), шлёт минимальный chat (max_tokens=1) на
// last-used модель, 200 → notePrewarmLive. Ошибки молча (Debug): тикер —
// наблюдатель, не ретраер. Останавливается вместе с процессом.
// Шторм-режим (волна 2026-09-20): в storm/global тик 15с→5с, проверка
// ТОЛЬКО заведомо холодных (не bad/park/heat) — упор из живых вместо
// ре-варма горячих. Раньше тикер в шторме молчал и метки протухали за 60с.
func (p *PoolTransport) startPrewarm(prov string, baseURL string, provProvider *provider.Provider) {
	if p.prewarmRunning.Swap(true) {
		return
	}
	go func() {
		t := time.NewTicker(prewarmTick)
		defer t.Stop()
		for range t.C {
			p.prewarmOnce(prov, baseURL, provProvider)
			// Шторм-дотики: пока burn активен — дополнительные проверки
			// холодных между основными тиками (5с вместо 15с).
			// 2026-10-06: сотня/мин без prewarm-меток валилась в
			// concentration — холодные нечем доказать. В шторме тикаем
			// каждые 2с до 5 проб подряд (не больше 1/2с — не жечь окна).
			if p.globalActive() || p.stormActive() {
				// Raised probe gap: fewer, slower probes so we don't burn the
				// /64 daily budget while the pool is already under 429 pressure.
				for range 2 {
					time.Sleep(10 * time.Second)
					if !p.globalActive() && !p.stormActive() {
						break
					}
					p.prewarmOnce(prov, baseURL, provProvider)
				}
			}
		}
	}()
}

// probeMinGap bounds how often ONE egress may be spent on a qualification
// probe. Probes are ordinary upstream requests and the free tier bills them
// against the /64 daily quota, so a 15s ticker per transport (three pool
// providers today, plus storm dodatki) could eat a fresh egress' whole day.
// Live markers make the repeat worthless anyway: a 2xx younger than this
// already proves the egress serves.
// 15 min, not 5: probes are SERVED requests, so 5 min spent 288 of the egress'
// ~800/day budget (~36%) proving liveness that real traffic proves for free.
// 15 min = 96/day (~12%) and a dead egress is still caught by live verdicts.
const probeMinGap = 30 * time.Minute

// probeBudget reports whether this egress may be probed now, and records the
// attempt when it may. recentLive = a 2xx marker younger than probeMinGap.
func (p *PoolTransport) probeBudget(key string, lastLive time.Time, now time.Time) bool {
	if key == "" {
		return false
	}
	if !lastLive.IsZero() && now.Sub(lastLive) < probeMinGap {
		return false
	}
	p.muMu.Lock()
	defer p.muMu.Unlock()
	if p.lastProbe == nil {
		p.lastProbe = map[string]time.Time{}
	}
	if at, ok := p.lastProbe[key]; ok && now.Sub(at) < probeMinGap {
		return false
	}
	p.lastProbe[key] = now
	return true
}

// prewarmOnce: одна проверка одного резервного egress.
func (p *PoolTransport) prewarmOnce(prov, baseURL string, provProvider *provider.Provider) {
	if baseURL == "" {
		return
	}
	// No probe on the normal path (#5218 EGRESS_PROBE_MODE=auto): if the
	// pool already has enough verified-live egresses, probing only burns the
	// /64 daily budget. Probe only when the live pool is thin.
	p.muMu.RLock()
	liveN := len(p.prewarmLive)
	p.muMu.RUnlock()
	if liveN >= 8 {
		return
	}
	storm := p.globalActive() || p.stormActive()
	p.muMu.RLock()
	model := p.prewarmModel
	p.muMu.RUnlock()
	if model == "" {
		// Never default to muse-spark: the free tier answers 403 on it 24/24
		// (route.go:1784-1787), so an empty default would condemn every egress
		// the probe touches. deepseek-v4.1-flash-free is the model the pool
		// actually serves (200s measured 2026-10-23).
		model = "deepseek-v4.1-flash-free"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	body := gateProbeBody(model)
	// 1 резервный лиз мимо горячего пути: acquireLease со своим контекстом,
	// сразу release — лиз не удерживаем.
	l, err := p.acquireLease(ctx, p.reqSeq.Add(1), model, stampFor(provProvider))
	if err != nil || l == nil {
		return
	}
	key := p.ipKey(l.proxy, l.publicIP)
	defer func() { _ = p.release(context.Background(), l.leaseID) }()
	// Пропускаем забаненные/запаркованные: их и так все знают, проверять нечего.
	if p.isBad(key, time.Now()) || p.isParked(key, time.Now()) {
		return
	}
	// Quota guard: probes are served requests. One per egress per probeMinGap,
	// and never while a fresh 2xx marker proves the egress already.
	p.muMu.RLock()
	liveAt := p.prewarmLive[key]
	p.muMu.RUnlock()
	if !p.probeBudget(key, liveAt, time.Now()) {
		return
	}
	// Шторм: проверяем ТОЛЬКО холодных (heat==0) — ре-варм горячих лил
	// +10% burn в общий котёл. Холодный 200 = упор для acquire.
	if storm && p.heatCount(key, time.Now()) > 0 {
		return
	}
	cl := p.clientFor(l.proxy)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return
	}
	r.Header.Set("Content-Type", "application/json")
	// Sticky identity: тот же UA + session, что hot-path шлёт с этого IP —
	// probe верифицирует то же окно, а не чужой fingerprint.
	if l.ua != "" {
		r.Header.Set("User-Agent", l.ua)
	} else {
		r.Header.Set("User-Agent", pinnedUA())
	}
	if k := p.currentKeyFor(prov); k != "" {
		r.Header.Set("Authorization", "Bearer "+k)
	}
	if l.session != "" {
		// Same triple the hot path ships: probe must verify that exact shape.
		setEgressSession(r.Header, l.session)
	}
	if provProvider != nil {
		setOpencodeHeadersAt(r, context.Background(), provProvider, key)
	}
	var resp2 *http.Response
	if provProvider != nil && isOpencodeUpstream(provProvider) && buntransport.Enabled() && l.proxy != "" {
		resp2, err = buntransport.Do(ctx, cl, r, l.proxy)
	} else {
		resp2, err = cl.Do(r)
	}
	resp := resp2
	if err != nil {
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// review_1 F2: живой 200 виден storm — иначе 1x1 живёт дольше
		// шторма. markGood НЕ зовём (флюк-лифт запрещён).
		p.notePrewarmLive(key, time.Now())
		p.noteStormOKKey(key, time.Now())
	}
}

// Pool-API breaker tuning: 3 consecutive hard failures open the breaker;
// transient pool answers (409 respawn jitter, 429/503 saturated) never
// count — only transport errors, pool-not-enabled and auth rejections.
const (
	apiFailThreshold  = 3
	apiDownCool       = 30 * time.Second
	apiDownCoolStable = 120 * time.Second
)

// apiProbeEvery bounds the half-open probe rate of the pool-API breaker: while
// the breaker is open one caller per interval may dial through. A warp-router
// respawn used to blackhole EVERY request for the full 30-120s cooldown
// (2252 "pool api restarting" 503s in one day) even though the API was already
// back; a dead API still fails and re-arms the breaker.
const apiProbeEvery = 3 * time.Second

// apiDown reports whether the pool-API breaker is open.
func (p *PoolTransport) apiDown() bool {
	until := p.apiDownUntil.Load()
	if until == 0 || time.Now().UnixNano() >= until {
		return false
	}
	now := time.Now().UnixNano()
	for {
		next := p.apiProbeAt.Load()
		if now < next {
			return true
		}
		if p.apiProbeAt.CompareAndSwap(next, now+int64(apiProbeEvery)) {
			return false
		}
	}
}

// apiWaitTick is the poll interval of waitAPI (a var so tests do not sleep 3s).
var apiWaitTick = apiProbeEvery

// apiWaitCap bounds how long a request may wait on the pool-API breaker: the
// half-open probe fires every apiProbeEvery, so a respawn window is usually
// seconds, not minutes.
const apiWaitCap = 20 * time.Second

// waitAPI blocks until the pool-API breaker closes, bounded by WaitForLease and
// apiWaitCap. Returns true when the breaker closed (the caller may proceed).
// Before this, an open breaker answered 503 with waited_ms=0 for every request
// in the window (2,978/day measured 2026-10-22) although the half-open probe
// proved the API back within seconds (review M1).
func (p *PoolTransport) waitAPI(ctx context.Context) bool {
	budget := p.WaitForLease
	if budget <= 0 {
		return false
	}
	if budget > apiWaitCap {
		budget = apiWaitCap
	}
	deadline := time.Now().Add(budget)
	waited := false
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(apiWaitTick):
		}
		waited = true
		if !p.apiDown() {
			if waited {
				p.log().Info("pool: pool API recovered while waiting", "budget", budget.String())
			}
			return true
		}
	}
	return false
}

// warnHomeDirectSuppressed — однократный сигнал в лог: конфиг просит
// direct_fallback, но второй ключ allow_home_ip_direct не выставлен, поэтому
// прямой выход запрещён (пул отдаёт честный 503). Так тихая утечка домашнего
// IP в апстрим становится видимой в логах.
func (p *PoolTransport) warnHomeDirectSuppressed() {
	p.homeDirectWarn.Do(func() {
		p.log().Warn("pool: direct_fallback is set but allow_home_ip_direct is not — direct home-IP egress disabled",
			"providers", p.Provider, "api", p.APIURL)
	})
}

// egressInflightReg — общий на процесс учёт одновременных лиз по egress-ключу
// (proxy|publicIP). Ключ включает прокси-URL пула, поэтому разные пулы не
// пересекаются. Счётчик живёт до release лиза; незакрытый счётчик только
// понижает приоритет egress (выбор мягкий), поэтому утечка ничего не ломает.
var egressInflightReg = struct {
	mu    sync.Mutex
	byKey map[string]int    // ipKey -> одновременных лиз
	key   map[string]string // leaseID -> ipKey
}{}

// noteLeaseStart/noteLeaseEnd — учёт одновременных лиз по egress-ключу.
func (p *PoolTransport) noteLeaseStart(l *poolLease) {
	if l == nil || l.leaseID == "" {
		return
	}
	key := p.ipKey(l.proxy, l.publicIP)
	if key == "" {
		return
	}
	egressInflightReg.mu.Lock()
	if egressInflightReg.byKey == nil {
		egressInflightReg.byKey = make(map[string]int, 8)
		egressInflightReg.key = make(map[string]string, 8)
	}
	if _, dup := egressInflightReg.key[l.leaseID]; !dup {
		egressInflightReg.key[l.leaseID] = key
		egressInflightReg.byKey[key]++
	}
	egressInflightReg.mu.Unlock()
}

func (p *PoolTransport) noteLeaseEnd(leaseID string) {
	if leaseID == "" {
		return
	}
	egressInflightReg.mu.Lock()
	if key, ok := egressInflightReg.key[leaseID]; ok {
		delete(egressInflightReg.key, leaseID)
		if egressInflightReg.byKey[key] > 0 {
			egressInflightReg.byKey[key]--
		}
	}
	egressInflightReg.mu.Unlock()
}

// inflightLocked возвращает число одновременных лиз на egress-ключе.
func (p *PoolTransport) inflightLocked(key string) int {
	if key == "" {
		return 0
	}
	egressInflightReg.mu.Lock()
	n := egressInflightReg.byKey[key]
	egressInflightReg.mu.Unlock()
	return n
}

// homeDirectAllowed — единственный источник правды о том, можно ли вообще
// выходить в апстрим с нашего IP: нужны ОБА ключа (direct_fallback и
// allow_home_ip_direct). Проверяют его и Acquire, и poolDeadLastResort.
func (p *PoolTransport) homeDirectAllowed() bool {
	return p.DirectFallback && p.AllowHomeIPDirect
}

// noteAPISuccess closes the breaker on any working pool-API answer.
func (p *PoolTransport) noteAPISuccess() {
	p.apiFails.Store(0)
	p.apiDownUntil.Store(0)
}

// isPoolAPIDead отличает мёртвый API от забитого пула: connection-refused /
// crash / DNS = API нет (брейкер); timeout/cancel при живом API = пул забит
// (брейкер не открываем — иначе шторм 429 сам себя добивает через apiDown).
func isPoolAPIDead(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "dial" {
			return true
		}
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "connection refused") || strings.Contains(msg, "no connection could be made") {
		return true
	}
	return false
}

// sleepOrCancel спит d, но просыпается сразу при отмене клиента:
// оборванный запрос не должен держать бюджет 2с-снами до дедлайна.
func sleepOrCancel(d time.Duration, ctx context.Context) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// isCancelErr: отмена клиента, не вердикт egress — без банов/свапов/учёта.
func isCancelErr(err error) bool {
	return errors.Is(err, context.Canceled)
}

// noteAPIFail records a hard pool-API failure; stable ones (pool disabled,
// auth) open a long cooldown, transport blips a short one.
func (p *PoolTransport) noteAPIFail(stable bool) {
	n := p.apiFails.Add(1)
	if n < apiFailThreshold {
		return
	}
	d := apiDownCool
	if stable {
		d = apiDownCoolStable
	}
	until := time.Now().Add(d).UnixNano()
	for {
		cur := p.apiDownUntil.Load()
		if until <= cur || p.apiDownUntil.CompareAndSwap(cur, until) {
			break
		}
	}
	// Pace the half-open probe with the cooldown: without this a dead API was
	// dialed once every apiProbeEvery (20/min) regardless of apiDownCool.
	p.apiProbeAt.Store(time.Now().Add(d).UnixNano())
	p.log().Warn("pool: api breaker open", "fails", n, "cooldown", d.String(), "stable", stable)
}

// effectiveToken returns the explicit transport token, or — when the
// transport runs unauthenticated beside a local warp-router — the API token
// read from the daemon's own config. The file is re-read when it changes
// on disk (mtime check) or at most every 5 min. sync.Once forever stalled
// token rotation and caused 401 drift after warp re-keyed api.auth_token.
func (p *PoolTransport) effectiveToken() string {
	if p.Token != "" {
		return p.Token
	}
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		p.muMu.Lock()
		tok := p.tokenFromFile
		p.muMu.Unlock()
		return tok
	}
	path := filepath.Join(local, "warp-router", "config.yaml")
	info, err := os.Stat(path)
	if err != nil {
		p.muMu.Lock()
		tok := p.tokenFromFile
		p.muMu.Unlock()
		return tok
	}
	p.muMu.Lock()
	cached := p.tokenFromFile
	cachedMtime := p.tokenFileMtime
	cachedAt := p.tokenFileCheckedAt
	p.muMu.Unlock()
	if !info.ModTime().After(cachedMtime) && time.Since(cachedAt) < 5*time.Minute && cached != "" {
		return cached
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cached
	}
	var cfg struct {
		API struct {
			AuthToken string `yaml:"auth_token"`
		} `yaml:"api"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil || cfg.API.AuthToken == "" {
		return cached
	}
	p.muMu.Lock()
	p.tokenFromFile = cfg.API.AuthToken
	p.tokenFileMtime = info.ModTime()
	p.tokenFileCheckedAt = time.Now()
	p.muMu.Unlock()
	return cfg.API.AuthToken
}

// currentKey returns the round-robin key index (empty when none configured).
func (p *PoolTransport) currentKey() string {
	if len(p.Keys) == 0 {
		return ""
	}
	return p.Keys[p.keyIdx.Load()%int64(len(p.Keys))]
}

// keysFor returns the key set that applies to the given provider: its own
// entry from ProviderKeys when declared, else the legacy shared Keys list.
func (p *PoolTransport) keysFor(prov string) []string {
	if p.ProviderKeys != nil {
		if ks, ok := p.ProviderKeys[prov]; ok {
			return ks
		}
	}
	return p.Keys
}

// currentKeyFor is currentKey scoped to one provider's key set, with its
// own RR wheel: the shared keyIdx serves legacy Keys, each ProviderKeys
// entry rotates independently (a shared wheel made provider rotation
// shift the global order and vice versa).
func (p *PoolTransport) currentKeyFor(prov string) string {
	if ks, ok := p.ProviderKeys[prov]; ok {
		if len(ks) == 0 {
			return ""
		}
		return ks[p.keyCounter(prov).Load()%int64(len(ks))]
	}
	return p.currentKey()
}

// keyCounter returns the RR counter for a key set (lazy, muMu-guarded;
// counters themselves atomic). "" = legacy shared Keys wheel.
func (p *PoolTransport) keyCounter(set string) *atomic.Int64 {
	if set == "" {
		return &p.keyIdx
	}
	p.muMu.Lock()
	defer p.muMu.Unlock()
	if p.keyIdxBySet == nil {
		p.keyIdxBySet = map[string]*atomic.Int64{}
	}
	c, ok := p.keyIdxBySet[set]
	if !ok {
		c = &atomic.Int64{}
		p.keyIdxBySet[set] = c
	}
	return c
}

// rotateKey advances the key wheel by one; rotateKeyFor is per-provider.
func (p *PoolTransport) rotateKey() {
	if len(p.Keys) > 1 {
		p.keyIdx.Add(1)
	}
}

// rotateKeyFor rotates for a specific provider's key set.
func (p *PoolTransport) rotateKeyFor(prov string) {
	if ks, ok := p.ProviderKeys[prov]; ok {
		if len(ks) > 1 {
			p.keyCounter(prov).Add(1)
		}
		return
	}
	p.rotateKey()
}

// pinnedUA is the upstream-facing opencode version. Boot default from npm
// registry (opencode-ai/latest): 1.18.31 @ 2026-09-14, weekly cadence.
// A background refresher (startUARefresh, 6h ticker, npm -> GitHub
// fallback) bumps it to the newest stable automatically, so the pool
// always looks like a fresh TUI. Rule: uniform UA per pool, never per-IP
// (varying patch per egress risks a throttled UA class). Stale minors
// throttle ~5/IP (user 2026-09-06).
const defaultPinnedUAVersion = "1.19.4"

// The triple must be ONE shipped CLI build: the previous pair (provider-utils
// 4.0.40 + bun 1.3.14) never shipped together with any opencode release and is
// a stable non-client fingerprint. Values below come from the MITM capture of
// the real CLI 1.19.4 (captured request log of the CLI).
const capturedUABuild = "1.19.4"
const pinnedProviderUtils = "4.0.23"
const pinnedBunRuntime = "1.4.4"

var pinnedUAVer atomic.Value // string
var uaDriftWarned sync.Map   // version -> warned
var uaRefreshRunning atomic.Bool

func pinnedUAVersion() string {
	if v, ok := pinnedUAVer.Load().(string); ok && v != "" {
		return v
	}
	return defaultPinnedUAVersion
}

func pinnedUA() string {
	return "opencode/" + pinnedUAVersion() + " ai-sdk/provider-utils/" + pinnedProviderUtils + " runtime/bun/" + pinnedBunRuntime
}

// isStableOpencodeVer accepts only X.Y.Z (no -dev/-rc prereleases).
func isStableOpencodeVer(v string) bool {
	p := strings.Split(v, ".")
	if len(p) != 3 {
		return false
	}
	for _, s := range p {
		if s == "" {
			return false
		}
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return false
			}
		}
	}
	return true
}

func setPinnedUA(v, src string) {
	if !isStableOpencodeVer(v) {
		return
	}
	cur := pinnedUAVersion()
	if v == cur {
		return
	}
	pinnedUAVer.Store(v)
	slog.Default().Debug("pool: pinned UA refreshed", "from", cur, "to", v, "src", src)
	// The provider-utils/bun pair is pinned from ONE captured CLI build; a
	// newer CLI version with those values is a tuple that may never have
	// shipped. Nothing we can derive at runtime tells us the pair, so say so
	// loudly: the fix is a fresh MITM capture + update of the two constants.
	if v != capturedUABuild {
		if _, warned := uaDriftWarned.LoadOrStore(v, struct{}{}); !warned {
			slog.Default().Warn("pool: UA version ahead of the captured tuple — provider-utils/bun unverified",
				"version", v, "captured", capturedUABuild,
				"provider_utils", pinnedProviderUtils, "bun", pinnedBunRuntime)
		}
	}
}

func fetchNpmOpencodeVersion() string {
	cl := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "https://registry.npmjs.org/opencode-ai/latest", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var d struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&d); err != nil {
		return ""
	}
	return strings.TrimSpace(d.Version)
}

func fetchGithubOpencodeVersion() string {
	cl := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/sst/opencode/releases/latest", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := cl.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var d struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&d); err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(d.TagName), "v")
}

func refreshPinnedUAOnce() {
	if v := fetchNpmOpencodeVersion(); isStableOpencodeVer(v) {
		setPinnedUA(v, "npm")
		return
	}
	if v := fetchGithubOpencodeVersion(); isStableOpencodeVer(v) {
		setPinnedUA(v, "github")
	}
}

func startUARefresh() {
	if uaRefreshRunning.Swap(true) {
		return
	}
	refreshPinnedUAOnce()
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for range t.C {
			refreshPinnedUAOnce()
		}
	}()
}

// leaseUA pins the opencode UA to pinnedUA: the free-tier window binds
// (IP, full-UA); stale versions throttle ~5 req/IP before 429.
func leaseUA(provProvider *provider.Provider, proxy, base string) string {
	// opencode free lane: ALWAYS the pinned CLI UA. The window binds (IP, full
	// UA), and a passthrough client string (OMP's ai-sdk/Bun UA) is a
	// fingerprint the real CLI never sends — one uniform value per pool.
	if provProvider != nil && isOpencodeUpstream(provProvider) {
		return pinnedUA()
	}
	if !strings.HasPrefix(base, "opencode/") {
		return base
	}
	return pinnedUA()
}

// identityLogged guards the one-shot per-session header evidence line: the
// free-tier contract is "one session per egress, both session headers equal,
// request id minted inside that session", and the cheapest way to keep that
// honest in production is to print the outgoing triple once per session.
// authClass never prints the secret: it classifies the outgoing Authorization
// so the log can prove CLI parity (`public`) without leaking a key.
func authClass(v string) string {
	switch {
	case v == "":
		return "none"
	case v == "Bearer public":
		return "public"
	default:
		return "key"
	}
}

var identityLogged sync.Map

func logEgressIdentity(t *PoolTransport, h http.Header, ses string) {
	if ses == "" || t == nil {
		return
	}
	if _, seen := identityLogged.LoadOrStore(ses, struct{}{}); seen {
		return
	}
	sid := h.Get("x-opencode-session-id")
	msg := h.Get("x-opencode-request")
	t.log().Info("pool: egress identity",
		"ses", sesPrefix(ses), "sid", sesPrefix(sid), "sid_match", sid == ses,
		"msg", sesPrefix(msg), "msg_ok", msg != "",
		"auth", authClass(h.Get("Authorization")), "accept", h.Get("Accept"),
		"client", h.Get("x-opencode-client"), "project", h.Get("x-opencode-project"),
		"ua", h.Get("User-Agent"))
}

// stickySession returns the stable x-opencode-session for this egress:
// the pooled genuine TUI session pinned to the egress on first use (not
// round-robined per request), or the stable process fallback when the
// harvest pool is empty.
// stickySession returns the stable x-opencode-session for this egress:
// pinned per egress key (one live TUI id each, spread across the harvest
// pool — not the single first-live id for all 28 backends), stable for the
// process lifetime, re-pinned on burn/expiry. Falls back to the stable
// process session when the harvest pool is empty.
func (p *PoolTransport) stickySession(key string) string {
	ses := getSessionPool().NextFor(key)
	if ses == "" {
		ses = fallbackSessionFor(key)
	}
	return ses
}

func (p *PoolTransport) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// poolEventSink is a process-wide fallback when a PoolTransport has no
// per-instance OnEvent (wired by main.go after BuildTransportOption builds
// the transport — the instance isn't reachable at subscription time).
var poolEventSink func(proxy, action string, ok bool, latencyMS int64, errText string)

// SetPoolEventSink installs the process-wide pool-path event sink
// (proxies_log). Same signature as proxypool.LogFunc.
func SetPoolEventSink(fn func(proxy, action string, ok bool, latencyMS int64, errText string)) {
	poolEventSink = fn
}

// poolEvent reports one pool-path outcome to the GUI feed. Never blocks:
// the store insert runs in the caller's goroutine but is a single-row
// SQLite write (<2ms); panics in foreign sinks are swallowed so a GUI bug
// can never break the request path.
func (p *PoolTransport) poolEvent(proxy, action string, ok bool, ms int64, errText string) {
	fn := p.OnEvent
	if fn == nil {
		fn = poolEventSink
	}
	if fn == nil {
		return
	}
	defer func() { _ = recover() }()
	fn(proxy, action, ok, ms, errText)
}

type poolLeaseResponse struct {
	LeaseID  string `json:"lease_id"`
	ProxyURL string `json:"proxy_url"`
	PublicIP string `json:"public_ip"`
	Country  string `json:"country"`
	// Epoch is the warp-router generation (v2 responses only; v1 omits it,
	// which decodes to 0 and leaves the invalidation logic dormant).
	Epoch uint64 `json:"epoch"`
	// Leases carries the warp-router v2 wrapped shape
	// ({"epoch":6,"leases":[{...}]}); v1 uses the flat shape (fields
	// top-level, Leases empty). Live 2026-10-14: v2/acquire returns
	// wrapped, flat-only decode yields empty LeaseID ("missing lease_id").
	Leases []poolLeaseResponse `json:"leases"`
}

type poolLease struct {
	leaseID  string
	proxy    string // full proxy URL (http://127.0.0.1:8620)
	publicIP string // egress IP as reported by the pool; may repeat across leases (anycast)
	country  string // egress country as reported by the pool (region memory)
	// Egress identity: sticky per-IP upstream headers so opencode keys
	// (IP, full-UA, session) into one stable session instead of seeing
	// identical fingerprints rotate across distinct IPs (bulk flag).
	ua      string // pinned User-Agent for this egress
	session string // sticky x-opencode-session for this egress
}

// poolRetryRT is the per-request RoundTripper. It performs the first attempt
// on the lease handed out by Acquire; on retryable responses it releases the
// current lease, acquires a fresh one and retries. Retryable = 429, 5xx, and
// poolDirectRT is the home-IP direct RoundTripper for the Acquire
// last-resort lease (pool API dead, E 2026-09-11): one direct upstream
// request with pool-path parity (UA pin, provider key, opencode session
// headers). Verdicts feed the home window (BanHomeCooldown/markGoodKey);
// transport errors surface raw. NOT a retry loop — one shot.
type poolDirectRT struct {
	t            *PoolTransport
	prov         string
	provProvider *provider.Provider
}

func (rt *poolDirectRT) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if req.Body != nil {
		req.Body.Close()
	}
	// Preamble injection disabled (MITM matrix 2026-10-24): the free-tier
	// gate now flags the incoherent combo — a system prompt carrying the
	// title-generator preamble ("Never use tools") together with a tools
	// array — as fabricated (403 FreeTierError on every egress), while the
	// same body WITHOUT the preamble passes. The 2026-10-23 recipe is dead.
	if false {
		var doc map[string]any
		if json.Unmarshal(body, &doc) == nil {
			if msgs, ok := doc["messages"].([]any); ok {
				preamble := "You are a title generator. You output ONLY a thread title. Nothing else.\n\n<task>\nGenerate a brief title.\n</task>\n\n<rules>\n- Never use tools\n</rules>"
				found := false
				for _, m := range msgs {
					if mm, ok := m.(map[string]any); ok {
						if r, _ := mm["role"].(string); r == "system" {
							if c, _ := mm["content"].(string); c != "" {
								if !strings.Contains(c, "You are a title generator") {
									mm["content"] = preamble + "\n\n" + c
								}
							} else {
								mm["content"] = preamble
							}
							found = true
							break
						}
					}
				}
				if !found {
					newMsg := map[string]any{"role": "system", "content": preamble}
					doc["messages"] = append([]any{newMsg}, msgs...)
				}
				if nb, err := json.Marshal(doc); err == nil {
					body = nb
				}
			}
		}
	}
	// Preamble injection disabled (MITM matrix 2026-10-24): see the note
	// above — the preamble+tools combo is now a gate-403 signal.
	if false && strings.HasPrefix(strings.TrimSpace(string(body)), "{") {
		var doc map[string]any
		if json.Unmarshal(body, &doc) == nil {
			if msgs, ok := doc["messages"].([]any); ok {
				preamble := "You are a title generator. You output ONLY a thread title. Nothing else.\n\n<task>\nGenerate a brief title.\n</task>\n\n<rules>\n- Never use tools\n</rules>"
				found := false
				for _, m := range msgs {
					if mm, ok := m.(map[string]any); ok {
						if r, _ := mm["role"].(string); r == "system" {
							if c, _ := mm["content"].(string); c != "" {
								if !strings.Contains(c, "You are a title generator") {
									mm["content"] = preamble + "\n\n" + c
								}
							} else {
								mm["content"] = preamble
							}
							found = true
							break
						}
					}
				}
				if !found {
					// prepend system message
					newMsg := map[string]any{"role": "system", "content": preamble}
					doc["messages"] = append([]any{newMsg}, msgs...)
				}
				if nb, err := json.Marshal(doc); err == nil {
					body = nb
				}
			}
		}
	}
	r := cloneRequest(req, body)
	// Провайдер-специфичный UA: home-leg кило тоже не должен притворяться opencode.
	st := stampFor(rt.provProvider)
	if st.ua != "" {
		r.Header.Set("User-Agent", st.ua)
	} else if ua := leaseUA(rt.provProvider, "", r.Header.Get("User-Agent")); ua != "" {
		r.Header.Set("User-Agent", ua)
	}
	if rt.prov != "" {
		if k := rt.t.currentKeyFor(rt.prov); k != "" {
			r.Header.Set("Authorization", "Bearer "+k)
		}
	}
	if rt.provProvider != nil {
		setOpencodeHeaders(r, r.Context(), rt.provProvider)
	}
	// Sticky identity for the home leg too: same session per home-IP, so
	// the home window is one stable session, not a rotating fingerprint.
	if ses := rt.t.stickySession(directHomeIPKey); ses != "" {
		setEgressSession(r.Header, ses)
	}
	// Deadline-clamp like tryDirectFallback: late Acquire-direct with small
	// remaining budget must not wait full 35s headers past gateway cut.
	remaining := 75 * time.Second
	if dl, ok := req.Context().Deadline(); ok {
		if left := time.Until(dl); left < remaining {
			remaining = left
		}
	}
	rht := 35 * time.Second
	if remaining < rht {
		rht = remaining
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = rht
	directClient := &http.Client{Transport: tr, Timeout: remaining}
	resp, err := directClient.Do(r)
	if err != nil {
		rt.t.log().Warn("pool: direct last-resort lease failed", "provider", rt.prov, "err", err)
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		drainClose(resp)
		if hours, ok := rt.t.hoursClassRA(resp.Header.Get("Retry-After")); ok {
			rt.t.markBad429(directHomeIPKey, hours, time.Now())
		} else {
			rt.t.banShort429(directHomeIPKey, shortPark, time.Now())
		}
		return nil, &UpstreamStatusError{Status: http.StatusTooManyRequests, Err: fmt.Errorf("provider %s: direct last-resort rate-limited", rt.prov)}
	}
	if resp.StatusCode >= 500 {
		drainClose(resp)
		rt.t.markBadKey(directHomeIPKey, time.Now())
		return nil, &UpstreamStatusError{Status: resp.StatusCode, Err: fmt.Errorf("provider %s: direct last-resort upstream %d", rt.prov, resp.StatusCode)}
	}
	rt.t.markGoodKey(directHomeIPKey)
	return resp, nil
}

// transport errors. The first non-retryable response wins; if every lease
// fails, the last upstream response is returned as-is (honest status, never
type poolRetryRT struct {
	t *PoolTransport

	mu           sync.Mutex
	cur          *poolLease         // initial lease or latest rotated one
	prov         string             // requesting provider: picks the auth key set
	provProvider *provider.Provider // full provider for AuthMode/ResponsesAPI parity
	model        string             // requested model: region memory is model-scoped
	// stamp — upstream-идентичность лизов (UA/session) этого провайдера:
	// не-opencode апстрим не делит окно/bans с opencode на общих /48.
	stamp leaseStamp
}

func (p *PoolTransport) Name() string { return "pool" }

// SelfRotating reports that poolRetryRT already rotates leases internally
// (fanout×rounds), so the router must not add a second external attempt that
// re-acquires the same backend and trips the repeated-lease check.
func (p *PoolTransport) SelfRotating() bool { return true }

func (p *PoolTransport) fanoutN() int {
	if p.Fanout > 0 {
		return p.Fanout
	}
	return 5
}

func (p *PoolTransport) roundsN() int {
	if p.Rounds > 0 {
		return p.Rounds
	}
	return 2
}

func (p *PoolTransport) cooldownN() time.Duration {
	if p.Cooldown > 0 {
		return p.Cooldown
	}
	return 60 * time.Second
}

// ipKey is the cooldown identity of a lease. WARP IPv4 egress is a shared
// Cloudflare anycast address (e.g. 104.28.203.249) repeated by every IPv4
// backend, so keying cooldown by proxy URL would waste attempts on the same
// upstream limit. Keying by public IP collapses all backends behind one
// anycast address into a single cooldown entry. For IPv6 we key by the exact
// egress IP — /48 collapsing burns 11 prefixes in ~11 requests and strands
// the pool for 8h (pool_state 11/12 prefix-banned). Per-exact-IP keeps the
// 20 backends distinct and lets Retry-After/60m reclaim work.
func (p *PoolTransport) ipKey(proxy, publicIP string) string {
	if publicIP != "" {
		return strings.TrimSpace(publicIP)
	}
	if u, err := url.Parse(proxy); err == nil && u.Host != "" {
		host := u.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		return strings.TrimSpace(host)
	}
	return strings.TrimSpace(proxy)
}

// v48 returns the /48 prefix of a v6 textual address. Kept for diagnostics
// and warp pool anycast grouping; ipKey no longer collapses to it.
func v48(ip string) string {
	ip = strings.TrimSpace(ip)
	if strings.Contains(ip, ".") && !strings.Contains(ip, ":") {
		return ip // pure v4 — leave as exact IP
	}
	if addr, err := netip.ParseAddr(ip); err == nil && addr.Is6() {
		b := addr.As16()
		// first 48 bits = 3 hextets
		return fmt.Sprintf("%x:%x:%x:", binary.BigEndian.Uint16(b[0:2]), binary.BigEndian.Uint16(b[2:4]), binary.BigEndian.Uint16(b[4:6]))
	}
	parts := strings.Split(ip, ":")
	if len(parts) < 3 {
		return ip
	}
	p := strings.Join(parts[:3], ":")
	if !strings.HasSuffix(p, ":") {
		p += ":"
	}
	return p
}

// triedKey is the RoundTrip dedup identity: the /48 budget group, not the
// exact egress IP. Warp runs 20 backends on ~9 distinct /48s and its
// LRU hands back same-group backends as "fresh" leases; exact-IP counting
// then mistakes same-budget repeats for fresh probes and inflates tried
// ~2.2x. Pure v4 passes through (anycast-collapsed already at ipKey).
func triedKey(ip string) string {
	return v48(ip)
}

// Exact-RA bans (user 2026-09-16): NO blanket cap — an egress stays out
// of rotation for exactly the Retry-After seconds its 429 named. Previous
// 8h cap (+60s buffer) either stranded live windows for hours or cut real
// burns short. Per-exact-IP keeps the 20 backends distinct (per-/48
// stranded 11 prefixes out of 20 back in 2026-09-02).

// shortPark parks a transient-throttle egress briefly (no hard flag, no
// persist): long enough to let the second-scale window reopen, short
// enough to rejoin rotation fast. 5s (was 30s): окно реально
// переоткрывается за ~5с (замер 2026-09-07, юзер подтвердил), park НЕ бан —
// acquire всё равно выдаёт запаркованные, guard решает. Серверный RA
// (markBadSoft/markBad429) НЕ тронут — именованный дедлайн ждём целиком.
// 2026-09-07: opencode omits Retry-After on Console-relayed 429s (shared
// downstream limit, not a per-IP window burn).
//
// globalClusterWindow bounds the detector: two hours-class Retry-Afters on
// DISTINCT egresses inside this window with deadlines within globalRADEps
// prove a shared budget (live probe 2026-09-08: RA 35392/35388/35375/35371
// across burned+fresh IPs and big+small prompts — one deadline ±20s).
// globalBurnCap bounds the backoff so a pathological RA can't silence the
// pool indefinitely; globalMinRA ignores sub-hour noise.
const globalClusterWindow = 3 * time.Minute
const globalRADEps = 120 * time.Second

// globalProbationWindow: сколько живёт «голос за переоткрытие» общего окна.
// 1 минута (до 2026-10-21) не набирала два 200 из РАЗНЫХ /48 под нагрузкой:
// при живом латче пул ходит 1x1 и трафик прибит к одной группе (region pin),
// поэтому окно истекало раньше второго голоса, а латч висел ровно на RA
// апстрима (77-95 мин; 2026-10-21: 302x503 подряд при bad={}/hardBad={}).
// 5 минут = 2-3 пробы разными egress успевают дать второй голос.
const globalProbationWindow = 5 * time.Minute

// Wave 2026-09-20: 60s убивал unban deadline-sweep (окно now+60s±120s
// никогда не накрывало часовые дедлайны) — флаг гас, тени висели до expiry,
// пила arm/ban по 4-8 мин. 8h = весь burn под флагом: 1x1 + skip rotate.
const globalBurnCap = 8 * time.Hour

// globalRAObs is one hours-class Retry-After sighting on one egress.
type globalRAObs struct {
	at time.Time // when observed
	ip string    // egress key
	ra int       // Retry-After seconds
}

// parseRASeconds parses Retry-After (delay-seconds or HTTP-date) into
// seconds. ok=false when missing/unparseable — caller treats that as
// transient, never global.
func parseRASeconds(retryAfter string) (secs int, ok bool) {
	s := strings.TrimSpace(retryAfter)
	if s == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n, true
	}
	if t, err := http.ParseTime(s); err == nil {
		d := int(time.Until(t).Seconds())
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// noteHoursRA records an hours-class sighting and reports whether the
// global burn is (now) active. Two sightings on DISTINCT IPs within
// globalClusterWindow whose RAs agree within globalRADEps arm the global
// backoff at min(RA)+60s buffer (capped): one shared deadline, no per-IP
// bans from here on. Same-IP repeats never arm it (that's a per-IP burn).
// Must hold no locks (takes muMu itself).
func (p *PoolTransport) noteHoursRA(ip string, raSecs int) (global bool) {
	now := time.Now()
	_ = now
	p.muMu.Lock()
	defer p.muMu.Unlock()
	if p.globalObs == nil {
		p.globalObs = make([]globalRAObs, 0, 8)
	}
	// Prune outside the cluster window; bound memory.
	kept := p.globalObs[:0]
	for _, o := range p.globalObs {
		if now.Sub(o.at) <= globalClusterWindow {
			kept = append(kept, o)
		}
	}
	p.globalObs = kept
	p.globalObs = append(p.globalObs, globalRAObs{at: now, ip: ip, ra: raSecs})
	if len(p.globalObs) > 16 {
		p.globalObs = p.globalObs[len(p.globalObs)-16:]
	}
	// Cluster: any DISTINCT-budget pair agreeing within eps?
	// Budgets are /48 groups, not exact IPs: two egresses of one /48
	// share a single window — their agreement proves nothing shared
	// (2026-09-09 no-VPN: 48ef+48ef armed a 1h47m global on one budget).
	best := 0
	for i := range p.globalObs {
		for j := i + 1; j < len(p.globalObs); j++ {
			a, b := p.globalObs[i], p.globalObs[j]
			if a.ip == b.ip || a.ip == "" || b.ip == "" {
				continue
			}
			if triedKey(a.ip) != "" && triedKey(a.ip) == triedKey(b.ip) {
				continue
			}
			d := a.ra - b.ra
			if d < 0 {
				d = -d
			}
			if d <= int(globalRADEps.Seconds()) {
				m := a.ra
				if b.ra < m {
					m = b.ra
				}
				if best == 0 || m < best {
					best = m
				}
			}
		}
	}
	if best > 0 {
		// Arm once: never extend a live flag. Every clustered hit during
		// a storm carries the same RA and would otherwise push the
		// deadline forward forever (self-sustaining global, 2026-09-08).
		if p.globalBurnUntil.IsZero() || !now.Before(p.globalBurnUntil) {
			cd := time.Duration(best)*time.Second + 60*time.Second
			if cd > globalBurnCap {
				cd = globalBurnCap
			}
			p.globalBurnUntil = now.Add(cd)
			p.globalRA = best
			// Голос, собранный ДО этой арм-точки, не доказывает переоткрытие
			// нового окна: без сброса один 200 из другой /48 в пределах
			// globalProbationWindow снимал СВЕЖИЙ латч (пила снять→сжечь→сесть,
			// ревью 2026-10-21).
			p.globalOKAt = time.Time{}
			p.globalOKGroup = ""
			// Arming was silent: next-storm triage could not tell which
			// pair armed, what deadline was agreed, arm-once vs active.
			p.globalGen = p.identGen.Load()
			p.globalArmedAt = now
			p.log().Warn("pool: global burn ARMED",
				"ip", ip, "ra", raSecs, "agreed_ra", best,
				"until", p.globalBurnUntil.Format(time.RFC3339), "obs", len(p.globalObs))
		}
		return true
	}
	return !p.globalBurnUntil.IsZero() && now.Before(p.globalBurnUntil)
}

// globalActive reports whether the shared-budget backoff is live. A flag armed
// against a retired identity generation is dead on arrival: the deadline
// described egresses that no longer exist.
func (p *PoolTransport) globalActive() bool {
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	if p.globalGen != 0 && p.globalGen != p.identGen.Load() {
		return false
	}
	return !p.globalBurnUntil.IsZero() && time.Now().Before(p.globalBurnUntil)
}

// globalStatus returns the backoff deadline + earliest RA for logs/headers.
func (p *PoolTransport) globalStatus() (until time.Time, ra int) {
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	return p.globalBurnUntil, p.globalRA
}

// minHoursBan: порог hours-класса Retry-After. Наблюдаемые классы: 349с
// минутные против 3324с часовых — порог между ними.
const minHoursBan = 30 * time.Minute
const shortPark = 5 * time.Second

// rotateClientTimeout: warp-router's rotate blocks until every worker is
// respawned (1-2 min). The old 10s client timeout reported success as
// "context deadline exceeded" on all 14 rotates of 2026-10-18 while the
// server kept respawning the pool.
const rotateClientTimeout = 180 * time.Second

// Already-spent egress handling (live forensics 2026-10-18): the pool hands
// out tunnel addresses whose upstream budget is gone — ipOK (successes in the
// current window, read at the 429) had a median of 2 across the pool, with 33
// addresses burning after exactly one 200, while genuinely fresh addresses
// served 200-600. A 5s park lets such an address rejoin rotation 120 times an
// hour; every request that samples it spends a probe and (in 1x1 storm mode)
// dies as a client-visible 503. Rate-shaped windows (the address served a lot
// before the 429) keep the 5s park: its window really may reopen in seconds.
const (
	spentOKFloor = 2
	spentPark    = 10 * time.Minute
	// Escalation for a window that keeps throttling: the pool's spent
	// addresses were re-probed every 5s (heat ring holds 3 min of hits), so
	// an address that burned 3+ times inside the ring is not a rate spike —
	// its budget is gone for a while. Escalating parks push it out without
	// banning a genuinely rate-shaped window on its first 429.
	burnParkFirst = 30 * time.Second
	burnParkMid   = 2 * time.Minute
)

// transientCooldown банит НЕ-window вердикты (транспорт-ошибка, anon-401,
// 5xx, silent): это не выгоревшее окно, а «отдохни». Было cooldownN=60с —
// при темпе 40/мин все 20 egress за минуты в 60с банах одновременно,
// свапать не на что. Окно реально переоткрывается за ~5с (замер
// 2026-09-07), серверный RA (markBadSoft/markBad429) НЕ тронут.
const transientCooldown = 5 * time.Second

// spreadWindow/spreadCandidates steer new requests across the pool: an
// egress handed out within the window is avoided when a colder backend is
// available, and among up to spreadCandidates fresh leases the
// least-recently-used wins. Sequential same-IP traffic is cheap (the
// free-tier window serves ~500/IP), so the window only steers bursts apart.
const spreadWindow = 5 * time.Second
const spreadCandidates = 3

// hoursClassRA turns an upstream Retry-After into a ban cooldown when it
// carries an hours-class window burn (RA >= minHoursBan). The cooldown is
// EXACTLY the named window (user 2026-09-16: no 8h cap, no +60s buffer).
// Anything shorter (or missing/unparseable) is a transient throttle: no ban.
// RFC 7231 §7.1.3: delay-seconds ("3324") or HTTP-date.
func (p *PoolTransport) hoursClassRA(retryAfter string) (time.Duration, bool) {
	s := strings.TrimSpace(retryAfter)
	if s == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(s); err == nil {
		d = time.Until(t)
		if d < 0 {
			d = 0
		}
	} else {
		return 0, false
	}
	if d < minHoursBan {
		return 0, false
	}
	return d, true
}

// Ban-классы по Retry-After (2026-09-16: детектим время из ошибки и
// баним РОВНО на него, а не фиксированные 30с/60с/8ч):
// - hours (>= minHoursBan): markBad429, hard-флаг, персист — окно
//   выгорело на часы, IP ждёт свой дедлайн целиком;
// - minute (1с..minHoursBan): markBadSoft ровно на RA-секунды, без
//   hard-флага, без персиста — окно переоткроется через минуты;
// - second (< 1с после округления, т.е. RA=0): короткий парк —
//   это джиттер/таймаут от частых запросов, а не выгоревший бюджет;
// - нет RA вообще: парк + heat (см. ветку no-RA ниже) — таймаут от
//   частых запросов, окно живо, банить нечего.
// minHoursBan: порог hours-класса. Наблюдаемые классы: 349с минутные
// против 3324с часовых — порог между ними. Минутные и короче — это
// секундные троттлы (окно переоткрывается за ~10с, замер 2026-09-07),
// банить на них — хоронить живые окна и гнать трафик на выживших.
// Секундный 429 (RA 0-5с или пустой при частых запросах) — это НЕ
// выгоревший бюджет, а таймаут/шейпер: банится только коротким парком,
// иначе пул после долгой работы уходит в вечный 429 — все egress
// по очереди получают длинные баны и некому свапать (2026-09-16).
// Диагностика застревания: pool_state.json bad растёт до ~числа
// бэкендов, heat/top по всем ключам, 200/0 мин — значит баны длиннее
// реальных окон: смотреть retry_after в логах против ban-длительностей.
// ЗАСТРЕВАНИЕ 429 НАВСЕГДА (2026-09-16, разбор): после долгой работы
// под нагрузкой пул перестаёт свапать и каждый запрос кончается 429.
// Корни (все три чинены здесь):
// 1) Секундные 429 без RA получали markBadKey(cooldownN=60с): 60с бан
//    на НЕвыгоревшее окно. При темпе N req/min все 20 egress за минуты
//    оказываются в 60с банах одновременно — свапать не на что, каждый
//    новый 429 продлевает баны (MIN-дедлайн только укорачивает один
//    ключ, но heat/парки держат остальные) → вечный 429.
//    Фикс: no-RA 429 = только shortPark(30с) + heat, БЕЗ markBadKey.
//    Окно живо — следующий свип его переиспользует.
// 2) Минутные RA (60-349с) получали markBadSoft(ровно RA) + acquire
//    их скипал (isBad) — при массовом 429 все egress в минутных банах,
//    acquire не находит cold (no cold lease), запрос умирает не
//    дождавшись переоткрытия, хотя окна откроются через минуту.
//    Фикс: ре-свип до дедлайна (внешний цикл выше) — запрос ждёт
//    переоткрытия внутри провайдера вместо 429 клиенту.
// 3) Storm/global 1x1 + heat-кап: при шторме effFanout=1 и каждый
//    запрос делает РОВНО 1 прогрев; heat>=heatMaxHits уводит все egress в hot,
//    acquire отдаёт hot-как-fresh, прогрев 429-ит снова → цикл.
//    Фикс: heat НЕ банит (только деприоритизация в hot-очередь), ре-свип
//    ждёт пока парки 5с (shortPark) дадут переоткрытие (heatWindow 3м остывает сам).
// Итог: долгие баны — ТОЛЬКО по явному времени из ошибки (RA >= 1с);
// всё остальное — парки/heat, которые истекают сами.
// (Старый комментарий про порог сохранён выше по смыслу.)

// pickSpread returns the index (into cands) of the least-recently-handed-out
// fresh lease, preferring ones NOT handed out within spreadWindow so burst
// traffic spreads across the pool. Returns 0 with no info. Ties keep order.
// Holds RLock for the whole pick: usedAt is written concurrently by markUsed.
// При MaxEgressInflight > 0 занятые сверх предела egress уступают свободным
// (мягкий per-IP cap): выбор остаётся LRU-спредом, но не отдаёт запрос тому
// IP, через который уже идёт предельное число параллельных.
func (p *PoolTransport) pickSpread(cands []*poolLease) int {
	if len(cands) == 0 {
		return 0
	}
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	now := time.Now()
	maxInf := p.MaxEgressInflight
	free := false
	if maxInf > 0 {
		for _, l := range cands {
			if p.inflightLocked(p.ipKey(l.proxy, l.publicIP)) < maxInf {
				free = true
				break
			}
		}
	}
	// When every candidate is at the inflight cap the soft filter used to be
	// dropped entirely, so a 10-request herd all landed on the LRU egress —
	// exactly the burst that trips upstream 429s. Instead the inflight count
	// becomes the PRIMARY criterion (fewest in flight wins, then cold/LRU), so
	// the pick stays correct even when a concurrent release lowers the minimum
	// between the two passes (review B8).
	best := -1
	bestInf := 1 << 30
	bestCold := false
	var bestAge time.Duration = -1
	for i, l := range cands {
		key := p.ipKey(l.proxy, l.publicIP)
		if maxInf > 0 && free && p.inflightLocked(key) >= maxInf {
			continue
		}
		inf := 0
		if maxInf > 0 && !free {
			inf = p.inflightLocked(key)
			if inf > bestInf {
				continue
			}
		}
		var age time.Duration
		if key == "" {
			age = time.Duration(1 << 62)
		} else if t, ok := p.usedAt[key]; !ok {
			age = time.Duration(1 << 62)
		} else {
			age = now.Sub(t)
		}
		cold := age >= spreadWindow
		if best < 0 || inf < bestInf || (inf == bestInf && ((cold && !bestCold) || (cold == bestCold && age > bestAge))) {
			best, bestCold, bestAge, bestInf = i, cold, age, inf
		}
	}
	if best < 0 {
		best = 0
	}
	return best
}

// usedWithin reports whether the egress was handed out within d.
func (p *PoolTransport) usedWithin(key string, d time.Duration) bool {
	if key == "" {
		return false
	}
	p.muMu.RLock()
	t, ok := p.usedAt[key]
	p.muMu.RUnlock()
	return ok && time.Since(t) < d
}

// markUsed records a hand-out for spread rotation. The map is bounded:
// warp rotates identities over days, so cold entries are pruned past 256.
// lastUsedAt reports when the egress last served a request (zero time = never).
func (p *PoolTransport) lastUsedAt(key string) time.Time {
	if key == "" {
		return time.Time{}
	}
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	return p.usedAt[key]
}

func (p *PoolTransport) markUsed(key string) {
	if key == "" {
		return
	}
	now := time.Now()
	p.muMu.Lock()
	if p.usedAt == nil {
		p.usedAt = map[string]time.Time{}
	}
	p.usedAt[key] = now
	if len(p.usedAt) > 256 {
		for k, t := range p.usedAt {
			if now.Sub(t) > time.Hour {
				delete(p.usedAt, k)
			}
		}
		if len(p.usedAt) > 256 {
			p.usedAt = map[string]time.Time{key: now}
		}
	}
	p.muMu.Unlock()
}

// markBadSoft records a minute-class window burn (Retry-After below
// minHoursBan): the upstream named a real cooldown, so the egress stays
// out of rotation for exactly that long — but WITHOUT the hardBad flag
// and WITHOUT disk persist (the window reopens in ~minutes). A 200 lifts
// it via markGood. Memory-only, like the park, but longer than shortPark
// (60s vs 5s): RA60 parks expired mid-cycle and the same budgets were
// re-warmed every ~2s (2026-09-14 relapse). Callers MUST gate on
// sub-hours RA first.
func (p *PoolTransport) markBadSoft(key string, cd time.Duration, now time.Time) {
	if key == "" || cd <= 0 {
		return
	}
	p.muMu.Lock()
	if p.bad == nil {
		p.bad = map[string]time.Time{}
	}
	if until, ok := p.bad[key]; !ok || now.Add(cd).Before(until) {
		if !p.hardBad[key] {
			p.bad[key] = now.Add(cd)
		}
	}
	p.muMu.Unlock()
	p.log().Warn("pool: mark minute-ban (no persist, no hard)",
		"ip", key, "prefix", v48(key), "cooldown", cd.String())
}

// markBad429 records an hours-class window burn (Retry-After >= minHoursBan).
// Callers MUST gate on hoursClassRA first — this function unconditionally
// bans and persists. Minute-class/missing 429s go through markBadSoft,
// banShort429 or plain rotation, never here.
func (p *PoolTransport) markBad429(key string, cd time.Duration, now time.Time) {
	if key == "" {
		return
	}
	// Full-RA per-IP bans stay (user directive 2026-10-25, confirmed by
	// measurement): the free lane's exhaustion answers home, warp, fresh
	// global proxies and genuine TUI sessions with the IDENTICAL
	// Retry-After until 00:00 UTC — the ban horizon is the server's own
	// honest verdict, and shortening it only generates probe-429 noise.
	p.muMu.Lock()
	if p.bad == nil {
		p.bad = map[string]time.Time{}
	}
	// Min-deadline: the FIRST sighting of a window is the true deadline.
	// Later 429s on the same IP carry the same RA +-20s (cluster jitter)
	// and must NOT push the deadline forward - overwrite turned every
	// re-hit into +minutes and made fresh burns look endless.
	if until, ok := p.bad[key]; !ok || now.Add(cd).Before(until) {
		p.bad[key] = now.Add(cd)
	}
	// Hard ban per exact IP: ANY hours-class window burn keeps the IP out until its
	// own deadline regardless of later 200s from other backends (or a stray 200 on
	// itself — anycast shadow / half-open noise, not a reopened window). The flag no
	// used to depend on reaching the old 8h cap: a 1h burn (3324s RA)
	// without the flag was lifted by the first stray 200 and shortened
	// to 60s by any soft markBadKey — the pool re-warmed a dead window
	// every minute (2026-09-12 evening storm).
	if p.hardBad == nil {
		p.hardBad = map[string]bool{}
	}
	p.hardBad[key] = true
	// Fresh window accounting: ipOK counts successes in the CURRENT window.
	// Reset on ban so ok_before shows what THIS window served, not the
	// lifetime total (which lied after a long-lived IP burned).
	okBefore := p.ipOK[key]
	if p.ipOK == nil {
		p.ipOK = map[string]int{}
	}
	p.ipOK[key] = 0
	wasProbed := p.ctrlProbed[key]
	totalBad := len(p.bad)
	totalHard := len(p.hardBad)
	p.muMu.Unlock()
	// Identity burned for hours: unpin its session so the egress never returns
	// carrying the id of a banned identity (short parks keep their pin — the
	// upstream window, not the identity, is what closed).
	if cd >= 30*time.Minute {
		getSessionPool().ReleaseEgress(key)
	}
	// Detailed 429 ban log: exact IP + its /48 prefix + history + pool pressure.
	p.log().Warn("pool: mark hours-ban",
		"ip", key, "prefix", v48(key), "ok_before", okBefore,
		"control_probed", wasProbed, "cooldown", cd.String(),
		"hard", true, "total_bad", totalBad, "hard_bad", totalHard)
	// The window that just closed served okBefore requests: feed the size
	// back into the learned per-egress budget (pool_diag.go).
	p.noteWindowSample(okBefore)
	p.saveState()
}

// unbanClusteredHours clears the per-IP hours-class bans that share the
// global window: any hard ban whose deadline falls within globalBurnUntil
// +- eps is a shadow of the same shared budget — banning it is pure loss.
// Older per-IP burns with an EARLIER deadline (real isolated burns from
// before the cluster) stay. Short parks always drop (15s noise vs hours
// of backoff). Members armed via globalObs are always cleared even if
// their deadline drifted outside the window (clock/RA jitter).
func (p *PoolTransport) unbanClusteredHours(key string) (cleared int) {
	p.muMu.Lock()
	if p.globalBurnUntil.IsZero() {
		p.muMu.Unlock()
		return 0
	}
	members := map[string]bool{key: true}
	eps := int(globalRADEps.Seconds())
	for _, o := range p.globalObs {
		d := o.ra - p.globalRA
		if d < 0 {
			d = -d
		}
		if d <= eps {
			members[o.ip] = true
		}
	}
	lo := p.globalBurnUntil.Add(-time.Duration(eps) * time.Second)
	hi := p.globalBurnUntil.Add(time.Duration(eps) * time.Second)
	memberHit := false
	for m := range members {
		if _, ok := p.bad[m]; ok {
			memberHit = true
			delete(p.bad, m)
			delete(p.hardBad, m)
			cleared++
		}
	}
	// Park is the ONLY guard against certain-429 re-warmups: wiping it
	// on every shadows-hit re-arms the park->serve->429 engine under the
	// global flag (2026-09-09 no-VPN: cleared=1 every hit, same egress
	// re-warmed for the whole storm). Clear parks only when the arm just
	// banned real members (first arm), never on steady-state shadows.
	// (memberHit captured BEFORE the delete above: testing after removal
	// under the same lock is deterministically false — review 2026-09-09.)
	if memberHit {
		for k := range p.park {
			delete(p.park, k)
			cleared++
		}
	}
	for k, dl := range p.bad {
		if !p.hardBad[k] {
			if time.Until(dl) <= shortPark+5*time.Second {
				delete(p.bad, k)
				cleared++
			}
			continue
		}
		if members[k] {
			continue // already cleared above
		}
		if (dl.After(lo) || dl.Equal(lo)) && (dl.Before(hi) || dl.Equal(hi)) {
			delete(p.bad, k)
			delete(p.hardBad, k)
			cleared++
		}
	}
	needSave := cleared > 0
	// NOTE: saveState takes muMu.RLock — NEVER call it under our write
	// Lock (Go RWMutex deadlocks RLock while writer holds). Save after
	// unlock below. Unban mutates persisted maps: without save a restart
	// resurrects the cleared shadows (global is memory-only, load brings
	// them back for full 8h — review 2026-09-09).
	p.muMu.Unlock()
	if needSave {
		p.saveState()
	}
	return cleared
}

// heavyPromptTokens is the double-note threshold: a 429 on a prompt this
// large burns token budget ~100x per hit, so storm (20/4m) and heat (2/3m)
// arm at flood speed. len/4 heuristic: 200k tok ~= 800KB, 500k ~= 2MB.
const heavyPromptTokens = 200000

// dataURLRe matches embedded base64 image payloads (responses shim passes
// image_url data: URLs verbatim): ~1.3MB chars per photo score as 300-500k
// "tokens" under len/4 while real vision cost is ~1-2k/image (100-500x
// overcount) — every photo request false-tripped heavy, and one heavy 429
// alone instant-hot-queued the egress (heat 2/2). Stripped before /4.
var dataURLRe = regexp.MustCompile(`data:image/[^;]+;base64,[A-Za-z0-9+/=\n\r]+`)

// estPromptTokens scores prompt weight for the storm/heat amplifier: text
// len/4 (standard EN heuristic) + flat 1500 per embedded image (real vision
// cost). Cyrillic undercount (~2x) deliberately left: conservative, fewer
// false bans on the workload this repo serves.
func estPromptTokens(body []byte) int {
	locs := dataURLRe.FindAllIndex(body, -1)
	base64Len := 0
	for _, l := range locs {
		base64Len += l[1] - l[0]
	}
	return (len(body)-base64Len)/4 + len(locs)*1500
}

// noteThrottleHit is the single choke point for 429 counting: heavy prompts
// count 2x (storm/heat arm faster). Recovery is symmetric (noteStormSuccess):
// one heavy 200 lifts, mirroring the 2x arm — light still needs 2 oks
func (rt *poolRetryRT) noteThrottleHit(key string, body []byte, now time.Time) {
	// A 429 disproves "recently live" for THIS egress (2026-10-22): the hours-class
	// path parks it for seconds only, so without invalidation pickLease kept handing
	// out egresses whose okAt was minutes old and burned a probe each time.
	rt.t.clearOKAt(key)
	rt.t.noteHeat(key, now)
	rt.t.noteStorm429Key(key, now)
	// Heavy prompts still double-note storm (flood-speed arm) but NOT heat:
	// one heavy 429 was instant-hot (heat 2/2) and heat-halve never cools it
	// back (map 2->1, next heavy +2->4->count 2). Storm keeps its 2x arm.
	if estPromptTokens(body) >= heavyPromptTokens {
		rt.t.noteStorm429Key(key, now)
	}
}

// noteStormSuccess mirrors noteThrottleHit on the 200 path: heavy 200s
// double-note storm recovery (one heavy 200 lifts the ok>=2 gate), light
// 200s single-note (fluke protection preserved). Heat already recovers in
// 1 ok via the heatGood halve; storm had no equivalent and stuck one extra
// 1x1 request past real recovery (more on probe-miss).
func (rt *poolRetryRT) noteStormSuccess(body []byte, now time.Time) {
	rt.noteStormSuccessKey("", body, now)
}

// noteStormSuccessKey: тот же 200 с привязкой к egress (per-egress лифт).
func (rt *poolRetryRT) noteStormSuccessKey(key string, body []byte, now time.Time) {
	rt.t.noteStormOKKey(key, now)
	if estPromptTokens(body) >= heavyPromptTokens {
		rt.t.noteStormOKKey(key, now)
	}
}

// heatWindow is the sliding window for per-egress 429 counting: hits inside it
// prove the budget is hot even when upstream names no Retry-After (2026-09-12
// evening storm: 600+ no-RA 429s in 14 min left zero trace). heatMaxHits inside
// the window deprioritizes the egress at acquire (tail of the queue, not a ban).
// A 200 forgives only pre-200 hits (heatGood timestamp); post-200 429s stay
// visible immediately (F2: full-clear masked them till window expiry).
// Memory-only, never
// persisted: after a restart the detector re-learns within a few 429s.
// heatMaxHits=2 (2026-09-16: при темпе 40/мин heat=3 за 10м набирают все 20
// egress одновременно — кап перестаёт различать; 2 срабатывает раньше,
// пока есть холодные для свапа). Window 3m (wave1 heat P1: 10m в 20x дольше
// 30s-парков — давно живые egress вечно hot; 3m покрывает ~2 reopen-цикла).
const heatWindow = 3 * time.Minute
const heatMaxHits = 2

// noteHeat records one 429 hit for the egress key (any class, incl. no-RA).
// Must hold no locks (takes muMu itself).
func (p *PoolTransport) noteHeat(key string, now time.Time) {
	if key == "" {
		return
	}
	cutoff := now.Add(-heatWindow).UnixNano()
	p.muMu.Lock()
	if p.heat == nil {
		p.heat = map[string][]int64{}
	}
	hits := append(p.heat[key], now.UnixNano())
	kept := hits[:0]
	for _, h := range hits {
		if h >= cutoff {
			kept = append(kept, h)
		}
	}
	p.heat[key] = kept
	p.muMu.Unlock()
}

// heatCount returns live 429 hits for the key inside heatWindow newer than the
// last 200 (heatGood): a 200 forgives only what came before it, post-200 429s
// count immediately (non-hard path; hard bans keep their deadline via
// unbanClusteredHours/expiry). Must hold no locks.
func (p *PoolTransport) heatCount(key string, now time.Time) int {
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	cutoff := now.Add(-heatWindow).UnixNano()
	good, hasGood := p.heatGood[key]
	n := 0
	for _, h := range p.heat[key] {
		if h < cutoff {
			continue
		}
		// F2: 200 прощает только хиты ДО него; пост-200 429 видны сразу.
		// Старое good>=oldest → 0 маскировало новые 429 до expiry среза
		// (429,429,200,429 давал 0 вместо 1).
		if hasGood && h <= good {
			continue
		}
		n++
	}
	return n
}

// heatCool records a 200 for fast-cool accounting. Must hold no locks.
func (p *PoolTransport) heatCool(key string, now time.Time) {
	if key == "" {
		return
	}
	p.muMu.Lock()
	if p.heatGood == nil {
		p.heatGood = map[string]int64{}
	}
	p.heatGood[key] = now.UnixNano()
	p.muMu.Unlock()
}

// regionBadTTL bounds per-model region memory: upstream region support
// shifts as clusters rebalance, so a rejection older than this is
// re-probed instead of skipped forever. Long enough that consecutive
// requests skip discovery (the common 1.3 case).
const regionBadTTL = 10 * time.Minute

// noteRegionBad remembers that model is not served in this egress' region.
// Model-scoped only: the egress stays fully usable for every other model
// (no cooldown/ban — region support is per (model, region), not a burned
// window). Must hold no locks (takes muMu itself).
func (p *PoolTransport) noteRegionBad(model, key string, now time.Time) {
	if model == "" || key == "" {
		return
	}
	p.muMu.Lock()
	if p.regionBad == nil {
		p.regionBad = map[string]map[string]time.Time{}
	}
	m := p.regionBad[model]
	if m == nil {
		m = map[string]time.Time{}
		p.regionBad[model] = m
	}
	m[key] = now.Add(regionBadTTL)
	p.muMu.Unlock()
}

// noteRegionGood pins the egress that served model after a region swap so
// the next request goes straight there. Clears that egress' bad entry for
// the model. Must hold no locks.
func (p *PoolTransport) noteRegionGood(model, key string) {
	if model == "" || key == "" {
		return
	}
	p.muMu.Lock()
	if p.regionGood == nil {
		p.regionGood = map[string]string{}
	}
	p.regionGood[model] = key
	if m := p.regionBad[model]; m != nil {
		delete(m, key)
		if len(m) == 0 {
			delete(p.regionBad, model)
		}
	}
	p.muMu.Unlock()
}

// regionGoodFor returns the pinned egress for model, if any. Must hold no locks.
func (p *PoolTransport) regionGoodFor(model string) (string, bool) {
	if model == "" {
		return "", false
	}
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	g, ok := p.regionGood[model]
	return g, ok
}

// isRegionBad reports whether model was recently region-rejected on key.
// Expired entries drop lazily. Must hold no locks.
func (p *PoolTransport) isRegionBad(model, key string, now time.Time) bool {
	if model == "" || key == "" {
		return false
	}
	p.muMu.Lock()
	defer p.muMu.Unlock()
	m := p.regionBad[model]
	until, ok := m[key]
	if !ok {
		return false
	}
	if now.Before(until) {
		return true
	}
	delete(m, key)
	if len(m) == 0 {
		delete(p.regionBad, model)
	}
	return false
}

// stormWindow bounds the pool-wide storm self-check: 429 hits inside it prove
// saturation even when upstream names no Retry-After (2026-09-13 midday:
// 1800+ no-RA 429s in 17 min, zero parks expired into action, heat capped
// recovery (ok-rate >= 50%) arms the self-throttle: fanout collapses to 1x1.
// 20 (was 60): 60 never fired during real storms — a single fluke-200
// interleaved in the flood disarmed fail-fast (2026-09-14 day: 87 RA60 in
// 7 min with ok interleaved, pit 0/87 only after full saturation).
const stormWindow = 4 * time.Minute

// Wave 2026-09-20: 20 за 4м при темпе 15/мин срабатывал через 80+с —
// весь разогрев шёл полным 5x2 в горячий пул. 12 включает 1x1 раньше,
// single-IP гейт ниже всё равно отделяет per-IP завал от shared.
const stormMax429 = 12

// noteStorm429 records one pool 429 hit (any egress) for the pool-wide storm
// self-check. Must hold no locks (takes muMu itself).
func (p *PoolTransport) noteStorm429(now time.Time) {
	p.noteStorm429Key("", now)
}

// stormHit — один 429-хит шторма с привязкой к egress: ключ нужен, чтобы
// отличить per-IP завал (один IP сыпет, остальные живут — баним только его)
// от shared-budget шторма (разные IP сыпят одни и те же RA — fail-fast 1x1).
type stormHit struct {
	ts  int64
	key string
}

func (p *PoolTransport) noteStorm429Key(key string, now time.Time) {
	cutoff := now.Add(-stormWindow).UnixNano()
	p.muMu.Lock()
	hits := append(p.storm429, now.UnixNano())
	kept := hits[:0]
	for _, h := range hits {
		if h >= cutoff {
			kept = append(kept, h)
		}
	}
	p.storm429 = kept
	kh := append(p.stormKeys, stormHit{ts: now.UnixNano(), key: key})
	kkept := kh[:0]
	for _, h := range kh {
		if h.ts >= cutoff {
			kkept = append(kkept, h)
		}
	}
	p.stormKeys = kkept
	p.muMu.Unlock()
}

// noteStormOK records one pool 200 for the storm self-check. The throttle
// lifts only on sustained recovery (2+ ok inside the window): a single
// fluke-200 interleaved in a 429 flood must not disarm fail-fast (wave1
// heat P1: ok-trickle reset the zero-200 gate while 87 RA60 flooded past).
func (p *PoolTransport) noteStormOK(now time.Time) {
	p.noteStormOKKey("", now)
}

// noteStormOKKey: тот же 200, но с привязкой к egress (per-egress лифт).
func (p *PoolTransport) noteStormOKKey(key string, now time.Time) {
	cutoff := now.Add(-stormWindow).UnixNano()
	p.muMu.Lock()
	hits := append(p.stormOKs, now.UnixNano())
	kept := hits[:0]
	for _, h := range hits {
		if h >= cutoff {
			kept = append(kept, h)
		}
	}
	p.stormOKs = kept
	kh := append(p.stormOKKeys, stormOKHit{ts: now.UnixNano(), key: key})
	kkept := kh[:0]
	for _, h := range kh {
		if h.ts >= cutoff {
			kkept = append(kkept, h)
		}
	}
	p.stormOKKeys = kkept
	p.muMu.Unlock()
}

// stormOKKeys — 200-хиты с привязкой к egress: per-egress лифт шторма.
// Живые данные 2026-09-23: 825×200 рядом с 835×503 — окна переоткрываются,
// но общий ok-rate 50% не набирается (stormOKs тонут в 429-кольце) и весь пул
// сидит в 1x1. Egress с 2+ свежими 200 — доказанно живой: ему полный свип
// сразу, без fail-fast упора.
type stormOKHit struct {
	ts  int64
	key string
}

// stormSingleIP: завал на ОДНОМ egress (один IP сыпет, остальные живут) —
// это per-IP burn, НЕ shared шторм: fail-fast 1x1 по всему пулу не нужен,
// heat этого IP и так уведёт его в hot. Порог: >=80% хитов окна с одного
// ключа при n>=stormMax429. Пустой ключ (legacy noteStorm429) считается
// «неизвестно» и НЕ голосует за single (консервативно = shared).

// stormSingleIPLocked: >=80% keyed хитов окна с одного ключа (n>=stormMax429).
// Порог = stormMax429 (12), не 20: иначе зона 12-19 клала весь пул в 1x1 ради
// одного битого egress (2026-09-24 мейн-IP: global-флаг + per-IP burn одного
// egress душили живые 50/50 лотереей 1x1). Флэп на границе гасится 80%-долей.
const stormSingleMinN = 12

// Вызывать под muMu (R или W). Пустые ключи игнорятся.
func (p *PoolTransport) stormSingleIPLocked(now time.Time) (string, bool) {
	cutoff := now.Add(-stormWindow).UnixNano()
	counts := map[string]int{}
	total := 0
	for _, h := range p.stormKeys {
		if h.ts < cutoff {
			continue
		}
		total++ // пустой ключ = неизвестный голос: разбавляет в shared,
		if h.key == "" {
			continue
		}
		counts[h.key]++
	}
	if total < stormSingleMinN {
		return "", false
	}
	best, bestN := "", 0
	for k, n := range counts {
		if n > bestN {
			best, bestN = k, n
		}
	}
	if best != "" && bestN*5 >= total*4 {
		return best, true
	}
	return "", false
}

// stormSingleIP: R-обёртка над stormSingleIPLocked (single-IP завал vs shared).
func (p *PoolTransport) stormSingleIP(now time.Time) (string, bool) {
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	return p.stormSingleIPLocked(now)
}

// stormActive reports a pool-wide storm: stormMax429+ 429s inside stormWindow
// without SUSTAINED recovery. Recovery is RATIO-based (wave 2026-09-20):
// absolute ok>=2 never fires in a 25% lottery (120 ok per 4min alongside
// 58 logical 503) — every request would run full 5x2 (~700 extra warmups
// per 14min into hot budgets). The throttle lifts only when ok PROVE the
// window reopened: ok-rate >= 50% (ok*2 >= n429) means half the pool serves,
// full sweeps are productive again. A lone fluke-200 in a flood (wave1
// heat P1: 1 ok vs 87 RA60) keeps the throttle: 1*2 < 87. Two ok vs 3x429
// (2*2 >= 3) lift — same as the old gate at the boundary.
// Must hold no locks.
// longWindows reports whether the pool currently carries hours-class bans
// (hardBad, or an active global burn) — when true, short wait-reopen is
// pointless: windows are far longer than any client budget, so an
// immediate tail beats waiting. Short 5s parks (kilo keyless) are NOT
// long windows: they reopen within seconds.
// Must hold no locks.
// poolSecondChancePeeks bounds the fresh-group probes after the first 429 in
// fail-fast mode. One probe was not enough: warp hands several IPs per /48, so
// a single peek often returned the very group already in `tried` and the
// request surfaced "saturated" while other groups stayed idle.
const poolSecondChancePeeks = 1

// poolHardBanMajority is the hard-ban count above which waiting for a window to
// reopen is pointless: the majority of the 20-backend warp pool is parked for
// hours. Below it, a handful of hours-class bans must NOT disable the
// short-park escape (2026-10-21: 6 bans of 20 muted it permanently and every
// 429 became an instant 503 although 13 healthy egresses were serving).
const poolHardBanMajority = 8

// hardBansCoverPool reports whether hours-class bans cover most of the pool.
func (p *PoolTransport) hardBansCoverPool(now time.Time) bool {
	if p.globalActiveAt(now) {
		return true
	}
	n := 0
	p.muMu.RLock()
	for k, hard := range p.hardBad {
		if !hard {
			continue
		}
		if until, ok := p.bad[k]; ok && until.After(now) {
			n++
		}
	}
	p.muMu.RUnlock()
	return n >= poolHardBanMajority
}

// globalActiveAt is the same predicate as globalActive for a supplied clock.
// One latch must answer to ONE rule: the deadline AND the identity generation
// that armed it. The time-only variant let a stale latch keep
// hardBansCoverPool/rounds decisions after a renewal disproved it (review B3).
func (p *PoolTransport) globalActiveAt(now time.Time) bool {
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	if p.globalGen != 0 && p.globalGen != p.identGen.Load() {
		return false
	}
	return !p.globalBurnUntil.IsZero() && now.Before(p.globalBurnUntil)
}

func (p *PoolTransport) longWindows(now time.Time) bool {
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	for k, hard := range p.hardBad {
		if !hard {
			continue
		}
		if until, ok := p.bad[k]; ok && until.After(now) {
			return true
		}
	}
	return !p.globalBurnUntil.IsZero() && now.Before(p.globalBurnUntil)
}

func (p *PoolTransport) stormActive() bool {
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	cutoff := time.Now().Add(-stormWindow).UnixNano()
	n := 0
	for _, h := range p.storm429 {
		if h >= cutoff {
			n++
		}
	}
	if n < stormMax429 {
		return false
	}
	ok := 0
	for _, h := range p.stormOKs {
		if h >= cutoff {
			ok++
		}
	}
	// Sustained recovery by share, not count: half-or-more ok means the
	// pool serves again. Heavy prompts double-note both rings symmetrically,
	// so the ratio stays unbiased.
	if ok*2 >= n {
		return false
	}
	return true
}

// stormLiveEgress: egress с 2+ свежими 200 в окне — доказанно живой.
// Per-egress лифт: такому egress acquire отдаёт полный свип сразу, даже пока
// общий шторм-флаг горит (окна переоткрываются по одному, не всем пулом).
// Порог 2 (не 1): один fluke-200 в 429-флуде лифтом не считается (та же логика,
// что у stormOKs). Must hold no locks.
func (p *PoolTransport) stormLiveEgress(key string) bool {
	if key == "" {
		return false
	}
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	cutoff := time.Now().Add(-stormWindow).UnixNano()
	n := 0
	for _, h := range p.stormOKKeys {
		if h.ts >= cutoff && h.key == key {
			n++
			if n >= 2 {
				return true
			}
		}
	}
	return false
}

// banShort429 parks a transient-throttle egress (minute-class or missing
// Retry-After) for d WITHOUT the hardBad flag and WITHOUT disk persist:
// the window reopens in seconds, so nothing must survive a restart.
// Park lives in its own memory-only map so the RoundTrip warmup-guard
// can walk past it without polluting the persisted bad/hard map with
// second-scale noise (acquire hands parked leases out; the guard decides).
// parkThrottled parks an egress after a non-hours 429. okBefore is the
// window's success count at the moment it throttled: a spent address
// (okBefore <= spentOKFloor) is parked long instead of rejoining rotation in
// 5s, while a rate-shaped window keeps the short park so a reopened window is
// picked up quickly. markGood/noteOK still lift any park on a 200.
func (p *PoolTransport) parkThrottled(key string, okBefore int, now time.Time) {
	if key == "" {
		return
	}
	// burns in the heat window (3 min), the 200-forgiveness included.
	burns := p.heatCount(key, now)
	park := shortPark
	reason := ""
	switch {
	case okBefore <= spentOKFloor && burns >= 1:
		// Arrived already spent: it had served <=2 requests and throttled
		// again. Live 2026-10-18: the median pool address looked like this.
		park, reason = spentPark, "arrived spent"
	case burns >= 5:
		park, reason = spentPark, "burning repeatedly"
	case burns >= 3:
		park, reason = burnParkMid, "burning"
	case burns >= 2:
		park, reason = burnParkFirst, "second burn"
	}
	if reason == "" {
		p.banShort429(key, shortPark, now)
		return
	}
	p.markBadSoft(key, park, now)
	p.log().Warn("pool: egress parked long",
		"ip", key, "prefix", v48(key), "ok_before", okBefore, "burns_3m", burns,
		"park", park.String(), "why", reason)
}

func (p *PoolTransport) banShort429(key string, d time.Duration, now time.Time) {
	if key == "" || d <= 0 {
		return
	}
	p.muMu.Lock()
	if p.park == nil {
		p.park = map[string]time.Time{}
	}
	// Refresh on every 429, even inside a live park: a fresh 429 proves the
	// window is still hot. One-shot parks never re-parked after expiry and
	// the egress was re-warmed by every sweep (2026-09-14 relapse).
	p.park[key] = now.Add(d)
	p.muMu.Unlock()
	p.log().Warn("pool: transient 429, short park (no persist)",
		"ip", key, "prefix", v48(key), "park", d.String())
}

type stateSnapshot struct {
	Bad        map[string]time.Time `json:"bad"`        // egress key -> cooldown until
	HardBad    map[string]bool      `json:"hardBad"`    // egress key -> exact-RA hours-class ban
	IPOK       map[string]int       `json:"ipOK"`       // egress key -> ok count before first 429
	CtrlProbed map[string]bool      `json:"ctrlProbed"` // egress key -> control probe done
	// review_1 A: global-флаг переживает рестарт — иначе тени без флага =
	// вечный 503 на часы (acquire не выдаёт лиз, детектор не переобучается).
	GlobalUntil time.Time        `json:"globalUntil,omitempty"`
	GlobalRA    int              `json:"globalRA,omitempty"`
	GlobalObs   []persistedRAObs `json:"globalObs,omitempty"`
}

// persistedRAObs — JSON-видимая копия globalRAObs (поля unexported).
type persistedRAObs struct {
	At time.Time `json:"at"`
	IP string    `json:"ip"`
	RA int       `json:"ra"`
}

// loadState restores persisted egress cooldowns after a restart. A burned
// exact-IP (exact-RA hours-class ban) must stay out of rotation across promotes —
// otherwise every restart re-storms the same exhausted windows from scratch
// (the "works before the change, 429 an hour after" cycle). Soft cooldowns
// (60s/15m) and probe/ok counters are restored too so a restart does not
// re-probe every hot backend.
func (p *PoolTransport) loadState() {
	if p.StateFile == "" {
		return
	}
	data, err := os.ReadFile(p.StateFile)
	if err != nil {
		// First run / missing file: cold start, nothing to restore.
		return
	}
	var snap stateSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		p.log().Warn("pool: state decode failed (ignored)", "err", err, "file", p.StateFile)
		return
	}
	now := time.Now()
	p.muMu.Lock()
	for k, until := range snap.Bad {
		if until.After(now) {
			if p.bad == nil {
				p.bad = map[string]time.Time{}
			}
			p.bad[k] = until
			if snap.HardBad[k] {
				if p.hardBad == nil {
					p.hardBad = map[string]bool{}
				}
				p.hardBad[k] = true
			}
		}
	}
	p.ipOK = snap.IPOK
	p.ctrlProbed = snap.CtrlProbed
	// review_1 A: восстанавливаем живой флаг (иначе тени без флага).
	if !snap.GlobalUntil.IsZero() && now.Before(snap.GlobalUntil) {
		p.globalBurnUntil = snap.GlobalUntil
		p.globalRA = snap.GlobalRA
		for _, po := range snap.GlobalObs {
			if now.Sub(po.At) <= globalClusterWindow {
				p.globalObs = append(p.globalObs, globalRAObs{at: po.At, ip: po.IP, ra: po.RA})
			}
		}
		p.log().Warn("pool: global burn restored across restart",
			"until", p.globalBurnUntil.Format(time.RFC3339), "ra", p.globalRA, "obs", len(p.globalObs))
	}
	nBad, nHard, nOK, nCtrl := len(p.bad), len(p.hardBad), len(p.ipOK), len(p.ctrlProbed)
	type restoredRow struct {
		k     string
		until time.Time
		hard  bool
		ok    int
	}
	var rows []restoredRow
	for k, until := range snap.Bad {
		if until.After(now) {
			rows = append(rows, restoredRow{k, until, snap.HardBad[k], p.ipOK[k]})
		}
	}
	p.muMu.Unlock()
	// Logs outside the exclusive lock (I/O under Lock is fragile — review 2026-09-09).
	p.log().Info("pool: state restored exact-RA per-IP",
		"bad", nBad, "hard_bad", nHard,
		"ip_ok", nOK, "ctrl_probed", nCtrl,
		"file", p.StateFile)
	for _, r := range rows {
		p.log().Info("pool: restored bad IP",
			"ip", r.k, "prefix", v48(r.k), "until", r.until.Format(time.RFC3339),
			"hard", r.hard, "ok_before", r.ok)
	}
}

// saveState writes current egress cooldowns to disk. It snapshots maps
// under muMu (copy-out, then unlock before I/O) and writes atomically
// via tmp+rename — callers must NOT hold muMu (saveState locks itself).
// Best-effort: a failed write only loses the cooldown memory, never
// affects routing. Concurrent callers serialize on fileMu (tmp+rename);
// order is NOT guaranteed (each saveState marshals fresh + writes async),
// so a stale snapshot may land after a fresh one and resurrect a cleared
// ban until expiry/200 — deletes exist (markGood/unban/lazy-expiry), the
// map is not monotonic. Generations (stateSeq/stateDone) order concurrent
// async snapshots: a stale write landing after a fresh one drops instead of
// resurrecting cleared bans; FlushState issues the newest generation.
func (p *PoolTransport) saveState() {
	if p.StateFile == "" {
		return
	}
	seq := p.stateSeq.Add(1) // поколение ДО снапшота (review_1 D): порядок = порядок мутаций, иначе старый снапшот с большим seq кладёт поверх свежего
	p.muMu.RLock()
	now := time.Now()
	snap := stateSnapshot{
		Bad:        make(map[string]time.Time, len(p.bad)),
		HardBad:    make(map[string]bool, len(p.hardBad)),
		IPOK:       make(map[string]int, len(p.ipOK)),
		CtrlProbed: make(map[string]bool, len(p.ctrlProbed)),
	}
	for k, until := range p.bad {
		if until.After(now) {
			snap.Bad[k] = until
			if p.hardBad[k] {
				snap.HardBad[k] = true
			}
		}
	}
	// review_1 A: флаг тоже персистим (живой — в будущее). Тени при
	// восстановленном флаге = shared burn, fail-fast вместо вечного 503.
	if !p.globalBurnUntil.IsZero() && now.Before(p.globalBurnUntil) {
		snap.GlobalUntil = p.globalBurnUntil
		snap.GlobalRA = p.globalRA
		for _, o := range p.globalObs {
			if now.Sub(o.at) <= globalClusterWindow {
				snap.GlobalObs = append(snap.GlobalObs, persistedRAObs{At: o.at, IP: o.ip, RA: o.ra})
			}
		}
	}
	for k, v := range p.hardBad {
		if _, ok := snap.Bad[k]; ok {
			snap.HardBad[k] = v
		}
	}
	for k, v := range p.ipOK {
		snap.IPOK[k] = v
	}
	for k, v := range p.ctrlProbed {
		snap.CtrlProbed[k] = v
	}
	p.muMu.RUnlock()
	data, err := json.Marshal(&snap)
	if err != nil {
		p.log().Warn("pool: state encode failed", "err", err)
		return
	}
	go p.saveStateSync(seq, data)
}

// saveStateSync выполняет ту же tmp+rename запись синхронно (тесты + редкие
// ручные вызовы). Основной путь идёт через saveState (async).
func (p *PoolTransport) saveStateSync(seq uint64, data []byte) {
	p.fileMu.Lock()
	defer p.fileMu.Unlock()
	// Stale async snapshots drop: a newer generation already hit disk.
	if seq <= p.stateDone.Load() {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p.StateFile), 0o700)
	tmp := fmt.Sprintf("%s.tmp.%d", p.StateFile, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		p.log().Warn("pool: state write failed", "err", err, "file", tmp)
		return
	}
	if err := os.Rename(tmp, p.StateFile); err != nil {
		p.log().Warn("pool: state rename failed", "err", err, "file", p.StateFile)
		_ = os.Remove(tmp)
		return
	}
	for {
		if d := p.stateDone.Load(); seq <= d || p.stateDone.CompareAndSwap(d, seq) {
			break
		}
	}
}

// FlushState делает синхронный снапшот состояния на диск (тесты, graceful
// shutdown). Основной путь saveState идёт async через saveStateSync.
func (p *PoolTransport) FlushState() {
	if p.StateFile == "" {
		return
	}
	p.muMu.RLock()
	now := time.Now()
	snap := stateSnapshot{
		Bad:        make(map[string]time.Time, len(p.bad)),
		HardBad:    make(map[string]bool, len(p.hardBad)),
		IPOK:       make(map[string]int, len(p.ipOK)),
		CtrlProbed: make(map[string]bool, len(p.ctrlProbed)),
	}
	for k, until := range p.bad {
		if until.After(now) {
			snap.Bad[k] = until
			if p.hardBad[k] {
				snap.HardBad[k] = true
			}
		}
	}
	// review_1 A (как saveState): живой global-флаг тоже персистим.
	if !p.globalBurnUntil.IsZero() && now.Before(p.globalBurnUntil) {
		snap.GlobalUntil = p.globalBurnUntil
		snap.GlobalRA = p.globalRA
		for _, o := range p.globalObs {
			if now.Sub(o.at) <= globalClusterWindow {
				snap.GlobalObs = append(snap.GlobalObs, persistedRAObs{At: o.at, IP: o.ip, RA: o.ra})
			}
		}
	}
	for k, v := range p.hardBad {
		if _, ok := snap.Bad[k]; ok {
			snap.HardBad[k] = v
		}
	}
	for k, v := range p.ipOK {
		snap.IPOK[k] = v
	}
	for k, v := range p.ctrlProbed {
		snap.CtrlProbed[k] = v
	}
	p.muMu.RUnlock()
	data, err := json.Marshal(&snap)
	if err != nil {
		return
	}
	p.saveStateSync(p.stateSeq.Add(1), data)
}

// when every egress is 429-exhausted. Fresh identities carry fresh per-IP
// budgets, so the pool comes back without a manual restart. Rate-limited
// to one rotate per 60s: respawning 20 workers takes ~1-2 min and
// per-request rotation would thresh. maybeRotatePoolForce bypasses the
// throttle when caller knows pool is exhausted.
func (p *PoolTransport) maybeRotatePoolForce() {
	// Throttled like the normal path: every exhausted request in a storm
	// "knows" the pool is spent, and unthrottled force-rotate made 17
	// exhausts/min fire 17 respawns — workers flap 30-90s each, acquires
	// 409, more exhausts. 2026-10-18 forensics: 14 rotates in 5h (one per
	// ~21 min) and the failure rate in the minute AFTER a rotate is 27.9%
	// against 7-20% otherwise — each rotate respawns 20 workers for 1-2 min.
	// 10 min keeps the re-arm path but stops paying that price continuously.
	const minInterval = 10 * time.Minute
	now := time.Now()
	last := p.lastRotate.Load()
	if now.Sub(time.Unix(0, last)) < minInterval {
		return
	}
	if !p.lastRotate.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	u := strings.TrimSuffix(p.APIURL, "/") + "/api/v1/pool/rotate"
	tok := p.effectiveToken()
	c := p.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: rotateClientTimeout}
	}
	go func() {
		req, err := http.NewRequest(http.MethodPost, u, nil)
		if err != nil {
			p.log().Warn("pool force-rotate: request build failed", "err", err)
			return
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.Do(req)
		if err != nil {
			p.log().Warn("pool force-rotate failed", "err", err)
			return
		}
		defer resp.Body.Close()
		var out struct {
			Respawned int `json:"respawned"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
		p.log().Warn("pool force-rotated (exhausted)", "status", resp.StatusCode, "respawned", out.Respawned)
	}()
}
func (p *PoolTransport) maybeRotatePool() {
	const minInterval = 60 * time.Second
	now := time.Now()
	last := p.lastRotate.Load()
	if now.Sub(time.Unix(0, last)) < minInterval {
		return
	}
	if !p.lastRotate.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	u := strings.TrimSuffix(p.APIURL, "/") + "/api/v1/pool/rotate"
	tok := p.effectiveToken()
	c := p.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	// Fire-and-forget: the 1-2 min worker respawn must not block the
	// request path; later requests find the pool revived.
	go func() {
		req, err := http.NewRequest(http.MethodPost, u, nil)
		if err != nil {
			p.log().Warn("pool rotate: request build failed", "err", err)
			return
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.Do(req)
		if err != nil {
			p.log().Warn("pool rotate failed", "err", err)
			p.poolEvent("", "rotate", false, 0, err.Error())
			return
		}
		defer resp.Body.Close()
		var out struct {
			Respawned int `json:"respawned"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
		p.log().Warn("pool rotated", "status", resp.StatusCode, "respawned", out.Respawned)
		p.poolEvent("", "rotate", resp.StatusCode < 300, 0, "")
	}()
}

// probeLease quickly verifies the lease's egress actually answers before the
// full 15s header attempt is spent on it (wedged tunnels respond to neither
// TLS nor HTTP). It GETs the provider's /models sibling path with a 3s
// budget; any HTTP response means the egress is alive, a timeout/transport
// error means rotate. GET /models does not consume the chat rate limit.
// probeLease is DEAD BY DESIGN (zero call sites): per-attempt probing was
// removed — GET /models burns the same (IP,UA) budget as the chat request,
// and a wedged tunnel is visible via the header error in seconds (RHT cuts).
// Kept for manual debugging only; do not re-enable per-attempt.
func (rt *poolRetryRT) probeLease(req *http.Request, lease *poolLease) bool {
	if lease.proxy == "" {
		return true
	}
	cl := rt.t.clientFor(lease.proxy)
	// 3s — контекстом, НЕ мутацией кэшированного клиента (иначе отравим
	// egress всем последующим: clientFor возвращает shared instance).
	ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
	defer cancel()
	p, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL.String(), nil)
	if err != nil {
		return true
	}
	p.URL.Path = path.Dir(req.URL.Path) + "/models"
	// Selective copy: only safe headers needed for probe; strip client auth.
	for _, k := range []string{"User-Agent", "Accept", "Accept-Encoding", "Content-Type"} {
		if v := req.Header.Get(k); v != "" {
			p.Header.Set(k, v)
		}
	}
	crypto.StripUpstreamHeaders(p.Header, crypto.DefaultStripHeaders)
	resp, err := cl.Do(p)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusInternalServerError {
		if len(rt.t.keysFor(rt.prov)) == 0 {
			return false
		}
	}
	return true
}

// isBad reports whether the egress key is still inside its cooldown.

// noteOK counts a successful (non-retryable) response for an egress key.
// The count is logged at the next 429 to show how much each IP window
// actually served before exhaustion — the "how many requests per IP before
// 429" observability.
func (p *PoolTransport) noteOK(key string, status int) {
	if key == "" {
		return
	}
	// Liveness ("the pool serves") must be proven by a 2xx: a non-retryable 4xx
	// through a lease (403 account gate, per-model FreeTierError) says nothing about
	// the shared budget, and letting it count lifted the collapse guard on live
	// traffic where 26 of 61 failures were gate403.
	serving := status >= 200 && status < 300
	if serving {
		p.lastPoolOK.Store(time.Now().UnixNano())
	}
	p.satTailStreak.Store(0)
	// Heal escalation is NOT reset here: any single 200 (e.g. one surviving
	// egress) used to zero healFails, so 32 rotates in 2.4h all ended with
	// "heal_fails=0 gap=4m0s" and 30 of them burned 40s of pool respawns for
	// nothing. Only a proven identity renewal (noteIdentityGroup) or nothing
	// at all resets the gap.
	p.muMu.Lock()
	defer p.muMu.Unlock()
	if p.ipOK == nil {
		p.ipOK = map[string]int{}
	}
	p.ipOK[key]++
	if serving && p.spent != nil {
		// Only a 2xx proves the window reopened; a 4xx through the lease (gate
		// 403, model error) says nothing about the budget.
		delete(p.spent, key)
	}
	if serving {
		if p.okAt == nil {
			p.okAt = map[string]int64{}
		}
		p.okAt[key] = time.Now().UnixNano()
	}
}

// noteSaturatedTail records one "pool saturated" tail verdict and returns the
// current streak length (reset by any pooled 200 in noteOK).
func (p *PoolTransport) noteSaturatedTail() int {
	return int(p.satTailStreak.Add(1))
}

// poolSilentFor reports whether no pool lease served a 200 for d while a
// saturation tail is on record: the pool answers acquires but hands out
// burned egresses. A fresh process without a single saturation has nothing to
// escape from (false), so a healthy pool never opens the home leg.
func (p *PoolTransport) poolSilentFor(d time.Duration) bool {
	if p.satTailStreak.Load() == 0 {
		return false
	}
	last := p.lastPoolOK.Load()
	if last == 0 {
		return true
	}
	return time.Since(time.Unix(0, last)) >= d
}

// IdentityGen reports how many identity recycles completed: every rotate that
// re-registered the WARP devices bumps it. A changed value means all
// per-identity state (bans, windows, model failure verdicts) is stale.
func (p *PoolTransport) IdentityGen() uint64 {
	if p == nil {
		return 0
	}
	return p.identGen.Load()
}

// bannedShare is the fraction of known egress identities that currently sit
// under a ban (soft cooldown or hours-class hard ban). It is the "pool is
// burning" signal: past ~half the pool, waiting without a rotate only burns
// the remaining identities.
func (p *PoolTransport) bannedShare() float64 {
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	now := time.Now()
	known := map[string]bool{}
	banned := 0
	for k, until := range p.bad {
		known[k] = true
		if now.Before(until) {
			banned++
		}
	}
	for k := range p.hardBad {
		// hardBad is a SET (deadline lives in bad): an hours-class ban counts
		// as banned for the whole flag lifetime.
		if !known[k] {
			known[k] = true
			banned++
		}
	}
	for k := range p.ipOK {
		known[k] = true
	}
	if len(known) == 0 {
		return 0
	}
	return float64(banned) / float64(len(known))
}

// egressGroupsLocked snapshots the /48 groups this transport knows about.
func (p *PoolTransport) egressGroupsLocked() map[string]bool {
	out := map[string]bool{}
	add := func(k string) {
		if g := triedKey(k); g != "" {
			out[g] = true
		}
	}
	for k := range p.bad {
		add(k)
	}
	for k := range p.park {
		add(k)
	}
	for k := range p.hardBad {
		add(k)
	}
	for k := range p.ipOK {
		add(k)
	}
	return out
}

// noteIdentityGroup confirms (or not) that a rotate actually renewed the egress
// prefixes: a 200 served through a /48 that was NOT in the pre-rotate snapshot
// proves warp re-registered the devices onto fresh budgets. Only then does the
// generation bump — which is what voids per-model lockouts and the shared-burn
// flag — because otherwise "new identities" is wishful thinking.
func (p *PoolTransport) noteIdentityGroup(key string) {
	if key == "" || isSyntheticEgressKey(key) {
		// A home-IP (or any synthetic) egress is NOT a pool identity: it has no
		// /48 of its own, so treating it as "a new prefix served" bumped identGen
		// and dropped the global latch + every identity-keyed ban on each
		// successful home-leg request. Observed live 2026-10-22 16:15:23:
		// "identities renewed — new egress prefix served group=direct-home-ip
		// global_cleared=true" right before "direct last-resort ok".
		return
	}
	p.healMu.Lock()
	expect, pre := p.healExpectRenewal, p.healPreGroups
	p.healMu.Unlock()
	if !expect {
		return
	}
	g := triedKey(key)
	if g == "" || pre[g] {
		return
	}
	p.healMu.Lock()
	p.healExpectRenewal = false
	p.healPreGroups = nil
	p.healFails = 0
	p.healMu.Unlock()
	p.identGen.Add(1)
	p.noteHealOutcome(true)
	p.muMu.Lock()
	hadGlobal := !p.globalBurnUntil.IsZero()
	p.globalBurnUntil = time.Time{}
	p.globalRA = 0
	p.globalObs = nil
	p.globalOKAt = time.Time{}
	p.globalOKGroup = ""
	p.globalArmedAt = time.Time{}
	p.muMu.Unlock()
	p.log().Warn("pool: identities renewed — new egress prefix served", "group", g,
		"global_cleared", hadGlobal, "generation", p.identGen.Load())
}

// healRotate recycles egress identities after the pool burned out.
//
// It is the automated form of the manual "restart warp-router" step: a rotate
// re-registers every WARP device, the pool returns on NEW /48 prefixes, and
// each identity starts with its own upstream window. Two live incidents
// (2026-10-21 21:26 manual restart, 2026-10-22 00:31 rotate) revived service
// this way while the router's shared-deadline flag ("global burn") stayed
// armed — so the old skip-if-global-burn branches protected nothing and cost
// hours of 503. Rotates escalate their own gap: a rotate that did not restore
// service (no pooled 200 afterwards) waits healGapFirst << fails up to
// healGapMax instead of flapping pool workers every minute.
func (p *PoolTransport) healRotate(reason string) {
	now := time.Now()
	p.healMu.Lock()
	gap := healGapFirst
	for i := 0; i < p.healFails && gap < healGapMax; i++ {
		gap *= 2
	}
	if gap > healGapMax {
		gap = healGapMax
	}
	if p.healRunning || (!p.healAt.IsZero() && now.Sub(p.healAt) < gap) {
		p.healMu.Unlock()
		return
	}
	p.healAt = now
	p.healRunning = true
	fails := p.healFails
	// Snapshot the prefixes we know BEFORE the rotate: renewal is proven only
	// by serving through a group outside this set.
	p.muMu.RLock()
	preGroups := p.egressGroupsLocked()
	p.muMu.RUnlock()
	p.healPreGroups = preGroups
	p.healExpectRenewal = true
	p.healMu.Unlock()

	// banned_share/latch make the DECISION auditable: a rotate costs a full
	// WARP re-registration (1-2 min of 409s, and the data path can drop with
	// EOF), so the log must show WHY it was wanted — majority-banned capacity
	// or an armed latch with a silent pool.
	latch, _ := p.globalStatus()
	p.log().Warn("pool: recycling egress identities (rotate)",
		"reason", reason, "heal_fails", fails, "gap", gap.String(),
		"since_pool_ok", time.Since(time.Unix(0, p.lastPoolOK.Load())).Round(time.Second).String(),
		"sat_streak", p.satTailStreak.Load(),
		"banned_share", float64(int64(p.bannedShare()*100))/100,
		"latch_until", latch.Format(time.RFC3339))

	go func() {
		defer func() {
			p.healMu.Lock()
			p.healRunning = false
			p.healMu.Unlock()
		}()
		// Escalation: a rotate that produces no pooled 200 within the settle
		// window did NOT restore service — widen the gap instead of respawning
		// 20 workers every four minutes (live 2026-10-18: failure rate 27.9% in
		// the minute after a rotate).
		okBefore := p.lastPoolOK.Load()
		time.AfterFunc(90*time.Second, func() {
			if p.lastPoolOK.Load() != okBefore {
				return
			}
			p.noteHealOutcome(false)
			p.healMu.Lock()
			fails := p.healFails
			p.healMu.Unlock()
			p.log().Warn("pool: heal did not restore service, widening gap", "heal_fails", fails)
		})
		u := strings.TrimSuffix(p.APIURL, "/") + "/api/v1/pool/rotate"
		tok := p.effectiveToken()
		c := p.HTTPClient
		if c == nil {
			c = &http.Client{Timeout: healClientTimeout}
		} else {
			c = &http.Client{Timeout: healClientTimeout, Transport: c.Transport}
		}
		req, err := http.NewRequest(http.MethodPost, u, nil)
		if err != nil {
			p.log().Warn("pool heal-rotate: request build failed", "err", err)
			return
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.Do(req)
		dropped := false
		if err != nil {
			// warp respawns workers synchronously and drops the connection
			// mid-respawn (observed every time); the pool does come back.
			dropped = true
			p.log().Warn("pool heal-rotate: connection dropped, assuming respawn", "err", err.Error())
		}
		if resp != nil {
			defer resp.Body.Close()
			var out struct {
				Respawned int `json:"respawned"`
			}
			_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
			p.log().Warn("pool heal-rotate done", "status", resp.StatusCode, "respawned", out.Respawned)
			p.poolEvent("", "rotate", resp.StatusCode < 300, 0, "")
		} else if dropped {
			p.poolEvent("", "rotate", true, 0, "")
		}
		// NO unconditional clear here. A rotate re-registers WARP devices but
		// can land on the SAME /48 prefixes (measured 2026-10-22 10:14: fresh
		// accounts came up in 2a09:bac1:47a0, already burned), so "new
		// generation" is only a claim until a 200 actually arrives through a
		// prefix that did not exist before the rotate. That proof lives in
		// noteIdentityGroup; the two-distinct-/48 probation (globalOKAt/
		// globalOKGroup) clears it as well. Anything else flaps the latch off
		// on a lie and drops the router out of fail-fast probing for nothing.
	}()
}

// noteHealOutcome resets or escalates the heal backoff: a pooled 200 means the
// recycle worked; a fresh heal that produced no 200 escalates the gap.
func (p *PoolTransport) noteHealOutcome(served bool) {
	p.healMu.Lock()
	defer p.healMu.Unlock()
	if served {
		p.healFails = 0
		return
	}
	if !p.healAt.IsZero() {
		p.healFails++
	}
}

// maybeRotateSaturated re-arms the pool after a sustained saturation streak,
// at most once per satRotateCooldown (see the const block for why).
func (p *PoolTransport) maybeRotateSaturated() {
	if p.satTailStreak.Load() < satRotateAfter {
		return
	}
	now := time.Now().UnixNano()
	last := p.lastSatRotate.Load()
	if last != 0 && now-last < int64(satRotateCooldown) {
		return
	}
	if !p.lastSatRotate.CompareAndSwap(last, now) {
		return
	}
	p.healRotate("sustained saturation tail")
}

// DirectHomeBanned reports whether the home-IP direct fallback is inside
// its cooldown — bad map OR transient park (a parked home is still burning;
// re-hitting it every request hammers one identity while pool API is dead).
func (p *PoolTransport) DirectHomeBanned() bool {
	now := time.Now()
	return p.isBad(directHomeIPKey, now) || p.isParked(directHomeIPKey, now)
}

// BanHomeCooldown records a direct-leg (home-IP) verdict so the next
// no-verdict request fails fast instead of re-hitting a burning window
// (review 2026-09-09: responses-fallback never banned home, every
// no-verdict request cost one home hit). Hours-class RA => exact named
// window; anything else => transient short-park.
func (p *PoolTransport) BanHomeCooldown(ra string) {
	now := time.Now()
	if cd, ok := p.hoursClassRA(ra); ok {
		p.markBad429(directHomeIPKey, cd, now)
		return
	}
	p.banShort429(directHomeIPKey, shortPark, now)
}

// okAge returns how long ago the egress served its last 200.
func (p *PoolTransport) okAge(key string) (time.Duration, bool) {
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	ts := p.okAt[key]
	if ts == 0 {
		return 0, false
	}
	return time.Since(time.Unix(0, ts)), true
}

// clearGlobalBurn drops the shared-burn latch and its probation state.
func (p *PoolTransport) clearGlobalBurn(reason string) {
	p.muMu.Lock()
	was := p.globalBurnUntil
	p.globalBurnUntil = time.Time{}
	p.globalRA = 0
	p.globalObs = nil
	p.globalOKAt = time.Time{}
	p.globalOKGroup = ""
	p.muMu.Unlock()
	if !was.IsZero() {
		p.log().Warn("pool: global burn latch cleared", "reason", reason)
	}
}

// noteHoursRAGuarded arms the shared-burn latch ONLY for a silent pool.
//
// The latch costs the whole pool its sweep: every request becomes one probe against
// an arbitrary egress, so with 17 of 20 burned the answer is a 503 with "(1
// throttled)" (live 2026-10-22 12:20-12:25: 45% of requests, 12242 collapse log
// lines in 1.5h). Two hours-class RAs from two /48s are NOT proof that every
// budget is burned: they are two burned egresses. While the pool still delivers a
// 200 inside poolLiveGrace the honest reading is a per-IP burn — ban that egress
// for its own deadline and keep walking the pool.
func (p *PoolTransport) noteHoursRAGuarded(key string, raSecs int, now time.Time) (global, clustered bool) {
	if !p.noteHoursRA(key, raSecs) {
		return false, false
	}
	if p.poolServing() {
		p.clearGlobalBurn("hours-RA cluster while the pool still serves")
		p.warnEvery("hours-ra-serving", 30*time.Second,
			"pool: hours-RA cluster NOT treated as global — pool serves, banning this egress only",
			"egress", key, "retry_after", raSecs)
		return false, true
	}
	return true, true
}

// burnCollapse reports whether the burn latch must collapse the sweep to ONE
// probe. Silence is the condition: a 200 inside poolLiveGrace proves the pool is
// alive, and collapsing a live pool is what turned requests into a lottery.
func (p *PoolTransport) burnCollapse(now time.Time) bool {
	until, _ := p.globalStatus()
	if until.IsZero() || !now.Before(until) {
		return false
	}
	return !p.poolServing()
}

// errNoSessionSupply marks a lease refused because the harvest ran out of
// genuine session ids. The cure is minting more ids, NOT recycling WARP
// identities: a rotate re-registers every egress and makes the shortage worse.
var errNoSessionSupply = errors.New("session supply exhausted")

// rotateStallWindow is how long a majority-banned pool must go without a 200
// before a rotate is worth its 30-90 s blackout.
const rotateStallWindow = 90 * time.Second

// rotateWanted is the ONE decision point for recycling WARP identities.
//
// burnCollapse: the shared-burn latch is armed AND the pool went silent. The
// latch alone must never rotate — it also arms for a single hours-class
// Retry-After on ONE egress, and the guard clears it seconds later, yet the
// armed instant would already have started a full rotate (live 2026-10-23
// 04:37:13 ARM -> 04:37:54 "rotate: EOF", data path dropped).
//
// bannedShare: a majority of the known egresses are banned. That is a CAPACITY
// move (a fresh /48 = a fresh daily quota) costing 30-90 s of "pool cycled"
// blackout, so it needs evidence that requests are actually FAILING: the pool
// must have served nothing for rotateStallWindow. Live 2026-10-23 18:19:40:
// banned_share=1.0 with a 200 six seconds earlier, the rotate fired and its
// window answered 503 while the surviving egresses were fine. A banned pool
// that still serves beats a fresh one that answers nothing while workers respawn.
//
// Only the Acquire-error path asks for the majority-ban branch (a request DID
// fail there); the request-start trigger asks for burnCollapse only, because at
// low traffic (1-16 req/min) a 90 s gap is idleness, not a stall.

func (p *PoolTransport) rotateWanted(now time.Time) bool {
	if p.burnCollapse(now) {
		return true
	}
	if p.bannedShare() < 0.5 {
		return false
	}
	return !p.poolServingRecently(rotateStallWindow)
}

// clearOKAt drops the "recently live" mark of an egress after a 429.
func (p *PoolTransport) clearOKAt(key string) {
	if key == "" {
		return
	}
	p.muMu.Lock()
	delete(p.okAt, key)
	p.muMu.Unlock()
}

// poolServingRecently reports whether ANY pooled 200 landed within d. It is the
// difference between "the pool is burned" and "a burned egress happened to be
// probed": live 2026-10-22 12:23, the global flag was armed for 14.6h while 12
// egresses kept serving 200s, and the 1x1 collapse turned every request into a
// coin flip (45% of requests answered 503 in 0.5-2s with "(1 throttled)").
func (p *PoolTransport) poolServingRecently(d time.Duration) bool {
	last := p.lastPoolOK.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(0, last)) < d
}

// poolServing reports whether the pool is demonstrably alive, INDEPENDENT of the
// arrival cadence. A time-since-last-200 test is cadence-bound: the measured light
// load is 1-16 req/min (4-60s gaps), so every request that arrives after a 15s gap
// would read "silent" and collapse — which is exactly how the 1x1 regime survived
// (714 of 717 saturated tails were "(1 throttled)" after a guard that keyed on a
// 15s window). Two independent proofs of life:
//   - a 200 younger than poolLiveGrace (fast path), or
//   - a healthy success SHARE over the recent pool attempts (stormOKs/storm429
//     rings, source of stormActive), so an idle-minute pool still counts as alive.
func (p *PoolTransport) poolServing() bool {
	if p.poolServingRecently(poolLiveGrace) {
		return true
	}
	p.muMu.RLock()
	cutoff := time.Now().Add(-stormWindow).UnixNano()
	oks, hits := 0, 0
	for _, ts := range p.stormOKs {
		if ts >= cutoff {
			oks++
		}
	}
	for _, ts := range p.storm429 {
		if ts >= cutoff {
			hits++
		}
	}
	p.muMu.RUnlock()
	if oks == 0 {
		// Not a single 200 in the whole window: the pool really is silent (a full
		// burn, a dead pool API, or startup). Collapse is honest here.
		return false
	}
	// Share, not count: an idle pool with 2 oks and 0 hits serves fine, while a
	// storm (oks*5 < hits) does not.
	return oks*5 >= hits
}

// warnEvery logs at most once per `every` for a given key.
func (p *PoolTransport) warnEvery(key string, every time.Duration, msg string, kv ...any) {
	now := time.Now()
	p.muMu.Lock()
	if p.logThrottle == nil {
		p.logThrottle = map[string]int64{}
	}
	if last := p.logThrottle[key]; last != 0 && now.Sub(time.Unix(0, last)) < every {
		p.muMu.Unlock()
		return
	}
	p.logThrottle[key] = now.UnixNano()
	p.muMu.Unlock()
	p.log().Warn(msg, kv...)
}

// okCount returns the number of successful responses seen for an egress key.
func (p *PoolTransport) okCount(key string) int {
	p.muMu.RLock()
	defer p.muMu.RUnlock()
	return p.ipOK[key]
}

// spentBanLadder is the progressive ban for an egress whose budget is
// provably gone. First hit keeps the historical 10-minute default (an address
// can look spent to one model and serve another), each further hit without a
// single 200 in between doubles the evidence: 30 minutes, then 2 hours.
func spentBanLadder(stage int) time.Duration {
	switch {
	case stage <= 1:
		return clusterBanCap
	case stage == 2:
		return 30 * time.Minute
	default:
		return 2 * time.Hour
	}
}

// burnProbeFanout bounds how many egresses one request may probe while the
// shared-budget latch is armed. The old hard 1x1 was a lottery: 18,105 of
// 18,579 saturated tails carried "(1 throttled)" while a handful of egresses
// kept serving 200 in the same minutes (the per-egress lift fired 2,406
// times). Three probes over the best-ranked candidates costs at most three
// upstream windows and keeps the live subset exploitable; with no promising
// candidate the pool is honestly dead and the probe stays single.
const burnProbeFanout = 3

// limitedProbe reports whether the request walks a limited probe budget, i.e.
// the fail-fast tail gates must apply: storm/global 1x1 and the latch's 3x1.
func limitedProbe(fanout, rounds int) bool {
	return fanout*rounds <= burnProbeFanout
}

func (p *PoolTransport) burnProbeFanout(now time.Time) int {
	if p.poolHasPromising(now) {
		return burnProbeFanout
	}
	return 1
}

// poolHasPromising reports whether any egress is worth a probe RIGHT NOW: it
// served a 2xx within okFreshWindow and is not banned/parked, or the pool as a
// whole is still serving. Lifetime counters (ipOK) must not qualify: after an
// identity renewal every stale key is neither bad nor parked, so a lifetime
// predicate would answer "yes" forever and the 1x1 guard would never arm
// (review B2).
func (p *PoolTransport) poolHasPromising(now time.Time) bool {
	p.muMu.RLock()
	keys := make([]string, 0, len(p.okAt))
	for k, at := range p.okAt {
		if now.Sub(time.Unix(0, at)) <= promisingWindow {
			keys = append(keys, k)
		}
	}
	p.muMu.RUnlock()
	for _, k := range keys {
		if p.isBad(k, now) || p.isParked(k, now) {
			continue
		}
		return true
	}
	return p.poolServing()
}

// okFresh reports whether key produced a 2xx within d. okAt is written only
// inside the serving guard of noteOK, so a gate-403 or a model 4xx through the
// lease does NOT make a spent address look productive (review B4).
func (p *PoolTransport) okFresh(key string, d time.Duration, now time.Time) bool {
	if key == "" {
		return false
	}
	p.muMu.RLock()
	at, ok := p.okAt[key]
	p.muMu.RUnlock()
	return ok && now.Sub(time.Unix(0, at)) <= d
}

// banSpent records "this egress budget is gone" and returns the ban duration
// from the ladder. Any 200 through the key clears the stage (markGoodKey).
func (p *PoolTransport) banSpent(key string, now time.Time) time.Duration {
	if key == "" {
		return 0
	}
	p.muMu.Lock()
	if p.spent == nil {
		p.spent = map[string]int{}
	}
	// Advance only when the previous ladder ban already expired: 2-3 requests
	// can be parked on the same /128 within milliseconds (poolAcquireBatch=3,
	// max_egress_inflight 4), and each in-flight 429 used to push the stage, so
	// one burst reached the 2h stage (review B7).
	if until, ok := p.bad[key]; ok && now.Before(until) {
		stage := p.spent[key]
		p.muMu.Unlock()
		return spentBanLadder(stage)
	}
	p.spent[key]++
	stage := p.spent[key]
	p.muMu.Unlock()
	d := spentBanLadder(stage)
	p.markBadKeyAtLeast(key, now, d)
	return d
}

// banSpentStage advances the ladder and returns its duration WITHOUT applying
// the ban itself (callers own markBad429/hard flags).
func (p *PoolTransport) banSpentStage(key string) time.Duration {
	if key == "" {
		return 0
	}
	p.muMu.Lock()
	if p.spent == nil {
		p.spent = map[string]int{}
	}
	p.spent[key]++
	stage := p.spent[key]
	p.muMu.Unlock()
	return spentBanLadder(stage)
}

// coldTransient429 reports whether a RA-less 429 is evidence of a spent budget
// rather than a burst shaper: the egress served NOTHING in this window. 92% of
// today's transient 429s matched this shape (median oks in the prior minute =
// 0), i.e. the pool was handing out addresses that arrived already burned.
// cold2xxWindow is how far back a 2xx still counts as "this address serves".
// Measured 2026-10-22: the 2xx-based predicate covers 98.1% of transient 429s
// vs 79.5% for the response counter (ipOK also counts 4xx), i.e. 5.4k real
// spent-address events/day were invisible.
const cold2xxWindow = 60 * time.Second

func coldTransient429(recent2xx bool, raSecs int, haveRA bool) bool {
	// An EXPLICIT Retry-After (including 0) is the upstream naming a shaper
	// window: the handler below treats RA=0 as "second shaper, live budget,
	// park only". Only a RA-LESS 429 on an address that served no 2xx in the
	// last cold2xxWindow is the spent-budget signature.
	_ = raSecs
	return !recent2xx && !haveRA
}

func (p *PoolTransport) isBad(key string, now time.Time) bool {
	if key == "" {
		return false
	}
	p.muMu.RLock()
	until, ok := p.bad[key]
	if !ok {
		p.muMu.RUnlock()
		return false
	}
	if now.Before(until) {
		p.muMu.RUnlock()
		return true
	}
	p.muMu.RUnlock()
	p.muMu.Lock()
	// re-check under write lock
	if until2, ok2 := p.bad[key]; ok2 && !now.Before(until2) {
		delete(p.bad, key)
		delete(p.hardBad, key) // hard banner expired with its bad entry
	}
	p.muMu.Unlock()
	return false
}

// isParked reports whether the egress is inside a transient short-park
// window (banShort429, ~15s). Acquire does NOT skip parked egress (hands
// it out; the RoundTrip warmup-guard walks past or warms it): skipping
// in acquire cycled the pool instead of retrying the live budget.
// Warming a parked lease produced the certain-429 park->serve->429
// perpetual engine (2026-09-08: 50/50 200/429 at 18/20 FREE) — hence the
// guard releases + walks on without burning tried on the first park-pass.
func (p *PoolTransport) isParked(key string, now time.Time) bool {
	if key == "" {
		return false
	}
	p.muMu.RLock()
	until, ok := p.park[key]
	p.muMu.RUnlock()
	if !ok {
		return false
	}
	if now.Before(until) {
		return true
	}
	p.muMu.Lock()
	if until2, ok2 := p.park[key]; ok2 && !now.Before(until2) {
		delete(p.park, key)
	}
	p.muMu.Unlock()
	return false
}

// isHardBanned reports whether the egress is in a REAL ban (hours-class
// hard flag or a long cooldown). Short-parked egress (transient 429, ~15s)
// is NOT hard-banned: acquire must still hand it out — the window reopens
// in seconds, and skipping it cycled the pool (pool cycled errors) instead
// of retrying the live budget. Lazy-expiry mirrors isBad.
func (p *PoolTransport) isHardBanned(key string, now time.Time) bool {
	if key == "" {
		return false
	}
	p.muMu.RLock()
	until, ok := p.bad[key]
	if !ok {
		p.muMu.RUnlock()
		return false
	}
	hard := p.hardBad[key]
	long := now.Add(shortPark + 5*time.Second).Before(until)
	if (hard || long) && now.Before(until) {
		p.muMu.RUnlock()
		return true
	}
	if now.Before(until) {
		// Live but non-hard short entry: nothing to delete, skip the
		// write-lock upgrade (hot path under burst — review 2026-09-09).
		p.muMu.RUnlock()
		return false
	}
	p.muMu.RUnlock()
	p.muMu.Lock()
	if until2, ok2 := p.bad[key]; ok2 && !now.Before(until2) {
		delete(p.bad, key)
		delete(p.hardBad, key)
	}
	p.muMu.Unlock()
	return false
}

func (p *PoolTransport) markBadKey(key string, now time.Time) {
	p.markBadKeyFor(key, now, p.cooldownN())
}

// markBadKeyFor банит на заданный срок (не трогая hard-флаг): infra-вердикты
// (транспорт/anon-401/5xx/silent) идут через transientCooldown ~5с, серверный
// RA (minutes/hours) — через markBadSoft/markBad429 как раньше.
// markBadKeyAtLeast applies a ban that may EXTEND an existing soft deadline.
// markBadKeyFor keeps the MIN rule (a shorter cooldown must never shorten a
// live deadline), but a ladder step is evidence with a longer TTL: swallowing
// it left the log claiming 30m/2h while `bad` still held 10m (review B4).
func (p *PoolTransport) markBadKeyAtLeast(key string, now time.Time, d time.Duration) {
	if key == "" || d <= 0 {
		return
	}
	p.muMu.Lock()
	if p.bad == nil {
		p.bad = map[string]time.Time{}
	}
	until := now.Add(d)
	if cur, ok := p.bad[key]; !ok || until.After(cur) {
		if !p.hardBad[key] {
			p.bad[key] = until
		}
	}
	p.muMu.Unlock()
	p.saveState()
}

func (p *PoolTransport) markBadKeyFor(key string, now time.Time, d time.Duration) {
	if key == "" || d <= 0 {
		return
	}
	p.muMu.Lock()
	if p.bad == nil {
		p.bad = map[string]time.Time{}
	}
	// MIN-deadline + never downgrade a hard ban (как markBadKey).
	if until, ok := p.bad[key]; !ok || now.Add(d).Before(until) {
		if !p.hardBad[key] {
			p.bad[key] = now.Add(d)
		}
	}
	p.muMu.Unlock()
	p.saveState()
}

func (p *PoolTransport) markBad(proxy string, now time.Time) {
	p.markBadKey(proxy, now)
}

// unusableCooldown is how long an egress whose cluster lacks the model
// (NOT_FOUND-as-200) stays blacklisted — long, because it is a stable
// property of the egress, not transient saturation.
const unusableCooldown = 15 * time.Minute

// markUnusableKey blacklists an egress well past the 429-cooldown so the
// retry loop does not keep re-probing a cluster that never serves the model.
func (p *PoolTransport) markUnusableKey(key string, now time.Time) {
	if key == "" {
		return
	}
	p.muMu.Lock()
	if p.bad == nil {
		p.bad = map[string]time.Time{}
	}
	// Same MIN rule: never shorten a live hard ban (15m NOT_FOUND vs
	// hours-burn on the same key — the longer verdict wins).
	if until, ok := p.bad[key]; !ok || now.Add(unusableCooldown).Before(until) {
		if !p.hardBad[key] {
			p.bad[key] = now.Add(unusableCooldown)
		}
	}
	p.muMu.Unlock()
	p.saveState()
}

// markGood clears the cooldown for a working egress: a 200 proves the
// window is live, hard flag included — a stranded live window never
// rejoins rotation otherwise (2026-09-07: 14/20 banned while serving).
func (p *PoolTransport) markGood(proxy string) {
	p.markGoodKey(proxy)
}
func (p *PoolTransport) markGoodKey(key string) {
	if key == "" {
		return
	}
	p.noteIdentityGroup(key)
	p.muMu.Lock()
	_, wasBad := p.bad[key]
	hard := p.hardBad[key]
	// Global probation counts every 200 including hard: the ban deadline
	// stays (only expiry/unbanClusteredHours rejoin it), but the vote for
	// shared-window reopen must not be lost — otherwise 5 hard shadows hold
	// the 8h flag with live 200s around (2026-09-24: 107x200 vs 33x503).
	clearedGlobal := false
	if !p.globalBurnUntil.IsZero() {
		grp := triedKey(key)
		if !p.globalOKAt.IsZero() && time.Since(p.globalOKAt) <= globalProbationWindow && p.globalOKGroup != "" && grp != "" && grp != p.globalOKGroup {
			// SUSTAINED recovery across DISTINCT budgets: two 200s from
			// different /48 groups inside the probation window prove the shared window
			// reopened. Same-group repeats (2026-09-09 no-VPN: one egress
			// served 8/9 ok while the storm ran) only re-arm probation.
			p.globalBurnUntil = time.Time{}
			p.globalRA = 0
			p.globalObs = nil
			p.globalOKAt = time.Time{}
			p.globalOKGroup = ""
			clearedGlobal = true
		} else if p.globalOKAt.IsZero() || time.Since(p.globalOKAt) > globalProbationWindow || p.globalOKGroup == "" {
			p.globalOKAt = time.Now()
			p.globalOKGroup = grp
		}
		// Same-group second 200 inside the minute: keep the first arm
		// (no clear, no re-arm) — waiting for a distinct-budget proof.
	}
	if hard {
		// Hours-class deadline stays authoritative (2026-09-12 re-burn);
		// the global vote above is already counted.
		p.muMu.Unlock()
		if clearedGlobal {
			p.log().Warn("pool: global burn cleared by sustained 200s", "egress", key)
		}
		if wasBad || clearedGlobal {
			p.saveState()
		}
		return
	}
	delete(p.bad, key)
	delete(p.hardBad, key)
	delete(p.park, key)
	delete(p.verifyAt, key)
	if p.spent != nil {
		delete(p.spent, key)
	}
	p.muMu.Unlock()
	if wasBad || clearedGlobal {
		p.saveState()
	}
	if clearedGlobal {
		p.log().Warn("pool: global burn cleared by sustained 200s", "egress", key)
	}
}

// appliesTo reports whether the pool transport serves the given provider.
// Providers set (all pool-mode providers) wins over the single Provider
// name; empty Providers + empty Provider means the pool serves everyone.
func (p *PoolTransport) appliesTo(name string) bool {
	if len(p.Providers) > 0 {
		return p.Providers[name]
	}
	return p.Provider == "" || p.Provider == name
}

func (p *PoolTransport) Acquire(ctx context.Context, req RequestContext, prov *provider.Provider) (*Lease, error) {
	// Отмена клиента — не вердикт пула: без ротации/свапа/бана, сразу ctx err.
	// Раньше abort уходил в maybeRotatePool (флап 20 воркеров) и в 503-хвост
	// "context canceled", засоряя статистику.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !p.appliesTo(req.Provider) {
		return &Lease{Client: newClient(prov), Proxy: prov.Egress}, nil
	}
	acqStart := time.Now()
	l, err := p.acquireLease(ctx, p.reqSeq.Add(1), req.Model, stampFor(prov))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// No usable lease. Rotation is the only exit that mints NEW identities
		// (new WARP registrations => new /48 => fresh upstream windows), and it
		// works even while the shared-deadline flag is armed — proven live on
		// 2026-10-22, where a rotate replaced 14/14 egress IPs during a global
		// burn and traffic resumed. The old skip-if-global-burn branch here was
		// the bug: it left the pool dead until a human restarted warp-router.
		switch {
		case errors.Is(err, errNoSessionSupply):
			// Supply, not egress: rotating mints new egresses that need ids
			// nobody has. Stay put; the minter refills the harvest.
			p.log().Warn("pool: lease refused for lack of sessions — no rotate",
				"provider", req.Provider)
		case p.rotateWanted(time.Now()):
			p.healRotate("acquire failed, pool burning and silent: " + err.Error())
		default:
			p.maybeRotatePool()
		}
		// Last-resort: pool API ITSELF is dead (breaker open) — лиз взять
		// негде вообще, защищать нечего. При opt-in + свежем home отдаём
		// direct-лизу (home-IP leg): VPN вкл/выкл и рестарт пула больше
		// не кладут очередь honest-503 (2026-09-11: pool api restarting
		// сыпал 429 на каждый запрос пока пул starting — OMP парсил
		// "too many requests" в RATE_LIMIT 30s+ и убивал ретраи через
		// retry.maxDelayMs; 503 = MODEL_CAPACITY, очередь ретраит).
		// Живой пул без готовой лизы (banned/no-cold/cycled — вердикта
		// апстрима НЕТ, мы даже не спросили) — тоже 503, NEVER 429:
		// повторные 429 без вердикта OMP эскалирует в 1800s
		// credential-блоки ("Provider requested 1800000ms wait, exceeds
		// retry.maxDelayMs", 2026-09-16: пул starting → cycled → шторм
		// 429 → смерть очереди). Настоящие upstream-429 (апстрим ответил)
		// идут как 429 через RoundTrip lastResp — swap mode не тронут.
		// Горячий home — тоже honest-429 (P2-5: не трогаем ниже).
		// Parked home also blocks: transient direct-429 parks home, re-hit
		// would hammer one identity every request while pool API is dead.
		if p.DirectFallback && !p.AllowHomeIPDirect {
			p.warnHomeDirectSuppressed()
		}
		if p.homeDirectAllowed() && p.apiDown() && !p.isBad(directHomeIPKey, time.Now()) && !p.isParked(directHomeIPKey, time.Now()) {
			p.log().Warn("pool: pool API dead, handing direct home-IP last-resort lease",
				"provider", req.Provider)
			drt := &poolDirectRT{t: p, prov: req.Provider, provProvider: prov}
			cl := &http.Client{Transport: drt}
			return &Lease{
				Client: cl,
				Proxy:  "direct-fallback",
				release: func(ctx context.Context) error {
					return nil
				},
			}, nil
		}
		// Pool-API restart window (breaker open): 503, not 429 — см.
		// acquireLease выше. Живой пул: honest-429 как раньше.
		if p.apiDown() && !p.waitAPI(context.Background()) {
			return nil, &UpstreamStatusError{Status: http.StatusServiceUnavailable,
				Err: fmt.Errorf("provider %s: pool api restarting, retry shortly", req.Provider)}
		}
		// NO home-IP fallback on live-pool exhaustion: any direct dial to
		// opencode.ai exposes the home IP to the upstream, which keys
		// (account, session, home-IP) into one identity — a burned home
		// window then drags the whole pool down (observed 2026-09-10:
		// fresh main IP fixed the same pool instantly). Surface 503 (no
		// verdict — см. выше), без Retry-After (swap mode 2026-09-11).
		return nil, &UpstreamStatusError{Status: http.StatusServiceUnavailable,
			Err: fmt.Errorf("provider %s: pool has no ready egress, retry shortly (%v)", req.Provider, err)}
	}
	rt := &poolRetryRT{t: p, cur: l, prov: req.Provider, provProvider: prov, model: req.Model, stamp: stampFor(prov)}
	cl := &http.Client{Transport: rt}
	waitedMs := time.Since(acqStart).Milliseconds()
	if waitedMs >= poolWaitReportMin.Milliseconds() {
		p.log().Info("pool: lease after wait", "provider", req.Provider,
			"waited_ms", waitedMs, "proxy", l.proxy, "banned_share", fmt.Sprintf("%.2f", p.bannedShare()))
		p.poolEvent(l.proxy, "wait", true, waitedMs, "")
	}
	return &Lease{
		Client:   cl,
		WaitedMs: waitedMs,
		Proxy:    l.proxy,
		release: func(ctx context.Context) error {
			return rt.releaseCurrent(ctx)
		},
	}, nil
}

// releaseCurrent releases the current lease once (idempotent after the first
// call because cur is set to nil).
func (rt *poolRetryRT) releaseCurrent(ctx context.Context) error {
	rt.mu.Lock()
	l := rt.cur
	rt.cur = nil
	rt.mu.Unlock()
	if l == nil {
		return nil
	}
	// Never release with the caller's context: it is usually cancelled by
	// the time a stream dies (client timeout, user abort), which would
	// silently drop the lease from the pool. The pool API is local and
	// fast; a bounded background release is always better than a leak.
	return rt.t.release(context.Background(), l.leaseID)
}

// acquireLease calls the pool API and returns a lease, skipping egresses
// that are still in hours-class ban (a fresh acquire may hand out a banned
// backend again; we release it immediately and ask for another one) and
// spreading across the pool: leases handed out within spreadWindow are
// set aside, up to spreadCandidates fresh leases are gathered, and the
// least-recently-used wins. Falls back to a hot lease only when nothing
// colder is available, then to a banned fallback when the pool is
// exhausted. Every non-returned lease is released exactly once.
// leaseStamp — upstream-идентичность лизa. Для НЕ-opencode апстримов
// (kilo и прочих keyless через пул) UA берётся из провайдера
// (config user_agent, напр. kilocode/1.0), а x-opencode-session не
// штампуется: иначе запросы kilo едут в окне opencode и делят её баны/лимиты
// с opencode на тех же /48 (510х 503, 6447 upstream-429 только с kilo- slice).
type leaseStamp struct {
	ua     string // "" = pinnedUA()
	sessOK bool   // штамповать ли x-opencode-session
}

func stampFor(provProvider *provider.Provider) leaseStamp {
	if provProvider == nil {
		return leaseStamp{sessOK: true}
	}
	if isOpencodeUpstream(provProvider) {
		return leaseStamp{sessOK: true}
	}
	ua := strings.TrimSpace(provProvider.UserAgent)
	return leaseStamp{ua: ua, sessOK: false}
}

// notePoolEpoch handles a warp-router generation change on a v2 response.
// The pool reuses the same local ports across restarts (rot=0 fresh
// workers), so bans keyed by IP from the previous generation describe
// identities that no longer exist — they would park healthy egresses for
// hours. Budget-class state (storm/heat/global) is generation-independent
// and survives; identity keyed state (bad/hardBad/regionGood/regionBad/
// ipOK/ctrlProbed) does not.
func (p *PoolTransport) notePoolEpoch(e uint64) {
	if !p.PoolV2 || e == 0 {
		return
	}
	p.muMu.Lock()
	if e == p.epochSeen {
		p.muMu.Unlock()
		return
	}
	first := p.epochSeen == 0
	from := p.epochSeen
	p.epochSeen = e
	p.bad = map[string]time.Time{}
	p.park = map[string]time.Time{}
	p.heat = map[string][]int64{}
	p.heatGood = map[string]int64{}
	p.hardBad = map[string]bool{}
	p.ipOK = map[string]int{}
	p.ctrlProbed = map[string]bool{}
	p.spent = map[string]int{}
	p.verifyAt = map[string]time.Time{}
	// okAt + latch: a renewal means the fleet under us is NEW, so "recently
	// live" marks and the shared-budget latch armed against the OLD fleet are
	// both stale; leaving them made burnProbeFanout answer 3 forever and kept
	// hardBansCoverPool true after the disproof (review B2/B3).
	p.okAt = map[string]int64{}
	p.globalBurnUntil = time.Time{}
	p.globalRA = 0
	p.globalObs = nil
	p.regionGood = map[string]string{}
	p.regionBad = map[string]map[string]time.Time{}
	p.muMu.Unlock()
	if !first {
		p.log().Warn("pool: warp-router epoch changed, identity-keyed ban/pin state cleared",
			"epoch_from", from, "epoch_to", e)
	}
}

// acquireLease — обёртка над acquireLeaseInner: единственная точка, где
// выданный пулом лиз попадает в учёт одновременных запросов по egress
// (MaxEgressInflight). Освобождение симметрично в release(id).
func (p *PoolTransport) acquireLease(ctx context.Context, reqID uint64, model string, stamp leaseStamp) (*poolLease, error) {
	l, err := p.acquireLeaseInner(ctx, reqID, model, stamp)
	if err != nil || l == nil {
		return l, err
	}
	p.noteLeaseStart(l)
	return l, nil
}

func (p *PoolTransport) acquireLeaseInner(ctx context.Context, reqID uint64, model string, stamp leaseStamp) (*poolLease, error) {
	// Bound the whole spin: 10s keeps the verdict inside poolTryDeadline;
	// WaitForLease widens it so an exhausted pool can WAIT for one egress
	// instead of throwing 503 instantly — clients retry a bounded number of
	// times (OMP: 10), and a 503 that arrives in 2 ms burns a retry without
	// giving a rotate (30-90s of worker respawn) any chance to land.
	spin := 10 * time.Second
	if p.WaitForLease > 0 {
		// Explicit budget wins in BOTH directions: stands and tests set a
		// short wait, production sets a long one (default 30s).
		spin = p.WaitForLease
	}
	aCtx, cancel := context.WithTimeout(ctx, spin)
	defer cancel()
	spinStart := time.Now()
	healArmed := false
	bodyBytes, err := json.Marshal(map[string]string{"country": p.Country})
	if err != nil {
		return nil, err
	}
	var last *poolLease
	var hot []*poolLease // fresh but handed out within spreadWindow
	var cands []*poolLease
	seen := map[string]bool{}
	// noID marks egresses this request already found identity-less. A refusal
	// must not end the request while OTHER ekeys can serve: with the harvest
	// smaller than the WARP churn most fresh ekeys have no id (live 2026-10-23:
	// 6324 x 503 while 33 egresses were answering 200), and pickLease prefers
	// pinned ekeys — one more spin usually finds one.
	noID := map[string]bool{}
	// supplyErr names the REAL reason when the spin ends with identity-less
	// egresses: "pool hot" would send the operator after a pool that answers
	// 200 (live 2026-10-23: every 503 was a session-supply refusal mistyped as
	// pool-hot once the loop retried instead of failing on the first ekey).
	supplyErr := func() error {
		if len(noID) == 0 {
			return nil
		}
		return fmt.Errorf("%w: %w: no unique session available for egress (%d egresses without identity)",
			ErrTransportUnavailable, errNoSessionSupply, len(noID))
	}
	// releaseLast returns the banned-fallback lease (the one egress kept out
	// of cands/hot by isBad) to the pool, exactly once. Idempotent (last is
	// nil'd), so the defer below makes a leak structurally impossible: every
	// exit — including future ones and the early returns inside finish()'s
	// callers — releases it or hands it to the caller, never both.
	// Background context, never the caller's: it is usually cancelled by the
	// time a request dies, which would silently drop the lease (same rule as
	// releaseCurrent).
	releaseLast := func() {
		if last != nil {
			_ = p.release(context.Background(), last.leaseID)
			last = nil
		}
	}
	defer releaseLast()
	// Breaker open (pool API dead/disabled): fail fast with zero TCP.
	// Restart-window verdict is 503, NEVER 429: a 429 makes OMP parse the
	// body for wait/reset hints (uT: "too many requests" → RATE_LIMIT 30s
	// up to hours-class "reset after Ns") and park the queue past
	// retry.maxDelayMs (300s), killing retries. A 503 classifies as
	// MODEL_CAPACITY (~45-75s backoff, still swappable) — the queue retries
	// instead of dying. No Retry-After either way (swap mode 2026-09-11).
	if p.apiDown() && !p.waitAPI(ctx) {
		return nil, &UpstreamStatusError{Status: http.StatusServiceUnavailable,
			Err: fmt.Errorf("%w: pool api restarting (retry now)", ErrTransportUnavailable)}
	}
	// releaseAll releases every lease in lists except the kept one.
	releaseAll := func(keep *poolLease, lists ...[]*poolLease) {
		for _, ls := range lists {
			for _, l := range ls {
				if l != nil && l != keep && l.leaseID != "" {
					_ = p.release(context.Background(), l.leaseID)
				}
			}
		}
	}
	// finish picks the winner from gathered candidates (or fallbacks),
	// releases everything else exactly once, marks the winner used, and
	// returns it — shared by all early exits. Heat-aware: hot egresses go
	// to the tail — the swap spreads onto cold budgets first (2026-09-12).
	// Queue order: coldest heat first; hot IPs kept winning the spread pick
	// and re-burning every request before this (2026-09-12 evening storm).
	pickCool := func(ls []*poolLease) *poolLease {
		// Region pin: after a region swap this model is served in one
		// egress' region — go straight there, skip discovery (~2-5s).
		if good, ok := p.regionGoodFor(model); ok && !p.globalActive() {
			// Под латчем pin прибивает все пробы к одному egress, а переоткрытие
			// общего окна требует двух 200 из РАЗНЫХ /48 (markGoodKey).
			// Без этого латч держался ровно на RA апстрима (2026-10-21).
			for _, l := range ls {
				if p.ipKey(l.proxy, l.publicIP) == good {
					return l
				}
			}
		}
		now := time.Now()
		// Per-egress лифт шторма: 2+ свежих 200 = доказанно живой — ходит первым,
		// раньше standby/prewarm-меток (те живут 60с и могут быть несвежими).
		// Свежий вердикт бьёт верифицированную метку: это живой факт, а не кэш.
		// Гейт как у остальных тиров: битый после 200 (isBad/isParked/heat>=cap)
		// лифтом не ходит — иначе re-burn горячего окна в обход hot-очереди.
		// First-match здесь валился в concentration: 3 egress с 2+ 200
		// забирали все входы, остальные 17 гнили без проб (2026-10-06:
		// 244+171+152 на трёх при 20 up). Собираем всех живых в подслайс
		// и размазываем через pickSpread (LRU), приоритет тира не меняется.
		live := ls[:0:0]
		for _, l := range ls {
			key := p.ipKey(l.proxy, l.publicIP)
			if p.stormLiveEgress(key) && !p.isBad(key, now) && !p.isParked(key, now) && p.heatCount(key, now) < heatMaxHits {
				live = append(live, l)
			}
		}
		if len(live) > 0 {
			// Pre-emptive budget tier: an egress whose current window has
			// already served the learned budget is nearly spent, so a fresh
			// egress wins even if its heat is slightly higher. When every
			// live egress is spent, ask the pool for a fresh identity
			// generation instead of burning the last requests of each window.
			if soft := p.softBudgetLimit(); soft > 0 {
				fresh := live[:0:0]
				for _, l := range live {
					if !p.budgetLow(p.ipKey(l.proxy, l.publicIP), soft) {
						fresh = append(fresh, l)
					}
				}
				if len(fresh) > 0 {
					return fresh[p.pickSpread(fresh)]
				}
				p.maybeRotatePool()
			}
			return live[p.pickSpread(live)]
		}
		// Standby-упор: verify-поток вернул IP в строй — ходит первым,
		// раньше prewarm-меток. Остальные запросы шли в живые, пока этот
		// проверялся мимо горячего пути.
		sblive := ls[:0:0]
		for _, l := range ls {
			if p.standbyFresh(p.ipKey(l.proxy, l.publicIP), now) {
				sblive = append(sblive, l)
			}
		}
		if len(sblive) > 0 {
			return sblive[p.pickSpread(sblive)]
		}
		// Prewarm-упор: верифицированный живой egress ходит первым —
		// под нагрузкой не слепой свип, а проверенный 200 < 60с.
		// Фактор не совпадения по банам: prewarmFresh исключает
		// забаненные/запаркованные (см. метод), совпадений с heat/hot нет —
		// heat уводит битые в hot раньше, чем prewarm их верифицирует.
		pwlive := ls[:0:0]
		for _, l := range ls {
			if p.prewarmFresh(p.ipKey(l.proxy, l.publicIP), now) {
				pwlive = append(pwlive, l)
			}
		}
		if len(pwlive) > 0 {
			return pwlive[p.pickSpread(pwlive)]
		}
		best := -1
		bestHeat := 0
		for i, l := range ls {
			h := p.heatCount(p.ipKey(l.proxy, l.publicIP), now)
			if best < 0 || h < bestHeat {
				best, bestHeat = i, h
				if h == 0 {
					break
				}
			}
		}
		return ls[best]
	}
	finish := func() (*poolLease, bool) {
		if len(cands) > 0 {
			var w *poolLease
			// Region pin searches cands first, then hot (wave1 acquire P1:
			// a good-lease parked in hot must not be missed — old pickCool
			// searched cands only and silently lost the pin).
			if good, ok := p.regionGoodFor(model); ok && !p.globalActive() {
				// Под латчем pin прибивает все пробы к одному egress, а переоткрытие
				// общего окна требует двух 200 из РАЗНЫХ /48 (markGoodKey).
				// Без этого латч держался ровно на RA апстрима (2026-10-21).
				for _, l := range cands {
					if p.ipKey(l.proxy, l.publicIP) == good {
						w = l
						break
					}
				}
				if w == nil {
					for _, l := range hot {
						k := p.ipKey(l.proxy, l.publicIP)
						// Hot-пин гейтится heat/ban/park: иначе удержание
						// горячего IP давало лотерею warmest по кругу.
						if k == good && p.heatCount(k, time.Now()) < heatMaxHits && !p.isBad(k, time.Now()) && !p.isParked(k, time.Now()) {
							w = l
							break
						}
					}
				}
			}
			if w == nil {
				if len(cands) == 1 {
					w = cands[0]
				} else {
					w = pickCool(cands)
				}
			}
			releaseAll(w, cands, hot)
			if last != w {
				releaseLast()
			}
			p.markUsed(p.ipKey(w.proxy, w.publicIP))
			if len(cands)+len(hot) > 1 {
				p.log().Info("pool: acquire spread pick", "req", reqID,
					"ip", p.ipKey(w.proxy, w.publicIP),
					"heat", p.heatCount(p.ipKey(w.proxy, w.publicIP), time.Now()),
					"cands", len(cands), "hot_set_aside", len(hot))
			}
			return w, true
		}
		if len(hot) > 0 {
			// Под штормом/global hot-как-fresh запрещён, НО с heat re-check:
			// hot из-за spread/group-dup/region (остывший) — одна warmest-проба,
			// иначе 0-прогревные хвосты морят probation-датчик 2x200 и global
			// не снимается. Истинно горячий (heat/bad/park) — честный хвост.
			if p.stormActive() || p.globalActive() {
				// F4: сканируем ВЕСЬ hot на первый cool — pickSpread выбирает
				// по usedAt (LRU), а не по heat: проверка только warmest давала
				// ложный honest-tail при живом cool в том же hot.
				var w *poolLease
				wk := ""
				wheat := 0
				wnow := time.Now()
				for _, h := range hot {
					hk := p.ipKey(h.proxy, h.publicIP)
					hheat := p.heatCount(hk, wnow)
					if hheat < heatMaxHits && !p.isBad(hk, wnow) && !p.isParked(hk, wnow) {
						w, wk, wheat = h, hk, hheat
						break
					}
				}
				if w == nil {
					// Half-open (2026-10-06): 0-прогревные хвосты держали
					// шторм вечно — stormOKs не росли без проб, а heat 3м /
					// region 10м за 5с-паузу не остывают (сон бесполезен).
					// Вместо сна — ОДИН прогрев warmest hot: окна умеют
					// переоткрываться раньше бана, 200 сразу кормит stormOKs
					// и снимает шторм, 429 паркует egress как обычно.
					// Re-warm исключён: выдаём ровно 1 лизу, остальные release.
					// Region-отвергнутые пропускаем: region-403 не пишет heat,
					// такой egress систематически выигрывал бы min-heat и сжигал
					// пробу в гарантированный region-403 вместо теста окна.
					// Весь hot отвергнут регионом — честный хвост (проба бессмысленна).
					var warmest *poolLease
					warmestK, warmestH := "", 0
					wnow = time.Now()
					found := false
					for _, h := range hot {
						hk := p.ipKey(h.proxy, h.publicIP)
						if model != "" && p.isRegionBad(model, hk, wnow) {
							continue
						}
						if hh := p.heatCount(hk, wnow); !found || hh < warmestH {
							warmest, warmestK, warmestH, found = h, hk, hh, true
						}
					}
					// Часовые hard-баны не прогреваем: окно закрыто надолго,
					// проба — чистый re-warm. Парки/heat — прогреваем.
					if found && !p.isBad(warmestK, wnow) {
						w, wk, wheat = warmest, warmestK, warmestH
						p.log().Info("pool: all hot under storm, half-open probe",
							"req", reqID, "ip", wk, "hot", len(hot), "heat", wheat)
					}
				}
				if w == nil {
					p.log().Info("pool: all hot under storm, honest tail (no re-warm)",
						"req", reqID, "hot", len(hot))
					releaseAll(nil, hot)
					releaseLast()
					return nil, false
				}
				releaseAll(w, hot)
				if last != w {
					releaseLast()
				}
				p.markUsed(wk)
				p.log().Info("pool: acquire all hot, storm single warmest probe",
					"req", reqID, "ip", wk, "hot", len(hot), "heat", wheat)
				return w, true
			}
			w := hot[p.pickSpread(hot)]
			releaseAll(w, hot)
			if last != w {
				releaseLast()
			}
			p.markUsed(p.ipKey(w.proxy, w.publicIP))
			p.log().Info("pool: acquire all hot, taking warmest",
				"req", reqID, "ip", p.ipKey(w.proxy, w.publicIP), "hot", len(hot))
			return w, true
		}
		return nil, false
	}
	// lastEmptyEpoch != 0 means the pool answered 200 with zero leases at least
	// once: after retries exhaust, surface the DISTINCT empty-batch error
	// (contract TestPoolV2EmptyBatchKeepsBans) instead of a generic pool-hot one.
	var lastEmptyEpoch uint64
	// softWaits bounds the "wait out a short park" budget per acquire call.
	softWaits := 0
	maxSpins := 24
	if p.WaitForLease > 0 {
		maxSpins = 400 // bounded by spinStart+aCtx, not by the counter
	}
	for attempts := range maxSpins {
		// Fast-path: a lone cold candidate wins immediately — same cost as
		// before (one acquire per request). Extra acquires happen ONLY on
		// hot collisions (burst on one backend): then gather up to
		// spreadCandidates and pick the coldest.
		// Wave 2026-09-20: в горячем пуле не крутим 24 спина в стену —
		// после 8 пустых спинов (всё bad/park/heat) сразу wait-reopen:
		// окна переоткрываются за секунды, следующий acquire заберёт
		// оживший egress. Экономия 5-8с на горячих запросах.
		if len(cands) == 1 && len(hot) == 0 && attempts >= 1 {
			w := cands[0]
			if last != w {
				releaseLast()
			}
			p.markUsed(p.ipKey(w.proxy, w.publicIP))
			return w, nil
		}
		if attempts >= 8 && len(cands) == 0 && len(hot) == 0 {
			releaseLast()
			if p.WaitForLease > 0 && aCtx.Err() == nil && time.Since(spinStart) < spin-spin/4 {
				// Everything is banned/parked/cycled. Do not 503 yet: keep the
				// caller's request alive while a rotate mints new identities and
				// the worker respawn settles (30-90s live). The journal marks
				// these as served-after-wait, so success rate stays honest.
				if p.globalActive() || p.bannedShare() >= 0.5 {
					p.healRotate("acquire spin: pool burned, waiting for one egress")
				}
				select {
				case <-time.After(700*time.Millisecond + time.Duration(rand.IntN(400))*time.Millisecond):
				case <-aCtx.Done():
					if lastEmptyEpoch != 0 {
						return nil, fmt.Errorf("%w: pool v2 empty leases batch (epoch %d)", ErrTransportUnavailable, lastEmptyEpoch)
					}
					return nil, fmt.Errorf("%w: pool hot, %d seen", ErrTransportUnavailable, len(seen))
				}
				continue
			}
			if lastEmptyEpoch != 0 {
				return nil, fmt.Errorf("%w: pool v2 empty leases batch (epoch %d)", ErrTransportUnavailable, lastEmptyEpoch)
			}
			if e := supplyErr(); e != nil {
				return nil, e
			}
			return nil, fmt.Errorf("%w: pool hot, %d seen", ErrTransportUnavailable, len(seen))
		}
		// review_2 F5: cands>0 при горевших нет смысла крутить до 24 —
		// finish() соберёт то же самое после 8 спинов (экономия спинов).
		if attempts >= 8 && len(cands) > 0 {
			if w, ok := finish(); ok {
				return w, nil
			}
		}
		if len(cands) >= spreadCandidates {
			if w, ok := finish(); ok {
				return w, nil
			}
		}
		if !healArmed && attempts == 0 && p.burnCollapse(time.Now()) {
			healArmed = true
			p.healRotate("acquire: burn collapse at request start")
		}
		acqURL := strings.TrimRight(p.APIURL, "/") + "/api/v1/pool/acquire"
		if p.PoolV2 {
			acqURL = strings.TrimRight(p.APIURL, "/") + "/api/v2/pool/acquire"
			// Без affinity: с "affinity":"<model>" warp v2 отдавал РОВНО ОДИН
			// лиз, всегда тот же самый (живой лог 2026-10-21 14:05-14:10:
			// 60+ acquire → один ip 2a09:bac5:48f0:1f5::32:112, "all hot,
			// taking warmest hot=1", ноль spread) — пул сжимался до одного
			// egress, потолок ёмкости = 1 IP, и batch-выбор не работал.
			bodyBytes = []byte(`{"n":` + strconv.Itoa(poolAcquireBatch) + `,"ttl_s":300}`)
		}
		p.apiVerOnce.Do(func() {
			if p.PoolV2 {
				p.log().Info("pool: api=v2 (batch lease selection active)", "url", acqURL, "batch", poolAcquireBatch)
			} else {
				p.log().Info("pool: api=v1 (single lease, batch selection inert)", "url", acqURL)
			}
		})
		r, err := http.NewRequestWithContext(aCtx, http.MethodPost, acqURL, bytes.NewReader(bodyBytes))
		if err != nil {
			releaseAll(nil, cands, hot)
			releaseLast()
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		if tok := p.effectiveToken(); tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := p.client().Do(r)
		if err != nil {
			// Разделяем смерть API и забитость пула: connection-refused /
			// timeout ДО ответа = API мёртв (брейкер); timeout/context-cancel
			// ПОСЛЕ борьбы за лизу при живом API = пул забит (wait-reopen,
			// брейкер не трогаем — иначе 3 забитых спина открывают apiDown
			// на 30-120с и все запросы получают 503 разом, хотя :8787 жив
			// и acquire отвечает 200, 2026-09-22 3418x restarting).
			if isPoolAPIDead(err) {
				p.noteAPIFail(false)
			} else {
				p.log().Info("pool: api busy, no breaker trip",
					"req", reqID, "err", err.Error())
			}
			releaseAll(nil, cands, hot)
			releaseLast()
			return nil, err
		}
		// Status 409/429/503 BEFORE json.Decode: 409 body is empty -> EOF never-handled.
		if res.StatusCode == http.StatusConflict {
			// A 409 carrying "pool not enabled" is STABLE (pool never
			// autostarted, e.g. port bind lost after reboot): sleep-looping
			// on it burns 10s + a dozen sockets per request for nothing.
			// Fail fast toward the long breaker; the throttled rotate
			// keeps attempting revival. Plain 409s (respawn jitter
			// 600-900ms) keep the old transient loop and never trip it.
			peek, _ := io.ReadAll(io.LimitReader(res.Body, 512))
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if bytes.Contains(bytes.ToLower(peek), []byte("not enabled")) {
				p.noteAPIFail(true)
				releaseAll(nil, cands, hot)
				releaseLast()
				return nil, fmt.Errorf("%w: pool not enabled", ErrTransportUnavailable)
			}
			// warp-router respawns whole pool on rotate (30-90s): 409 is transient jitter 600-900ms.
			jitter := time.Duration(600+rand.IntN(301)) * time.Millisecond
			select {
			case <-time.After(jitter):
			case <-aCtx.Done():
				releaseAll(nil, cands, hot)
				releaseLast()
				if e := supplyErr(); e != nil {
					return nil, e
				}
				return nil, aCtx.Err()
			}
			continue
		}
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			select {
			case <-time.After(400 * time.Millisecond):
			case <-aCtx.Done():
				releaseAll(nil, cands, hot)
				releaseLast()
				if e := supplyErr(); e != nil {
					return nil, e
				}
				return nil, aCtx.Err()
			}
			continue
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			// Auth rejections never heal by retrying: long breaker.
			// Other pool-API errors: short breaker after threshold.
			p.noteAPIFail(res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden)
			releaseAll(nil, cands, hot)
			releaseLast()
			return nil, fmt.Errorf("%w: pool status %d", ErrTransportUnavailable, res.StatusCode)
		}
		var out poolLeaseResponse
		decodeErr := json.NewDecoder(res.Body).Decode(&out)
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if decodeErr != nil {
			releaseAll(nil, cands, hot)
			releaseLast()
			return nil, decodeErr
		}
		// warp-router v2 wraps the lease ({"epoch":N,"leases":[{...}]});
		// take the first lease, keeping the top-level epoch when the
		// inner one omits it. Extra leases (batch over-provision) are
		// released immediately — a forgotten lease_id pins a warp slot
		// until TTL (300s).
		if out.LeaseID == "" && len(out.Leases) > 0 {
			topEpoch := out.Epoch
			p.log().Info("pool: acquire batch", "req", reqID, "n", len(out.Leases),
				"distinct48", distinctGroups(out.Leases, p))
			chosen, rest := p.pickLease(out.Leases, time.Now(), stamp.sessOK)
			for _, extra := range rest {
				if extra.LeaseID != "" {
					_ = p.release(context.Background(), extra.LeaseID)
				}
			}
			out = chosen
			if out.Epoch == 0 {
				out.Epoch = topEpoch
			}
		}
		if out.LeaseID == "" {
			releaseAll(nil, cands, hot)
			releaseLast()
			if p.PoolV2 && len(out.Leases) == 0 {
				lastEmptyEpoch = out.Epoch
				// 200 с пустым батчем = ВСЕ слоты заняты (acquire его отдаёт,
				// когда inflight-лимит выбран), а не «пул сломан». Раньше это
				// был мгновенный 503 «no ready egress» — именно тот класс, из-за
				// которого «всё падает при малейшей нагрузке» (2026-10-21:
				// 7 мгновенных 503 в 13:45, пока пул ждал освобождения слота).
				// Ждём слот как на 429/503, в пределах aCtx (10 c) и tryCount.
				select {
				case <-time.After(400*time.Millisecond + time.Duration(rand.IntN(200))*time.Millisecond):
				case <-aCtx.Done():
					return nil, fmt.Errorf("%w: pool v2 empty leases batch (epoch %d)", ErrTransportUnavailable, out.Epoch)
				}
				continue
			}
			return nil, fmt.Errorf("%w: pool lease missing lease_id", ErrTransportUnavailable)
		}
		if out.ProxyURL == "" {
			releaseAll(nil, cands, hot)
			releaseLast()
			return nil, fmt.Errorf("%w: empty pool lease", ErrTransportUnavailable)
		}
		// Epoch invalidates identity-keyed bans — only on a VALID lease.
		// An empty/error batch must not wipe live bans (churn unban loop).
		p.notePoolEpoch(out.Epoch)
		// Pool API answered with a usable lease: it is alive, close any
		// tripped breaker immediately (revival must not wait out a
		// cooldown while leases flow again).
		p.noteAPISuccess()
		l := &poolLease{leaseID: out.LeaseID, proxy: out.ProxyURL, publicIP: out.PublicIP, country: out.Country}
		ekey0 := p.ipKey(out.ProxyURL, out.PublicIP)
		if noID[ekey0] {
			_ = p.release(context.Background(), out.LeaseID)
			continue
		}
		// Pool-side liveness for the session pool: a pin whose egress the pool
		// no longer hands out is dead weight and starves new ekeys.
		if stamp.sessOK {
			getSessionPool().NoteEgressSeen(ekey0)
		}
		// Провайдер-специфичная идентичность: не-opencode апстрим получает
		// свой UA и без x-opencode-session (см. stampFor).
		if stamp.ua != "" {
			l.ua = stamp.ua
		} else {
			l.ua = pinnedUA()
		}
		if stamp.sessOK {
			// Pool lane: ONLY a genuine harvested id. When the harvest is
			// exhausted the egress has no identity — refuse it rather than
			// share one across IPs or ship a fabricated ses_* (the gate
			// rejects generated ids on every egress).
			l.session = getSessionPool().NextFor(ekey0)
			if l.session == "" {
				_ = p.release(context.Background(), out.LeaseID)
				noID[ekey0] = true
				p.log().Warn("pool: no free session for egress — lease refused",
					"ip", ekey0, "prefix", v48(ekey0), "distinct_no_id", len(noID))
				// Try another egress before failing the request: 12 distinct
				// identity-less ekeys means the supply really is exhausted.
				if len(noID) >= 12 {
					return nil, fmt.Errorf("%w: %w: no unique session available for egress",
						ErrTransportUnavailable, errNoSessionSupply)
				}
				continue
			}
		}
		// Dedup by ProxyURL not ipKey: V4 anycast collapses 20 backends to 1 IP (20→1).
		dedupKey := out.ProxyURL
		if seen[dedupKey] {
			// Pool cycled back to seen proxy: all distinct backends exhausted.
			_ = p.release(context.Background(), out.LeaseID)
			if w, ok := finish(); ok {
				p.log().Info("pool: acquire cycled back to seen proxy",
					"proxy", dedupKey, "ip", p.ipKey(out.ProxyURL, out.PublicIP), "prefix", v48(p.ipKey(out.ProxyURL, out.PublicIP)), "seen", len(seen), "attempt", attempts)
				return w, nil
			}
			releaseLast()
			return nil, fmt.Errorf("%w: pool cycled, %d seen", ErrTransportUnavailable, len(seen))
		}
		seen[dedupKey] = true
		// Skip egress inside ANY live ban (hours hard, minute soft, no-RA
		// soft), keyed by exact public IP. Short-parked egress (transient
		// 429, shortPark 5s window) is NOT skipped: the window reopens in seconds
		// and skipping it caused acquire-cycling (pool cycled) instead of
		// retrying the live budget. Minute-soft (RA60) MUST skip: without
		// it acquire re-hands the same hot budgets as fresh every sweep
		// (2026-09-14 death: 87 RA60 in 7 min, bad 12, pool cycled 4-5).
		ekey := p.ipKey(out.ProxyURL, out.PublicIP)
		if p.isBad(ekey, time.Now()) {
			p.muMu.RLock()
			until := p.bad[ekey]
			hard := p.hardBad[ekey]
			okN := p.ipOK[ekey]
			p.muMu.RUnlock()
			p.log().Info("pool: acquire skip banned IP",
				"req", reqID, "ip", ekey, "prefix", v48(ekey), "hard", hard, "until", until.Format(time.RFC3339), "ok_before", okN, "seen", len(seen))
			// Короткий софт-парк (не hard): окно откроется через секунды.
			// Если это ЕДИНСТВЕННЫЙ лиз (affinity-пин отдаёт 1), прежний код
			// сразу уходил в "pool cycled" и отдавал 503 за 5-17 мс без
			// единой попытки к апстриму — 6 подряд в живом логе 13:48
			// (ревью 2026-10-21). Ждём открытия окна, но не дольше 3 с
			// суммарно и только для soft-парков.
			if !hard {
				// Wait the ban OUT instead of cycling the batch: a 10s
				// minute-ban used to skip this 3s-bounded wait, cycle, and
				// answer 503 in 5-11 ms while WaitForLease sat unused (live
				// 2026-10-22 19:21: 18x503 "pool cycled, N seen" under a
				// 20-way burst). Bounded per wait (softWaitCap) and in total
				// (softWaits), and clamped by the acquire context.
				if w := time.Until(until); w > 0 && w <= softWaitCap && softWaits < 3 {
					softWaits++
					select {
					case <-time.After(w):
					case <-aCtx.Done():
					}
				}
			}
			if last == nil {
				last = l
			} else {
				_ = p.release(context.Background(), out.LeaseID)
			}
			continue
		}
		// Region memory (model-scoped): an egress that rejected THIS model
		// with region_error is deprioritized for it — but stays fully
		// usable for every other model (no ban: region support is per
		// (model, region), not a burned window). Hot queue, not skip:
		// finish() uses it only when nothing colder exists.
		if model != "" && p.isRegionBad(model, ekey, time.Now()) {
			hot = append(hot, l)
			continue
		}
		// Heat cap: an egress with heatMaxHits+ 429s inside heatWindow is NOT
		// handed out as fresh — park it in the hot queue instead of candidates.
		// fast-path) and every request re-burned a full warmup on a dead budget
		// (2026-09-13 midday: 1800+ 429 at x4-7 tempo, all p50 ~1.2s single
		// warmups, pool never rotated away because nothing recorded the heat).
		if p.heatCount(ekey, time.Now()) >= heatMaxHits {
			hot = append(hot, l)
			// Под штормом/global не копим 24 спина: 3 hot при пустых cands =
			// доказанный all-hot — но только если ВЕСЬ hot истинно горячий.
			// Смешанный hot (heat + spread/group-dup/region-cooled) без re-check
			// ложно-хвостил с 0 прогревами и морил probation-датчик 2x200
			// (F1): сканируем hot тем же предикатом, что finish-проба; найден
			// cooled — копить спин дальше до finish, а не тейлить.
			if len(cands) == 0 && len(hot) >= 3 && (p.stormActive() || p.globalActive()) {
				cooled := false
				for _, h := range hot {
					hk := p.ipKey(h.proxy, h.publicIP)
					hnow := time.Now()
					if p.heatCount(hk, hnow) < heatMaxHits && !p.isBad(hk, hnow) && !p.isParked(hk, hnow) {
						cooled = true
						break
					}
				}
				if !cooled {
					// Half-open (2026-10-06): как в finish-хвосте — ищем
					// warmest hot на 1 прогрев вместо хвоста. Часовые баны
					// не трогаем (re-warm), парки/heat — прогреваем.
					// Region-отвергнутые пропускаем (см. finish-ветку выше).
					// Без перетасовки cands/hot: cooled=true продолжает спин,
					// finish() соберёт hot естественным путём.
					warmestK, warmestH := "", 0
					hnow := time.Now()
					found := false
					for _, h := range hot {
						hk := p.ipKey(h.proxy, h.publicIP)
						if model != "" && p.isRegionBad(model, hk, hnow) {
							continue
						}
						if hh := p.heatCount(hk, hnow); !found || hh < warmestH {
							warmestK, warmestH, found = hk, hh, true
						}
					}
					if found && !p.isBad(warmestK, hnow) {
						p.log().Info("pool: storm hot pileup, half-open continue",
							"req", reqID, "ip", warmestK, "hot", len(hot), "heat", warmestH)
						cooled = true
					}
				}
				if !cooled {
					p.log().Info("pool: storm hot pileup, honest tail (no 24-spin)",
						"req", reqID, "hot", len(hot))
					releaseAll(nil, hot)
					releaseLast()
					return nil, fmt.Errorf("%w: pool all hot under storm, %d hot", ErrTransportUnavailable, len(hot))
				}
			}
			continue
		}
		// this request unless everything gathered is hot.
		// Region pin: this model is served in this egress' region (pinned
		// after a swap) — take it IMMEDIATELY, no spread gathering, no
		// spread-window deferral. The whole point of the pin is skipping
		// the discovery sweep; waiting burns the saved latency. (Also:
		// with regionBad siblings parked in hot, the cold set may never
		// reach spreadCandidates and the spin would hit the 10s budget on
		// an empty pool.)
		if model != "" {
			if good, ok := p.regionGoodFor(model); ok && good == ekey && !p.globalActive() {
				// Третья точка пина (ревью 2026-10-21): под латчем ранний возврат
				// в пин-egress не давал набрать второй голос из другой /48.
				releaseAll(l, cands, hot)
				if last != l {
					releaseLast()
				}
				p.markUsed(ekey)
				return l, nil
			}
		}
		// Spread: a lease handed out within the window is set aside for
		// this request unless everything gathered is hot.
		if p.usedWithin(ekey, spreadWindow) {
			hot = append(hot, l)
			continue
		}
		// Group-aware spread: warp LRU hands back same-/48 backends as
		// fresh by design; cands must be distinct budget groups, otherwise
		// 3 leases collapse to 1 budget and tried floor never fills.
		// F3: same-/48 сиблинг горячего hot-члена — тоже в hot: heat keyed
		// exact-IP и слеп к shared-/48 бюджету, иначе B(heat 0) греет бюджет
		// G уже горящего A(heat 2) как "холодный". Только v6-группы, только
		// heat/bad-источник (region/spread-дубли не заражают).
		if g := triedKey(ekey); g != "" {
			dup := false
			for _, c := range cands {
				if triedKey(p.ipKey(c.proxy, c.publicIP)) == g {
					dup = true
					break
				}
			}
			if !dup {
				hnow := time.Now()
				for _, h := range hot {
					hk := p.ipKey(h.proxy, h.publicIP)
					if triedKey(hk) == g && (p.heatCount(hk, hnow) >= heatMaxHits || p.isBad(hk, hnow)) {
						dup = true
						break
					}
				}
			}
			if dup {
				hot = append(hot, l)
				continue
			}
		}
		cands = append(cands, l)
	}
	if w, ok := finish(); ok {
		return w, nil
	}
	releaseLast()
	if lastEmptyEpoch != 0 {
		return nil, fmt.Errorf("%w: pool v2 empty leases batch (epoch %d)", ErrTransportUnavailable, lastEmptyEpoch)
	}
	return nil, fmt.Errorf("%w: no cold lease after %d acquires", ErrTransportUnavailable, 24)
}
func (p *PoolTransport) release(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	// Симметрично acquireLease: любой путь возврата лиза (releaseCurrent,
	// ротация в ретрае, standby/prewarm) снимает счётчик одновременных.
	p.noteLeaseEnd(id)
	var b *bytes.Buffer
	relURL := strings.TrimRight(p.APIURL, "/") + "/api/v1/pool/release"
	if p.PoolV2 {
		relURL = strings.TrimRight(p.APIURL, "/") + "/api/v2/pool/release"
		b = bytes.NewBufferString(`{"lease_ids":["` + id + `"]}`)
	} else {
		b = bytes.NewBufferString(`{"lease_id":"` + id + `"}`)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, relURL, b)
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	if tok := p.effectiveToken(); tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := p.client().Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("pool release status %d", res.StatusCode)
	}
	return nil
}

func (p *PoolTransport) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// RoundTrip tries up to Fanout fresh leases per round, Rounds rounds, until a
// non-retryable response arrives. Bound the whole retry phase to
// poolTryDeadline so a saturated pool still yields a verdict — but long-
// thinking free models (upstream 20-90s+ before first token) need headroom:
// poolHeaderThinkTimeout 90s covers header wait (thinking), probe+sniff add
// up to ~18s, so the budget stays above think+headroom: a single slow header
// must not eat the whole retry window (healthy egresses got ms-remainders
// and false cools). 5×2 fanout retries fit inside gateway 180s. Sniff 15s
// only guards silent bodies after headers.
// poolTryDeadline: полный бюджет ретраев внутри провайдера
// (2026-09-16: клиент 429/403 не видит — запрос пережидает минутные баны
// ре-свипами внутри провайдера, хвост — честный 503 для ретрая очереди).
// СТРОГО меньше gateway 180s: verdict обязан вернуться до того, как gateway
// отрежет клиента (300s висел дольше gateway, verdict приходил в пустоту,
// клиент видел 502 вместо честного 503). 165s ≈ 12 ре-свипов + один
// 150s think-коридор; handoff до gateway 15s (verdict пишется быстро,
// direct-leg тоже клампится к poolTry-дедлайну, наружу не вылезает).
// softWaitCap bounds ONE soft-park wait inside a sweep. 3s covered only the
// 5s parks; the upstream's minute-bans (10s measured) fell through it and the
// batch cycled to an instant 503 (review: zero-chance-503 class).
const softWaitCap = 12 * time.Second

// poolAcquireBatch is how many leases one v2 acquire asks warp-router for.
// The batch is the rotation currency: warp hands out a mix of healthy egresses
// and /48s that are already hard-banned for hours, so a single-lease draw turns
// one unlucky pick into an honest "pool saturated" while healthy groups sit
// idle. Extras are released immediately (a held lease_id pins a warp slot for
// its TTL).
const poolAcquireBatch = 1

// distinctGroups counts distinct /48 groups inside a v2 batch: diversity is the
// thing that bounds load capacity (a batch of three leases from one group is
// one budget, not three).
func distinctGroups(ls []poolLeaseResponse, p *PoolTransport) int {
	seen := map[string]struct{}{}
	for _, l := range ls {
		seen[v48(p.ipKey(l.ProxyURL, l.PublicIP))] = struct{}{}
	}
	return len(seen)
}

// pickLease chooses the healthiest lease of a v2 batch: an egress that is
// neither banned nor parked, coldest heat first, and only then falls back to
// the first entry. Live 2026-10-21: 20 backends in 8 /48 groups, 6 IPs
// hard-banned for ~14h, traffic still 503'd "saturated (1 throttled)" because
// the request took whatever lease warp listed first — usually a banned /48.
func (p *PoolTransport) pickLease(ls []poolLeaseResponse, now time.Time, preferPin bool) (poolLeaseResponse, []poolLeaseResponse) {
	// Tie-break по последнему использованию: у здоровых лизов heat=0, иначе
	// всегда побеждал ПЕРВЫЙ лиз батча (warp отдаёт стабильный порядок), т.е.
	// ротации не было вовсе — живой лог 2026-10-21 14:17-14:22: distinct48=3
	// в каждом батче, но 13 из 14 попыток на одном IP.
	// Preference ladder (2026-10-22): (1) not banned/parked, (2) served a 200
	// recently — the burned/live split inside one batch is invisible otherwise,
	// so pickLease drew a lottery while half the pool was serving, (3) coldest
	// heat, (4) LRU.
	best, bestHeat, bestBad, bestStale := -1, 0, true, true
	var bestUsed time.Time
	bestHasID := false
	for i, l := range ls {
		if l.LeaseID == "" || l.ProxyURL == "" {
			continue
		}
		key := p.ipKey(l.ProxyURL, l.PublicIP)
		// Identity first: a lease whose egress owns no session id CANNOT serve
		// the pool lane (the refusal is deliberate — no id sharing across IPs),
		// so handing it out burns a round trip. With the harvest smaller than
		// the WARP churn this was the 503 storm of 2026-10-23 (6324 x 503 while
		// 33 egresses were serving fine).
		hasID := !preferPin || getSessionPool().HasPin(key)
		bad := p.isBad(key, now) || p.isParked(key, now)
		age, seen := p.okAge(key)
		stale := !seen || age > okFreshWindow
		h := p.heatCount(key, now)
		used := p.lastUsedAt(key)
		var better bool
		switch {
		case best == -1:
			better = true
		case hasID != bestHasID:
			better = hasID
		case bad != bestBad:
			better = bestBad && !bad
		case stale != bestStale:
			better = bestStale && !stale
		case h != bestHeat:
			better = h < bestHeat
		default:
			// Sticky (review M5): among LIVE egresses keep the one that served
			// most recently. Measured 2026-10-22: the pool abandoned live
			// egresses after 23.6 200s/run while clean runs reached 111 (4.7x
			// early) and every re-draw spends a fresh /64 budget — the unit the
			// upstream actually meters. Among STALE candidates LRU stays: the
			// only way to find a live one is to rotate the warmups.
			if !stale && !bestStale && !bestUsed.IsZero() {
				better = used.After(bestUsed)
			} else {
				better = used.Before(bestUsed)
			}
		}
		if better {
			best, bestBad, bestStale, bestHeat, bestUsed = i, bad, stale, h, used
			bestHasID = hasID
		}
	}
	if best == -1 {
		return ls[0], ls[1:]
	}
	// Диагностика выбора батча: без неё не видно, ПОЧЕМУ побеждает один и тот
	// же лиз (bad/parked/heat/used). Семплируем 1 из 8 вызовов.
	if len(ls) > 1 && p.pickLogN.Add(1)%8 == 0 {
		parts := make([]string, 0, len(ls))
		for _, l := range ls {
			k := p.ipKey(l.ProxyURL, l.PublicIP)
			age, seen := p.okAge(k)
			live := seen && age <= okFreshWindow
			parts = append(parts, fmt.Sprintf("%s{bad=%t park=%t live=%t heat=%d used=%s}",
				v48(k), p.isBad(k, now), p.isParked(k, now), live, p.heatCount(k, now),
				p.lastUsedAt(k).Format("15:04:05")))
		}
		p.log().Info("pool: pick detail", "chosen", v48(p.ipKey(ls[best].ProxyURL, ls[best].PublicIP)), "batch", strings.Join(parts, " "))
	}
	rest := make([]poolLeaseResponse, 0, len(ls)-1)
	rest = append(rest, ls[:best]...)
	rest = append(rest, ls[best+1:]...)
	return ls[best], rest
}

var poolTryDeadline = 100 * time.Second

// poolHeaderThinkTimeout: ожидание headers на одном attempt (думание модели
// ПОСЛЕ отправки запроса). WARP-туннели сами не сыплются (0 транспортных
// обрывов за ночь при 7173×200, 2026-09-19) — тишина headers это думание,
// а не мёртвый egress. Резать его баном неверно: окно уже потрачено
// (warmup сожжён), а вердикта ноль. Пробой think-таймаута НЕ банит egress
// (см. isThinkTimeout в err-ветке ниже), только свапает дальше.
const poolHeaderThinkTimeout = 90 * time.Second

// poolSmallThinkWait bounds the header wait of a SMALL request. A hung egress
// used to consume the WHOLE retry budget: live 2026-10-23 19:00-19:30 all 30
// 503s were "pool retry budget exhausted (100s) with no upstream response"
// while other egresses were answering in 2s — the first attempt ate 90s of
// think time and left 10s, so the serving egress never got a turn. A small
// request never legitimately needs 90s of prefill (measured header_ms: 192 of
// 252 under 5s, 49 at 5-20s, 10 at 20-60s); a large one does (the operator's
// 645k-token session), so the long wait stays for it.
const (
	poolSmallThinkWait   = 45 * time.Second
	poolSmallPromptBytes = 256 << 10
)

// poolThinkWait returns the header wait for one attempt: the size-aware cap,
// never more than what the retry budget has left.
func poolThinkWait(remaining time.Duration, contentLen int64) time.Duration {
	w := poolHeaderThinkTimeout
	if contentLen >= 0 && contentLen < poolSmallPromptBytes {
		w = poolSmallThinkWait
	}
	if remaining > 0 && remaining < w {
		w = remaining
	}
	return w
}

// minWalkEgress bounds the walk-floor loop extension (attempt budget): the
// loop runs at most this many upstream warmups per request. The HONESTY
// gate itself counts /48 budget groups (triedExhaustFloor), not attempts:
// live budgets are ~9, so 7 groups is the honest near-full sweep while
// ~10 attempts (5x2 default) give repeats room to resolve. A full 20-sweep
// burned 20 upstream windows per logical 429 (2026-09-11 storm: one failed
// request parked the whole pool). Bounded by poolTryDeadline: the sweep
// stops early on budget expiry with whatever honest verdict we have.
const minWalkEgress = 9

// triedExhaustFloor gates the tried-map short-circuit: a cycled-back lease
// proves exhaustion only after this many DISTINCT budget groups were probed
// in this request (or 3 repeats in a row — see repeatHits reset below). A
// single repeat means the pool cycled, not that every egress is hot.
// Tried keys by /48 budget group (triedKey): live budgets are ~9 (warp
// 20 backends on 9x/48), so 7 of ~9 is the honest near-full sweep.
const triedExhaustFloor = 7

// isSyntheticEgressKey reports artificial egress keys that do not represent a
// WARP identity (home-IP direct leg and friends).
func isSyntheticEgressKey(key string) bool {
	return key == directHomeIPKey || key == "direct-fallback"
}

// egressSourceHeader marks WHERE a response was actually served (pool lane vs the
// direct home-IP leg) so the request log can never hide the difference.
const egressSourceHeader = "X-AI-Router-Egress"

// directHomeIPKey is the artificial egress key for the home-IP direct
// fallback: it gets the same 429/RA cooldown treatment as pool egresses so a
// burned home window isn't hammered by every exhausted request.
const directHomeIPKey = "direct-home-ip"

// Saturation escape hatches (incident 2026-10-17 19:18-19:51: 27 minutes of
// 1x1 -> 429 -> 503 while the home-IP leg served 200s, because every
// internal exit was unreachable in that mode).
const (
	// poolEmptyGrace: no pooled 200 for this long AND a saturation tail on
	// record => the pool is alive but silent; the direct home-IP leg may
	// take over instead of returning 503 forever.
	poolEmptyGrace = 20 * time.Second
	// poolLiveGrace: a pooled 200 younger than this proves the pool is ALIVE,
	// so a burn latch (which may have been armed by one burned egress) must not
	// collapse the sweep to a single probe. Measured 2026-10-22: the latch
	// stayed armed for hours while 12 of 20 egresses served.
	poolLiveGrace = 15 * time.Second
	// okFreshWindow: how long a 200 keeps an egress "recently live" for the
	// lease picker. Upstream windows measured today stayed open for hours, so
	// ten minutes of memory is conservative — it only breaks ties between
	// egresses that carry no other evidence.
	okFreshWindow = 10 * time.Minute
	// promisingWindow: how long a 2xx keeps the pool "worth probing". Upstream
	// windows measured today stay open for hours while the live cadence is
	// 1-16 req/min, so using the picker's 10-minute freshness window as the
	// probe promise collapsed the walk to fanout=1 after every idle gap -
	// exactly when survivors should be searched.
	promisingWindow = 2 * time.Hour
	// clusterBanCap bounds a per-egress ban whose hours-class RA was ALSO reported
	// by another /48: shared deadlines must not strand live egresses for 12+ hours.
	clusterBanCap = 10 * time.Minute
	// satRotateAfter: consecutive saturated tails before the pool is
	// re-armed by rotation. The old gate (`rateLimitHits >= 3`) is
	// unreachable in the 1x1 fail-fast mode, where a request probes exactly
	// one egress, so saturation could never trigger a rotate.
	satRotateAfter = 1
	// satRotateCooldown: a rotate respawns pool workers for 1-2 minutes, so
	// rotating more often turns the pool into a permanent "starting" flap —
	// the exact reason rotation was removed from the tail verdict before.
	// satRotateCooldown: a rotate respawns all workers for 1-2 minutes, and
	// the revenue loss is measurable — 2026-10-18: failure rate 27.9% in the
	// minute after a rotate vs 7-20% otherwise, 14 rotates in 5h.
	satRotateCooldown = 15 * time.Minute
	// Identity recycle gaps. POST /api/v1/pool/rotate re-registers every WARP
	// device, so the pool comes back on NEW /48 prefixes with fresh upstream
	// windows — measured 2026-10-22 00:31: 14/14 public IPs changed
	// (bac5:48f3:1f5::32:147, bac5:48f4:682::a6:9, bac1:47a0:170::3cc:3a ...)
	// and traffic resumed while the old shared-deadline flag stayed armed.
	// That measurement is why rotation is no longer skipped during a global
	// burn: the flag describes identities that a rotate retires.
	healGapFirst      = 10 * time.Minute
	healGapMax        = 30 * time.Minute
	healClientTimeout = 200 * time.Second
	// poolWaitReportMin: waits shorter than this are ordinary acquire jitter,
	// not a journal-worthy "served after wait" event.
	poolWaitReportMin = 1500 * time.Millisecond
)

// regionExhaustedHeader marks the synthetic region verdict that RoundTrip
// returns when EVERY egress in the sweep was region-rejected. The outer
// tryProvider loop must NOT swap on it: it is already the product of a full
// 5x2 sweep, and swapping would run up to 5 more full sweeps (60 warmups)
// on a request that is заведомо decided. Header check is O(1), no body peek.
const regionExhaustedHeader = "X-Pool-Region-Exhausted"

// gateTerminalHeader marks an inner-decided TERMINAL account-gate 403
// (FreeTier/billing): the outer tryProvider loop must surface it honestly
// instead of re-swapping full sweeps on a decided request (same role as
// regionExhaustedHeader for region verdicts).
const gateTerminalHeader = "X-Pool-Gate-Terminal"

// maxGateSwaps caps in-sweep swaps on account-gate 403 per RoundTrip: the// gate is egress-correlated (2026-09-18 live: same account+model+minute,
// one egress 200 while others 403), so a few swaps find a clean egress;
// beyond the cap the request is honestly terminal (true account ban).
const maxGateSwaps = 6

// gateEgressBan parks a gate-rejected egress briefly: long enough to steer
// the current sweep away from it, short enough to never shrink the pool
// under concurrent OMP load (2026-09-18: 2m bans stacked across
// simultaneous requests until acquire found no cold lease — big OMP held
// the warm egress while the small probe died with "acquire all hot"; the
// gate is egress-correlated, so a 10s steer is enough — next request
// re-probes it. A 200 lifts any ban via markGood.)
const gateEgressBan = 10 * time.Second

// maxGateResweeps bounds the windowed-gate wait-out: a FreeTier body parks
// egresses for gateEgressBan, so the sweep may wait the park out and retry
// up to this many times before the honest terminal (2026-10-14: the real
// TUI direct gets the same body; the verdict is per-IP window, not account).
const maxGateResweeps = 2

// syntheticGateVerdict rebuilds a readable honest-403 verdict from a drained
// account-gate answer, carrying gateTerminalHeader so tailVerdict and the
// outer loop surface it as 403 instead of mapping to 503.
func syntheticGateVerdict(req *http.Request, peek []byte, ekey string) *http.Response {
	if len(peek) > 8<<10 {
		peek = peek[:8<<10]
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set(gateTerminalHeader, "1")
	if ekey != "" {
		// The session pool keys gate streaks on (session, egress): without the
		// egress the streak collapsed to the bare id, so the SECOND gate in
		// gateStreakTTL burned a healthy harvested session no matter which
		// egress answered (live 2026-10-22 17:54: two failed requests burned
		// both fresh ids, log "session burned … ekey=false", and every
		// router-minted request became a guaranteed 403).
		h.Set(sessionGateEgressHeader, ekey)
	}
	return &http.Response{
		StatusCode: http.StatusForbidden,
		Status:     "403 Forbidden",
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(append([]byte(nil), peek...))),
		Request:    req,
	}
}

// isWindowedGateBody reports whether a gate body is the WINDOWED FreeTier
// kind ("free tier can only be used from within OpenCode") rather than an
// account ban (billing/credits). Verified 2026-10-14: the real opencode TUI
// hitting opencode.ai direct gets the identical body while warp egresses
// alternate 200/gate per minute — the verdict opens and closes with the
// per-IP window, so the sweep should wait it out instead of terminating.
func isWindowedGateBody(b []byte) bool {
	lb := bytes.ToLower(b)
	for _, m := range [][]byte{
		[]byte("only be used from within"),
		[]byte("free tier can only"),
		[]byte("freetiererror"),
	} {
		if bytes.Contains(lb, m) {
			return true
		}
	}
	return false
}

// isAccountGateBody reports whether a 403 body is an account-scoped gate
// rejection (FreeTier/billing), NOT an egress-scoped verdict: no other
// egress can serve it, so swapping/banning is pure loss. Narrow markers
// only; runs on 403 responses, never on success content.
func isAccountGateBody(b []byte) bool {
	lb := bytes.ToLower(b)
	for _, m := range [][]byte{
		[]byte("freetier"),
		[]byte("free tier"),
		[]byte("free-tier"),
		[]byte("only be used from within"),
		[]byte("no payment method"),
		[]byte("credits"),
		[]byte("billing"),
	} {
		if bytes.Contains(lb, m) {
			return true
		}
	}
	return false
}

// isRegionForbiddenBody reports whether a response body smells like a
// region rejection (opencode 403 region_error, muse on unsupported
// regions). Requires "region" PLUS a region-specific marker — a bare
// "error"/"failed"/"403" next to the word "region" (e.g. in model content)
// must never trigger a swap.
func isRegionForbiddenBody(b []byte) bool {
	lb := bytes.ToLower(b)
	if !bytes.Contains(lb, []byte("region")) {
		return false
	}
	for _, m := range [][]byte{
		[]byte("region_error"),
		[]byte("region not supported"),
		[]byte("region unsupported"),
		[]byte("unsupported region"),
		[]byte("not served"),
		[]byte("not supported"),
		[]byte("unsupported"),
		[]byte("not allowed"),
		[]byte("blocked"),
		[]byte("forbidden"),
	} {
		if bytes.Contains(lb, m) {
			return true
		}
	}
	return false
}

// isRegionErrorEvent is the strict sniff-path matcher for 200-SSE error
// events: only an explicit region_error code (or region + not-supported
// wording) counts. Success content mentioning regions never matches.
func isRegionErrorEvent(b []byte) bool {
	lb := bytes.ToLower(b)
	if bytes.Contains(lb, []byte("region_error")) {
		return true
	}
	return bytes.Contains(lb, []byte("region")) && bytes.Contains(lb, []byte("not supported"))
}

// syntheticRegionVerdict builds an honest readable 403 verdict for an
// egress whose region the upstream rejected: status preserved, body fresh
// JSON with region_error markers so the gateway formats it as region_error.
// Carries regionExhaustedHeader so the outer loop never re-swaps a decided
// sweep. excerpt (truncated upstream body) is embedded for debuggability —
// the original message is otherwise lost behind the generic text.
func syntheticRegionVerdict(req *http.Request, excerpt []byte) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set(regionExhaustedHeader, "1")
	ex := strings.TrimSpace(strings.ToValidUTF8(string(excerpt), ""))
	if len(ex) > 500 {
		ex = ex[:500]
	}
	var bodyStr string
	if ex == "" {
		bodyStr = `{"error":{"message":"region not supported on this egress, no available region after swap","type":"invalid_request_error","code":"region_error","param":"region_error"}}`
	} else {
		enc, _ := json.Marshal(map[string]any{"error": map[string]any{
			"message": "region not supported on this egress, no available region after swap",
			"type":    "invalid_request_error", "code": "region_error", "param": "region_error",
			"upstream_excerpt": ex,
		}})
		bodyStr = string(enc)
	}
	body := io.NopCloser(strings.NewReader(bodyStr))
	return &http.Response{
		StatusCode: http.StatusForbidden,
		Status:     "403 Forbidden",
		Header:     h,
		Body:       body,
		Request:    req,
	}
}

func (rt *poolRetryRT) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if req.Body != nil {
		req.Body.Close()
	}
	// Preamble injection disabled (MITM matrix 2026-10-24): the free-tier
	// gate now flags the incoherent combo — a system prompt carrying the
	// title-generator preamble ("Never use tools") together with a tools
	// array — as fabricated (403 FreeTierError on every egress), while the
	// same body WITHOUT the preamble passes. The 2026-10-23 recipe is dead.
	if false {
		var doc map[string]any
		if json.Unmarshal(body, &doc) == nil {
			if msgs, ok := doc["messages"].([]any); ok {
				preamble := "You are a title generator. You output ONLY a thread title. Nothing else.\n\n<task>\nGenerate a brief title.\n</task>\n\n<rules>\n- Never use tools\n</rules>"
				found := false
				for _, m := range msgs {
					if mm, ok := m.(map[string]any); ok {
						if r, _ := mm["role"].(string); r == "system" {
							if c, _ := mm["content"].(string); c != "" {
								if !strings.Contains(c, "You are a title generator") {
									mm["content"] = preamble + "\n\n" + c
								}
							} else {
								mm["content"] = preamble
							}
							found = true
							break
						}
					}
				}
				if !found {
					newMsg := map[string]any{"role": "system", "content": preamble}
					doc["messages"] = append([]any{newMsg}, msgs...)
				}
				if nb, err := json.Marshal(doc); err == nil {
					body = nb
				}
			}
		}
	}
	// Preamble injection disabled (MITM matrix 2026-10-24): see the note
	// above — the preamble+tools combo is now a gate-403 signal.
	if false && strings.HasPrefix(strings.TrimSpace(string(body)), "{") {
		var doc map[string]any
		if json.Unmarshal(body, &doc) == nil {
			if msgs, ok := doc["messages"].([]any); ok {
				preamble := "You are a title generator. You output ONLY a thread title. Nothing else.\n\n<task>\nGenerate a brief title.\n</task>\n\n<rules>\n- Never use tools\n</rules>"
				found := false
				for _, m := range msgs {
					if mm, ok := m.(map[string]any); ok {
						if r, _ := mm["role"].(string); r == "system" {
							if c, _ := mm["content"].(string); c != "" {
								if !strings.Contains(c, "You are a title generator") {
									mm["content"] = preamble + "\n\n" + c
								}
							} else {
								mm["content"] = preamble
							}
							found = true
							break
						}
					}
				}
				if !found {
					// prepend system message
					newMsg := map[string]any{"role": "system", "content": preamble}
					doc["messages"] = append([]any{newMsg}, msgs...)
				}
				if nb, err := json.Marshal(doc); err == nil {
					body = nb
				}
			}
		}
	}
	// Prewarm: запоминаем last-used модель (что реально гоняют) + стартуем
	// фоновый тикер один раз (baseURL из провайдера). startUARefresh держит
	// upstream UA на свежем stable (npm -> GitHub, 6h). startStandbyVerify —
	// +1 поток: очередь 429-битых IP, проверка мимо горячего пути.
	rt.t.rememberModel(rt.model)
	// UA tuple is PINNED to the captured build (1.19.4 / provider-utils 4.0.23 /
	// bun 1.4.4). Auto-refreshing only the version from npm/github produced a
	// tuple that never shipped upstream and caused 429s on every egress.
	// startUARefresh()  // disabled: pin provider UA tuple
	if rt.provProvider != nil {
		rt.t.startPrewarm(rt.prov, rt.provProvider.BaseURL, rt.provProvider)
		rt.t.startStandbyVerify(rt.prov, rt.provProvider.BaseURL, rt.provProvider)
	}

	deadline := time.Now().Add(poolTryDeadline)
	if dl, ok := req.Context().Deadline(); ok {
		if left := time.Until(dl); left < poolTryDeadline {
			deadline = time.Now().Add(left)
		}
	}
	reqID := rt.t.reqSeq.Add(1)
	var lastResp *http.Response
	retryable := func(st int) bool {
		return st == http.StatusTooManyRequests || st >= 500
	}
	// tried tracks every pool budget group already probed in THIS request
	// so the loop short-circuits as soon as the pool cycled back to a group
	// we already hit. Keyed by /48 budget group (triedKey), NOT exact IP:
	// live budgets are ~9 (warp 20 backends share 9x/48), so exact-IP
	// counting covered the same 9 budgets ~2.2x and called repeats fresh.
	tried := map[string]bool{}
	if rt.cur != nil && rt.cur.proxy != "" {
		tried[triedKey(rt.t.ipKey(rt.cur.proxy, rt.cur.publicIP))] = true
	}
	var rateLimitHits int
	var serverErrHits int
	var regionHits int
	var gateHits int
	var gateResweeps int
	var parkPasses int
	var verdictlessWaits int
	var stormWaits int
	var shortPoolWaits int
	exhausted := false
	repeatHits := 0
	maxRepeatHits := 0
	secondChance := false
	effFanout, effRounds := rt.t.fanoutN(), rt.t.roundsN()
	if until, gra := rt.t.globalStatus(); !until.IsZero() && time.Now().Before(until) && !rt.t.burnCollapse(time.Now()) {
		// The latch says "shared budget burned", but a pooled 200 landed within
		// poolLiveGrace: the pool is demonstrably alive, so the 1x1 collapse
		// below would turn every request into a lottery on one egress. Live
		// 2026-10-22 12:20-12:25: latch armed 14.6h, 12 egresses serving, and
		// 45% of requests answered 503 in 0.5-2s with "(1 throttled)".
		rt.t.warnEvery("global-live", 60*time.Second,
			"pool: burn flag set but a 200 was served recently — full sweep, no 1x1 collapse",
			"req", reqID, "until", until.Format(time.RFC3339), "global_ra", gra,
			"last_ok", time.Since(time.Unix(0, rt.t.lastPoolOK.Load())).Round(time.Second).String())
	} else if until, gra := rt.t.globalStatus(); !until.IsZero() && time.Now().Before(until) {
		// Shared budget burned: FAIL FAST with the honest 429 on ONE probe.
		// The 1x3 triple-probe still burned 3 upstream windows per logical
		// request during a global burn (2026-09-11 storm: burst neighbours
		// re-warmed the same hot budgets, self-sustaining the cycle).
		// One probe is enough: a live egress answers 200 immediately,
		// a hot one confirms the shared deadline. No outward Retry-After
		// (swap mode): the tail verdict is a 503 the client retries on.
		effFanout, effRounds = rt.t.burnProbeFanout(time.Now()), 1
		rt.t.log().Warn("pool: global burn active, fail-fast probe mode",
			"req", reqID, "until", until.Format(time.RFC3339), "global_ra", gra,
			"fanout", effFanout)
	} else if rt.t.stormActive() && !rt.t.NoStormCollapse && !rt.t.poolServing() {
		// Одиночный probe под штормом корректен только когда пул реально молчит:
		// stormActive считает 429 в кольце, а 200 в нём же — доказательство жизни.
		// Пока 200 идут, свип обязан идти: иначе 1x1 превращает живой пул в лотерею
		// (714 из 717 «saturated (1 throttled)» при живых 19 egress).
		// Single-IP завал — НЕ шторм: один egress сыпет, остальные живут.
		// Fail-fast 1x1 по всему пулу здесь вредил бы (роняем throughput
		// ради одного битого IP, heat и так уводит его в hot). Полный
		// свип идёт дальше и находит живые.
		if single, ok := rt.t.stormSingleIP(time.Now()); ok {
			rt.t.log().Warn("pool: single-egress burn, no fail-fast (heat owns it)",
				"req", reqID, "egress", single)
		} else {
			// Pool-wide storm WITHOUT hours-class RA (2026-09-13 midday: 1800+
			// no-RA 429s in 17 min at x4-7 tempo): heat without a single ok —
			// every egress burns faster than any window reopens. Full 5x2
			// sweep only deepens it (10 warmups per logical request into hot
			// budgets). Collapse to 1x1 like the global path: one probe proves
			// the class; 2 ok (1 heavy) lift the throttle (stormActive ok>=2
			// gate; heavy 200s double-note recovery symmetrically).
			effFanout, effRounds = 1, 1
			rt.t.log().Warn("pool: storm active, fail-fast single-probe mode",
				"req", reqID)
		}
	}
	// Per-egress лифт шторма (2026-09-23): входная лиза уже на доказанно живом
	// egress (2+ свежих 200) — fail-fast 1x1 не нужен, идём полным свипом сразу.
	// Без этого живые окна душили лотереей 1x1: 835×503 рядом с 825×200.
	// Под global тоже разрешён: лиза уже выбрана на живом egress, полный свип
	// с него не re-warm'ит горячие бюджеты — холодные входы global-1x1 целы.
	// Тот же гейт, что в pickCool: битый после 200 лифтом не ходит.
	// failFastProbe: the request walks a LIMITED budget (storm/global 1x1 or
	// the latch's 3x1), so the tail gates below must apply — otherwise a 3x1
	// sweep runs to triedExhaustFloor=7 and burns 7-9 upstream windows per
	// logical request (review B3).
	stormCollapse := effFanout == 1
	if stormCollapse && rt.cur != nil {
		liveKey := rt.t.ipKey(rt.cur.proxy, rt.cur.publicIP)
		now := time.Now()
		if rt.t.stormLiveEgress(liveKey) && !rt.t.isBad(liveKey, now) && !rt.t.isParked(liveKey, now) && rt.t.heatCount(liveKey, now) < heatMaxHits {
			effFanout, effRounds = rt.t.fanoutN(), rt.t.roundsN()
			rt.t.log().Warn("pool: proven-live ingress, storm fail-fast bypassed",
				"req", reqID, "egress", liveKey)
		}
	}
	// minWalkEgress upstream warmups before an honest 429. Default 5×2=10
	// already covers ~9 live /48 budgets; extend only when configured
	// below the floor. Extra rounds cost a full upstream window each when
	// every egress is hot — the 20-sweep burned the pool per request.
	// Global fail-fast stays 1×1 by design.
	if effFanout != 1 && !rt.t.globalActive() {
		// While the shared latch is armed the extra rounds only re-warm a
		// burned pool (10 upstream windows per logical request measured
		// 2026-10-22); the ladder bans already keep spent addresses out.
		for effFanout*effRounds < minWalkEgress {
			effRounds++
		}
	}
	// failFastProbe: the request walks a LIMITED budget, so the tail gates below
	// must apply — otherwise a small sweep runs to triedExhaustFloor=7 and burns
	// 7-9 upstream windows per logical request. Computed AFTER every widening
	// above: freezing it before the proven-live bypass made the widened 5x2 walk
	// still tail out on the first 429 (review B1).
	failFastProbe := limitedProbe(effFanout, effRounds)

	// Внешний ре-свип: пока жив poolTry-бюджет — повторные полные свипы
	// 5x2 поверх (2026-09-16: ретрай внутри провайдера, клиент 429/403 не
	// видит вообще). Баны/парки между свипами истекают, окна
	// переоткрываются — следующий свип забирает ожившие egress.
resweep:
	for {
		for attempt := 0; attempt < effFanout; attempt++ {
			// Отмена клиента: лизу вернуть, IP не свапать, баны/heat не трогать.
			if req.Context().Err() != nil {
				_ = rt.releaseCurrent(context.Background())
				return nil, req.Context().Err()
			}
			attemptStart := time.Now()
			var acquireMs, headerMs, sniffMs int64
			if time.Now().After(deadline) {
				// honest upstream verdict we already have rather than keep
				// burning attempts on egresses that are all hot.
				rt.t.log().Warn("pool: retry budget exhausted",
					"req", reqID, "budget", poolTryDeadline.String(), "attempt", attempt,
					"tried", len(tried), "rate_limit_hits", rateLimitHits, "server_err_hits", serverErrHits)
				// No verdict, no direct: without an upstream answer there is
				// nothing for the home leg to retry (same invariant as tail
				// and acquire-fail — review 2026-09-09).
				// Verdict-less — тоже 503 UpstreamStatusError, НЕ plain error:
				// gateway маппит plain в 502, клиент ретраит иначе.
				if lastResp != nil {
					return rt.tailVerdict(req, body, retryable, deadline, reqID, lastResp, rateLimitHits, regionHits)
				}
				return nil, &UpstreamStatusError{Status: http.StatusServiceUnavailable,
					Err: fmt.Errorf("provider %s: pool retry budget exhausted (%s) with no upstream response", rt.prov, poolTryDeadline)}
			}
			rt.mu.Lock()
			lease := rt.cur
			rt.mu.Unlock()
			if lease == nil {
				acqStart := time.Now()
				if left := time.Until(deadline); left < 10*time.Second && left <= 0 {
					break resweep
				}
				// Short-budget tail: clamp acquire to the remaining slice.
				// Closure-scoped defer (no defer-in-loop): each iteration's
				// timer dies with the call, not piled to RoundTrip return.
				l, aerr := func() (*poolLease, error) {
					if left := time.Until(deadline); left < 10*time.Second {
						cctx, cancel := context.WithTimeout(req.Context(), left)
						defer cancel()
						return rt.t.acquireLease(cctx, reqID, rt.model, rt.stamp)
					}
					return rt.t.acquireLease(req.Context(), reqID, rt.model, rt.stamp)
				}()
				acquireMs = time.Since(acqStart).Milliseconds()
				if aerr != nil {
					// Отмена клиента: без maybeRotate-хвоста/сна — сразу ctx err.
					if req.Context().Err() != nil || isCancelErr(aerr) {
						_ = rt.releaseCurrent(context.Background())
						if ctxErr := req.Context().Err(); ctxErr != nil {
							return nil, ctxErr
						}
						return nil, aerr
					}
					// Pool itself is exhausted (every lease banned/parked)
					// with NO upstream verdict yet for this request.
					// Раньше: мгновенный возврат ошибки (клиент видел 429/
					// transport). Теперь (2026-09-16): НЕ возвращаем —
					// ждём переоткрытия внутри провайдера: короткая пауза
					// и повтор acquire до дедлайна. Баны минутные, окна
					// переоткрываются — следующий acquire заберёт оживший
					// egress. Дедлайн истёк — хвост вернёт 503 (см. ниже).
					// Home-IP leg не трогаем вообще: direct жжёт home
					// (2026-09-09 storm) и больше не используется.
					// When upstream already answered (lastResp!=nil)
					// direct stays a last chance (tryDirectFallback ниже).
					if lastResp == nil {
						// Мёртвая везде модель (NOT_FOUND/5xx на всех прогретых):
						// свежие лизы делят ту же мёртвую модель (см. хвост
						// exhausted ниже) — ждать reopen 165с бессмысленно,
						// сразу ранний хвост. Region-only сюда не входит:
						// region-свап ещё может найти живой egress.
						if serverErrHits > 0 {
							rt.t.log().Warn("pool: acquire exhausted, dead-model verdicts seen, early tail",
								"req", reqID, "err", aerr.Error(),
								"tried", len(tried), "server_err_hits", serverErrHits)
							break resweep
						}
						// Verdict-less ранний хвост: покрытие уже честное
						// (tried>=floor = почти все ~9 живых /48 опрошены),
						// а вердикта нет — ждать reopen до deadline (165с)
						// бессмысленно: окна минутные/часовые. Быстрый
						// 503-хвост вместо удержания клиента весь budget.
						if len(tried) >= triedExhaustFloor {
							rt.t.log().Warn("pool: acquire exhausted, coverage honest, early tail",
								"req", reqID, "err", aerr.Error(),
								"tried", len(tried), "rate_limit_hits", rateLimitHits)
							break resweep
						}
						if time.Now().After(deadline) {
							rt.t.log().Warn("pool: request verdict: acquire exhausted, deadline over",
								"req", reqID, "err", aerr.Error(),
								"tried", len(tried), "rate_limit_hits", rateLimitHits)
							break resweep
						}
						// Verdict-less cap: окна минутные/часовые, 200-датчик
						// переоткрытия тухнет вместе с холдом (prewarm/standby
						// молчат на забитом пуле). Больше 2 пауз подряд —
						// быстрый хвост вместо удержания клиента весь budget.
						verdictlessWaits++
						if verdictlessWaits > 2 {
							rt.t.log().Warn("pool: verdict-less waits capped, early tail",
								"req", reqID, "err", aerr.Error(),
								"tried", len(tried), "waits", verdictlessWaits, "rate_limit_hits", rateLimitHits)
							break resweep
						}
						rt.t.log().Info("pool: acquire exhausted, waiting for reopen",
							"req", reqID, "err", aerr.Error(),
							"waits", verdictlessWaits,
							"left", time.Until(deadline).String())
						_ = rt.releaseCurrent(req.Context())
						if sleep := time.Until(deadline); sleep > 2*time.Second {
							if !sleepOrCancel(2*time.Second, req.Context()) {
								_ = rt.releaseCurrent(context.Background())
								return nil, req.Context().Err()
							}
						} else if sleep > 0 {
							if !sleepOrCancel(sleep, req.Context()) {
								_ = rt.releaseCurrent(context.Background())
								return nil, req.Context().Err()
							}
						}
						continue
					}
					return rt.tailVerdict(req, body, retryable, deadline, reqID, lastResp, rateLimitHits, regionHits)
				}
				lKey := rt.t.ipKey(l.proxy, l.publicIP)
				tKey := triedKey(lKey)
				if tKey != "" && tried[tKey] {
					// Pool cycled back to an already-probed BUDGET GROUP.
					// Honest verdict only after most groups probed: a single
					// repeat means the pool cycled, not that every budget
					// is hot (warp LRU-by-/48 hands back same-group backends
					// as "fresh" leases by design).
					_ = rt.t.release(context.Background(), l.leaseID)
					repeatHits++
					// Честный repeat-гейт (волна 50 + fix 2026-09-24): tried
					// кумулятивен сквозь ре-свипы (max 5-6 за проход, 9+ за два),
					// floor 7 достижим. Вторая нога требует 5 distinct — LRU
					// same-/48 bias давал tail при 3/9 (3 повтора одной группы
					// при 5 живых — рано, 5/9 ещё не честно).
					if len(tried) >= triedExhaustFloor || (repeatHits >= 3 && len(tried) >= 5) {
						if lastResp != nil {
							return rt.tailVerdict(req, body, retryable, deadline, reqID, lastResp, rateLimitHits, regionHits)
						}
						// Вердикта нет — ждём переоткрытия внутри (2026-09-16),
						// а не отдаём ошибку клиенту. Но при честном покрытии
						// (tried>=floor) reopen ждать бессмысленно: окна
						// минутные/часовые — быстрый хвост вместо 165с.
						if len(tried) >= triedExhaustFloor {
							rt.t.log().Warn("pool: no fresh group, coverage honest, early tail",
								"req", reqID, "tried", len(tried), "repeats", repeatHits)
							break resweep
						}
						if time.Now().After(deadline) {
							break resweep
						}
						rt.t.log().Info("pool: no fresh group, waiting for reopen",
							"req", reqID, "tried", len(tried), "repeats", repeatHits,
							"left", time.Until(deadline).String())
						_ = rt.releaseCurrent(req.Context())
						// Coverage stays cumulative (as resweep footer): a wipe
						// here reset 3->0 every 2s and floor 7 never filled.
						// Пик копим в maxRepeatHits как в футере — иначе третья
						// нога rotate (max>=3 && hits>=5) слепнет на циклах,
						// ушедших в 2с-паузу внутри свипа (P1).
						if repeatHits > maxRepeatHits {
							maxRepeatHits = repeatHits
						}
						repeatHits = 0
						parkPasses = 0
						if sleep := time.Until(deadline); sleep > 2*time.Second {
							if !sleepOrCancel(2*time.Second, req.Context()) {
								_ = rt.releaseCurrent(context.Background())
								return nil, req.Context().Err()
							}
						} else if sleep > 0 {
							if !sleepOrCancel(sleep, req.Context()) {
								_ = rt.releaseCurrent(context.Background())
								return nil, req.Context().Err()
							}
						}
						continue
					}
					rt.t.log().Info("pool: lease repeat, walking on",
						"req", reqID, "ip", lKey, "group", tKey, "tried", len(tried), "repeats", repeatHits)
					_ = rt.releaseCurrent(req.Context())
					continue
				}
				if tKey != "" {
					tried[tKey] = true
					repeatHits = 0
					parkPasses = 0 // fresh progress resets the all-parked allowance
				}
				rt.mu.Lock()
				rt.cur = l
				lease = l
				rt.mu.Unlock()
			}
			// Lease banned AFTER acquire (burst neighbour hit hours-RA on
			// the same egress): warming it burns ~1s for a certain 429 and
			// eats a floor slot. Release and walk on WITHOUT marking tried
			// or counting a hit — only warmed-up verdicts consume the
			// honesty budget. Parked leases no longer exist (transient 429s
			// walk on unparked); isParked stays as a guard for stale/foreign
			// park entries only.
			if pk := rt.t.ipKey(lease.proxy, lease.publicIP); rt.t.isParked(pk, time.Now()) || rt.t.isBad(pk, time.Now()) {
				// Parked lease inside a MIXED pool: release and walk on
				// without burning the tried slot — a live egress may sit
				// later in the sweep. But when EVERYTHING is parked the
				// walk would spin forever (acquire re-hands the same
				// parked leases, tried stays empty): allow the warmup
				// through on the second park-pass so the request ends
				// with the certain 429 instead of the poolTry deadline.
				if failFastProbe {
					// Global fail-fast single-probe: walking on ends the request with zero warmups
					// (no second attempt exists) -> mass fast 502 (2026-09-12 storm: 436x no-lease).
					// Warm the parked lease straight through: one probe gives honest 429/200.
					rt.t.log().Info("pool: global single-probe, warming parked lease",
						"req", reqID, "lease", lease.proxy, "ip", pk,
						"attempt", attempt, "acquire_ms", acquireMs)
				} else if parkPasses >= 1 {
					rt.t.log().Info("pool: all parked, warming parked lease",
						"req", reqID, "lease", lease.proxy, "ip", pk,
						"attempt", attempt, "acquire_ms", acquireMs)
				} else {
					parkPasses++
					rt.t.log().Info("pool: lease parked after acquire, walking on",
						"req", reqID, "lease", lease.proxy, "ip", pk,
						"attempt", attempt, "acquire_ms", acquireMs)
					// Tried slot stays: the group was probed this request,
					// deleting it rolled coverage back on every shortPark.
					_ = rt.releaseCurrent(req.Context())
					continue
				}
			}
			rt.t.log().Info("pool: attempt",
				"req", reqID, "lease", lease.proxy, "ip", rt.t.ipKey(lease.proxy, lease.publicIP),
				"attempt", attempt, "acquire_ms", acquireMs)

			// Без per-attempt probe: GET /models тоже идёт в то же (IP,UA)
			// окно и жжёт бюджет; мёртвый туннель и так виден по header-err
			// за секунды, а не 90с (RHT режет).

			cl := rt.t.clientFor(lease.proxy)
			r := cloneRequest(req, body)
			// Sticky per-egress identity: same UA + session per IP, so the
			// upstream window (IP, full-UA, session) looks like one stable
			// session instead of rotating identical bulk fingerprints.
			if lease != nil && lease.ua != "" {
				r.Header.Set("User-Agent", lease.ua)
			} else if ua := leaseUA(rt.provProvider, lease.proxy, r.Header.Get("User-Agent")); ua != "" {
				r.Header.Set("User-Agent", ua)
			}
			if lease != nil && lease.session != "" {
				// Full triple: both session headers + a request id minted in
				// THIS session (setOpencodeHeaders stamped the pool default).
				setEgressSession(r.Header, lease.session)
				logEgressIdentity(rt.t, r.Header, lease.session)
			}
			// poolTry hard deadline: abort in-flight header wait when budget
			// expires, but do NOT tie successful streaming body to deadline —
			// body must survive past poolTry (gateway 180s is outer bound).
			remaining := time.Until(deadline)
			if remaining <= 0 {
				remaining = time.Millisecond
			}
			// Size-aware think wait: leaves the rest of the budget for a second
			// egress instead of spending it all on a hung one.
			thinkWait := poolThinkWait(remaining, r.ContentLength)
			attemptCtx, attemptCancel := context.WithCancel(r.Context())
			r = r.WithContext(attemptCtx)
			type doRes struct {
				resp *http.Response
				err  error
			}
			ch := make(chan doRes, 1)
			hdrStart := time.Now()
			go func(req *http.Request, cl *http.Client) {
				resp, err := cl.Do(req)
				ch <- doRes{resp, err}
			}(r, cl)
			var resp *http.Response
			var err error
			select {
			case got := <-ch:
				resp, err = got.resp, got.err
				if err != nil {
					attemptCancel()
				} else {
					// Keep attemptCtx alive for streaming body; cancel on close.
					resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: attemptCancel}
				}
			case <-time.After(thinkWait):
				attemptCancel()
				err = context.DeadlineExceeded // poolTry slice expired, not the egress
				// Drain goroutine to avoid leak; body already cancelled.
				go func() {
					got := <-ch
					if got.resp != nil && got.resp.Body != nil {
						got.resp.Body.Close()
					}
				}()
				err = context.DeadlineExceeded
			}
			headerMs = time.Since(hdrStart).Milliseconds()
			budgetExpired := errors.Is(err, context.DeadlineExceeded) && time.Now().After(deadline)
			// Think-таймаут (RHT-пробой): upstream принял запрос и молчит —
			// модель думает, туннель жив (WARP сам не сыплется: 0 транспортных
			// обрывов за ночь при 7173×200). Это НЕ вердикт egress: окно уже
			// потрачено, банить его за думание — наказывать живой бэкенд за
			// чужую работу. Только свап дальше, без markBadKeyFor/serverErrHits.
			isThinkTimeout := err != nil && thinkWait > 5*time.Second &&
				headerMs >= int64((thinkWait-5*time.Second)/time.Millisecond)
			if err != nil {
				// Отмена клиента посреди warmup: лизу вернуть, egress не банить,
				// heat/шторм не кормить, дальше не свапать — запрос мёртв.
				if req.Context().Err() != nil || isCancelErr(err) {
					_ = rt.releaseCurrent(context.Background())
					if ctxErr := req.Context().Err(); ctxErr != nil {
						return nil, ctxErr
					}
					return nil, err
				}
				// acquires skip it, then rotate to a fresh lease. Without
				// this, every round re-tries the same dead proxies and a
				// request can burn fanout×rounds×25s (100s+) on failures.
				// Budget expiry is NOT an egress verdict: the lease never
				// answered because OUR slice ran out — cooling it punishes
				// a live backend for чужой съеденный бюджет (review 2026-09-09).
				if lease != nil && !budgetExpired && !isThinkTimeout {
					key := rt.t.ipKey(lease.proxy, lease.publicIP)
					rt.t.markBadKeyFor(key, time.Now(), transientCooldown)
					rt.t.log().Warn("pool: egress failed, cooling down",
						"req", reqID, "egress", key, "err", err.Error(), "cooldown", transientCooldown.String(),
						"acquire_ms", acquireMs, "header_ms", headerMs)
				} else if isThinkTimeout {
					rt.t.log().Warn("pool: egress thinking too long, swapping without ban",
						"req", reqID, "egress", rt.t.ipKey(lease.proxy, lease.publicIP),
						"header_ms", headerMs)
				}
				// Consecutive-5xx: transport/header error is not a model verdict —
				// the egress never answered 5xx, so it breaks the streak
				// (else 500/err/500 falsely trips dead-model).
				serverErrHits = 0
				_ = rt.releaseCurrent(req.Context())
				continue
			}
			if resp.StatusCode == http.StatusUnauthorized {
				if ks := rt.t.keysFor(rt.prov); len(ks) > 0 {
					drainClose(resp)
					rt.t.rotateKeyFor(rt.prov)
					rt.t.log().Warn("pool: upstream 401, rotating key",
						"req", reqID, "ip", rt.t.ipKey(lease.proxy, lease.publicIP),
						"provider", rt.prov,
						"key_after", rt.t.currentKeyFor(rt.prov), "keys", len(ks))
					// Consecutive-5xx: 401 interleaves with 500/429 live — breaks streak.
					serverErrHits = 0
					_ = rt.releaseCurrent(req.Context())
					continue
				}
				// Anonymous provider got 401: opencode's UA/IP gate flaps —
				// the same request passes on another egress (verified live
				// during Console instability: 401/500/429 interleaved).
				// Retry on a fresh lease instead of relaying the failure;
				// cycling keys cannot help (none configured), but egress
				// rotation does.
				rt.t.markBadKeyFor(rt.t.ipKey(lease.proxy, lease.publicIP), time.Now(), transientCooldown)
				rt.t.log().Warn("pool: egress 401, cooling down",
					"req", reqID, "egress", rt.t.ipKey(lease.proxy, lease.publicIP),
					"cooldown", transientCooldown.String())
				// Consecutive-5xx: anonymous 401 also breaks the streak.
				serverErrHits = 0
				_ = rt.releaseCurrent(req.Context())
				continue
			}
			if resp.StatusCode == http.StatusForbidden {
				// 403 region_error (muse-spark not served in this egress
				// region): swap to a FRESH lease inside the same request.
				// Non-region 403 is honest terminal and state-neutral:
				// return it WITHOUT markGood/noteOK/heatCool — a policy
				// rejection is not proof of a live window. Region rejection
				// is a property of (model, egress region), NOT a burned
				// window: no cooldown/ban, but remembered model-scoped so
				// the next request skips the dead egress for THIS model.
				peek, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
				if isRegionForbiddenBody(peek) {
					// Gate verdict sticks (same guard as non-region/retryable):
					// gate→region mix must not swap the terminal gate body
					// (loses noteSessionGate accounting in route.go:1254).
					if lastResp != nil && lastResp.Header.Get(gateTerminalHeader) != "" {
						drainClose(resp)
						_ = rt.releaseCurrent(req.Context())
						continue
					}
					drainClose(resp)
					if lastResp != nil {
						drainClose(lastResp)
					}
					lastResp = syntheticRegionVerdict(req, peek)
					regionHits++
					ekey := rt.t.ipKey(lease.proxy, lease.publicIP)
					rt.t.noteRegionBad(rt.model, ekey, time.Now())
					rt.t.log().Warn("pool: egress region-forbidden, swapping lease",
						"req", reqID, "egress", ekey,
						"region_hits", regionHits, "header_ms", headerMs, "model", rt.model)
					// Consecutive-5xx: region-403 тоже не-5xx (волна 50).
					serverErrHits = 0
					_ = rt.releaseCurrent(req.Context())
					continue
				}
				// Account-gate 403 (FreeTier/billing: "only usable from within
				// OpenCode", "no payment method"): EGRESS-CORRELATED, not
				// account-scoped (2026-09-18 live: same account+model+minute,
				// one egress 200 while others 403). Swap leases like
				// non-region 403, capped so a true account ban still
				// surfaces honest 403 fast instead of burning the sweep
				// budget. Cap-exceeded (or first sighting when swaps are
				// exhausted) returns the live body terminal with
				// gateTerminalHeader (no bans — state-neutral, outer loop
				// must not re-swap a decided gate).
				// Region checked first above (markers disjoint in practice).
				if isAccountGateBody(peek) {
					// Body-shape verdict (2026-10-23): a 403 whose REQUEST never
					// carried the gate shape (no canonical system head, <2 tools,
					// stream:false — combo steps, GUI health, hubmod probes) says
					// nothing about this egress. Stamping it parked 10s per hit,
					// burned the swap budget and re-registered the whole pool on
					// one GUI click. Terminal, no ban, no session streak.
					if !gateShapeOK(req.Context()) {
						resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(peek), resp.Body))
						resp.Header.Set(gateTerminalHeader, "1")
						if lastResp != nil {
							drainClose(lastResp)
						}
						rt.t.log().Warn("pool: 403 from a body without gate shape — egress not blamed",
							"req", reqID, "egress", rt.t.ipKey(lease.proxy, lease.publicIP),
							"header_ms", headerMs, "model", rt.model)
						return resp, nil
					}
					ekey := rt.t.ipKey(lease.proxy, lease.publicIP)
					if gateHits >= maxGateSwaps {
						resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(peek), resp.Body))
						resp.Header.Set(gateTerminalHeader, "1")
						// Egress identity for the session streak: the gate is
						// egress-correlated, so route.go must record the hit
						// against (session, this-egress), never session-only.
						resp.Header.Set(sessionGateEgressHeader, ekey)
						// Windowed FreeTier gate (2026-10-14 live: real TUI
						// direct gets the same body; egresses alternate
						// 200/gate per minute): parks expire in gateEgressBan,
						// so before admitting the terminal verdict, wait the
						// park out and re-sweep — bounded by
						// maxGateResweeps and the round-trip deadline.
						if isWindowedGateBody(peek) && gateResweeps < maxGateResweeps &&
							time.Now().Before(deadline.Add(-12*time.Second)) {
							drainClose(resp)
							gateResweeps++
							gateHits = 0
							_ = rt.releaseCurrent(req.Context())
							rt.t.log().Warn("pool: windowed gate — re-sweep after park",
								"req", reqID, "egress", ekey, "n", gateResweeps,
								"left", time.Until(deadline).String())
							select {
							case <-time.After(gateEgressBan + time.Second):
							case <-req.Context().Done():
								return nil, req.Context().Err()
							}
							continue
						}
						rt.t.log().Warn("pool: egress account-gate 403, gate budget spent, terminal",
							"req", reqID, "egress", ekey,
							"gate_hits", gateHits, "header_ms", headerMs, "model", rt.model)
						// Gate-drought heal: the sweep burned its whole budget
						// and every reachable egress answered the windowed
						// FreeTier body (2026-10-22 bursts every ~1-2h) — the
						// current WARP generation has no clean identity left.
						// Keep the honest 403 for THIS request, but re-register
						// identities (automated "restart warp-router") so the
						// next attempt lands on fresh /48 prefixes instead of
						// waiting the windows out. healRotate is throttled by
						// healGapFirst..healGapMax and no-ops while a rotate is
						// in flight; a true account ban (non-windowed body)
						// never rotates the pool.
						if isWindowedGateBody(peek) {
							rt.t.healRotate("windowed gate drought")
						}
						return resp, nil
					}
					gateHits++
					drainClose(resp)
					if lastResp != nil {
						drainClose(lastResp)
					}
					// r, not req: the synthetic verdict must expose the
					// request that was actually SENT (setEgressSession has
					// put the leased session on it), otherwise route.go
					// records the gate against no session at all.
					lastResp = syntheticGateVerdict(r, peek, ekey)
					// MIN-deadline is deliberate (1523-1528): a 10s gate park
					// must not shorten a live minute/RA ban on the same IP.
					if !rt.t.isBad(ekey, time.Now()) {
						rt.t.markBadSoft(ekey, gateEgressBan, time.Now())
					}
					rt.t.log().Warn("pool: egress account-gate 403, swapping lease",
						"req", reqID, "egress", ekey,
						"gate_hits", gateHits, "header_ms", headerMs, "model", rt.model)
					serverErrHits = 0
					_ = rt.releaseCurrent(req.Context())
					continue
				}
				// Non-region 403: НЕ терминал — свапаем внутри свипа как
				// region (2026-09-16: юзер видит частые 403, ретрай должен
				// быть внутри провайдера, клиент 403 не видит вообще).
				// Gate-вердикт липнет: смешанный gate→non-region затирал
				// gate-terminal в saturated-503 — такой 403 пропускаем без
				// затирания lastResp (gate-тела идут своей веткой выше и
				// gateHits-бюджет тратят там). Честный 403 важнее маски.
				if lastResp != nil && lastResp.Header.Get(gateTerminalHeader) != "" {
					drainClose(resp)
					_ = rt.releaseCurrent(req.Context())
					continue
				}
				drainClose(resp)
				if lastResp != nil {
					drainClose(lastResp)
				}
				lastResp = syntheticVerdict(req, http.StatusForbidden, "")
				ekey := rt.t.ipKey(lease.proxy, lease.publicIP)
				if !gateShapeOK(req.Context()) {
					// Same rule as the account-gate branch: a shape-less request
					// never indicts an egress.
					rt.t.log().Warn("pool: non-region 403 from a body without gate shape — egress not blamed",
						"req", reqID, "egress", ekey, "header_ms", headerMs, "model", rt.model)
					_ = rt.releaseCurrent(req.Context())
					continue
				}
				rt.t.markBadSoft(ekey, time.Minute, time.Now())
				rt.t.log().Warn("pool: egress non-region 403, swapping lease",
					"req", reqID, "egress", ekey,
					"header_ms", headerMs, "model", rt.model)
				// Consecutive-5xx: non-region-403 тоже не-5xx (волна 50).
				serverErrHits = 0
				_ = rt.releaseCurrent(req.Context())
				continue
			}
			if retryable(resp.StatusCode) {
				// Keep the VERDICT, not the drained body: route.go closes
				// 429/5xx without reading, so today the closed body is
				// invisible — but any future body reader gets "read on
				// closed body". Synthesize a fresh readable 1:1 verdict
				// (status preserved) — review 2026-09-09. Upstream
				// Retry-After is NOT carried out (syntheticVerdict drops
				// it: internal bans already consumed it, outward RA stops
				// the swap mode).
				// Gate-вердикт липнет: смешанный gate→429 отдавал saturated-503
				// вместо честного 403 (gate-terminal терялся в tailVerdict).
				if lastResp != nil && lastResp.Header.Get(gateTerminalHeader) != "" {
					drainClose(resp)
					_ = rt.releaseCurrent(req.Context())
					continue
				}
				ra0 := resp.Header.Get("Retry-After")
				lastResp = syntheticVerdict(req, resp.StatusCode, ra0)
				key := rt.t.ipKey(lease.proxy, lease.publicIP)
				if resp.StatusCode != http.StatusTooManyRequests {
					// Transient 5xx (500/502/503/524) is NOT a burned window:
					// short cooldown only, keep walking the pool. Treating it
					// like 429 froze every egress for 15 min on a single
					// transient 500 (opencode 500s non-stream) — pool-wide
					// death after ~6 hits.
					rt.t.markBadKeyFor(key, time.Now(), transientCooldown)
					// Consecutive, не cumulative: 429/region/silent между 5xx
					// сбрасывают счётчик ниже (иначе 500/429/500 ложно
					// детектит смерть модели).
					serverErrHits++
					// Upstream 500 на глубоком думании (muse-spark free-tier,
					// 2026-09-19): кластер роняет stripe целиком, соседние
					// egress при этом живые — брейк после 2 подряд жег
					// запрос раньше, чем свип доходил до живых. Порог 6:
					// мёртвая модель всё равно упрётся (все egress 500),
					// а думающая получит шанс на живом бэкенде.
					if serverErrHits >= 6 {
						rt.t.log().Warn("pool: upstream 5xx budget exhausted, breaking",
							"req", reqID, "hits", serverErrHits, "last_status", resp.StatusCode)
						exhausted = true
						_ = rt.releaseCurrent(req.Context())
						if lastResp != nil {
							return rt.tailVerdict(req, body, retryable, deadline, reqID, lastResp, rateLimitHits, regionHits)
						}
						break resweep
					}
					rt.t.log().Warn("pool: egress 5xx, cooling down",
						"req", reqID, "egress", key, "status", resp.StatusCode, "cooldown", transientCooldown.String(), "header_ms", headerMs)
					_ = rt.releaseCurrent(req.Context())
					continue
				}
				// This egress is hot: remember it — by public IP so every
				// backend behind the same anycast address cools down
				// together — then rotate. 429 cooldown follows the
				// upstream's Retry-After so a window rejoins rotation the
				// moment it reopens (a fixed 8h+ would strand live windows
				// for a day).
				okN := rt.t.okCount(key)
				ra := resp.Header.Get("Retry-After")
				// Hours-class Retry-After (3324s burn): this IP is dead
				// for hours — ban it for the FULL window but keep walking
				// the pool, the next egress may be live. Outward
				// Retry-After is never set (syntheticVerdict drops it):
				// tailVerdict maps the 429 to a 503 without Retry-After so
				// the client keeps retrying/swapping; the internal ban
				// stays full.
				if cd, hours := rt.t.hoursClassRA(ra); hours {
					raSecs, _ := parseRASeconds(ra)
					globalCluster, clustered := rt.t.noteHoursRAGuarded(key, raSecs, time.Now())
					if globalCluster {
						// SAME hours-class RA on distinct egresses: shared
						// budget, not a per-IP burn. No per-IP ban (shadows
						// of rotated identities are pure loss), no rotate
						// (fresh identities inherit the deadline). Short
						// park only. Next request enters fail-fast 1x1
						// (global block above): one probe confirms the
						// shared deadline, the verdict leaves as 503
						// without Retry-After (no outward RA: swap mode).
						cleared := rt.t.unbanClusteredHours(key)
						rt.t.banShort429(key, shortPark, time.Now())
						rt.noteThrottleHit(key, body, time.Now())
						until2, _ := rt.t.globalStatus()
						rt.t.log().Warn("pool: GLOBAL burn shadows unbanned",
							"req", reqID, "cleared", cleared, "until", until2.Format(time.RFC3339))
						// Wave 2026-09-20: не крутим ре-свипы до 165с в общий
						// сгоревший дедлайн — ранний хвост сразу, клиент
						// ретраит через ~60с, пул не самоподдерживает burn.
						_ = rt.releaseCurrent(req.Context())
						rateLimitHits++
						// Consecutive-5xx: global-429 тоже не-5xx (волна 50).
						serverErrHits = 0
						if lastResp != nil {
							return rt.tailVerdict(req, body, retryable, deadline, reqID, lastResp, rateLimitHits, regionHits)
						}
						return nil, &UpstreamStatusError{Status: http.StatusServiceUnavailable,
							Err: fmt.Errorf("provider %s: global budget burned, retry shortly", rt.prov)}
					}
					rt.t.log().Warn("pool: hours-class Retry-After, banning IP, walking on",
						"req", reqID, "egress", key, "retry_after", ra, "ok_before", okN, "cooldown", cd.String(), "header_ms", headerMs)
					// Isolated window burn: ban for the FULL hours RA the upstream named
					// (exact, no cap). Re-probing after 60s re-burns a full warmup on a dead
					// window and, worse, hands the same burned budget to the NEXT request
					// as a "fresh" lease — under RA the budget is shared, so new leases
					// inherit the same deadline and 429 again (2026-09-12: per-request
					// 60s re-probe kept the pool hot all evening). The IP rejoins only
					// after its own deadline; global-clustered shadows are still unbanned
					// above via unbanClusteredHours.
					// Clustered hours-RA (the same deadline reported by >=2 distinct
					// /48s) is a SHARED budget fact, not a per-IP one: banning the full
					// 12.5-15.8h per egress strands egresses that still answer 200s.
					// Measured 2026-10-22: 89 bans of ~45100s while 19 of 20 egresses
					// served 200s inside the same 10 minutes. Bounded ban keeps the
					// dead window out of the hot path without emptying the pool.
					cdBan := cd
					if clustered && cdBan > clusterBanCap {
						// Escalating ladder instead of a fixed 10-minute cap:
						// the cap alone let the same burned /128 be re-probed
						// ~7 times per day (3292 hours-429s on ~478 addresses).
						// First hit stays 10m, then 30m, then 2h — never the
						// full 12h window, which would strand live siblings.
						if esc := rt.t.banSpentStage(key); esc > 0 && esc < cdBan {
							cdBan = esc
						}
					}
					rt.t.markBad429(key, cdBan, time.Now())
					rt.noteThrottleHit(key, body, time.Now())
					// No outward Retry-After (см. выше): свап-режим, клиент
					// не должен бэкоффиться и стопить следующие свапы.
					// Consecutive-5xx: hours-429 тоже не-5xx (волна 50).
					serverErrHits = 0
					_ = rt.releaseCurrent(req.Context())
					rateLimitHits++
					if len(tried) >= triedExhaustFloor {
						rt.t.log().Warn("pool: all budget groups hot, tail verdict",
							"req", reqID, "egress", key, "tried", len(tried), "last_status", resp.StatusCode)
						if lastResp != nil {
							return rt.tailVerdict(req, body, retryable, deadline, reqID, lastResp, rateLimitHits, regionHits)
						}
						return nil, &UpstreamStatusError{Status: http.StatusServiceUnavailable,
							Err: fmt.Errorf("provider %s: pool all %d budget groups hot", rt.prov, len(tried))}
					}
					continue
				}
				// Любой не-hours 429 паркуется (без персиста/hard): без
				// следа каждый запрос жжёт прогрев в те же hot-IP и свап
				// топчется (2026-09-12: 600+ no-RA 429 за 14 мин). Парк —
				// всегда; именованный софт-бан — только по RA >= 1с (ниже).
				// Адрес, отслуживший окну <=2 запроса, паркуется надолго
				// (spentPark): он пришёл уже выжженным. 200 снимает оба
				// через markGood.
				// The cold branch below is the single OWNER of a spent-address
				// 429: parking it too double-counted one event in the coverage
				// metrics and saved state twice (review B6).
				raSecs, raOK := parseRASeconds(ra)
				haveRA := raOK
				recent2xx := rt.t.okFresh(key, cold2xxWindow, time.Now())
				cold := coldTransient429(recent2xx, raSecs, haveRA)
				if !cold {
					rt.t.parkThrottled(key, okN, time.Now())
				}
				// Бан РОВНО на время из ошибки (2026-09-16):
				// - RA >= minHoursBan: hours-класс выше уже забанил на
				//   полный дедлайн + hard (ветка hoursClassRA);
				// - RA 1с..minHoursBan: markBadSoft ровно на RA-секунды —
				//   окно назвало свой дедлайн, ждём именно его;
				// - RA = 0с (явный "0"/прошедшая HTTP-дата): секундный
				//   шейпер от частых запросов, НЕ выгоревший бюджет —
				//   только парк, без бана (иначе вечный 429, см. разбор
				//   застревания выше);
				// - RA нет вообще: таймаут/шейпер от частых запросов —
				//   только парк + heat, БЕЗ markBadKey (60с бан на живое
				//   окно за минуты кладёт все 20 egress и свапать не на
				//   что — вечный 429, см. разбор выше). Окно живо,
				//   следующий ре-свип его переиспользует.
				if cold {
					// Not a burst shaper: an address handed to us already spent.
					// A 5s park forgets it between sweeps, so the same dead /128
					// was re-drawn ~7x (29888 transient 429s/day). Ladder-ban it
					// instead; one 200 anywhere lifts the ban (markGoodKey).
					if cd := rt.t.banSpent(key, time.Now()); cd > 0 {
						rt.t.log().Warn("pool: cold egress without budget, ladder ban",
							"req", reqID, "egress", key, "ban", cd.String(),
							"ok_before", okN, "retry_after", ra, "status", resp.StatusCode)
					}
					rt.noteThrottleHit(key, body, time.Now())
					// The ladder ban must stay falsifiable: queue the address for
					// the standby verifier (max_tokens=1, off the hot path), else
					// "one 200 anywhere lifts the ban" could never happen before
					// the ban itself expires.
					rt.t.enqueueStandby(key)
					serverErrHits = 0
					_ = rt.releaseCurrent(req.Context())
					rateLimitHits++
					continue
				}
				if secs, ok := parseRASeconds(ra); ok && secs > 0 && secs < int(minHoursBan.Seconds()) {
					rt.t.markBadSoft(key, time.Duration(secs)*time.Second, time.Now())
					// Wave 2026-09-20: минутные RA (60-120с) на DISTINCT IP —
					// тоже shared budget, но детектор их не видел (только
					// hours-класс). Без hard-бана: кластерим, чистим парки,
					// уходим в fail-fast вместо per-IP бана-пилы.
					if secs >= 30 {
						if global := rt.t.noteHoursRA(key, secs); global {
							cleared := rt.t.unbanClusteredHours(key)
							// review_1 B: unban стирает только что названный
							// минутный soft-бан (members содержит key) — без
							// парка следующий acquire выдаст тот же egress
							// (certain-429). Парк как в hours-ветке.
							rt.t.banShort429(key, shortPark, time.Now())
							until3, _ := rt.t.globalStatus()
							rt.t.log().Warn("pool: MINUTE-class global burn shadows unbanned",
								"req", reqID, "cleared", cleared, "until", until3.Format(time.RFC3339))
							serverErrHits = 0
							if lastResp != nil {
								return rt.tailVerdict(req, body, retryable, deadline, reqID, lastResp, rateLimitHits, regionHits)
							}
							return nil, &UpstreamStatusError{Status: http.StatusServiceUnavailable,
								Err: fmt.Errorf("provider %s: global budget burned, retry shortly", rt.prov)}
						}
					}
				}
				rt.t.log().Warn("pool: egress throttled, rotating",
					"req", reqID, "lease", lease.proxy, "egress", key, "ok_before", okN, "status", resp.StatusCode,
					"retry_after", ra, "header_ms", headerMs)
				// Учёт шторма: no-RA и минутные 429 — тоже хиты шторма.
				// Без этого stormActive() мёртв (счётчик пишут только hours-ветки),
				// fail-fast 1x1 не включается, и каждый запрос жжёт полный 5x2
				// свип в горячий пул, углубляя шторм (2026-09-16: 88 throttled
				// за запрос при темпе 40/мин — ни одного storm-лога).
				rt.noteThrottleHit(key, body, time.Now())
				// 429-битый IP — в очередь verify-потока: проверка max_tokens=1
				// мимо горячего пути, 200 вернёт его в ротацию сразу, остальные
				// запросы тем временем идут в живые.
				rt.t.enqueueStandby(key)
				// Consecutive-5xx: не-5xx (429/region) обнуляет счётчик смерти
				// модели — иначе 500/429/500 ложно брейкает (волна 50).
				serverErrHits = 0
				_ = rt.releaseCurrent(req.Context())
				rateLimitHits++
				// Wave 2026-09-20 (40% saturated-лотерея): в fail-fast 1x1
				// первый 429 — не приговор: половина пула жива, вторая
				// проба на другом egress стоит 1 прогрев вместо 10.
				// Пересдача один раз на запрос, только при живом бюджете —
				// и только на НЕПРОБОВАННОЙ /48-группе: пересдача на той же
				// группе это re-warm того же окна (no-swap 2026-09-21).
				// Tail only after the LIMITED budget is actually walked: one 429 out
				// of a 3-probe budget is not a dead pool (96.5% of today's saturated
				// tails were literal "(1 throttled)" because this fired on the first).
				probeHits := rateLimitHits + serverErrHits
				if failFastProbe && !secondChance && probeHits >= effFanout && time.Until(deadline) > 10*time.Second {
					// Peek is a freshness probe, not a full spin: 3s cap on ONE peek,
					// up to poolSecondChancePeeks probes. Одна проба почти всегда
					// возвращала ту же /48-группу, что уже в tried (warp раздаёт по
					// нескольку IP на группу), и запрос уходил в saturated-503 при
					// 5 живых группах рядом (2026-10-21: 15% ошибок под нагрузкой).
					pctx, pcancel := context.WithTimeout(req.Context(), 3*time.Second)
					var peek *poolLease
					var perr error
					peeks := poolSecondChancePeeks
					if effFanout > 1 {
						peeks = 1
					}
					for probe := 0; probe < peeks; probe++ {
						peek, perr = rt.t.acquireLease(pctx, reqID, rt.model, rt.stamp)
						if perr != nil || peek == nil {
							break
						}
						pk := triedKey(rt.t.ipKey(peek.proxy, peek.publicIP))
						if !tried[pk] {
							break
						}
						_ = rt.t.release(context.Background(), peek.leaseID)
						peek = nil
					}
					pcancel()
					if perr == nil && peek != nil {
						pkey := triedKey(rt.t.ipKey(peek.proxy, peek.publicIP))
						_ = rt.t.release(context.Background(), peek.leaseID)
						if !tried[pkey] {
							secondChance = true
							burned := rt.t.ipKey(lease.proxy, lease.publicIP)
							tried[triedKey(burned)] = true
							rt.t.log().Info("pool: fail-fast second chance, re-dealing",
								"req", reqID, "burned", burned, "fresh_group", pkey)
							continue
						}
					}
					// Short-park storm (kilo keyless): no fresh /48 on peek, but
					// every ban is a 5s park and the client budget is alive —
					// windows reopen within seconds, so an immediate "saturated"
					// tail is a false negative (2026-10-12: 62% of tails had
					// <=4 warms of the 7-group floor, 843 closed <5s while
					// parks are 5s). Two short waits + re-sweep beat the 503;
					// hours-class bans or a global burn keep the honest tail.
					if shortPoolWaits < 2 && time.Until(deadline) > 20*time.Second && !rt.t.hardBansCoverPool(time.Now()) {
						shortPoolWaits++
						_ = rt.releaseCurrent(req.Context())
						rt.t.log().Info("pool: short-park wait before saturated tail",
							"req", reqID, "rate_limit_hits", rateLimitHits, "tried", len(tried),
							"waits", shortPoolWaits, "left", time.Until(deadline).String())
						select {
						case <-req.Context().Done():
							return nil, req.Context().Err()
						case <-time.After(2500 * time.Millisecond):
						}
						secondChance = false
						continue
					}
					// No fresh group on peek: honest tail immediately —
					// a re-sweep now re-warms burned budgets (all cands are
					// same-/48 or parked); waiting only burns client budget.
					if lastResp != nil {
						return rt.tailVerdict(req, body, retryable, deadline, reqID, lastResp, rateLimitHits, regionHits)
					}
					return nil, rt.saturatedError(rateLimitHits, regionHits)
				}
				// storm wait-reopen (2026-09-23): в 1x1 после пересдачи —
				// НЕ мгновенный хвост при 1-2 throttled: окна
				// переоткрываются за секунды, половина пула
				// жива — короткая пауза + новый свип на свежую
				// группу вместо 503 (88x saturated-1 за 30 мин).
				// Мгновенный хвост только когда ждать
				// бессмысленно: shared-дедлайн (global burn),
				// честное покрытие (tried>=floor) или мёртвый
				// бюджет (<15с). Страх review_2 F3 про ~80 прогревов
				// не подтверждается: каждый свип паркует/
				// банит прогретый egress, следующий свип
				// идёт в СВЕЖУЮ группу, а не в re-warm.
				if failFastProbe && secondChance {
					honest := len(tried) >= triedExhaustFloor
					sharedBurn := false
					sharedBurnHours := false
					if until, gra := rt.t.globalStatus(); !until.IsZero() && time.Now().Before(until) {
						sharedBurn = true
						// Часы-истина vs минуты-ложь: минутный/стал-флаг при живых
						// окнах рядом — лотерея 1x1, а не shared-дедлайн. Мгновенный
						// хвост только при часовом burn; при минутном разрешаем
						// 1 reopen-паузу чтобы probation-датчик 2x200 дожил.
						if gra >= int(minHoursBan.Seconds()) {
							sharedBurnHours = true
						}
					}
					tailNow := honest || time.Until(deadline) < 15*time.Second
					if sharedBurn && !tailNow {
						tailNow = sharedBurnHours || stormWaits >= 1
					}
					if tailNow {
						_, gra := rt.t.globalStatus()
						rt.t.log().Warn("pool: storm 1x1 exhausted, tail verdict",
							"req", reqID, "rate_limit_hits", rateLimitHits, "region_hits", regionHits,
							"tried", len(tried), "shared_burn", sharedBurn, "shared_hours", sharedBurnHours,
							"global_ra", gra, "storm_waits", stormWaits, "verdictless_waits", verdictlessWaits)
						if lastResp != nil {
							return rt.tailVerdict(req, body, retryable, deadline, reqID, lastResp, rateLimitHits, regionHits)
						}
						return nil, rt.saturatedError(rateLimitHits, regionHits)
					}
					// Storm reopen cap: пауза имеет смысл, пока свипы греют
					// свежие группы; больше 5 пауз — быстрый хвост.
					// 2026-10-06: шторм-коллапс all-hot держал 503 ~48 мин —
					// short-парки 5-15с не успевали остыть за 3 паузы x 2с.
					stormWaits++
					if stormWaits > 5 {
						_, gra := rt.t.globalStatus()
						rt.t.log().Warn("pool: storm reopen capped, tail verdict",
							"req", reqID, "rate_limit_hits", rateLimitHits, "region_hits", regionHits,
							"tried", len(tried), "waits", stormWaits, "global_ra", gra,
							"verdictless_waits", verdictlessWaits)
						if lastResp != nil {
							return rt.tailVerdict(req, body, retryable, deadline, reqID, lastResp, rateLimitHits, regionHits)
						}
						return nil, rt.saturatedError(rateLimitHits, regionHits)
					}
					_, gra := rt.t.globalStatus()
					rt.t.log().Info("pool: storm 1x1 waiting for reopen, re-dealing",
						"req", reqID, "rate_limit_hits", rateLimitHits, "tried", len(tried),
						"waits", stormWaits, "global_ra", gra,
						"left", time.Until(deadline).String())
					_ = rt.releaseCurrent(req.Context())
					secondChance = false
					// Пауза 5с перед ре-свипом: short-парки 5-15с должны
					// остыть, иначе следующий свип снова в all-hot (см. выше).
					// Бюджет проверен выше: tailNow уже исключил <15с.
					if !sleepOrCancel(5*time.Second, req.Context()) {
						return nil, req.Context().Err()
					}
					continue
				}
				continue
			}
			// Success / non-retryable client error: this egress works —
			// clear any cooldown and keep the current lease owned by the
			// caller's Release (streaming body stays valid).
			//
			// BUT: an upstream may still lie with a 200: some opencode CF
			// clusters answer SSE "error NOT_FOUND" (model not deployed on
			// that anycast class) with HTTP 200. Sniff the first SSE event
			// and, if it is NOT_FOUND, blacklist this egress long-term
			// (15 min — a stable property of the cluster, not saturation)
			// and rotate; the loop falls through to the next backend or to
			// the home-IP direct fallback instead of serving the error.
			sniffStart := time.Now()
			sniffed, isNotFound, aborted, isRegionErr := rt.sniffNotFound(resp)
			sniffMs = time.Since(sniffStart).Milliseconds()
			if aborted {
				// Upstream sent nothing within the sniff budget: the
				// egress is silent/black-holed. Cool it down (without this
				// every request re-burns the 15s budget on the same dead
				// egress) and rotate — a wedged stream must not hold the
				// caller for minutes.
				if lease != nil {
					key := rt.t.ipKey(lease.proxy, lease.publicIP)
					rt.t.markBadKeyFor(key, time.Now(), transientCooldown)
					rt.t.log().Warn("pool: egress silent, cooling down",
						"req", reqID, "egress", key, "cooldown", transientCooldown.String(),
						"header_ms", headerMs, "sniff_ms", sniffMs)
				}
				// Consecutive-5xx: silent egress is wedged, not a dead model.
				serverErrHits = 0
				_ = rt.releaseCurrent(req.Context())
				continue
			} else if isRegionErr {
				// 200-SSE whose first event is a region_error (responses
				// path): same swap as the 403 branch, but inside ONE sweep
				// — tried is shared, no outer re-acquire. No ban (per
				// (model, region) property); remembered model-scoped. The
				// sniff buffer is already drained, so lastResp is synthetic.
				// Gate verdict sticks here too (gate→region-as-200 mix).
				if lastResp != nil && lastResp.Header.Get(gateTerminalHeader) != "" {
					drainClose(resp)
					_ = rt.releaseCurrent(req.Context())
					continue
				}
				drainClose(resp)
				if lastResp != nil {
					drainClose(lastResp)
				}
				lastResp = syntheticRegionVerdict(req, nil)
				regionHits++
				ekey2 := ""
				if lease != nil {
					ekey2 = rt.t.ipKey(lease.proxy, lease.publicIP)
					rt.t.noteRegionBad(rt.model, ekey2, time.Now())
				}
				rt.t.log().Warn("pool: egress region-forbidden-as-200, swapping lease",
					"req", reqID, "egress", ekey2,
					"region_hits", regionHits, "header_ms", headerMs, "sniff_ms", sniffMs, "model", rt.model)
				// Consecutive-5xx: region property, not a model death.
				serverErrHits = 0
				_ = rt.releaseCurrent(req.Context())
				continue
			} else if isNotFound {
				// Do NOT set lastResp: the body is already drained (EOF)
				// and a 200 verdict would RecordSuccess + serve an empty
				// 200 to the client instead of the 404 cascade — review
				// 2026-09-09. Drain for conn reuse and walk on; the tail
				// returns an honest error when nothing else answered.
				drainClose(resp)
				key := rt.t.ipKey(lease.proxy, lease.publicIP)
				rt.t.log().Warn("pool: egress NOT_FOUND-as-200, blacklisting",
					"req", reqID, "egress", key, "blacklist", unusableCooldown.String())
				rt.t.markUnusableKey(key, time.Now())
				// Покрытие как у 429-ветки: иначе 3 блеклиста не добирают
				// tried-floor 7 и запрос ре-свипает до poolTry-дедлайна 165с
				// (тест TestPoolAllNotFoundIsNot200 висел; в проде то же —
				// мёртвая модель держала клиента весь бюджет).
				if gk := triedKey(key); gk != "" {
					tried[gk] = true
				}
				// NOT_FOUND-as-200: кластер не знает модель. Счётчик как у 5xx —
				// мёртвая везде модель должна брейкать рано, а не ре-свипать 165с.
				serverErrHits++
				// Мёртвая везде модель: все прогретые egress отвечают NOT_FOUND —
				// дальше греть нечего, ранний хвост вместо ре-свипов до 165с.
				if serverErrHits >= 6 {
					rt.t.log().Warn("pool: upstream NOT_FOUND budget exhausted, breaking",
						"req", reqID, "hits", serverErrHits)
					_ = rt.releaseCurrent(req.Context())
					break resweep
				}
				_ = rt.releaseCurrent(req.Context())
				if len(tried) >= triedExhaustFloor {
					rt.t.log().Warn("pool: all budget groups NOT_FOUND, early tail",
						"req", reqID, "tried", len(tried))
					break resweep
				}
				continue
			} else if sniffed != nil {
				resp.Body = sniffed
			}
			rt.t.noteOK(rt.t.ipKey(lease.proxy, lease.publicIP), resp.StatusCode)
			rt.t.heatCool(rt.t.ipKey(lease.proxy, lease.publicIP), time.Now())
			rt.noteStormSuccessKey(rt.t.ipKey(lease.proxy, lease.publicIP), body, time.Now())
			rt.t.markGood(rt.t.ipKey(lease.proxy, lease.publicIP))
			// Region pin: this egress served the model after regionHits
			// swaps — the next request goes straight here, skipping the
			// discovery sweep (~2-5s on muse-1.3).
			if regionHits > 0 {
				rt.t.noteRegionGood(rt.model, rt.t.ipKey(lease.proxy, lease.publicIP))
			}
			rt.t.log().Info("pool: ok",
				"req", reqID, "lease", lease.proxy, "ip", rt.t.ipKey(lease.proxy, lease.publicIP),
				"status", resp.StatusCode,
				"acquire_ms", acquireMs, "header_ms", headerMs,
				"sniff_ms", sniffMs, "total_ms", time.Since(attemptStart).Milliseconds())
			rt.t.poolEvent(lease.proxy, "acquire", true, time.Since(attemptStart).Milliseconds(), "")
			return resp, nil
		}
		// Конец свипа без успеха: слот чистим. Бюджет жив — пауза и новый
		// свип (tried НЕ сбрасываем — покрытие 9x/48 кумулятивно, волна 50;
		// истекшие парки снова живые); бюджет мёртв — выход на хвост.
		_ = rt.releaseCurrent(req.Context())
		if time.Now().After(deadline) {
			break
		}
		rt.t.log().Info("pool: re-sweep, budget left",
			"req", reqID, "left", time.Until(deadline).String(),
			"rate_limit_hits", rateLimitHits, "server_err_hits", serverErrHits,
			"region_hits", regionHits)
		// Короткая пауза, чтобы окна успели переоткрыться, а не молотить
		// hot-бюджеты вхолостую. Ниже дедлайна не спим.
		if req.Context().Err() != nil {
			_ = rt.releaseCurrent(context.Background())
			return nil, req.Context().Err()
		}
		if sleep := time.Until(deadline); sleep > 2*time.Second {
			if !sleepOrCancel(2*time.Second, req.Context()) {
				_ = rt.releaseCurrent(context.Background())
				return nil, req.Context().Err()
			}
		} else if sleep > 0 {
			if !sleepOrCancel(sleep, req.Context()) {
				_ = rt.releaseCurrent(context.Background())
				return nil, req.Context().Err()
			}
			break
		}
		// Новый свип поверх: tried НЕ сбрасываем (волна 50): покрытие 9x/48
		// кумулятивно — второй проход добивает оставшиеся группы, а не
		// перегревает те же 5. Сбрасываем только повторы/парки; пик повторов
		// копим в maxRepeatHits — иначе rotate-гейт ниже не видит LRU-циклы
		// (repeats>=3 при малом tried) после сброса.
		// exhausted НЕ сбрасываем: смерть модели переживает ре-свип.
		if repeatHits > maxRepeatHits {
			maxRepeatHits = repeatHits
		}
		repeatHits = 0
		parkPasses = 0
	}
	// Pool is spent for this request. A burned pool must be re-armed with
	// fresh WARP identities — кроме мёртвой МОДЕЛИ (5xx-exhausted): свежие
	// identity делят ту же мёртвую модель, респаун 1-2 мин бесполезен
	// (как global burn выше). Волна 50.
	// Rotate только при доказанном исчерпании: покрытие + вердикты (свип обошёл
	// все бюджеты И 429 это подтвердили) или дедлайн съеден, или LRU-циклы
	// (maxRepeats + hits при малом tried — пик копим, т.к. футер сбрасывает
	// repeatHits каждый свип). Чистый tried>=floor без 429 — транспорт/
	// NOT_FOUND/region набрали покрытие без выгорания: respawn 24 бесполезен.
	// Иначе каждый быстрый 1x1-хвост жал бы respawn 24 при троттле 60с — flap
	// starting вместо службы.
	if activityGlobal := rt.t.globalActive(); activityGlobal {
		// The burn flag names a retired generation: rotate instead of waiting
		// it out (see healRotate). Escalating gap keeps this from flapping.
		rt.t.healRotate("saturated tail during global burn")
	} else if exhausted {
		rt.t.log().Info("pool: dead-model 5xx, skipping rotate (fresh identities share the dead model)",
			"req", reqID, "server_err_hits", serverErrHits)
	} else if (len(tried) >= triedExhaustFloor && rateLimitHits >= 3) || time.Now().After(deadline) || (maxRepeatHits >= 3 && rateLimitHits >= 5) {
		rt.t.maybeRotatePool()
	}
	// Хвост: 429/403 наружу НЕ отдаём (OMP NC эскалирует их в часовые
	// credential-блоки 1800000ms > maxDelayMs 300s → смерть очереди после
	// 1 попытки, 2026-09-16). Вместо вердикта — 503 без Retry-After:
	// OMP классифицирует как MODEL_CAPACITY и ретраит очередь, а не
	// убивает. Тело честное (что было внутри) для диагностики.
	// Главный хвост — через общий helper: 429/403 маппятся в 503,
	// прочие вердикты (5xx) идут как есть без Retry-After.
	if lastResp != nil {
		return rt.tailVerdict(req, body, retryable, deadline, reqID, lastResp, rateLimitHits, regionHits)
	}
	// Silent-tail verdict: everything parked/skipped, zero warmups.
	// Тоже 503, не plain: см. выше (review 2026-09-16, волна 50).
	rt.t.log().Warn("pool: request verdict: no upstream verdict",
		"req", reqID, "rounds", effRounds, "fanout", effFanout,
		"tried", len(tried), "rate_limit_hits", rateLimitHits,
		"server_err_hits", serverErrHits, "region_hits", regionHits)
	return nil, &UpstreamStatusError{Status: http.StatusServiceUnavailable,
		Err: fmt.Errorf("provider %s: pool has no ready egress, retry shortly (%d rounds x %d fanout)", rt.prov, effRounds, effFanout)}
}

// tailVerdict возвращает накопленный вердикт с единой политикой наружу
// (2026-09-16): 429/403 клиент не видит никогда — вместо них 503 без
// Retry-After (OMP ретраит очередь, а не убивает часовыми блоками).
// direct-хвост — последний шанс перед маппингом. Возвращает (resp, nil)
// для успеха/direct, или (nil, err503) для маппинга, или (lastResp, nil)
// для прочих вердиктов (5xx).
func (rt *poolRetryRT) tailVerdict(req *http.Request, body []byte, retryable func(int) bool, deadline time.Time, reqID uint64, lastResp *http.Response, rateLimitHits, regionHits int) (*http.Response, error) {
	if lastResp == nil {
		rt.t.poolEvent("", "acquire", false, 0, "no upstream verdict")
		return nil, nil
	}
	if dr := rt.tryDirectFallback(req, body, retryable, deadline, reqID); dr != nil {
		return dr, nil
	}
	if lastResp.StatusCode == http.StatusTooManyRequests {
		drainClose(lastResp)
		rt.t.log().Warn("pool: verdict mapped to 503 (client must retry, not die)",
			"req", reqID, "was", lastResp.StatusCode,
			"rate_limit_hits", rateLimitHits, "region_hits", regionHits)
		rt.t.poolEvent("", "acquire", false, 0, "saturated: throttled egress")
		// Rotate: раньше — только из resweep-хвоста (tried>=floor/дедлайн),
		// потому что «каждый saturated-1 жал respawn» клал пул во flap.
		// Сейчас ротацию запускает saturatedError — но лишь после
		// satRotateAfter подряд и не чаще satRotateCooldown (3 мин > время
		// респауна 1-2 мин), так что flap-сценарий не возвращается, а
		// молчащий пул перестаёт вечно отдавать 503 (инцидент 2026-10-17).
		return nil, rt.saturatedError(rateLimitHits, regionHits)
	}
	// Region-exhausted 403 — честный 403 наружу (модель не served в регионе:
	// ретраить очередь бессмысленно, клиент должен увидеть region_error).
	// Gate-terminal 403 — тоже честный 403 наружу (исчерпанный гейт-бюджет:
	// клиент видит настоящую причину, а не 503-маску).
	// Non-region 403 (лимитный класс того же провайдера) маппится в 503.
	if lastResp.StatusCode == http.StatusForbidden && lastResp.Header.Get(regionExhaustedHeader) == "" && lastResp.Header.Get(gateTerminalHeader) == "" {
		drainClose(lastResp)
		rt.t.log().Warn("pool: verdict mapped to 503 (client must retry, not die)",
			"req", reqID, "was", lastResp.StatusCode,
			"rate_limit_hits", rateLimitHits, "region_hits", regionHits)
		rt.t.poolEvent("", "acquire", false, 0, "saturated: throttled egress")
		return nil, rt.saturatedError(rateLimitHits, regionHits)
	}
	return lastResp, nil
}

// syntheticVerdict rebuilds a readable 1:1 verdict response from a
// drained upstream answer: status preserved, body fresh. Upstream
// Retry-After is deliberately NOT forwarded: internal bans already consumed
// it, and an outward RA makes OMP/Omni back off and stop swapping (swap
// mode 2026-09-11: requests exist to rotate egress, backoff blocks that).
// RoundTrip must never return a drained+closed body with nil error —
// http.Client callers (and any future body reader) get "read on closed
// body" or an empty success instead of the real verdict.
func syntheticVerdict(req *http.Request, status int, retryAfter string) *http.Response {
	h := http.Header{}
	_ = retryAfter // consumed internally, never forwarded outward
	h.Set("Content-Type", "application/json")
	body := io.NopCloser(strings.NewReader(
		fmt.Sprintf(`{"error":{"message":"pool egress verdict %d","type":"rate_limit_error","code":"pool_verdict"}}`, status)))
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     h,
		Body:       body,
		Request:    req,
	}
}

// saturatedError builds the terminal "pool saturated" 503 (no Retry-After —
// OMP escalates a 429 into hour-long credential blocks) and runs the
// saturation escape hatch: a sustained streak of these means the pool keeps
// handing out burned egresses, so re-arm it (rotate) instead of answering 503
// forever (incident 2026-10-17).
func (rt *poolRetryRT) saturatedError(rateLimitHits, regionHits int) error {
	rt.t.noteSaturatedTail()
	rt.t.maybeRotateSaturated()
	return &UpstreamStatusError{Status: http.StatusServiceUnavailable,
		Err: fmt.Errorf("provider %s: pool saturated, retry shortly (%d throttled, %d region)", rt.prov, rateLimitHits, regionHits)}
}

// poolDeadLastResort reports whether the home-IP direct leg may be tried:
// pool API dead (breaker open) OR the pool is alive but has served nothing for
// poolEmptyGrace while saturation tails pile up, and the home window itself is
// fresh. A healthy pool never opens this path. Both keys are required here
// (direct_fallback + allow_home_ip_direct): the 2026-10-19 home-IP 403 «abusive
// traffic» ban came from direct legs we never meant to authorize.
func (rt *poolRetryRT) poolDeadLastResort() bool {
	now := time.Now()
	if !rt.t.homeDirectAllowed() {
		return false
	}
	if !rt.t.apiDown() && !rt.t.poolSilentFor(poolEmptyGrace) {
		return false
	}
	return !rt.t.isBad(directHomeIPKey, now) && !rt.t.isParked(directHomeIPKey, now)
}

func (rt *poolRetryRT) tryDirectFallback(req *http.Request, body []byte, retryable func(int) bool, deadline time.Time, reqID uint64) *http.Response {
	// Second key (allow_home_ip_direct) is enforced inside poolDeadLastResort:
	// direct_fallback alone never dials the upstream from our own IP.
	// Last-resort only: pool API dead (breaker open) OR the pool stopped
	// serving (saturated for poolEmptyGrace) AND the home window is fresh.
	// A healthy pool hands out live egresses and never reaches this: going
	// direct there would burn the home (IP,UA) window for nothing.
	if !rt.poolDeadLastResort() {
		return nil
	}
	rt.t.log().Warn("pool: last-resort direct home-IP leg",
		"req", reqID, "api_down", rt.t.apiDown(), "pool_silent", rt.t.poolSilentFor(poolEmptyGrace),
		"saturated_streak", rt.t.satTailStreak.Load())
	r := cloneRequest(req, body)
	// Pin UA like the pool path: the free-tier window binds (IP, full-UA).
	if ua := leaseUA(rt.provProvider, "", r.Header.Get("User-Agent")); ua != "" {
		r.Header.Set("User-Agent", ua)
	}
	// Auth parity with the pool path: pool warmups carry the provider key
	// (currentKeyFor); a bare direct without it earns 401 on keyed routes.
	if rt.prov != "" {
		if k := rt.t.currentKeyFor(rt.prov); k != "" {
			r.Header.Set("Authorization", "Bearer "+k)
		}
	}
	// Opencode session headers: zen gates free tier on x-opencode-session
	// (400 MissingSessionID without it). Pool path gets them via doRequest;
	// the direct leg builds raw, so stamp here from provProvider.
	if rt.provProvider != nil {
		setOpencodeHeaders(r, r.Context(), rt.provProvider)
	}
	if ses := rt.t.stickySession(directHomeIPKey); ses != "" {
		setEgressSession(r.Header, ses)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		remaining = 5 * time.Second
	}
	rht := 35 * time.Second
	if remaining < rht {
		rht = remaining
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = rht
	total := 75 * time.Second
	if remaining < total {
		total = remaining
	}
	directClient := &http.Client{Transport: tr, Timeout: total}
	dCtx, dCancel := context.WithDeadline(r.Context(), deadline)
	r = r.WithContext(dCtx)
	resp, err := directClient.Do(r)
	if err == nil && resp != nil && resp.Body != nil {
		resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: dCancel}
	} else {
		dCancel()
	}
	if err != nil {
		rt.t.log().Warn("pool: direct last-resort failed", "req", reqID, "err", err)
		return nil
	}
	if retryable(resp.StatusCode) {
		drainClose(resp)
		cd := rt.t.cooldownN()
		if resp.StatusCode == http.StatusTooManyRequests {
			if hours, ok := rt.t.hoursClassRA(resp.Header.Get("Retry-After")); ok {
				cd = hours
				rt.t.markBad429(directHomeIPKey, cd, time.Now())
			} else {
				cd = shortPark
				rt.t.banShort429(directHomeIPKey, cd, time.Now())
			}
		} else {
			rt.t.markBadKey(directHomeIPKey, time.Now())
		}
		rt.t.log().Warn("pool: direct last-resort also limited",
			"req", reqID, "status", resp.StatusCode, "cooldown", cd.String())
		return nil
	}
	rt.t.markGoodKey(directHomeIPKey)
	// Attribution marker: request_log.proxy carries the last LANE lease, so a
	// home-leg success was indistinguishable from a pooled one and masked exactly
	// the outage it papers over (2026-10-22 16:15: 101 "direct last-resort ok" vs
	// 37 rows tagged with lane proxies). The gateway honours this header.
	if resp.Header != nil {
		resp.Header.Set(egressSourceHeader, directHomeIPKey)
	}
	rt.t.log().Warn("pool: direct last-resort ok", "req", reqID, "status", resp.StatusCode)
	return resp
}

// deployed on this egress' cluster) masked as HTTP 200. On a plain success
// it returns a wrapped body prefixed with the bytes already consumed; on
// NOT_FOUND it returns (nil, true).
const sniffLimit = 8192

const sniffFirstByteTimeout = 15 * time.Second

// sniffTimedBody races the FIRST payload read against a deadline; later
// reads pass straight through with no extra cost. Unlike the previous
// goroutine-per-read design, it uses a context timeout on the underlying
// connection when available — no leaked goroutine on timeout.
type sniffTimedBody struct {
	io.ReadCloser
	d    time.Duration
	once bool
	mu   sync.Mutex
}

var errSniffTimeout = errors.New("pool: upstream silent, no first byte within sniff budget")

func (b *sniffTimedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	first := !b.once
	b.once = true
	b.mu.Unlock()
	if !first {
		return b.ReadCloser.Read(p)
	}
	// Use connection deadline when the body wraps a net.Conn-backed stream.
	// For plain ReadClosers fall back to context-style timeout via
	// goroutine only on first byte — but make it cancellable by closing
	// the body on timeout.
	type res struct {
		n   int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		n, err := b.ReadCloser.Read(p)
		select {
		case ch <- res{n, err}:
		default:
			// Body already timed out and closed; drop leaked result.
		}
	}()
	select {
	case r := <-ch:
		return r.n, r.err
	case <-time.After(b.d):
		// Closing body unblocks the leaked reader goroutine in most
		// http.Transport cases (the connection read returns).
		_ = b.ReadCloser.Close()
		return 0, errSniffTimeout
	}
}
func (rt *poolRetryRT) sniffNotFound(resp *http.Response) (body io.ReadCloser, isNotFound, aborted, isRegionErr bool) {
	if resp == nil || resp.Body == nil {
		return nil, false, false, false
	}
	sniffBody := &sniffTimedBody{ReadCloser: resp.Body, d: sniffFirstByteTimeout}
	var buf []byte
	tmp := make([]byte, 4096)
	for len(buf) < sniffLimit {
		n, err := sniffBody.Read(tmp)
		if errors.Is(err, errSniffTimeout) {
			// Kill the worker read (it returns once the body closes) and
			// signal rotation. Bytes that raced in before the close are
			// lost — acceptable: this egress is being abandoned anyway.
			_ = resp.Body.Close()
			return nil, false, true, false
		}
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if bytes.Contains(buf, []byte("NOT_FOUND")) || bytes.Contains(buf, []byte("Model not found")) {
				// Drain whatever remains so the connection can be released.
				_, _ = io.Copy(io.Discard, resp.Body)
				return nil, true, false, false
			}
			// Region rejection masked as HTTP 200 (responses-path SSE
			// error event): strict matcher, only explicit region_error
			// or region+not-supported. Same swap as the 403 branch, but
			// inside this sweep — no outer re-acquire, no extra rounds.
			if isRegionErrorEvent(buf) {
				_, _ = io.Copy(io.Discard, resp.Body)
				return nil, false, false, true
			}
			if bytes.Contains(buf, []byte{'\n', '\n'}) {
				break // first SSE event complete — this is a real success
			}
		}
		if err != nil {
			break
		}
	}
	if len(buf) == 0 {
		return resp.Body, false, false, false
	}
	return &sniffedBody{Reader: io.MultiReader(bytes.NewReader(buf), resp.Body), closer: resp.Body}, false, false, false
}

// sniffedBody replays sniffed prefix bytes, then streams the rest — and
// forwards Close to the underlying body so cancelOnCloseBody still fires
// (plain NopCloser swallowed it, leaking conn+ctx per 200).
type sniffedBody struct {
	io.Reader
	closer io.Closer
}

func (b *sniffedBody) Close() error { return b.closer.Close() }

// cancelOnCloseBody ties per-attempt context cancel to streaming body lifecycle.
// The body outlives poolTry deadline but is cancelled on parent abort or Close.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *cancelOnCloseBody) Close() error {
	b.once.Do(func() { b.cancel() })
	return b.ReadCloser.Close()
}

// clientFor возвращает кэшированный HTTP-клиент на egress-прокси (один на
// proxy URL, cap 64): раньше Clone на каждый attempt тёк (сотни Transports
// за 100-параллель, TIME_WAIT churn). Кэш под pxMu; evict oldest при
// переполнении. Timeout НЕ мутируем на возвращённом (probeLease 3s убран
// в контекст — см. ниже), иначе отравим egress всем.
func (p *PoolTransport) clientFor(proxy string) *http.Client {
	p.pxMu.Lock()
	defer p.pxMu.Unlock()
	if p.pxClients == nil {
		p.pxClients = map[string]*http.Client{}
	}
	if cl, ok := p.pxClients[proxy]; ok {
		return cl
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return &http.Client{Timeout: 300 * time.Second}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(u)
	tr.ResponseHeaderTimeout = poolHeaderThinkTimeout
	// opencode free-tier gate (MITM matrix 2026-10-24): upstream answers
	// 403 FreeTierError whenever this leg negotiates h2 — the real TUI
	// (undici) speaks http/1.1, and a Go probe with forced 1.1 passes the
	// same gate byte-identically. Disable Go's h2 wiring AND offer ALPN
	// http/1.1 explicitly: an empty offer let the server speak h2 anyway
	// (h2 SETTINGS frames parsed as malformed http/1.1). Every other
	// upstream accepts http/1.1 (no multiplexing loss — the pool already
	// runs per-request leases with max_egress_inflight).
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	tr.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	// Debug lever for wire-capture drills (fake pool -> mitm proxy with a
	// self-signed cert): accept any upstream cert so the exact request bytes
	// can be intercepted. Never set in production.
	if os.Getenv("AI_ROUTER_TLS_DEBUG") == "1" {
		tr.TLSClientConfig.InsecureSkipVerify = true
	}
	cl := &http.Client{Transport: tr}
	if len(p.pxClients) >= 64 {
		for k := range p.pxClients {
			delete(p.pxClients, k)
			break
		}
	}
	p.pxClients[proxy] = cl
	return cl
}
