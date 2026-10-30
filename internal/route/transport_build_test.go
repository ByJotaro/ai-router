package route

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/proxypool"
)

// Both server entrypoints (cmd/ai-router, cmd/omnigate-gui) must resolve the
// same transport from the same config; a missed call here silently degrades
// a configured proxy/pool requirement to a direct-connect fail-open.
func TestBuildTransportOptionSelectsHubEgressWhenEnabled(t *testing.T) {
	cfg := &config.Config{
		Egress: config.EgressConfig{Enabled: true, BaseURL: "http://127.0.0.1:8801"},
		Providers: map[string]config.ProviderConf{
			"a": {Transport: config.TransportConf{Mode: "warp"}},
		},
	}
	pc := proxypool.New(proxypool.Config{BaseURL: cfg.Egress.BaseURL})
	opt := BuildTransportOption(cfg, pc)
	if opt == nil {
		t.Fatal("expected a hub-egress transport option, got nil")
	}
	o := Options{}
	opt(&o)
	ht, ok := o.Transport.(HubTransport)
	if !ok {
		t.Fatalf("Transport = %T, want HubTransport", o.Transport)
	}
	if ht.Pool != pc {
		t.Fatal("HubTransport must reuse the caller's proxypool.Client instance")
	}
}

// Контракт (не конкретный тип): pool-режим провайдера даёт работающий
// pool-транспорт — fail-closed по умолчанию и со своим APIURL. Тип мог быть
// *PoolTransport, стал *poolGroup (per-provider изоляция 2026-10-12) — тест
// проверяет поведение, а не реализацию.
func TestBuildTransportOptionSelectsPoolTransport(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConf{
			"a": {Transport: config.TransportConf{Mode: "pool", APIURL: "http://127.0.0.1:9999"}},
		},
	}
	opt := BuildTransportOption(cfg, nil)
	if opt == nil {
		t.Fatal("expected a pool transport option, got nil")
	}
	o := Options{}
	opt(&o)
	if o.Transport == nil {
		t.Fatal("transport is nil")
	}
	if o.Transport.Name() != "pool" {
		t.Fatalf("Transport.Name() = %q, want pool", o.Transport.Name())
	}
	var g *poolGroup
	switch v := o.Transport.(type) {
	case *poolGroup:
		g = v
	case *PoolTransport:
		if v.DirectFallback {
			t.Fatal("pool transport must remain fail-closed (DirectFallback=false)")
		}
		if v.APIURL != "http://127.0.0.1:9999" {
			t.Fatalf("APIURL = %q", v.APIURL)
		}
		return
	default:
		t.Fatalf("Transport = %T, want pool transport", o.Transport)
	}
	pt, ok := g.byProv["a"].(*PoolTransport)
	if !ok || pt == nil {
		t.Fatal("provider a has no pool transport")
	}
	if pt.DirectFallback {
		t.Fatal("pool transport must remain fail-closed (DirectFallback=false)")
	}
	if pt.APIURL != "http://127.0.0.1:9999" {
		t.Fatalf("APIURL = %q", pt.APIURL)
	}
}

func TestBuildTransportOptionNilWhenNoTransportConfigured(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConf{
			"a": {},
		},
	}
	if opt := BuildTransportOption(cfg, nil); opt != nil {
		t.Fatal("expected nil option when no transport is configured")
	}
}

func TestBuildTransportOptionSelectsIPRotateTransport(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConf{
			"opencode": {
				BaseURL: "https://opencode.ai/zen/v1",
				Transport: config.TransportConf{
					Mode:       "iprotate",
					Fanout:     5,
					Rounds:     2,
					CooldownS:  30,
					ResolveTTLS: 60,
					SeedIPs:    []string{"172.65.90.20"},
				},
			},
		},
	}
	opt := BuildTransportOption(cfg, nil)
	if opt == nil {
		t.Fatal("expected an iprotate transport option, got nil")
	}
	o := Options{}
	opt(&o)
	it, ok := o.Transport.(*IPRotateTransport)
	if !ok {
		t.Fatalf("Transport = %T, want *IPRotateTransport", o.Transport)
	}
	if it.Host != "opencode.ai" || it.Port != "443" {
		t.Fatalf("Host=%q Port=%q, want opencode.ai / 443", it.Host, it.Port)
	}
	if it.Fanout != 5 || it.Rounds != 2 {
		t.Fatalf("Fanout=%d Rounds=%d, want 5 / 2", it.Fanout, it.Rounds)
	}
}

