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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

const shutdownWait = 5 * time.Second

// serve runs the API until ctx is cancelled. It refuses to listen when the
// config is invalid, including when it has no local key (ADR 0003).
func serve(ctx context.Context, args []string, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "path to the config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		report(stderr, "spillway: serve: unexpected argument %q\n\n%s", fs.Arg(0), usage)
		return 2
	}
	explicitConfig := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicitConfig = true
		}
	})

	cfg, err := config.LoadWithEnv(*configPath, lookupEnv)
	if err != nil {
		reportConfigLoadError(stderr, *configPath, explicitConfig, err)
		return 1
	}
	scrubber := redact.NewScrubber(cfg.Secrets()...)
	var logLevel slog.Level
	if err := logLevel.UnmarshalText([]byte(cfg.Logging.Level)); err != nil {
		report(stderr, "spillway: invalid logging level: %v\n", err)
		return 1
	}
	logger := slog.New(redact.NewHandler(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: logLevel}), scrubber))
	// Names the running copy and config so an older server or a different config is visible.
	executable, err := os.Executable()
	if err != nil {
		executable = "unknown"
	}
	absoluteConfig, err := filepath.Abs(*configPath)
	if err != nil {
		absoluteConfig = *configPath
	}
	logger.InfoContext(ctx, "spillway starting", "version", currentVersion(), "executable", executable, "config", absoluteConfig)
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
		report(stderr, "spillway: listen on %s: %v\nChoose an available address in server.listen, then rerun `spillway serve`.\n", cfg.Server.Listen, err)
		return 1
	}
	baseURL := "http:" + "/" + "/" + ln.Addr().String()
	logger.InfoContext(ctx, "next steps", "api", baseURL+"/v1", "models", baseURL+"/v1/models", "dashboard", baseURL+"/dashboard")
	if err := serveUntilDone(ctx, ln, handler, logger, cfg.Server.ShutdownTimeout); err != nil {
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
			if baseURL == "" {
				preset, _ := providers.LookupPreset(name)
				baseURL = preset.DefaultBaseURL
			}
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
				preset, _ := providers.LookupPreset(name)
				baseURL = preset.DefaultBaseURL
				if name == "ollama" {
					baseURL += "/v1"
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
	var classifier router.DifficultyClassifier
	if cfg.Router.Classifier != nil {
		baseURL := cfg.Providers["ollama"].BaseURL
		if baseURL == "" {
			preset, _ := providers.LookupPreset("ollama")
			baseURL = preset.DefaultBaseURL
		}
		model, err := providers.ParseModelRef(cfg.Router.Classifier.Model)
		if err != nil {
			return nil, fmt.Errorf("parse router.classifier.model: %w", err)
		}
		classifier, err = router.NewOllamaClassifier(baseURL, model.Model)
		if err != nil {
			return nil, fmt.Errorf("build local router classifier: %w", err)
		}
	}
	planner, err := router.New(router.Options{
		Tiers: tiers, Auto: cfg.Router.Auto, Declarations: declarations,
		DifficultyTiers: cfg.Router.Difficulty, Rules: rules, Classifier: classifier,
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
		Providers:        set,
		Router:           planner,
		Tiers:            tiers,
		LocalKeys:        keys,
		AllowedHosts:     allowedHosts,
		Logger:           logger,
		UsageRecorder:    usageRecorder,
		DashboardStore:   usageStore,
		Dashboard:        web.Handler(),
		CostEstimator:    budget.NewPricer(cfg),
		DashboardPricing: cfg.Pricing,
		BudgetGate:       budgetGate,
		ExactCache:       exactCache,
		Now:              now,
		Gateway:          api.Gateway{Version: currentVersion(), DashboardAssets: web.Assets()},
	}
	for name, provider := range cfg.Providers {
		preset, _ := providers.LookupPreset(name)
		options.DashboardProviders = append(options.DashboardProviders, api.DashboardProvider{
			Name: name, CredentialRequired: preset.RequiresKey, CredentialPresent: !provider.APIKey.IsZero(),
		})
	}
	if tracker != nil {
		options.Tracker = tracker
	}
	return api.NewHandler(options)
}

// serveUntilDone serves on ln until ctx is cancelled, then shuts down
// gracefully and finally cancels requests still running.
func serveUntilDone(ctx context.Context, ln net.Listener, h http.Handler, logger *slog.Logger, grace time.Duration) error {
	base, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	requests := newRequestTracker()
	srv := api.NewServer(h, logger)
	srv.BaseContext = func(net.Listener) context.Context { return base }
	srv.Handler = requests.middleware(h)

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.InfoContext(ctx, "listening", "addr", ln.Addr().String())

	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.InfoContext(ctx, "shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	cancelled := requests.active()
	if cancelled == 0 {
		var serveErr error
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("serve: %w", err)
		}
		if shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
			return errors.Join(serveErr, fmt.Errorf("shutdown: %w", shutdownErr))
		}
		return serveErr
	}
	cancelRequests()
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	waitCtx, cancelWait := context.WithTimeout(context.WithoutCancel(ctx), shutdownWait)
	defer cancelWait()
	unit := "requests"
	if cancelled == 1 {
		unit = "request"
	}
	waitErr := fmt.Errorf("shutdown timed out; cancelled %d in-flight %s", cancelled, unit)
	if !requests.wait(waitCtx) {
		waitErr = fmt.Errorf("shutdown timed out; cancelled %d in-flight %s, %d handler(s) still running", cancelled, unit, requests.active())
	}
	if shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
		return errors.Join(fmt.Errorf("shutdown: %w", shutdownErr), waitErr)
	}
	return waitErr
}

type requestTracker struct {
	mu          sync.Mutex
	activeCount int
	idle        chan struct{}
	wg          sync.WaitGroup
}

func newRequestTracker() *requestTracker {
	idle := make(chan struct{})
	close(idle)
	return &requestTracker{idle: idle}
}

func (r *requestTracker) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.start()
		defer r.finish()
		next.ServeHTTP(w, req)
	})
}

func (r *requestTracker) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeCount == 0 {
		r.idle = make(chan struct{})
	}
	r.activeCount++
	r.wg.Add(1)
}

func (r *requestTracker) finish() {
	r.wg.Done()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activeCount--
	if r.activeCount == 0 {
		close(r.idle)
	}
}

func (r *requestTracker) active() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeCount
}

func (r *requestTracker) wait(ctx context.Context) bool {
	r.mu.Lock()
	idle := r.idle
	r.mu.Unlock()
	select {
	case <-idle:
		r.wg.Wait()
		return true
	case <-ctx.Done():
		return false
	}
}
