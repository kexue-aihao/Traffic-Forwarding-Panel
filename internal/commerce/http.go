package commerce

import (
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type HTTPOptions struct {
	Authenticate func(*http.Request) (contract.User, error)
	EPay         payment.EPay
	PublicOrigin string // Explicit canonical origin when behind a trusted reverse proxy.
	Channels     map[string]Channel
}

func (s *Service) Register(mux *http.ServeMux, o HTTPOptions) {
	send := func(w http.ResponseWriter, v any, e error) {
		w.Header().Set("Content-Type", "application/json")
		if e != nil {
			status := http.StatusBadRequest
			message := "Request could not be completed"
			code := "commerce_error"
			if errors.Is(e, payment.ErrUnsupported) {
				status = 422
				code = "unsupported"
				message = "该支付协议不支持此操作"
			}
			if errors.Is(e, ErrPaymentUncertain) {
				status = 409
				code = "payment_uncertain"
				message = "支付结果正在核实，请查询原订单，勿重复付款"
			}
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
			json.NewEncoder(w).Encode(contract.APIError{Code: code, Error: message})
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
		page, size := commercePage(r)
		v, total, e := s.PlansPage(r.Context(), page, size)
		send(w, map[string]any{"items": v, "total": total}, e)
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
		page, size := commercePage(r)
		v, total, e := s.LedgerPage(r.Context(), u.ID, page, size)
		send(w, map[string]any{"items": v, "total": total}, e)
	}))
	mux.HandleFunc("GET /api/v1/orders", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		page, size := commercePage(r)
		v, total, e := s.OrdersPage(r.Context(), u.ID, page, size)
		send(w, map[string]any{"items": v, "total": total}, e)
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
		var v Order
		var e error
		if channel, ok := o.Channels[p.Channel]; ok {
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			v, e = s.CreateAdapterOrder(r.Context(), u.ID, p.Channel, p.Key, p.Amount, channel, ip)
		} else {
			v, e = s.CreateOrder(r.Context(), u.ID, p.Channel, p.Key, p.Amount, o.EPay)
		}
		send(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/orders/{id}/reconcile", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.ReconcileOrder(r.Context(), u.ID, r.PathValue("id"), o.Channels)
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
			c := channel{ID: name, Name: name, Status: "unconfigured", Reason: "merchant configuration required"}
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
			if channel, ok := o.Channels[name]; ok && channel.Adapter != nil {
				c.Enabled = true
				c.Status = "configured_unverified"
				c.Reason = "merchant end-to-end verification not recorded"
			}
			list = append(list, c)
		}
		send(w, map[string]any{"items": list, "total": len(list)}, nil)
	}))
	epayNotify := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		if e := r.ParseForm(); e != nil {
			http.Error(w, "fail", 400)
			return
		}
		if channel, ok := o.Channels["epay"]; ok && channel.Adapter != nil {
			status, e := channel.Adapter.VerifyNotify([]byte(r.Form.Encode()), "application/x-www-form-urlencoded")
			if e == nil {
				e = s.ConfirmStatus(r.Context(), "epay", status)
			}
			if e != nil {
				http.Error(w, "fail", 400)
				return
			}
			w.Write([]byte(channel.Adapter.Capabilities().NotifyAck))
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
	}
	mux.HandleFunc("GET /api/v1/payments/epay/notify", epayNotify)
	mux.HandleFunc("POST /api/v1/payments/epay/notify", epayNotify)
	mux.HandleFunc("POST /api/v1/payments/{channel}/notify", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("channel")
		channel, ok := o.Channels[name]
		if !ok || channel.Adapter == nil || name == "epay" {
			http.NotFound(w, r)
			return
		}
		raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if e != nil {
			http.Error(w, "fail", 400)
			return
		}
		status, e := channel.Adapter.VerifyNotify(raw, r.Header.Get("Content-Type"))
		if e == nil {
			e = s.ConfirmStatus(r.Context(), name, status)
		}
		if e != nil {
			http.Error(w, "fail", 400)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(strings.TrimSpace(channel.Adapter.Capabilities().NotifyAck)))
	})
}

func commercePage(r *http.Request) (int, int) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		page = 1000000
	}
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	return page, size
}
