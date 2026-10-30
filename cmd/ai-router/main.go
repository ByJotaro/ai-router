// Command ai-router is an OpenAI-compatible LLM gateway and hub module:
// POST /v1/chat/completions (streaming + tools), GET /v1/models, virtual
// keys with quotas, provider key pools and fallback routing.
package main

import (
	"maps"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/ByJotaro/ai-router/internal/breaker"
	"github.com/ByJotaro/ai-router/internal/config"
	"github.com/ByJotaro/ai-router/internal/crypto"
	"github.com/ByJotaro/ai-router/internal/gateway"
	"github.com/ByJotaro/ai-router/internal/gui"
	"github.com/ByJotaro/ai-router/internal/hubmod"
	"github.com/ByJotaro/ai-router/internal/keys"
	"github.com/ByJotaro/ai-router/internal/lockout"
	"github.com/ByJotaro/ai-router/internal/logring"
	"github.com/ByJotaro/ai-router/internal/pool"
	"github.com/ByJotaro/ai-router/internal/provider"
	"github.com/ByJotaro/ai-router/internal/proxypool"
	"github.com/ByJotaro/ai-router/internal/route"
	"github.com/ByJotaro/ai-router/internal/store"
)

// Version is stamped at build time via -ldflags.
var Version = "dev"

func maskTokenShort(t string) string {
	if t == "" {
		return "(empty)"
	}
	if len(t) <= 8 {
		return strings.Repeat("*", len(t))
	}
	if len(t) <= 12 {
		return t[:4] + "..." + t[len(t)-4:]
	}
	return t[:6] + "***" + t[len(t)-4:]
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: ai-router [--config path] [command] [flags]")
	fmt.Fprintln(os.Stderr, "commands: serve, status, providers, keys, models, usage, routes, config, doctor, test")
	fmt.Fprintln(os.Stderr, "  serve      run gateway (default)")
	fmt.Fprintln(os.Stderr, "  status     show status")
	fmt.Fprintln(os.Stderr, "  providers  list providers")
	fmt.Fprintln(os.Stderr, "  keys       list virtual keys")
	fmt.Fprintln(os.Stderr, "  models     list models")
	fmt.Fprintln(os.Stderr, "  usage      show usage per key")
	fmt.Fprintln(os.Stderr, "  routes     show breaker/lockout/routing state")
	fmt.Fprintln(os.Stderr, "  config     show config (masked)")
	fmt.Fprintln(os.Stderr, "  doctor     run diagnostics")
	fmt.Fprintln(os.Stderr, "  test       probe a model")
	fmt.Fprintln(os.Stderr, "flags:")
	fmt.Fprintln(os.Stderr, "  -c, -config, --config path   config file path")
	fmt.Fprintln(os.Stderr, "  --json                        json output (models/usage/routes/config/doctor/status)")
	fmt.Fprintln(os.Stderr, "  --version                     show version")
	fmt.Fprintln(os.Stderr, "  --help, -h                    show help")
	fmt.Fprintln(os.Stderr, "serve flags:")
	fmt.Fprintln(os.Stderr, "  --cli                         headless mode: no tray/window, logs to stdout")
	fmt.Fprintln(os.Stderr, "  --egress                      force enable the egress proxy pool (server only)")
	fmt.Fprintln(os.Stderr, "  --no-autostart                stand: never touch HKCU Run autostart")
	fmt.Fprintln(os.Stderr, "  --minimized                   start the panel window minimised (used by autostart)")
}

