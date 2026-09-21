package alerts

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/platform"
)

func (m *Manager) Register(mux *http.ServeMux, control *platform.Server) {
	send := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, status int, message string) {
		send(w, status, contract.APIError{Code: http.StatusText(status), Error: message})
	}
	mux.HandleFunc("GET /api/v1/alert-policy", control.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		p, err := m.Policy(r.Context())
		if err != nil {
			fail(w, 500, "alert policy unavailable")
			return
		}
		send(w, 200, p)
	}))
	mux.HandleFunc("PUT /api/v1/alert-policy", control.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		actor, _ := platform.UserFromContext(r.Context())
		if actor.Role != "admin" {
			fail(w, 403, "forbidden")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var p Policy
		if decoder.Decode(&p) != nil || decoder.Decode(new(any)) != io.EOF {
			fail(w, 400, "invalid alert policy")
			return
		}
		out, err := m.SetPolicy(r.Context(), actor.ID, p)
		if errors.Is(err, ErrConflict) {
			fail(w, 409, "alert policy changed; reload before saving")
			return
		}
		if err != nil {
			fail(w, 400, "invalid alert policy")
			return
		}
		send(w, 200, out)
	}))
}
