package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/store"
)

// requestRecord collects what inner handlers learn about a request, for the
// one access log line written when the request ends. It never holds a key,
// a header value or a body.
type requestRecord struct {
	clientApp string
	provider  string
	model     string
	tier      string
	route     string
	attempts  int
	stream    bool
	err       error
	usage     store.Request
	attemptAt time.Time
}

func (rec *requestRecord) setProvider(ref providers.ModelRef) {
	rec.provider, rec.model = ref.Provider, ref.Model
}

func (rec *requestRecord) beginTier(tier string) {
	rec.tier = tier
}

func (rec *requestRecord) beginAttempt(ref providers.ModelRef) {
	rec.attempts++
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
	rec.attemptAt = time.Time{}
}

func (rec *requestRecord) setStream() {
	rec.stream = true
}

type recordKey struct{}

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
			if usage.ErrorKind == "" && sw.status >= http.StatusBadRequest {
				usage.ErrorKind = "request_error"
			}
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
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