func main() {
	args := os.Args[1:]

	// Global pre-scan: -config/--config/-c/--help/--version before cmd.
	cfgPath := config.ConfigPath()
	// scan for global flags that appear before the first non-flag (command)
	// This makes `ai-router -config path status` work same as `ai-router status -config path`.
	filtered := make([]string, 0, len(args))
	cmd := ""
	cmdArgs := []string{}
	foundCmd := false
	i := 0
	for i < len(args) {
		a := args[i]
		if !foundCmd && (a == "-c" || a == "-config" || a == "--config") {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "flag needs an argument: %s\n", a)
				os.Exit(2)
			}
			cfgPath = args[i+1]
			i += 2
			continue
		}
		if !foundCmd && strings.HasPrefix(a, "-c=") {
			cfgPath = strings.TrimPrefix(a, "-c=")
			i++
			continue
		}
		if !foundCmd && strings.HasPrefix(a, "--config=") {
			cfgPath = strings.TrimPrefix(a, "--config=")
			i++
			continue
		}
		if !foundCmd && strings.HasPrefix(a, "-config=") {
			cfgPath = strings.TrimPrefix(a, "-config=")
			i++
			continue
		}
		if !foundCmd && (a == "--help" || a == "-h" || a == "-help" || a == "help") {
			printUsage()
			os.Exit(0)
		}
		if !foundCmd && (a == "--version" || a == "-version" || a == "-v" || a == "--Version") {
			fmt.Printf("ai-router %s\n", Version)
			os.Exit(0)
		}
		if !foundCmd && strings.HasPrefix(a, "-") {
			// unknown flag before cmd — could be --json, --cli etc that belong to command.
			// Stop pre-scan and let command flagset handle it. Keep it in filtered for later.
			// But we haven't identified cmd yet, so we treat this as still before cmd.
			// To avoid mis-identifying, break and treat remaining as after-cmd args for default serve.
			// For `ai-router --json models`, the --json is for models command, but it appears before cmd.
			// We should allow --json before cmd as well: keep it.
			filtered = append(filtered, a)
			i++
			continue
		}
		if !foundCmd && !strings.HasPrefix(a, "-") {
			cmd = a
			foundCmd = true
			// remaining after cmd
			cmdArgs = args[i+1:]
			break
		}
		i++
	}
	if !foundCmd {
		// no command found; default to serve, remaining is whatever we collected (flags like --cli, --egress, etc)
		// filtered contains any --json etc that appeared before (should be treated as serve flags)
		cmd = "serve"
		// if filtered is non-empty, those are flags for serve; prepend them to cmdArgs
		// Actually if we never found cmd, we have been collecting filtered? We didn't set cmdArgs.
		// Let's re-derive: args that were not consumed as global config are in filtered + remaining tail
		cmdArgs = filtered
		// plus any args not yet processed (i already at len)
		if i < len(args) {
			cmdArgs = append(cmdArgs, args[i:]...)
		}
		// also need to include the case where we broke early due to unknown flag — filtered already has them
	} else {
		// we found cmd; but we may have collected some global-like flags in filtered that were before cmd and are not config.
		// Those should be prepended to cmdArgs (e.g., `ai-router --json models` => --json before models)
		if len(filtered) > 0 {
			cmdArgs = append(filtered, cmdArgs...)
		}
	}

	// Now handle per-command flag parsing, with global -config also allowed after cmd.
	// We create a FlagSet per command that includes -config/-c and -json where applicable.

	switch cmd {
	case "serve":
		fs := flag.NewFlagSet("ai-router serve", flag.ExitOnError)
		// define config flags (override pre-scanned cfgPath if provided after cmd)
		fs.StringVar(&cfgPath, "config", cfgPath, "config file path")
		fs.StringVar(&cfgPath, "c", cfgPath, "config file path (short)")
		// need to handle --config as alias: flag package treats --config same as -config, so one definition covers both
		egressForce := fs.Bool("egress", false, "force enable the egress proxy pool (server only)")
		cliMode := false
		fs.BoolVar(&cliMode, "cli", false, "headless mode: no tray/window, logs to stdout")
		noAuto := fs.Bool("no-autostart", false, "stand: never touch HKCU Run (isolated test stands)")
		minimizedFlag := fs.Bool("minimized", false, "start the panel window minimised (autostart)")
		// help/version for serve as well
		helpFlag := fs.Bool("help", false, "show help")
		fs.Bool("h", false, "show help (alias)")
		versionFlag := fs.Bool("version", false, "show version")
		if err := fs.Parse(cmdArgs); err != nil {
			os.Exit(2)
		}
		if *helpFlag || fs.Lookup("h") != nil && fs.Lookup("h").Value.String() == "true" {
			printUsage()
			os.Exit(0)
		}
		if *versionFlag {
			fmt.Printf("ai-router %s\n", Version)
			os.Exit(0)
		}
		runDaemon(cfgPath, *egressForce, cliMode, *noAuto, *minimizedFlag)
	case "status":
		fs := flag.NewFlagSet("ai-router status", flag.ExitOnError)
		fs.StringVar(&cfgPath, "config", cfgPath, "config file path")
		fs.StringVar(&cfgPath, "c", cfgPath, "config file path (short)")
		jsonOut := fs.Bool("json", false, "json output")
		helpFlag := fs.Bool("help", false, "show help")
		fs.Bool("h", false, "show help")
		if err := fs.Parse(cmdArgs); err != nil {
			os.Exit(2)
		}
		if *helpFlag || fs.Lookup("h").Value.String() == "true" {
			fmt.Fprintln(os.Stderr, "usage: ai-router status [-config path] [--json]")
			os.Exit(0)
		}
		cmdStatus(cfgPath, *jsonOut)
	case "providers":
		fs := flag.NewFlagSet("ai-router providers", flag.ExitOnError)
		fs.StringVar(&cfgPath, "config", cfgPath, "config file path")
		fs.StringVar(&cfgPath, "c", cfgPath, "config file path (short)")
		jsonOut := fs.Bool("json", false, "json output")
		helpFlag := fs.Bool("help", false, "show help")
		fs.Bool("h", false, "show help")
		if err := fs.Parse(cmdArgs); err != nil {
			os.Exit(2)
		}
		if *helpFlag || fs.Lookup("h").Value.String() == "true" {
			fmt.Fprintln(os.Stderr, "usage: ai-router providers [-config path] [--json]")
			os.Exit(0)
		}
		cmdProviders(cfgPath, *jsonOut)
	case "keys":
		fs := flag.NewFlagSet("ai-router keys", flag.ExitOnError)
		fs.StringVar(&cfgPath, "config", cfgPath, "config file path")
		fs.StringVar(&cfgPath, "c", cfgPath, "config file path (short)")
		jsonOut := fs.Bool("json", false, "json output")
		helpFlag := fs.Bool("help", false, "show help")
		fs.Bool("h", false, "show help")
		if err := fs.Parse(cmdArgs); err != nil {
			os.Exit(2)
		}
		if *helpFlag || fs.Lookup("h").Value.String() == "true" {
			fmt.Fprintln(os.Stderr, "usage: ai-router keys [-config path] [--json]")
			os.Exit(0)
		}
		cmdKeys(cfgPath, *jsonOut)
	case "models":
		fs := flag.NewFlagSet("ai-router models", flag.ExitOnError)
		fs.StringVar(&cfgPath, "config", cfgPath, "config file path")
		fs.StringVar(&cfgPath, "c", cfgPath, "config file path (short)")
		jsonOut := fs.Bool("json", false, "json output")
		helpFlag := fs.Bool("help", false, "show help")
		fs.Bool("h", false, "show help")
		if err := fs.Parse(cmdArgs); err != nil {
			os.Exit(2)
		}
		if *helpFlag || fs.Lookup("h").Value.String() == "true" {
			fmt.Fprintln(os.Stderr, "usage: ai-router models [-config path] [--json]")
			os.Exit(0)
		}
		cmdModels(cfgPath, *jsonOut)
	case "usage":
		fs := flag.NewFlagSet("ai-router usage", flag.ExitOnError)
		fs.StringVar(&cfgPath, "config", cfgPath, "config file path")
		fs.StringVar(&cfgPath, "c", cfgPath, "config file path (short)")
		jsonOut := fs.Bool("json", false, "json output")
		helpFlag := fs.Bool("help", false, "show help")
		fs.Bool("h", false, "show help")
		if err := fs.Parse(cmdArgs); err != nil {
			os.Exit(2)
		}
		if *helpFlag || fs.Lookup("h").Value.String() == "true" {
			fmt.Fprintln(os.Stderr, "usage: ai-router usage [-config path] [--json]")
			os.Exit(0)
		}
		cmdUsage(cfgPath, *jsonOut)
	case "routes":
		fs := flag.NewFlagSet("ai-router routes", flag.ExitOnError)
		fs.StringVar(&cfgPath, "config", cfgPath, "config file path")
		fs.StringVar(&cfgPath, "c", cfgPath, "config file path (short)")
		jsonOut := fs.Bool("json", false, "json output")
		helpFlag := fs.Bool("help", false, "show help")
		fs.Bool("h", false, "show help")
		if err := fs.Parse(cmdArgs); err != nil {
			os.Exit(2)
		}
		if *helpFlag || fs.Lookup("h").Value.String() == "true" {
			fmt.Fprintln(os.Stderr, "usage: ai-router routes [-config path] [--json]")
			os.Exit(0)
		}
		cmdRoutes(cfgPath, *jsonOut)
	case "config":
		fs := flag.NewFlagSet("ai-router config", flag.ExitOnError)
		fs.StringVar(&cfgPath, "config", cfgPath, "config file path")
		fs.StringVar(&cfgPath, "c", cfgPath, "config file path (short)")
		jsonOut := fs.Bool("json", false, "json output")
		helpFlag := fs.Bool("help", false, "show help")
		fs.Bool("h", false, "show help")
		if err := fs.Parse(cmdArgs); err != nil {
			os.Exit(2)
		}
		if *helpFlag || fs.Lookup("h").Value.String() == "true" {
			fmt.Fprintln(os.Stderr, "usage: ai-router config [-config path] [--json]")
			os.Exit(0)
		}
		cmdConfigShow(cfgPath, *jsonOut)
	case "doctor":
		fs := flag.NewFlagSet("ai-router doctor", flag.ExitOnError)
		fs.StringVar(&cfgPath, "config", cfgPath, "config file path")
		fs.StringVar(&cfgPath, "c", cfgPath, "config file path (short)")
		jsonOut := fs.Bool("json", false, "json output")
		helpFlag := fs.Bool("help", false, "show help")
		fs.Bool("h", false, "show help")
		if err := fs.Parse(cmdArgs); err != nil {
			os.Exit(2)
		}
		if *helpFlag || fs.Lookup("h").Value.String() == "true" {
			fmt.Fprintln(os.Stderr, "usage: ai-router doctor [-config path] [--json]")
			os.Exit(0)
		}
		cmdDoctor(cfgPath, *jsonOut)
	case "test":
		fs := flag.NewFlagSet("ai-router test", flag.ExitOnError)
		fs.StringVar(&cfgPath, "config", cfgPath, "config file path")
		fs.StringVar(&cfgPath, "c", cfgPath, "config file path (short)")
		modelFlag := fs.String("model", "", "model to test (default: first configured)")
		if err := fs.Parse(cmdArgs); err != nil {
			os.Exit(2)
		}
		// also check for global help/version after cmd? test has its own
		cmdTest(cfgPath, *modelFlag)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		printUsage()
		os.Exit(2)
	}
}

