package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Polqt/spillway/internal/budget"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/router"
	"github.com/Polqt/spillway/internal/store"
)

// requestRecord collects what inner handlers learn about a request, for the
// one access log line written when the request ends. It never holds a key,
// a header value or a body.
type requestRecord struct {
	clientApp             string
	provider              string
	model                 string
	tier                  string
	route                 string
	attempts              int
	stream                bool
	err                   error
	usage                 store.Request
	attemptAt             time.Time
	pricer                attemptPricer
	gate                  *budget.Gate
	now                   func() time.Time
	estimatedPromptTokens int64
	reportedCost          json.RawMessage
	totalUsageSource      string
}

type attemptPricer interface {
	Estimate(providers.ModelRef, int64, int64, json.RawMessage) budget.Cost
}

func (rec *requestRecord) configure(pricer attemptPricer, body []byte, gate *budget.Gate, now func() time.Time) {
	rec.pricer = pricer
	rec.gate = gate
	rec.now = now
	if requirements, err := router.Detect(body); err == nil {
		rec.estimatedPromptTokens = int64(requirements.EstimatedTokens) * 2
	}
}

func (rec *requestRecord) setProvider(ref providers.ModelRef) {
	rec.provider, rec.model = ref.Provider, ref.Model
}

func (rec *requestRecord) beginTier(tier string) {
	rec.tier = tier
}

func (rec *requestRecord) beginAttempt(ref providers.ModelRef) {
	rec.attempts++
	rec.usage.PromptTokens = 0
	rec.usage.CompletionTokens = 0
	rec.usage.UsageSource = "none"
	rec.reportedCost = nil
	if len(rec.usage.Attempts) >= 16 {
		return
	}
	rec.usage.Attempts = append(rec.usage.Attempts, store.Attempt{Provider: ref.Provider, Model: ref.Model})
	rec.attemptAt = time.Now()
}

func (rec *requestRecord) endAttempt(outcome string) {
	if len(rec.usage.Attempts) == 0 || rec.attemptAt.IsZero() {
		return
	}
	attempt := &rec.usage.Attempts[len(rec.usage.Attempts)-1]
	attempt.Outcome = outcome
	attempt.LatencyMS = time.Since(rec.attemptAt).Milliseconds()
	if outcome != "success" && rec.usage.UsageSource != "reported" && rec.usage.UsageSource != "estimated" {
		rec.usage.PromptTokens = rec.estimatedPromptTokens
		rec.usage.UsageSource = "estimated"
	}
	attempt.PromptTokens = rec.usage.PromptTokens
	attempt.CompletionTokens = rec.usage.CompletionTokens
	attempt.UsageSource = rec.usage.UsageSource
	at := time.Now()
	if rec.now != nil {
		at = rec.now()
	}
	attempt.At = at
	if rec.pricer != nil {
		cost := rec.pricer.Estimate(providers.ModelRef{Provider: attempt.Provider, Model: attempt.Model}, attempt.PromptTokens, attempt.CompletionTokens, rec.reportedCost)
		attempt.CostMicroUSD, attempt.CostSource = cost.MicroUSD, cost.Source
		rec.usage.CostMicroUSD = saturatingAdd(rec.usage.CostMicroUSD, cost.MicroUSD)
		if rec.usage.CostSource == "" {
			rec.usage.CostSource = cost.Source
		} else if rec.usage.CostSource != cost.Source {
			rec.usage.CostSource = "mixed"
		}
		if rec.gate != nil {
			rec.gate.ChargeFor(providers.ModelRef{Provider: attempt.Provider, Model: attempt.Model}, at, cost.MicroUSD)
		}
	}
	rec.usage.PromptTokens = attempt.PromptTokens
	rec.usage.CompletionTokens = attempt.CompletionTokens
	rec.usage.UsageSource = attempt.UsageSource
	if rec.totalUsageSource == "" || attempt.UsageSource == "estimated" {
		rec.totalUsageSource = attempt.UsageSource
	} else if rec.totalUsageSource != attempt.UsageSource {
		rec.totalUsageSource = "estimated"
	}
	rec.attemptAt = time.Time{}
}

const maxInt64 = int64(^uint64(0) >> 1)

func saturatingAdd(a, b int64) int64 {
	if b > 0 && a > maxInt64-b {
		return maxInt64
	}
	return a + b
}

func (rec *requestRecord) setStream() {
	rec.stream = true
}

type recordKey struct{}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// recordFrom returns the request's record. Handlers outside accessLog get a
// throwaway record, so callers never check for nil.
func recordFrom(ctx context.Context) *requestRecord {
	if rec, ok := ctx.Value(recordKey{}).(*requestRecord); ok {
		return rec
	}
	return &requestRecord{}
}

// statusWriter captures the status and size of a response.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func accessLog(logger *slog.Logger, recorder interface {
	Record(context.Context, store.Request) error
}, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &requestRecord{}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), recordKey{}, rec)))
		if sw.status == 0 {
			// net/http sends 200 for a handler that writes nothing.
			sw.status = http.StatusOK
		}
		if recorder != nil && r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
			usage := rec.usage
			if rec.totalUsageSource != "" {
				usage.UsageSource = rec.totalUsageSource
			}
			usage.At = start
			usage.ClientApp = rec.clientApp
			usage.Route = rec.route
			usage.Tier = rec.tier
			usage.Provider = rec.provider
			usage.Model = rec.model
			usage.Status = sw.status
			usage.Stream = rec.stream
			usage.LatencyMS = time.Since(start).Milliseconds()
			if usage.UsageSource == "" {
				usage.UsageSource = "none"
			}
			if usage.CostSource == "" {
				usage.CostSource = "none"
			}
			if usage.ErrorKind == "" && sw.status >= http.StatusBadRequest {
				usage.ErrorKind = "request_error"
			}
			// Allow the serialized SQLite writer to drain a burst after the client request completes.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
			if err := recorder.Record(ctx, usage); err != nil {
				logger.ErrorContext(ctx, "record usage failed", "error_kind", "store_write_failed", "error", err)
			}
			cancel()
		}

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"latency_ms", time.Since(start).Milliseconds(),
			"bytes", sw.bytes,
		}
		if rec.clientApp != "" {
			attrs = append(attrs, "client_app", rec.clientApp)
		}
		if rec.provider != "" {
			attrs = append(attrs, "provider", rec.provider, "model", rec.model, "stream", rec.stream)
		}
		if rec.tier != "" {
			attrs = append(attrs, "tier", rec.tier, "attempts", rec.attempts)
		}
		if rec.route != "" {
			attrs = append(attrs, "route", rec.route)
		}
		if rec.err != nil {
			attrs = append(attrs, "error", rec.err.Error())
		}
		// Client mistakes such as an unknown model are not Spillway errors.
		level := slog.LevelInfo
		if sw.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		logger.Log(r.Context(), level, "request", attrs...)
	})
}

// recoverPanics turns a handler panic into an OpenAI 500 so one bad request
// cannot drop the connection without an answer. It sits outside accessLog,
// so the panicking request is logged here, not there.
func recoverPanics(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer answerPanic(logger, w, r)
		next.ServeHTTP(w, r)
	})
}

// answerPanic must be deferred directly so that its recover call stops the
// panic. http.ErrAbortHandler is re-raised because net/http uses it to abort
// a response on purpose.
func answerPanic(logger *slog.Logger, w http.ResponseWriter, r *http.Request) {
	v := recover()
	if v == nil {
		return
	}
	if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(v)
	}
	logger.ErrorContext(r.Context(), "panic", "method", r.Method, "path", r.URL.Path, "panic", v)
	writeInternalError(w)
}
