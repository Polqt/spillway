package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Polqt/spillway/apps/spillway/web"
	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/budget"
	"github.com/Polqt/spillway/internal/cache"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/gemini"
	"github.com/Polqt/spillway/internal/providers/mock"
	"github.com/Polqt/spillway/internal/providers/openaicompat"
	"github.com/Polqt/spillway/internal/quota"
	"github.com/Polqt/spillway/internal/redact"
	"github.com/Polqt/spillway/internal/router"
	"github.com/Polqt/spillway/internal/store"
)

// shutdownGrace is how long in-flight requests get to finish after the
// context is cancelled, before they are cancelled too.
const shutdownGrace = 10 * time.Second

// serve runs the API until ctx is cancelled. It refuses to listen when the
// config is invalid, including when it has no local key (ADR 0003).
func serve(ctx context.Context, args []string, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to the config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		report(stderr, "spillway: serve: unexpected argument %q\n\n%s", fs.Arg(0), usage)
		return 2
	}

	cfg, err := config.LoadWithEnv(*configPath, lookupEnv)
	if err != nil {
		report(stderr, "spillway: %v\n", err)
		return 1
	}
	scrubber := redact.NewScrubber(cfg.Secrets()...)
	logger := slog.New(redact.NewHandler(slog.NewTextHandler(stderr, nil), scrubber))
	var usageStore *store.Store
	if cfg.Store != nil {
		usageStore, err = store.Open(ctx, store.Options{Path: cfg.Store.Path, Retention: cfg.Store.Retention})
		if err != nil {
			report(stderr, "spillway: %v\n", err)
			return 1
		}
		defer func() {
			if err := usageStore.Close(); err != nil {
				logger.ErrorContext(ctx, "close usage store", "error", err)
			}
		}()
	}
	attested := make([]string, 0)
	for name, provider := range cfg.Providers {
		if provider.FreeTier {
			attested = append(attested, name)
		}
	}
	slices.Sort(attested)
	logger.InfoContext(ctx, "free-tier providers are user-attested", "providers", attested)
	handler, err := newHandler(ctx, cfg, logger, usageStore)
	if err != nil {
		report(stderr, "spillway: %v\n", err)
		return 1
	}

	if !cfg.Server.Loopback() {
		logger.WarnContext(ctx, "listening on a non-loopback address; the API is reachable from the network and the Host check is off, so use long random local keys",
			"addr", cfg.Server.Listen)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Server.Listen)
	if err != nil {
		report(stderr, "spillway: listen on %s: %v\n", cfg.Server.Listen, err)
		return 1
	}
	if err := serveUntilDone(ctx, ln, handler, logger); err != nil {
		report(stderr, "spillway: %v\n", err)
		return 1
	}
	return 0
}