func logger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// buildComponents resolves providers (skipping broken ones), loads keys and
// wires the router — shared by the daemon and the test command.
func buildComponents(cfg *config.Config, log *slog.Logger) (*route.Router, *keys.Store, *store.Store) {
	st, err := store.Open(config.DBPath())
	if err != nil {
		log.Warn("sqlite store unavailable, running without persistence", "err", err)
		st = nil
	}
	cipher := crypto.New()
	if st != nil && !cipher.Enabled() {
		log.Info("key encryption disabled (set " + cfg.Encryption.KeyEnv + " to enable)")
	}

	provs := map[string]*provider.Provider{}
	for name, pc := range cfg.Providers {
		if pc.KeySwap == "" {
			pc.KeySwap = cfg.Defaults.KeySwap
		}
		if len(pc.ErrorPolicy) == 0 {
			if cfg.Defaults.ErrorPolicy != nil {
				pc.ErrorPolicy = make(map[string]string, len(cfg.Defaults.ErrorPolicy))
				maps.Copy(pc.ErrorPolicy, cfg.Defaults.ErrorPolicy)
			}
		}
		p, err := provider.Resolve(name, pc)
		if err != nil {
			log.Warn("provider skipped", "provider", name, "err", err)
			continue
		}
		provs[name] = p
	}

	opts := []route.Option{
		route.WithProvidersOrder(cfg.ProvidersOrder),
		route.WithStrategy(cfg.Routing.Strategy),
		route.WithTiers(cfg.Routing.Tiers),
		route.WithBreaker(breaker.Config{
			MaxFailures:       cfg.Resilience.Breaker.MaxFailures,
			Recovery:          time.Duration(cfg.Resilience.Breaker.RecoverySeconds) * time.Second,
			HalfOpenSuccesses: cfg.Resilience.Breaker.HalfOpenSuccesses,
		}),
		route.WithCooldown(pool.Config{
			Cooldown:          time.Duration(cfg.Resilience.Cooldown.BaseSeconds) * time.Second,
			BackoffBase:       time.Duration(cfg.Resilience.Cooldown.BaseSeconds) * time.Second,
			BackoffMax:        time.Duration(cfg.Resilience.Cooldown.MaxSeconds) * time.Second,
			BackoffFactor:     cfg.Resilience.Cooldown.Factor,
			RespectRetryAfter: cfg.Resilience.Cooldown.RespectRetryAfter,
		}),
		route.WithLockout(lockout.Config{
			MaxFailures: cfg.Resilience.Lockout.MaxFailures,
			Window:      time.Duration(cfg.Resilience.Lockout.WindowSeconds) * time.Second,
			Lockout:     time.Duration(cfg.Resilience.Lockout.LockoutSeconds) * time.Second,
		}),
		route.WithEncrypt(cipher.Encrypt),
		route.WithDecrypt(cipher.Decrypt),
	}
	var hubPool *proxypool.Client
	if cfg.Egress.Enabled && cfg.Egress.BaseURL != "" {
		hubPool = proxypool.New(proxypool.Config{
			BaseURL:     cfg.Egress.BaseURL,
			Token:       cfg.Egress.Token,
			Strategy:    cfg.Egress.Strategy,
			PerRequest:  cfg.Egress.Rotation.PerRequest,
			RotateAfter: cfg.Egress.Rotation.RotateAfter,
		})
		if st != nil {
			hubPool.SetLog(func(proxy, action string, ok bool, ms int64, errText string) {
				_ = st.InsertProxiesLog(proxy, action, ok, ms, errText)
			})
		}
	}
	if tOpt := route.BuildTransportOption(cfg, hubPool); tOpt != nil {
		opts = append(opts, tOpt)
	}
	// Pool-путь :8787 пишет исходы в ту же ленту proxies_log, что hub-путь:
	// вкладка Прокси видит acquire/rotate каждого запроса, а не только кнопки.
	if st != nil {
		route.SetPoolEventSink(func(proxy, action string, ok bool, ms int64, errText string) {
			_ = st.InsertProxiesLog(proxy, action, ok, ms, errText)
		})
	}
	r := route.New(provs, opts...)
	if st != nil {
		r.ReloadBreakers(st.LoadBreaker)
		r.RestorePoolState(st.LoadPoolKeys)
	}

	ks := keys.New(nil, cipher)
	if st != nil {
		ks = keys.New(st.DB(), cipher)
	}
	if err := ks.Load(); err != nil {
		log.Warn("state load", "err", err)
	}
	if err := ks.SetConfig(cfg.Keys); err != nil {
		log.Warn("keys config", "err", err)
	}
	if st != nil {
		seedCatalog(st, provs)
		ks.SetPriceResolver(priceResolver(st))
	}
	return r, ks, st
}

