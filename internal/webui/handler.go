// Package webui serves precompiled, locally hosted application assets.
package webui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:assets
var files embed.FS

// Register serves the two shells and assets without falling back for API paths.
func Register(mux *http.ServeMux) {
	assets, err := fs.Sub(files, "assets")
	if err != nil {
		panic(err)
	}
	server := http.FileServer(http.FS(assets))
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "." || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		info, e := fs.Stat(assets, name)
		if e != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		if mime, ok := mimeTypes[path.Ext(name)]; ok {
			w.Header().Set("Content-Type", mime)
		}
		if name == "index.html" {
			body, err := fs.ReadFile(assets, name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(body))
			return
		}
		server.ServeHTTP(w, r)
	})))
	index := func(w http.ResponseWriter, r *http.Request) {
		body, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			http.Error(w, "UI assets unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(body)
	}
	mux.HandleFunc("GET /{$}", index)
	mux.HandleFunc("GET /admin", index)
	mux.HandleFunc("GET /admin/{$}", index)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":"not_found","error":"接口不存在"}`))
	})
}

var mimeTypes = map[string]string{
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".html":  "text/html; charset=utf-8",
	".svg":   "image/svg+xml",
	".woff2": "font/woff2",
	".woff":  "font/woff",
	".txt":   "text/plain; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".png":   "image/png",
	".ico":   "image/x-icon",
}

// Security applies to the complete mux, including errors and payment callbacks.
func Security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