// TestPoolTransportPerProviderIsolation — регрессия 2026-10-12 (кило 24% 503
// при opencode 0.8% в те же минуты): каждый pool-провайдер обязан получить
// СВОЙ PoolTransport (свой ban/heat/storm state и свои fanout/rounds), иначе
// шторм одного провайдера парковал egress всем остальным, а fanout/rounds
// сидились от случайного «первого в map-order».
func TestPoolTransportPerProviderIsolation(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir()) // не трогать живой pool-state на этой машине
	cfg := &config.Config{
		Server: config.ServerConfig{Port: 9001},
		Providers: map[string]config.ProviderConf{
			"kilocode": {
				Transport: config.TransportConf{Mode: "pool", Fanout: 3, Rounds: 1, DirectFallback: false, APIURL: "http://127.0.0.1:8787"},
				APIKeys:   []string{},
			},
			"opencode": {
				Transport: config.TransportConf{Mode: "pool", Fanout: 7, Rounds: 4, DirectFallback: true, Token: "tok-opencode"},
				APIKeys:   []string{"sk-opencode"},
			},
			"opencode-zen": {
				Transport: config.TransportConf{Mode: "pool", Fanout: 5, Rounds: 2, DirectFallback: true},
				APIKeys:   []string{"sk-zen"},
			},
		},
	}
	tp := poolTransport(cfg)
	if tp == nil {
		t.Fatal("poolTransport returned nil")
	}
	g, ok := tp.(*poolGroup)
	if !ok {
		t.Fatalf("transport = %T, want *poolGroup", tp)
	}
	if len(g.byProv) != 3 {
		t.Fatalf("byProv = %d, want 3 (по транспорту на каждого pool-провайдера)", len(g.byProv))
	}
	kiloTP, ok1 := g.byProv["kilocode"].(*PoolTransport)
	ocTP, ok2 := g.byProv["opencode"].(*PoolTransport)
	if !ok1 || !ok2 || kiloTP == nil || ocTP == nil {
		t.Fatal("kilocode/opencode transport missing")
	}
	kilo, oc := kiloTP, ocTP
	if kilo == oc {
		t.Fatal("кило и opencode делят один PoolTransport — баны/heat шторма одного паркуют egress другому")
	}
	if kilo.Fanout != 3 || kilo.Rounds != 1 || oc.Fanout != 7 || oc.Rounds != 4 {
		t.Fatalf("fanout/rounds не per-provider: kilo=%d/%d opencode=%d/%d", kilo.Fanout, kilo.Rounds, oc.Fanout, oc.Rounds)
	}
	if !oc.DirectFallback || kilo.DirectFallback {
		t.Fatalf("direct_fallback перепутан: opencode=%v kilo=%v", oc.DirectFallback, kilo.DirectFallback)
	}
	if kilo.Token != "" || oc.Token != "tok-opencode" {
		t.Fatalf("token не per-provider: kilo=%q opencode=%q", kilo.Token, oc.Token)
	}
	if kilo.StateFile == oc.StateFile {
		t.Fatalf("state-файл общий (%s) — бан-состояние обязано быть раздельным", kilo.StateFile)
	}
	// appliesTo: транспорт отвечает только за своего провайдера.
	if !kilo.appliesTo("kilocode") || kilo.appliesTo("opencode") {
		t.Fatal("appliesTo сломан после разделения")
	}
	// Acquire диспетчерится по имени провайдера.
	if _, err := g.Acquire(context.Background(), RequestContext{Provider: "kilocode"}, &provider.Provider{Name: "kilocode"}); err == nil {
		// живой pool API не поднят — ошибка ожидаема, но не «no transport».
		t.Log("acquire err (оживаемо без pool API):", err)
	}
	if _, err := g.Acquire(context.Background(), RequestContext{Provider: "no-such"}, &provider.Provider{Name: "no-such"}); err == nil {
		t.Fatal("Acquire на неизвестного провайдера должен падать")
	}
	// SelfRotating/FlushState — контракты диспетчера.
	if !g.SelfRotating() {
		t.Fatal("poolGroup обязан быть SelfRotating")
	}
	g.FlushState()
}

