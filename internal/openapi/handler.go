// Package openapi embeds the generated versioned API contract.
package openapi

import (
	"bytes"
	_ "embed"
	"net/http"
	"time"
)

//go:generate go run ./generate
//go:embed openapi.json
var document []byte

func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "openapi.json", time.Time{}, bytes.NewReader(document))
	})
}