// newHandler wires the configured providers and local keys into the API.
func newHandler(ctx context.Context, cfg config.Config, logger *slog.Logger, usageStore *store.Store) (http.Handler, error) {
	var ps []providers.Provider
	for name := range cfg.Providers {
		switch name {
		case "mock":
			p, err := mock.New(mock.Options{Now: time.Now})
			if err != nil {
				return nil, fmt.Errorf("build provider mock: %w", err)
			}
			ps = append(ps, p)
		case "gemini":
			baseURL := cfg.Providers[name].BaseURL
			p, err := gemini.New(gemini.Options{
				APIKey: cfg.Providers[name].APIKey, BaseURL: baseURL, Timeout: cfg.Providers[name].Timeout,
			})
			if err != nil {
				return nil, fmt.Errorf("build provider gemini: %w", err)
			}
			ps = append(ps, p)
		case "groq", "openrouter", "ollama":
			baseURL := cfg.Providers[name].BaseURL
			if baseURL == "" {
				switch name {
				case "groq":
					baseURL = "https://api.groq.com/openai/v1"
				case "openrouter":
					baseURL = "https://openrouter.ai/api/v1"
				case "ollama":
					baseURL = "http://localhost:11434/v1"
				}
			} else if name == "ollama" && !strings.HasSuffix(strings.TrimRight(baseURL, "/"), "/v1") {
				baseURL = strings.TrimRight(baseURL, "/") + "/v1"
			}
			p, err := openaicompat.New(openaicompat.Options{
				Name: name, BaseURL: baseURL, APIKey: cfg.Providers[name].APIKey, Timeout: cfg.Providers[name].Timeout,
			})
			if err != nil {
				return nil, fmt.Errorf("build provider %s: %w", name, err)
			}
			ps = append(ps, p)
		default:
			return nil, fmt.Errorf("provider %q has no adapter", name)
		}
	}
	set, err := providers.NewSet(ps...)
	if err != nil {
		return nil, fmt.Errorf("register providers: %w", err)
	}

	keys := make([]api.LocalKey, len(cfg.Server.APIKeys))
	for i, k := range cfg.Server.APIKeys {
		keys[i] = api.LocalKey{Name: k.Name, Key: k.Key, Dashboard: k.Dashboard}
	}
	tiers := make(map[string][]providers.ModelRef, len(cfg.Tiers))
	for name, tier := range cfg.Tiers {
		candidates := make([]providers.ModelRef, len(tier.Models))
		for i, model := range tier.Models {
			ref, err := providers.ParseModelRef(model)
			if err != nil {
				return nil, fmt.Errorf("tier %s model %d: %w", name, i, err)
			}
			candidates[i] = ref
		}
		tiers[name] = candidates
	}
	declarations := make(map[providers.ModelRef]router.Declaration, len(cfg.Models))
	for model, declaration := range cfg.Models {
		ref, err := providers.ParseModelRef(model)
		if err != nil {
			return nil, fmt.Errorf("model declaration %s: %w", model, err)
		}
		capabilities := make([]router.Capability, len(declaration.Capabilities))
		for i, capability := range declaration.Capabilities {
			capabilities[i] = router.Capability(capability)
		}
		declarations[ref] = router.Declaration{Capabilities: capabilities, ContextWindow: declaration.ContextWindow}
	}
	rules := make([]router.Rule, len(cfg.Router.Rules))
	for i, rule := range cfg.Router.Rules {
		rules[i] = router.Rule{
			EstimatedInputTokensGTE:  rule.When.EstimatedInputTokensGTE,
			RequestedOutputTokensGTE: rule.When.RequestedOutputTokensGTE,
			ToolsPresent:             rule.When.ToolsPresent,
			Difficulty:               rule.Difficulty,
		}
	}
	planner, err := router.New(router.Options{
		Tiers: tiers, Auto: cfg.Router.Auto, Declarations: declarations,
		DifficultyTiers: cfg.Router.Difficulty, Rules: rules,
	})
	if err != nil {
		return nil, fmt.Errorf("build router: %w", err)
	}
	var tracker *quota.Tracker
	now := time.Now
	if len(tiers) > 0 {
		candidates := make([]providers.ModelRef, 0)
		for _, refs := range tiers {
			candidates = append(candidates, refs...)
		}
		tracker, err = quota.New(quota.Options{Now: now, Candidates: candidates})
		if err != nil {
			return nil, fmt.Errorf("build quota tracker: %w", err)
		}
	}
	var budgetGate *budget.Gate
	if cfg.Budgets != nil {
		budgetGate, err = budget.NewGate(ctx, budget.GateOptions{Limits: *cfg.Budgets, Providers: cfg.Providers, Store: usageStore, Now: now})
		if err != nil {
			return nil, fmt.Errorf("build budget gate: %w", err)
		}
	}
	var exactCache *cache.Cache
	if cfg.Cache != nil && cfg.Cache.Exact {
		exactCache, err = cache.New(cfg.Cache.MaxMB<<20, cfg.Cache.TTL, now)
		if err != nil {
			return nil, fmt.Errorf("build exact cache: %w", err)
		}
	}
	var allowedHosts []string
	if cfg.Server.Loopback() {
		allowedHosts = api.LoopbackHosts(cfg.Server.Host())
	}
	var usageRecorder interface {
		Record(context.Context, store.Request) error
	}
	if usageStore != nil {
		usageRecorder = usageStore
	}
	options := api.Options{
		Providers:      set,
		Router:         planner,
		Tiers:          tiers,
		LocalKeys:      keys,
		AllowedHosts:   allowedHosts,
		Logger:         logger,
		UsageRecorder:  usageRecorder,
		DashboardStore: usageStore,
		Dashboard:      web.Handler(),
		CostEstimator:  budget.NewPricer(cfg),
		BudgetGate:     budgetGate,
		ExactCache:     exactCache,
		Now:            now,
	}
	if tracker != nil {
		options.Tracker = tracker
	}
	return api.NewHandler(options)
}

// serveUntilDone serves on ln until ctx is cancelled, then shuts down
// gracefully and finally cancels requests still running.
func serveUntilDone(ctx context.Context, ln net.Listener, h http.Handler, logger *slog.Logger) error {
	base, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	srv := api.NewServer(h, logger)
	srv.BaseContext = func(net.Listener) context.Context { return base }

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.InfoContext(ctx, "listening", "addr", ln.Addr().String())

	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.InfoContext(ctx, "shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	cancelRequests()
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	if shutdownErr != nil {
		return fmt.Errorf("shutdown: %w", shutdownErr)
	}
	return nil
}
