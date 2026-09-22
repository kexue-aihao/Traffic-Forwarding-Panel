// Package webui serves precompiled, locally hosted application assets.
package webui

import (
	"bytes"
	"compress/gzip"
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed all:assets
var files embed.FS

// Register serves the two shells and assets without falling back for API paths.
func Register(mux *http.ServeMux) {
	if err := RegisterWithOptions(mux, Options{}); err != nil {
		panic(err)
	}
}

type Options struct {
	HTMLPath    string
	DisableGzip bool
}

func ValidateDirectory(directory string) error {
	for _, name := range []string{"index.html", "assets/app.js", "assets/app.css", "assets/theme.js"} {
		if _, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(name))); err != nil {
			return errors.New("html-path 必须包含 index.html 和 assets 下的前端产物；可用 -export-html 导出")
		}
	}
	return nil
}

func RegisterWithOptions(mux *http.ServeMux, opts Options) error {
	assets, err := fs.Sub(files, "assets")
	if err != nil {
		return err
	}
	indexFiles := assets
	if opts.HTMLPath != "" {
		if err := ValidateDirectory(opts.HTMLPath); err != nil {
			return err
		}
		indexFiles = os.DirFS(opts.HTMLPath)
		assets, err = fs.Sub(indexFiles, "assets")
		if err != nil {
			return err
		}
	}
	serve := func(w http.ResponseWriter, r *http.Request, source fs.FS, name string) {
		info, err := fs.Stat(source, name)
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		body, err := fs.ReadFile(source, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		mime := mimeTypes[path.Ext(name)]
		if mime != "" {
			w.Header().Set("Content-Type", mime)
		}
		if !opts.DisableGzip && (strings.HasPrefix(mime, "text/") || mime == "image/svg+xml" || strings.HasPrefix(mime, "application/json")) {
			w.Header().Add("Vary", "Accept-Encoding")
			if len(body) > 512 && r.Header.Get("Range") == "" && acceptsGzip(r.Header.Get("Accept-Encoding")) {
				var compressed bytes.Buffer
				zw := gzip.NewWriter(&compressed)
				_, _ = zw.Write(body)
				_ = zw.Close()
				body = compressed.Bytes()
				w.Header().Set("Content-Encoding", "gzip")
			}
		}
		http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(body))
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "." || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		serve(w, r, assets, name)
	})))
	index := func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, indexFiles, "index.html")
	}
	mux.HandleFunc("GET /{$}", index)
	mux.HandleFunc("GET /admin", index)
	mux.HandleFunc("GET /admin/{$}", index)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":"not_found","error":"接口不存在"}`))
	})
	return nil
}

func acceptsGzip(header string) bool {
	wildcard := false
	for _, part := range strings.Split(header, ",") {
		pieces := strings.Split(strings.TrimSpace(part), ";")
		quality := 1.0
		for _, parameter := range pieces[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && key == "q" {
				quality, _ = strconv.ParseFloat(value, 64)
			}
		}
		if pieces[0] == "gzip" {
			return quality > 0 && quality <= 1
		}
		if pieces[0] == "*" {
			wildcard = quality > 0 && quality <= 1
		}
	}
	return wildcard
}

// Export never overwrites an existing directory or an operator's custom frontend.
func Export(directory string) error {
	if err := os.MkdirAll(filepath.Dir(directory), 0755); err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0755); err != nil {
		return errors.New("前端导出目录必须不存在，请使用新目录")
	}
	if err := fs.WalkDir(files, "assets", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(directory, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.Mkdir(target, 0755)
		}
		body, err := files.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0644)
	}); err != nil {
		return err
	}
	body, err := files.ReadFile("assets/index.html")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "index.html"), body, 0644)
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
