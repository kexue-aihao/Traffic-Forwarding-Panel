package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedAssetsAndShells(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux)
	handler := Security(mux)
	for _, endpoint := range []string{"/", "/admin", "/admin/", "/assets/app.js", "/assets/app.css", "/assets/theme.js"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", endpoint, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", endpoint, w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing no-store")
		}
		if strings.HasSuffix(endpoint, ".js") && !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
			t.Fatalf("invalid JS MIME: %s", endpoint)
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self';") {
			t.Fatal("CSP scripts must remain self-only")
		}
	}
	fs.WalkDir(files, "assets", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/"+name, nil))
		if w.Code != 200 {
			t.Errorf("embedded asset inaccessible %s: %d", name, w.Code)
		}
		return nil
	})
	for _, endpoint := range []string{"/api/v1/does-not-exist", "/assets/", "/assets/no-such.js", "/assets/fonts/"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", endpoint, nil))
		if w.Code != 404 {
			t.Errorf("%s expected 404, got %d", endpoint, w.Code)
		}
		if strings.HasPrefix(endpoint, "/api/") && !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			t.Fatal("API miss returned HTML")
		}
	}
}
