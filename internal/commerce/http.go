package commerce

import (
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/httporigin"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type HTTPOptions struct {
	Authenticate func(*http.Request) (contract.User, error)
	EPay         payment.EPay
	PublicOrigin string // Explicit canonical origin when behind a trusted reverse proxy.
	TrustProxy   bool
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
			if r.Method != "GET" && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				origin, e := url.Parse(r.Header.Get("Origin"))
				scheme := httporigin.Scheme(r, o.TrustProxy)
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
	mux.HandleFunc("GET /api/v1/purchases/{id}/funding", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		rows, err := s.PurchaseFunding(r.Context(), u.ID, r.PathValue("id"), u.Role == "admin")
		send(w, map[string]any{"items": rows}, err)
	}))
	mux.HandleFunc("POST /api/v1/purchases/{id}/refund", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		if u.Role != "admin" {
			http.Error(w, "administrator required", 403)
			return
		}
		var in struct {
			Amount int64  `json:"amount_cents,string"`
			Key    string `json:"idempotency_key"`
			Reason string `json:"reason"`
		}
		if err := decode(w, r, &in); err != nil {
			send(w, nil, err)
			return
		}
		v, err := s.RefundPurchase(r.Context(), u.ID, r.PathValue("id"), in.Key, in.Reason, in.Amount)
		send(w, v, err)
	}))
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
	mux.HandleFunc("PATCH /api/v1/plans/{id}", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		if u.Role != "admin" {
			http.Error(w, "forbidden", 403)
			return
		}
		var in struct {
			Active bool `json:"active"`
		}
		if e := decode(w, r, &in); e != nil {
			send(w, nil, e)
			return
		}
		send(w, map[string]any{"id": r.PathValue("id"), "active": in.Active}, s.SetPlanActive(r.Context(), r.PathValue("id"), in.Active))
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
			// 充值金额以元为单位（"100" 或 "100.00"）；amount_cents 是为旧
			// 客户端保留的兼容字段，两者只能给一个。
			Yuan   string `json:"amount"`
			Amount int64  `json:"amount_cents,string"`
			Key    string `json:"idempotency_key"`
		}
		if e := decode(w, r, &p); e != nil {
			send(w, nil, e)
			return
		}
		amount := p.Amount
		if strings.TrimSpace(p.Yuan) != "" {
			if amount != 0 {
				send(w, nil, errors.New("amount 与 amount_cents 只能给一个"))
				return
			}
			parsed, err := contract.ParseAmount(p.Yuan)
			if err != nil {
				send(w, nil, err)
				return
			}
			amount = parsed
		}
		var v Order
		var e error
		if channel, ok := o.Channels[p.Channel]; ok {
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			v, e = s.CreateAdapterOrder(r.Context(), u.ID, p.Channel, p.Key, amount, channel, ip)
		} else {
			v, e = s.CreateOrder(r.Context(), u.ID, p.Channel, p.Key, amount, o.EPay)
		}
		send(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/orders/{id}/reconcile", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.ReconcileOrder(r.Context(), u.ID, r.PathValue("id"), o.Channels)
		send(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/orders/{id}/close", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.CloseOrder(r.Context(), u.ID, r.PathValue("id"))
		send(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/orders/{id}/refund", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		if u.Role != "admin" {
			http.Error(w, "forbidden", 403)
			return
		}
		var in struct {
			Amount int64  `json:"amount_cents,string"`
			Key    string `json:"idempotency_key"`
			Reason string `json:"reason"`
		}
		if e := decode(w, r, &in); e != nil {
			send(w, nil, e)
			return
		}
		v, e := s.RequestRefund(r.Context(), u.ID, r.PathValue("id"), in.Key, in.Reason, in.Amount)
		send(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/refunds/{id}/resolve", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		if u.Role != "admin" {
			http.Error(w, "forbidden", 403)
			return
		}
		var in struct {
			Completed bool   `json:"completed"`
			Evidence  string `json:"evidence"`
		}
		if e := decode(w, r, &in); e != nil {
			send(w, nil, e)
			return
		}
		v, e := s.ResolveRefund(r.Context(), u.ID, r.PathValue("id"), in.Evidence, in.Completed)
		send(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/orders/{id}/refunds", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.Refunds(r.Context(), u.ID, r.PathValue("id"), u.Role == "admin")
		send(w, map[string]any{"items": v}, e)
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
			Plan        string `json:"plan_id"`
			Version     int64  `json:"expected_version"`
			PlanVersion int64  `json:"expected_plan_version"`
			Key         string `json:"idempotency_key"`
		}
		if e := decode(w, r, &p); e != nil {
			send(w, nil, e)
			return
		}
		v, e := s.PurchaseQuote(r.Context(), u.ID, p.Plan, p.Key, p.Version, p.PlanVersion)
		send(w, v, e)
	}))
	mux.HandleFunc("PUT /api/v1/plans/{id}", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		if u.Role != "admin" {
			http.Error(w, "forbidden", 403)
			return
		}
		var p Plan
		if err := decode(w, r, &p); err != nil {
			send(w, nil, err)
			return
		}
		p.ID = r.PathValue("id")
		v, e := s.UpdatePlan(r.Context(), p)
		send(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/addon-purchases", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		var p struct {
			Plan        string `json:"plan_id"`
			Key         string `json:"idempotency_key"`
			Version     int64  `json:"expected_version"`
			PlanVersion int64  `json:"expected_plan_version"`
		}
		if err := decode(w, r, &p); err != nil {
			send(w, nil, err)
			return
		}
		v, e := s.PurchaseAddon(r.Context(), u.ID, p.Plan, p.Key, p.Version, p.PlanVersion)
		send(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/purchases", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.PurchaseHistory(r.Context(), u.ID)
		send(w, map[string]any{"items": v}, e)
	}))
	mux.HandleFunc("GET /api/v1/auto-renew", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.AutoRenewStatus(r.Context(), u.ID)
		send(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/auto-renew", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		var in struct {
			Enabled bool   `json:"enabled"`
			PlanID  string `json:"plan_id"`
		}
		if e := decode(w, r, &in); e != nil {
			send(w, nil, e)
			return
		}
		if !in.Enabled {
			in.PlanID = ""
		}
		e := s.SetAutoRenew(r.Context(), u.ID, in.PlanID, in.Enabled)
		send(w, map[string]any{"enabled": in.Enabled, "plan_id": in.PlanID}, e)
	}))
	mux.HandleFunc("GET /api/v1/redeem-codes", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		if u.Role != "admin" {
			http.Error(w, "forbidden", 403)
			return
		}
		v, e := s.RedeemCodes(r.Context())
		send(w, map[string]any{"items": v}, e)
	}))
	mux.HandleFunc("DELETE /api/v1/redeem-codes/{id}", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		if u.Role != "admin" {
			http.Error(w, "forbidden", 403)
			return
		}
		send(w, nil, s.RevokeRedeemCode(r.Context(), r.PathValue("id")))
	}))
	mux.HandleFunc("GET /api/v1/commission-policy", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		if u.Role != "admin" {
			http.Error(w, "forbidden", 403)
			return
		}
		rate, e := s.CommissionRate(r.Context())
		send(w, map[string]any{"rate_bps": rate}, e)
	}))
	mux.HandleFunc("POST /api/v1/commissions/{id}/resolve", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		if u.Role != "admin" {
			http.Error(w, "forbidden", 403)
			return
		}
		var in struct {
			Action string `json:"action"`
			Reason string `json:"reason"`
		}
		if e := decode(w, r, &in); e != nil {
			send(w, nil, e)
			return
		}
		send(w, nil, s.ResolveCommission(r.Context(), u.ID, r.PathValue("id"), in.Action, in.Reason))
	}))
	mux.HandleFunc("POST /api/v1/redeem-codes", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		if u.Role != "admin" {
			http.Error(w, "forbidden", 403)
			return
		}
		var in struct {
			Amount  int64      `json:"amount_cents,string"`
			PlanID  string     `json:"plan_id"`
			MaxUses int64      `json:"max_uses"`
			Expires *time.Time `json:"expires_at"`
		}
		if e := decode(w, r, &in); e != nil {
			send(w, nil, e)
			return
		}
		v, plain, e := s.CreateRedeemCode(r.Context(), in.Amount, in.PlanID, in.MaxUses, in.Expires)
		if e == nil {
			m := map[string]any{"id": v.ID, "code": plain, "code_hint": v.CodeHint, "amount_cents": strconv.FormatInt(v.Amount, 10), "plan_id": v.PlanID, "max_uses": v.MaxUses, "expires_at": v.ExpiresAt}
			send(w, m, nil)
			return
		}
		send(w, nil, e)
	}))
	mux.HandleFunc("POST /api/v1/redeem", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		var in struct {
			Code string `json:"code"`
		}
		if e := decode(w, r, &in); e != nil {
			send(w, nil, e)
			return
		}
		v, e := s.RedeemCode(r.Context(), u.ID, in.Code)
		send(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/referrals", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, plain, e := s.CreateReferralCode(r.Context(), u.ID)
		if e == nil {
			send(w, map[string]any{"code": plain, "code_hint": v.CodeHint, "created_at": v.CreatedAt}, nil)
			return
		}
		send(w, nil, e)
	}))
	mux.HandleFunc("POST /api/v1/referrals/bind", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		var in struct {
			Code string `json:"code"`
		}
		if e := decode(w, r, &in); e != nil {
			send(w, nil, e)
			return
		}
		send(w, map[string]any{"bound": true}, s.BindReferral(r.Context(), u.ID, in.Code))
	}))
	mux.HandleFunc("PUT /api/v1/commission-policy", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		if u.Role != "admin" {
			http.Error(w, "forbidden", 403)
			return
		}
		var in struct {
			Rate int64 `json:"rate_bps"`
		}
		if e := decode(w, r, &in); e != nil {
			send(w, nil, e)
			return
		}
		send(w, in, s.SetCommissionRate(r.Context(), in.Rate))
	}))
	mux.HandleFunc("GET /api/v1/commissions", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.Commissions(r.Context(), u.ID)
		send(w, map[string]any{"items": v}, e)
	}))
	mux.HandleFunc("POST /api/v1/webhooks", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		var in struct {
			Format string   `json:"format"`
			URL    string   `json:"url"`
			Events []string `json:"events"`
		}
		if e := decode(w, r, &in); e != nil {
			send(w, nil, e)
			return
		}
		v, secret, e := s.CreateNotification(r.Context(), u.ID, in.URL, in.Events, u.Role == "admin", in.Format)
		if e == nil {
			send(w, map[string]any{"id": v.ID, "url": v.URL, "events": v.Events, "secret": secret, "created_at": v.CreatedAt}, nil)
			return
		}
		send(w, nil, e)
	}))
	mux.HandleFunc("GET /api/v1/webhooks", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.ListWebhooks(r.Context(), u.ID)
		send(w, map[string]any{"items": v}, e)
	}))
	mux.HandleFunc("PUT /api/v1/webhooks/{id}", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		var in WebhookSettings
		if err := decode(w, r, &in); err != nil {
			send(w, nil, err)
			return
		}
		send(w, nil, s.UpdateWebhook(r.Context(), u.ID, r.PathValue("id"), in))
	}))
	mux.HandleFunc("DELETE /api/v1/webhooks/{id}", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		send(w, nil, s.DeleteWebhook(r.Context(), u.ID, r.PathValue("id")))
	}))
	mux.HandleFunc("GET /api/v1/webhook-deliveries", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		v, e := s.WebhookDeliveries(r.Context(), u.ID)
		send(w, map[string]any{"items": v}, e)
	}))
	mux.HandleFunc("GET /api/v1/events", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		v, e := s.EventsForActor(r.Context(), u, limit)
		send(w, map[string]any{"items": v}, e)
	}))
	mux.HandleFunc("GET /api/v1/payment-channels", secure(func(w http.ResponseWriter, r *http.Request, u contract.User) {
		// 通道列表同时是报价来源：前端拿 fee 与 rate 就能算出「充值 100 元
		// 实付多少、折合多少 USDT」，不必自己再猜一遍规则。
		type channel struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			Enabled        bool   `json:"enabled"`
			Status         string `json:"status"`
			Reason         string `json:"reason"`
			FeePercent     string `json:"fee_percent"`
			FeeFixed       string `json:"fee_fixed"`
			CryptoCurrency string `json:"crypto_currency,omitempty"`
			Rate           string `json:"rate,omitempty"`
		}
		list := []channel{}
		for _, name := range []string{"epay", "epusdt", "bepusdt", "tokenpay", "cryptomus"} {
			c := channel{ID: name, Name: name, Status: "unconfigured", Reason: "merchant configuration required", FeePercent: "0.00", FeeFixed: "0.00"}
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
				c.FeePercent = contract.FormatAmount(int64(channel.FeeBPS))
				c.FeeFixed = contract.FormatAmount(channel.FeeFixed)
				c.CryptoCurrency = channel.CryptoCurrency
				c.Rate = channel.Rate
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
