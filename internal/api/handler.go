// Package api serves the OpenAI-compatible HTTP API: local key auth, the
// loopback Host check, chat completions and the OpenAI error shape.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/redact"
	"github.com/Polqt/spillway/internal/router"
	"github.com/Polqt/spillway/internal/store"
)

// LocalKey is one local key and the name of the client app that presents it.
type LocalKey struct {
	Name string
	Key  redact.Secret
}

// providerSet is the part of providers.Set the API uses.
type providerSet interface {
	Lookup(name string) (providers.Provider, bool)
	Providers() []providers.Provider
}

type candidateTracker interface {
	Available(ref providers.ModelRef) (bool, time.Time, string)
	Observe(ref providers.ModelRef, err error, hint providers.RateLimit)
}

// Planner resolves virtual model requests and lists configured virtual models.
type Planner interface {
	Plan(model string, body []byte) (router.Plan, bool, error)
	VirtualModels() []string
}

// Options configures NewHandler.
type Options struct {
	Providers providerSet
	// Router plans auto and tier requests. When nil, NewHandler builds a tier-only planner from Tiers.
	Router Planner
	// Tiers maps virtual tier names to candidates in their configured order.
	Tiers map[string][]providers.ModelRef
	// Tracker stores cooldowns for configured tier candidates.
	Tracker candidateTracker
	// LocalKeys must hold at least one non-empty key; there is no
	// unauthenticated mode (ADR 0003).
	LocalKeys []LocalKey
	// AllowedHosts, when non-nil, enables the Host check. Set it to
	// LoopbackHosts only when listening on loopback.
	AllowedHosts []string
	Logger       *slog.Logger
	// UsageRecorder records completed chat request metadata when configured.
	UsageRecorder interface {
		Record(context.Context, store.Request) error
	}
	CostEstimator attemptPricer
	// Now supplies the clock used to format cooldown retry times.
	Now               func() time.Time
	BodyReadTimeout   time.Duration
	ChunkWriteTimeout time.Duration
	ModelsTimeout     time.Duration
}

// NewHandler returns the API handler. Middleware runs outermost first:
// recover, access log, Host check, auth, then the routes.
func NewHandler(o Options) (http.Handler, error) {
	if o.Providers == nil {
		return nil, errors.New("api: Options.Providers is required")
	}
	if o.Logger == nil {
		return nil, errors.New("api: Options.Logger is required")
	}
	if o.BodyReadTimeout <= 0 {
		o.BodyReadTimeout = 30 * time.Second
	}
	if o.ChunkWriteTimeout <= 0 {
		o.ChunkWriteTimeout = 30 * time.Second
	}
	if o.ModelsTimeout <= 0 {
		o.ModelsTimeout = 10 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	auth, err := newAuthenticator(o.LocalKeys)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	planner := o.Router
	if planner == nil {
		var err error
		planner, err = router.New(router.Options{Tiers: o.Tiers})
		if err != nil {
			return nil, err
		}
	}
	mux.Handle("GET /v1/models", &modelsHandler{providers: o.Providers, logger: o.Logger, timeout: o.ModelsTimeout, virtualModels: planner.VirtualModels()})
	mux.Handle("POST /v1/chat/completions", &chatHandler{
		providers: o.Providers, bodyReadTimeout: o.BodyReadTimeout,
		chunkWriteTimeout: o.ChunkWriteTimeout, logger: o.Logger, router: planner,
		tracker: o.Tracker, now: o.Now, captureStreamUsage: o.UsageRecorder != nil, pricer: o.CostEstimator,
	})
	// Patterns without a method catch the wrong-method case, because the
	// stock mux would answer it in plain text.
	mux.Handle("/healthz", methodNotAllowed("GET, HEAD"))
	mux.Handle("/v1/models", methodNotAllowed("GET"))
	mux.Handle("/v1/chat/completions", methodNotAllowed("POST"))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "Not found. Spillway serves POST /v1/chat/completions.", typeInvalidRequest, "not_found")
	})

	var h http.Handler = mux
	h = auth.middleware(h)
	if o.AllowedHosts != nil {
		h = hostCheck(o.AllowedHosts, h)
	}
	h = accessLog(o.Logger, o.UsageRecorder, h)
	h = recoverPanics(o.Logger, h)
	return h, nil
}

// healthz answers "ok" and nothing else, so an unauthenticated caller learns
// no version, provider or configuration.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

func methodNotAllowed(allow string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed. Use "+allow+".", typeInvalidRequest, "method_not_allowed")
	})
}
