package httpapi

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed ui/index.html ui/app.js ui/style.css ui/favicon.svg
var uiFiles embed.FS

func serveUI(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	if name == "" {
		name = "index.html"
	}
	types := map[string]string{"index.html": "text/html; charset=utf-8", "app.js": "text/javascript; charset=utf-8", "style.css": "text/css; charset=utf-8", "favicon.svg": "image/svg+xml"}
	contentType, ok := types[name]
	if !ok || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", contentType)
	data, err := uiFiles.ReadFile("ui/" + name)
	if err != nil {
		http.Error(w, "static resource unavailable", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}
