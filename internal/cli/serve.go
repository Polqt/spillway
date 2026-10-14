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
	"time"

	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/mock"
	"github.com/Polqt/spillway/internal/redact"
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
	handler, err := newHandler(cfg, logger)
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
func newHandler(cfg config.Config, logger *slog.Logger) (http.Handler, error) {
	var ps []providers.Provider
	for name := range cfg.Providers {
		switch name {
		case "mock":
			p, err := mock.New(mock.Options{Now: time.Now})
			if err != nil {
				return nil, fmt.Errorf("build provider mock: %w", err)
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
		keys[i] = api.LocalKey{Name: k.Name, Key: k.Key}
	}
	var allowedHosts []string
	if cfg.Server.Loopback() {
		allowedHosts = api.LoopbackHosts(cfg.Server.Host())
	}
	return api.NewHandler(api.Options{
		Providers:    set,
		LocalKeys:    keys,
		AllowedHosts: allowedHosts,
		Logger:       logger,
	})
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