// seedCatalog records configured model prices into models_catalog so cost
// accounting (budget_usd) has pricing data even before an online sync.
// Prices are stored as integer cents per 1M tokens; deleted stale models
// that are no longer present in config no longer keep a zero-cost ghost.
func seedCatalog(st *store.Store, provs map[string]*provider.Provider) {
	keep := map[string]struct{}{}
	for _, p := range provs {
		if p.CostPer1MIn <= 0 && p.CostPer1MOut <= 0 {
			continue
		}
		for _, m := range p.Models {
			_ = st.UpsertModel(m, p.Name, int64(p.CostPer1MIn*100), int64(p.CostPer1MOut*100))
			keep[m] = struct{}{}
		}
	}
	if len(keep) == 0 {
		return
	}
	_ = st.DeleteStaleModels(keep)
}

// savePoolKey adapts store.SavePoolKey to pool.KeyState.
func savePoolKey(st *store.Store) func(pool.KeyState) error {
	return func(ks pool.KeyState) error {
		return st.SavePoolKey(store.PoolKeyRow{
			Provider:         ks.Provider,
			KeyHash:          ks.KeyHash,
			ValueEnc:         ks.ValueEnc,
			Failures:         ks.Failures,
			Disabled:         ks.Disabled,
			DisabledUntil:    ks.DisabledUntil,
			BackoffLevel:     ks.BackoffLevel,
			RateLimitedUntil: ks.RateLimitedUntil,
			LastErr:          ks.LastErr,
		})
	}
}