// storm_collapse: false убирает провайдера из storm-collapse (kilo keyless:
// баны 5s, коллапс 1x1 давал преждеверные 503). Absent/nil = коллапс как был.
func TestBuildTransportStormCollapseOptOut(t *testing.T) {
	off := false
	cfg := &config.Config{
		Providers: map[string]config.ProviderConf{
			"kilo": {Transport: config.TransportConf{Mode: "pool", APIURL: "http://127.0.0.1:9999", StormCollapse: &off}},
			"other": {Transport: config.TransportConf{Mode: "pool", APIURL: "http://127.0.0.1:9999"}},
		},
	}
	opt := BuildTransportOption(cfg, nil)
	if opt == nil {
		t.Fatal("expected a pool transport option, got nil")
	}
	o := Options{}
	opt(&o)
	g, ok := o.Transport.(*poolGroup)
	if !ok {
		t.Fatalf("Transport = %T, want *poolGroup", o.Transport)
	}
	kiloPT, ok := g.byProv["kilo"].(*PoolTransport)
	if !ok {
		t.Fatalf("kilo transport = %T, want *PoolTransport", g.byProv["kilo"])
	}
	if !kiloPT.NoStormCollapse {
		t.Error("storm_collapse=false must set NoStormCollapse")
	}
	if other := g.byProv["other"]; other != nil {
		if otherPT, ok := other.(*PoolTransport); ok && otherPT.NoStormCollapse {
			t.Error("absent storm_collapse must keep the collapse (default)")
		}
	}
}

func TestPoolGroupRegistersDirectMode(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConf{
		"pooly": {BaseURL: "https://x.example", Transport: config.TransportConf{Mode: "pool"}},
		"local": {BaseURL: "http://127.0.0.1:9009/v1", Transport: config.TransportConf{Mode: "direct"}},
		"none":  {BaseURL: "https://y.example"},
	}}
	g := poolTransport(cfg)
	if g == nil {
		t.Fatal("pool group must exist (pooly declares pool)")
	}
	grp := g.(*poolGroup)
	if _, ok := grp.byProv["local"]; !ok {
		t.Fatal("direct-mode provider must be registered in the group")
	}
	if _, ok := grp.byProv["none"]; ok {
		t.Fatal("transport-less provider must NOT be registered (fails closed)")
	}
	l, err := grp.Acquire(context.Background(), RequestContext{Provider: "local"}, nil)
	if err != nil || l == nil || l.Client == nil {
		t.Fatalf("direct acquire must return a plain client lease, got %v %v", l, err)
	}
	if l.Proxy != "" {
		t.Fatalf("direct lease must carry no proxy, got %q", l.Proxy)
	}
	if l.Client.Timeout <= 0 {
		t.Fatal("direct lease без таймаута: зависший локальный мост паркует слот запроса навсегда")
	}
}

