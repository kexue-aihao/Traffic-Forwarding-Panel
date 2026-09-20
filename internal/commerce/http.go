package commerce

import (
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
	"io"
	"net/http"
	"net/url"
)

type HTTPOptions struct {
	Authenticate func(*http.Request) (contract.User, error)
	EPay         payment.EPay
	PublicOrigin string // Explicit canonical origin when behind a trusted reverse proxy.
}

func (s *Service) Register(mux *http.ServeMux, o HTTPOptions) {
	send := func(w http.ResponseWriter, v any, e error) {
		w.Header().Set("Content-Type", "application/json")
		if e != nil {
			status := http.StatusBadRequest
			message := "Request could not be completed"
			if errors.Is(e, ErrFunds) {
				message = "Insufficient available balance"
			}
			if errors.Is(e, ErrConflict) {
				status = 409
			}
			if errors.Is(e, sql.ErrNoRows) {
				status = 404
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(contract.APIError{Code: "commerce_error", Error: message})
			return
		}
		json.NewEncoder(w).Encode(v)
	}
	decode := func(w http.ResponseWriter, r *http.Request, v any) error {
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		if err := d.Decode(v); err != nil {
			return err
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return errors.New("expected one JSON value")
		}
		return nil
	}
	secure := func(fn func(http.ResponseWriter, *http.Request, contract.User)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u, e := o.Authenticate(r)
			if e != nil || u.Disabled {
				http.Error(w, "unauthorized", 401)
				return
			}
			if r.Method != "GET" && r.Header.Get("Authorization") == "" {
				origin, e := url.Parse(r.Header.Get("Origin"))
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				expectedOrigin := scheme + "://" + r.Host
				if o.PublicOrigin != "" {
					expectedOrigin = o.PublicOrigin
				}
				if r.Header.Get("X-Requested-With") != "fetch" || e != nil || origin.Host != r.Host || origin.Scheme+"://"+origin.Host != expectedOrigin || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" {
					http.Error(w, "invalid origin", 403)
					return
				}
			}
			fn(w, r, u)
		}
	}
	mux.HandleFunc("GET /api/v1/plans", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.Plans(r.Context())
		send(w, map[string]any{"items": v, "total": len(v)}, e)
	}))
	mux.HandleFunc("POST /api/v1/plans", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		if u.Role != "admin" {
			http.Error(w, "forbidden", 403)
			return
		}
		var p Plan
		if e := decode(w, r, &p); e != nil {
			send(w, nil, e)
			return
		}
		v, e := s.CreatePlan(r.Context(), p)
		send(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/wallet", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.Wallet(r.Context(), u.ID)
		send(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/ledger", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.Ledger(r.Context(), u.ID)
		send(w, map[string]any{"items": v, "total": len(v)}, e)
	}))
	mux.HandleFunc("GET /api/v1/orders", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.Orders(r.Context(), u.ID)
		send(w, map[string]any{"items": v, "total": len(v)}, e)
	}))
	mux.HandleFunc("POST /api/v1/orders", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		var p struct {
			Channel string `json:"channel"`
			Amount  int64  `json:"amount_cents,string"`
			Key     string `json:"idempotency_key"`
		}
		if e := decode(w, r, &p); e != nil {
			send(w, nil, e)
			return
		}
		v, e := s.CreateOrder(r.Context(), u.ID, p.Channel, p.Key, p.Amount, o.EPay)
		send(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/entitlement", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.Entitlement(r.Context(), u.ID)
		if errors.Is(e, sql.ErrNoRows) {
			send(w, nil, nil)
			return
		}
		send(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/purchases", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		var p struct {
			Plan    string `json:"plan_id"`
			Version int64  `json:"expected_version"`
			Key     string `json:"idempotency_key"`
		}
		if e := decode(w, r, &p); e != nil {
			send(w, nil, e)
			return
		}
		v, e := s.Purchase(r.Context(), u.ID, p.Plan, p.Key, p.Version)
		send(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/payment-channels", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		type channel struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
		}
		list := []channel{}
		for _, name := range []string{"epay", "epusdt", "bepusdt", "tokenpay", "cryptomus", "cyber"} {
			c := channel{ID: name, Name: name, Status: "not_implemented", Reason: "adapter implementation pending"}
			if name == "cyber" {
				c.Status = "blocked"
				c.Reason = "provider identity and versioned API documentation required"
			}
			if name == "epay" {
				c.Status = "unconfigured"
				c.Reason = "merchant configuration required"
				if _, e := o.EPay.Create("configuration-check", 1); e == nil {
					c.Enabled = true
					c.Status = "configured_unverified"
					c.Reason = "merchant end-to-end verification not recorded"
				}
			}
			list = append(list, c)
		}
		send(w, map[string]any{"items": list, "total": len(list)}, nil)
	}))
	mux.HandleFunc("/api/v1/payments/epay/notify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		if e := r.ParseForm(); e != nil {
			http.Error(w, "fail", 400)
			return
		}
		order, transaction, amount, e := o.EPay.Verify(r.Form)
		if e == nil {
			e = s.ConfirmPayment(r.Context(), "epay", order, transaction, amount)
		}
		if e != nil {
			http.Error(w, "fail", 400)
			return
		}
		w.Write([]byte("success"))
	})
}