// priceResolver builds a USD-per-1M-tokens resolver from the models catalog.
// Integer cents per 1M are read directly; legacy REAL columns are handled by
// the store migration (cents are populated from REAL*100).
func priceResolver(st *store.Store) keys.PriceFunc {
	return func(model string) (int64, int64) {
		if st == nil {
			return 0, 0
		}
		in, out := st.GetModelCost(model)
		return in, out
	}
}

func runDaemon(cfgPath string, egressForce bool, headless bool, noAuto bool, minimized bool) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fatal("config", err)
	}
	// Enforce the HKCU Run autostart switch from config: true registers
	// this binary at Windows login, false removes the entry — a real
	// settings toggle for self-start. Stands must NEVER touch it:
	// --no-autostart / AI_ROUTER_NO_AUTOSTART=1 skip enforcement
	// (2026-10-14: стенды угоняли Run-key своим exe, autostart:false его удалял).
	// NOTE: --cli does NOT skip: headless is the normal prod-server mode.
	if egressForce {
		cfg.Egress.Enabled = true
	}
	log := logger(cfg.Server.LogLevel)
	// Второй запуск (клик по exe при живом инстансе) не умирает молча на
	// bind: порт занят — значит, живой демон; поднимаем его панель и выходим
	// (2026-10-19: клик по exe «ничего не делает» — старый процесс держал :9001).
	if !headless {
		if probe, perr := net.Listen("tcp", cfg.Addr()); perr != nil {
			if gui.RaiseExistingWindow() {
				log.Info("another ai-router is live — raised its panel", "addr", cfg.Addr())
				os.Exit(0)
			}
			log.Error("gateway bind failed — is another instance running?", "addr", cfg.Addr(), "err", perr)
			gui.ShowAlreadyRunning(cfg.Addr())
			os.Exit(1)
		} else {
			_ = probe.Close()
		}
	}
	if noAuto || os.Getenv("AI_ROUTER_NO_AUTOSTART") != "" {
		log.Info("autostart", "skipped_stand", true)
	} else if err := enforceAutoStart(cfg.AutoStart); err != nil {
		log.Warn("autostart toggle failed", "err", err)
	} else {
		log.Info("autostart", "enabled", cfg.AutoStart)
	}
	router, ks, st := buildComponents(cfg, log)
	ring := logring.New(1000)

	gopts := []gateway.Option{
		gateway.WithTelemetry(cfg.Telemetry.Headers),
		gateway.WithSanitizeErrors(cfg.Sanitize.Errors),
	}
	if st != nil {
		gopts = append(gopts, gateway.WithStore(st))
	}
	if cfg.Idempotency.Enabled && st != nil {
		gopts = append(gopts, gateway.WithIdempotency(true, time.Duration(cfg.Idempotency.TTLSeconds)*time.Second, st.DB()))
	}
	g := gateway.New(router, ks, cfg.AdminToken, ring, gopts...)
	hm := hubmod.New(cfg, router, ks, ring, Version)
	if err := hm.Start(); err != nil {
		log.Warn("hub module start", "err", err)
	} else {
		hm.WireGateway(g)
		log.Info("hub module serving", "hub", cfg.Hub.URL, "module", cfg.Hub.Module, "addr", cfg.Hub.Addr)
	}

	ln, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		// Fail fast instead of parking a zombie process: the common cause
		// is another ai-router instance already bound to the port.
		log.Error("gateway bind failed — is another instance running?", "addr", cfg.Addr(), "err", err)
		os.Exit(1)
	}
	srv := &http.Server{Addr: cfg.Addr(), Handler: g.Handler()}
	go func() {
		log.Info("ai-router listening", "addr", cfg.Addr(), "version", Version)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("gateway server stopped", "err", err)
		}
	}()

	// Unified GUI (default, mirroring warp-router): attach the omnigate
	// tray+window to THIS process on the same live router/keys instances —
	// one binary, one state.db handle. --cli runs bare headless instead.
	//
	// GUI exit does NOT stop the gateway (review 2026-10-17, r19 F1): the
	// panel is a view, the router is the service its clients depend on. A
	// closed window, a dead WebView2 or a tray failure used to kill :9001
	// mid-traffic — only the external watchdog task kept it alive on jotaro.
	// Now the daemon keeps serving headless until a real signal arrives.
	if !headless {
		var hubPool *proxypool.Client
		if cfg.Egress.Enabled && cfg.Egress.BaseURL != "" {
			hubPool = proxypool.New(proxypool.Config{
				BaseURL:    cfg.Egress.BaseURL,
				Token:      cfg.Egress.Token,
				Strategy:   cfg.Egress.Strategy,
				PerRequest: cfg.Egress.Rotation.PerRequest,
				RotateAfter: cfg.Egress.Rotation.RotateAfter,
			})
		}
		log.Info("gui attached", "mode", "tray+window", "minimized", minimized)
		if err := gui.Launch(router, ks, cfg, cfgPath, Version, hubPool, true, minimized); err != nil {
			log.Warn("gui exited with error, serving headless", "err", err)
		} else {
			log.Info("gui closed, serving headless")
		}
		// Persist bans synchronously: the GUI goroutine is gone, so the
		// async state saves it used to trigger may never land.
		if fl, ok := router.Transport().(route.StateFlusher); ok {
			fl.FlushState()
		}
		log.Info("gateway still serving; close the process (or send SIGINT/SIGTERM) to stop",
			"addr", cfg.Addr(), "pid", os.Getpid(), "headless", true)
	}

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	log.Info("shutting down")
	// Flush persistable transport state synchronously: the pool path saves
	// async, so the last bans before SIGTERM would be lost on restart/promote.
	if fl, ok := router.Transport().(route.StateFlusher); ok {
		fl.FlushState()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// --- CLI subcommands ---

func cmdStatus(cfgPath string, jsonOut bool) {
	cfg, err := config.Read(cfgPath)
	if err != nil {
		fatal("config", err)
	}
	router, ks, _ := buildComponents(cfg, logger("warn"))
	if jsonOut {
		out := map[string]any{
			"version":      Version,
			"server":       cfg.Addr(),
			"admin_token":  maskTokenShort(cfg.AdminToken),
			"providers":    len(router.Providers()),
			"models":       len(router.Models()),
			"virtual_keys": len(ks.List()),
			"hub":          map[string]any{"url": cfg.Hub.URL, "module": cfg.Hub.Module, "register": cfg.Hub.Register},
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}
	fmt.Printf("ai-router %s\n", Version)
	fmt.Printf("  server:      %s\n", cfg.Addr())
	fmt.Printf("  admin token: %s\n", maskTokenShort(cfg.AdminToken))
	fmt.Printf("  providers:   %d\n", len(router.Providers()))
	fmt.Printf("  models:      %d\n", len(router.Models()))
	fmt.Printf("  virtual keys:%d\n", len(ks.List()))
	fmt.Printf("  hub:         %s module=%s register=%v\n", cfg.Hub.URL, cfg.Hub.Module, cfg.Hub.Register)
}

func cmdProviders(cfgPath string, jsonOut bool) {
	cfg, err := config.Read(cfgPath)
	if err != nil {
		fatal("config", err)
	}
	router := route.New(nil)
	var names []string
	for name, pc := range cfg.Providers {
		p, err := provider.Resolve(name, pc)
		if err != nil {
			fmt.Printf("  %-20s SKIPPED: %v\n", name, err)
			continue
		}
		router.Add(p)
		names = append(names, name)
	}
	sort.Strings(names)
	if jsonOut {
		out := []map[string]any{}
		for _, n := range names {
			p := cfg.Providers[n]
			out = append(out, map[string]any{
				"name":     n,
				"type":     p.Type,
				"keys":     len(p.APIKeys),
				"models":   p.Models,
				"base_url": p.BaseURL,
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"providers": out})
		return
	}
	for _, n := range names {
		p := cfg.Providers[n]
		fmt.Printf("  %-20s type=%s keys=%d models=%v base_url=%s\n", n, p.Type, len(p.APIKeys), p.Models, p.BaseURL)
		for i, k := range p.APIKeys {
			fmt.Printf("      key[%d] len=%d\n", i, len(k))
		}
	}
}

func cmdKeys(cfgPath string, jsonOut bool) {
	cfg, err := config.Read(cfgPath)
	if err != nil {
		fatal("config", err)
	}
	ks := keys.New(nil, nil)
	_ = ks.Load()
	_ = ks.SetConfig(cfg.Keys)
	views := ks.List()
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"keys": views})
		return
	}
	if len(views) == 0 {
		fmt.Println("  no virtual keys configured")
		return
	}
	for _, v := range views {
		rev := ""
		if v.Revoked {
			rev = " (revoked)"
		}
		fmt.Printf("  %-20s %s tokens=%d/%d reqs=%d/%d models=%v%s\n",
			v.Name, v.Key, v.UsageTokens, v.QuotaTokens, v.UsageReqs, v.QuotaReqs, v.Models, rev)
	}
}