// TestPoolStateFilePortScoped — регрессия аудита 2026-10-13: имя state-файла
// пула несёт порт инстанса. Два инстанса с общим LOCALAPPDATA на разных портах
// (LIVE :9001 и стенд :9003) перезаписывали 8h-баны друг друга last-wins, так
// что бенч-прогон стирал LIVE уже запаркованные /48 и LIVE рештурмовал их
// с нуля. Пишется только pool_state_<name>__<port>.json; легаси
// pool_state_<name>.json читается один раз как fallback, чтобы промоут не
// забыл баны, набранные до появления дискриминатора.
func TestPoolStateFilePortScoped(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if err := os.MkdirAll(config.BaseDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	provs := map[string]config.ProviderConf{
		"kilocode": {Transport: config.TransportConf{Mode: "pool", APIURL: "http://127.0.0.1:8787"}},
	}
	build := func(port int) *poolGroup {
		t.Helper()
		tp := poolTransport(&config.Config{Server: config.ServerConfig{Port: port}, Providers: provs})
		if tp == nil {
			t.Fatal("poolTransport returned nil")
		}
		return tp.(*poolGroup)
	}
	poolOf := func(g *poolGroup, name string) *PoolTransport {
		t.Helper()
		pt, ok := g.byProv[name].(*PoolTransport)
		if !ok {
			t.Fatalf("%s transport = %T, want *PoolTransport", name, g.byProv[name])
		}
		return pt
	}

	// Легаси-состояние: бан, записанный до появления порта в имени файла.
	now := time.Now()
	legacy := config.PoolStatePathFor(0, "kilocode")
	seed := &PoolTransport{StateFile: legacy}
	seed.markBad429("2a09:bac5:48f3:", time.Hour, now)
	seed.FlushState()

	g9003 := build(9003)
	g9001 := build(9001)
	pt9003, pt9001 := poolOf(g9003, "kilocode"), poolOf(g9001, "kilocode")

	if want := config.PoolStatePathFor(9003, "kilocode"); pt9003.StateFile != want {
		t.Fatalf("state-файл :9003 = %q, want %q", pt9003.StateFile, want)
	}
	if want := config.PoolStatePathFor(9001, "kilocode"); pt9001.StateFile != want {
		t.Fatalf("state-файл :9001 = %q, want %q", pt9001.StateFile, want)
	}
	if pt9003.StateFile == pt9001.StateFile {
		t.Fatal("LIVE и стенд делят один state-файл — баны перезаписываются last-wins")
	}
	// Первая загрузка: легаси прочитан, но state-файл — уже новый.
	if !pt9003.isBad("2a09:bac5:48f3:", now.Add(time.Minute)) {
		t.Fatal("fallback на легаси pool_state_<name>.json не восстановил бан")
	}
	if !pt9001.isBad("2a09:bac5:48f3:", now.Add(time.Minute)) {
		t.Fatal("fallback на легаси не работает для :9001")
	}

	// Баны, набранные позже, живут строго в своём порту.
	pt9003.markBad429("2a09:bac5:aaaa:", time.Hour, now)
	pt9003.FlushState()
	pt9001.markBad429("2a09:bac5:bbbb:", time.Hour, now)
	pt9001.FlushState()
	if pt9001.isBad("2a09:bac5:aaaa:", now) {
		t.Fatal("бан стенда :9003 протёк в LIVE :9001")
	}
	if pt9003.isBad("2a09:bac5:bbbb:", now) {
		t.Fatal("бан LIVE :9001 протёк в стенд :9003")
	}

	// Рестарт: порт берёт баны из своего файла, а не из легаси.
	fresh9003 := poolOf(build(9003), "kilocode")
	if !fresh9003.isBad("2a09:bac5:aaaa:", now) {
		t.Fatal("бан стенда :9003 не выжил рестарт")
	}
	if fresh9003.isBad("2a09:bac5:bbbb:", now) {
		t.Fatal("после рестарта стенд подтянул бан LIVE из легаси")
	}
	// Легаси не переписан и не удалён.
	if !stateFileExists(legacy) {
		t.Fatal("легаси pool_state_<name>.json переписан/удалён — fallback ломается в тишине")
	}
}

// TestDirectProviderTransportSharesOneTimedClient — direct-транспорт отдаёт
// один построенный http.Client с таймаутом: per-Acquire &http.Client{} прятал
// пул соединений (и его сокеты) под каждый запрос и не имел Timeout вообще.
func TestDirectProviderTransportSharesOneTimedClient(t *testing.T) {
	tr := newDirectProviderTransport(0)
	if tr.client == nil {
		t.Fatal("direct transport без клиента")
	}
	if tr.client.Timeout != time.Duration(directTimeoutS)*time.Second {
		t.Fatalf("default timeout = %v, want %ds", tr.client.Timeout, directTimeoutS)
	}
	l1, err := tr.Acquire(context.Background(), RequestContext{Provider: "local"}, nil)
	if err != nil || l1 == nil || l1.Client == nil {
		t.Fatalf("acquire: %v %v", l1, err)
	}
	l2, err := tr.Acquire(context.Background(), RequestContext{Provider: "local"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l1.Client != l2.Client {
		t.Fatal("каждый Acquire строит новый *http.Client — пул соединений течёт по запросу")
	}
	if l1.Proxy != "" {
		t.Fatalf("direct lease must carry no proxy, got %q", l1.Proxy)
	}

	// timeout_s провайдера уважается (как в newClient).
	if got := newDirectProviderTransport(42).client.Timeout; got != 42*time.Second {
		t.Fatalf("timeout_s=42 дал %v", got)
	}

	// Провязка: режим direct в группе получает тот же общий таймаутованный клиент.
	t.Setenv("LOCALAPPDATA", t.TempDir())
	cfg := &config.Config{Providers: map[string]config.ProviderConf{
		"pooly": {Transport: config.TransportConf{Mode: "pool"}},
		"local": {BaseURL: "http://127.0.0.1:9009/v1", Transport: config.TransportConf{Mode: "direct"}, TimeoutS: 300},
	}}
	grp := poolTransport(cfg).(*poolGroup)
	la, err := grp.Acquire(context.Background(), RequestContext{Provider: "local"}, nil)
	if err != nil || la == nil || la.Client == nil {
		t.Fatalf("group acquire: %v %v", la, err)
	}
	lb, err := grp.Acquire(context.Background(), RequestContext{Provider: "local"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if la.Client != lb.Client {
		t.Fatal("direct-лизы в группе не делят один клиент")
	}
	if la.Client.Timeout != 300*time.Second {
		t.Fatalf("timeout_s=300 дал %v", la.Client.Timeout)
	}
}
