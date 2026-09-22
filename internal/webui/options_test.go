package webui

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportExternalUIAndCompression(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "public")
	if err := Export(dir); err != nil {
		t.Fatal(err)
	}
	if err := Export(dir); err == nil {
		t.Fatal("existing frontend overwritten")
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(strings.Repeat("external-ui", 100)), 0644); err != nil {
		t.Fatal(err)
	}
	for _, disabled := range []bool{false, true} {
		mux := http.NewServeMux()
		if err := RegisterWithOptions(mux, Options{HTMLPath: dir, DisableGzip: disabled}); err != nil {
			t.Fatal(err)
		}
		for _, encoding := range []string{"gzip", "gzip;q=0, *;q=1", "br"} {
			r := httptest.NewRequest("GET", "/admin", nil)
			r.Header.Set("Accept-Encoding", encoding)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			compressed := w.Header().Get("Content-Encoding") == "gzip"
			if compressed != (!disabled && encoding == "gzip") {
				t.Fatal("wrong encoding negotiation", encoding, disabled)
			}
			body := w.Body.Bytes()
			if compressed {
				zr, err := gzip.NewReader(w.Body)
				if err != nil {
					t.Fatal(err)
				}
				body, err = io.ReadAll(zr)
				if err != nil {
					t.Fatal(err)
				}
				zr.Close()
			}
			if w.Code != 200 || !strings.Contains(string(body), "external-ui") {
				t.Fatal("external frontend unavailable")
			}
		}
	}
}