func cmdModels(cfgPath string, jsonOut bool) {
	cfg, err := config.Read(cfgPath)
	if err != nil {
		fatal("config", err)
	}
	router, _, _ := buildComponents(cfg, logger("warn"))
	models := router.Models()
	sort.Strings(models)
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"models": models, "count": len(models)})
		return
	}
	if len(models) == 0 {
		fmt.Println("  no models configured")
		return
	}
	fmt.Printf("  models (%d):\n", len(models))
	for _, m := range models {
		fmt.Printf("    - %s\n", m)
	}
}

func cmdUsage(cfgPath string, jsonOut bool) {
	cfg, err := config.Read(cfgPath)
	if err != nil {
		fatal("config", err)
	}
	_, ks, _ := buildComponents(cfg, logger("warn"))
	views := ks.List()
	var totalTokens, totalReqs int64
	for _, v := range views {
		totalTokens += v.UsageTokens
		totalReqs += v.UsageReqs
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"keys":           views,
			"total_tokens":   totalTokens,
			"total_requests": totalReqs,
		})
		return
	}
	fmt.Printf("  usage: total_tokens=%d total_requests=%d keys=%d\n", totalTokens, totalReqs, len(views))
	for _, v := range views {
		fmt.Printf("    %-20s tokens=%d reqs=%d models=%v\n", v.Name, v.UsageTokens, v.UsageReqs, v.Models)
	}
}

