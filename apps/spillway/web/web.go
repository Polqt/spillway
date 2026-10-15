// Package web embeds the local dashboard shell and its static assets.
package web

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed dashboard.html dashboard.css dashboard.js
var files embed.FS

// Handler serves the dashboard shell and its embedded assets.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := map[string]string{
			"/dashboard":     "dashboard.html",
			"/dashboard.css": "dashboard.css",
			"/dashboard.js":  "dashboard.js",
		}[r.URL.Path]
		if name == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		content, err := files.ReadFile(name)
		if err != nil {
			http.Error(w, "dashboard asset unavailable", http.StatusInternalServerError)
			return
		}
		contentType := "text/html; charset=utf-8"
		if strings.HasSuffix(name, ".css") {
			contentType = "text/css; charset=utf-8"
		} else if strings.HasSuffix(name, ".js") {
			contentType = "text/javascript; charset=utf-8"
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(content)
		}
	})
}
