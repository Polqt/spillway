//go:build dashboardassets

// Package web embeds the local dashboard shell and its static assets.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed dist
var files embed.FS

// Assets reports "embedded": the built Preact assets are embedded.
func Assets() string {
	return "embedded"
}

// Handler serves the dashboard shell and its embedded assets.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		name := "dist/index.html"
		contentType := "text/html; charset=utf-8"
		if r.URL.Path != "/dashboard" {
			if !strings.HasPrefix(r.URL.Path, "/assets/") {
				http.NotFound(w, r)
				return
			}
			asset := strings.TrimPrefix(r.URL.Path, "/")
			if !fs.ValidPath(asset) || !strings.HasPrefix(asset, "assets/") {
				http.NotFound(w, r)
				return
			}
			name = "dist/" + asset
			switch path.Ext(asset) {
			case ".css":
				contentType = "text/css; charset=utf-8"
			case ".js":
				contentType = "text/javascript; charset=utf-8"
			default:
				http.NotFound(w, r)
				return
			}
		}

		content, err := files.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			// #nosec G705 -- content is compiled into the binary from local dashboard assets.
			_, _ = w.Write(content)
		}
	})
}