func cmdRoutes(cfgPath string, jsonOut bool) {
	cfg, err := config.Read(cfgPath)
	if err != nil {
		fatal("config", err)
	}
	router, _, _ := buildComponents(cfg, logger("warn"))
	breakers := router.BreakerState()
	lockoutState := router.LockoutState()
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"breakers": breakers,
			"lockout":  lockoutState,
			"strategy": cfg.Routing.Strategy,
			"tiers":    cfg.Routing.Tiers,
		})
		return
	}
	fmt.Printf("  routing: strategy=%s tiers=%v\n", cfg.Routing.Strategy, cfg.Routing.Tiers)
	fmt.Printf("  breakers (%d):\n", len(breakers))
	for name, b := range breakers {
		fmt.Printf("    %-20s %+v\n", name, b)
	}
	fmt.Printf("  lockout (%d):\n", len(lockoutState))
	for m, st := range lockoutState {
		fmt.Printf("    %-30s %+v\n", m, st)
	}
}

func cmdConfigShow(cfgPath string, jsonOut bool) {
	cfg, err := config.Read(cfgPath)
	if err != nil {
		fatal("config", err)
	}
	// mask secrets
	masked := map[string]any{
		"server":      cfg.Server,
		"admin_token": maskTokenShort(cfg.AdminToken),
		"hub":         cfg.Hub,
		"providers":   cfg.Providers,
		"keys":        cfg.Keys,
		"resilience":  cfg.Resilience,
		"routing":     cfg.Routing,
		"egress":      cfg.Egress,
	}
	// mask provider api_keys
	maskedProviders := map[string]any{}
	for name, pc := range cfg.Providers {
		maskedKeys := make([]string, len(pc.APIKeys))
		for i, k := range pc.APIKeys {
			maskedKeys[i] = maskTokenShort(k)
		}
		maskedProviders[name] = map[string]any{
			"type":     pc.Type,
			"base_url": pc.BaseURL,
			"api_keys": maskedKeys,
			"models":   pc.Models,
		}
	}
	masked["providers"] = maskedProviders
	masked["egress"] = map[string]any{
		"enabled": cfg.Egress.Enabled,
		"base_url": cfg.Egress.BaseURL,
		"token":   maskTokenShort(cfg.Egress.Token),
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(masked)
		return
	}
	fmt.Printf("  config: %s\n", cfgPath)
	fmt.Printf("  server: %s log=%s\n", cfg.Addr(), cfg.Server.LogLevel)
	fmt.Printf("  admin_token: %s\n", maskTokenShort(cfg.AdminToken))
	fmt.Printf("  providers: %d\n", len(cfg.Providers))
	for name, pc := range cfg.Providers {
		fmt.Printf("    %-20s base_url=%s models=%v keys=%d\n", name, pc.BaseURL, pc.Models, len(pc.APIKeys))
	}
	fmt.Printf("  hub: %s module=%s\n", cfg.Hub.URL, cfg.Hub.Module)
}

