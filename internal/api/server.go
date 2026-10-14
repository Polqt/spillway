package api

import (
	"log/slog"
	"net/http"
	"time"
)

// NewServer returns an http.Server for h. ReadTimeout and WriteTimeout stay
// 0 on purpose: net/http applies them to the whole request, which would cut
// long streams. Request bodies and slow clients are bounded per request.
func NewServer(h http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}