func cmdDoctor(cfgPath string, jsonOut bool) {
	cfg, err := config.Read(cfgPath)
	report := map[string]any{"config_path": cfgPath}
	checks := []map[string]any{}
	add := func(name, status, detail string) {
		checks = append(checks, map[string]any{"name": name, "status": status, "detail": detail})
	}
	if err != nil {
		add("config", "fail", err.Error())
		report["checks"] = checks
		report["ok"] = false
		if jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(report)
			return
		}
		fmt.Printf("doctor: config read failed: %v\n", err)
		return
	}
	add("config", "ok", cfg.Addr())
	// admin token check with bounds guard
	if cfg.AdminToken == "" {
		add("admin_token", "warn", "empty")
	} else if len(cfg.AdminToken) < 16 {
		add("admin_token", "warn", "short")
	} else {
		add("admin_token", "ok", maskTokenShort(cfg.AdminToken))
	}
	// providers
	router, ks, st := buildComponents(cfg, logger("warn"))
	add("providers", "ok", fmt.Sprintf("%d", len(router.Providers())))
	add("models", "ok", fmt.Sprintf("%d", len(router.Models())))
	add("keys", "ok", fmt.Sprintf("%d", len(ks.List())))
	if st != nil {
		add("store", "ok", config.DBPath())
	} else {
		add("store", "warn", "no DB")
	}
	// check db file
	if _, err := os.Stat(config.DBPath()); err == nil {
		add("db_file", "ok", config.DBPath())
	} else {
		add("db_file", "warn", "missing, will be created")
	}
	ok := true
	for _, c := range checks {
		if c["status"] == "fail" {
			ok = false
			break
		}
	}
	report["checks"] = checks
	report["ok"] = ok
	report["version"] = Version
	report["providers"] = len(router.Providers())
	report["models"] = len(router.Models())
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return
	}
	fmt.Printf("doctor %s cfg=%s\n", Version, cfgPath)
	for _, c := range checks {
		fmt.Printf("  %-15s %-6s %s\n", c["name"], c["status"], c["detail"])
	}
	if ok {
		fmt.Println("  ok")
	} else {
		fmt.Println("  issues found")
	}
}

func cmdTest(cfgPath, model string) {
	cfg, err := config.Read(cfgPath)
	if err != nil {
		fatal("config", err)
	}
	log := logger("warn")
	router, _, _ := buildComponents(cfg, log)
	if model == "" {
		models := router.Models()
		if len(models) == 0 {
			fatal("test", fmt.Errorf("no models configured — add providers with models"))
		}
		model = models[0]
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"max_tokens": 16,
		"stream":     false,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	res, err := router.Call(ctx, model, body, false)
	if err != nil {
		fatal("test", err)
	}
	defer res.Resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Resp.Body, 1<<20))
	if res.Status >= 400 {
		fmt.Printf("  FAIL model=%s provider=%s status=%d %s\n", model, res.Provider, res.Status, truncate(string(data), 200))
		os.Exit(1)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage any `json:"usage"`
	}
	_ = json.Unmarshal(data, &out)
	content := ""
	if len(out.Choices) > 0 {
		content, _ = out.Choices[0].Message.Content.(string)
	}
	fmt.Printf("  OK model=%s provider=%s (%s) content=%q usage=%v\n", model, res.Provider, time.Since(start).Round(time.Millisecond), truncate(content, 120), out.Usage)
}

// enforceAutoStart mirrors cfg.AutoStart into the HKCU Run key so the
// gateway starts at Windows login. exe path = os.Executable (stable after
// the binary is copied out of Temp). The Run command carries --minimized:
// логин стартует в трей свёрнутым, ручной клик по exe показывает панель
// сразу (2026-10-19).
func enforceAutoStart(enabled bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.CREATE_SUB_KEY|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	const name = "ai-router"
	if !enabled {
		_ = k.DeleteValue(name)
		return nil
	}
	return k.SetStringValue(name, `"`+exe+`" --minimized`)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "ai-router: %s: %v\n", what, err)
	os.Exit(1)
}
