// Package app composes independently tested control, commerce and UI modules.
package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/alerts"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/openapi"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/platform"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/webui"
)

type Options struct {
	Origin        string
	SecureCookies bool
	EPay          payment.EPay
	Channels      map[string]commerce.Channel
}

type App struct {
	Alerts   *alerts.Manager
	Handler  http.Handler
	Platform *platform.Server
	Commerce *commerce.Service
	channels map[string]commerce.Channel
}

func New(ctx context.Context, store *storage.Store, opts Options) (*App, error) {
	channels := make(map[string]commerce.Channel, len(opts.Channels)+1)
	for name, channel := range opts.Channels {
		channels[name] = channel
	}
	// The legacy environment configuration uses the same adapter and retry
	// worker as the operator-owned JSON file. Explicit file configuration wins.
	if _, exists := channels["epay"]; !exists && opts.EPay.Gateway != "" && opts.EPay.Key != "" {
		if opts.EPay.NotifyURL == "" {
			opts.EPay.NotifyURL = strings.TrimRight(opts.Origin, "/") + "/api/v1/payments/epay/notify"
		}
		if opts.EPay.ReturnURL == "" {
			opts.EPay.ReturnURL = strings.TrimRight(opts.Origin, "/") + "/#/commerce"
		}
		if !validPaymentURL(opts.EPay.NotifyURL, false) || !validPaymentURL(opts.EPay.ReturnURL, true) {
			return nil, errors.New("public HTTPS EPay callback URLs required")
		}
		adapter, err := payment.NewAdapter(payment.Configuration{Kind: "epay", Gateway: opts.EPay.Gateway, MerchantID: opts.EPay.PID, Key: opts.EPay.Key})
		if err != nil {
			return nil, errors.New("invalid legacy EPay configuration")
		}
		channels["epay"] = commerce.Channel{Adapter: adapter, NotifyURL: opts.EPay.NotifyURL, ReturnURL: opts.EPay.ReturnURL}
	}
	billing := commerce.New(store.DB, store.Dialect, func(ctx context.Context, fn func(*sql.Tx) error) error { return store.Write(ctx, storage.Critical, fn) })
	billing.CheckAccount = func(ctx context.Context, tx *sql.Tx, user string) error {
		query := "SELECT disabled FROM cp_users WHERE id=?"
		if store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		var disabled int
		err := tx.QueryRowContext(ctx, store.Rebind(query), user).Scan(&disabled)
		if errors.Is(err, sql.ErrNoRows) || disabled != 0 {
			return commerce.ErrAccountDisabled
		}
		return err
	}
	if err := billing.Migrate(ctx); err != nil {
		return nil, err
	}
	control := platform.New(store, platform.Options{Origin: opts.Origin, SecureCookies: opts.SecureCookies, Entitlements: billing, LeaseCurrent: billing.LeaseCurrent, RetireLease: billing.RetireLease, ResourceLimits: billing.LimitsTx})
	billing.PaymentAllowed = control.PaymentAllowed
	if err := control.MigrateProbeHistory(ctx); err != nil {
		return nil, err
	}
	monitor := alerts.New(store, billing)
	if err := monitor.Migrate(ctx); err != nil {
		return nil, err
	}
	billing.EventVisible = monitor.Visible
	mux := http.NewServeMux()
	control.Register(mux)
	billing.Register(mux, commerce.HTTPOptions{Authenticate: control.Authenticate, EPay: opts.EPay, PublicOrigin: opts.Origin, Channels: channels})
	monitor.Register(mux, control)
	openapi.Register(mux)
	webui.Register(mux)
	return &App{Alerts: monitor, Handler: webui.Security(mux), Platform: control, Commerce: billing, channels: channels}, nil
}

// RunBackground is started only in server mode, never during restore or local
// account commands. It returns after all workers stop, before closing SQL.
func (a *App) RunBackground(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Go(func() { a.Alerts.Run(ctx) })
	workers.Go(func() { a.Platform.RunProbeHistory(ctx) })
	workers.Go(func() { a.Platform.RunOperationLoop(ctx) })
	workers.Go(func() { _ = a.Platform.RunTaskLoop(ctx) })
	workers.Go(func() { _ = a.Commerce.RunReconciliation(ctx, a.channels) })
	workers.Go(func() { _ = a.Commerce.RunAutoRenewLoop(ctx) })
	workers.Go(func() { _ = a.Commerce.RunWebhookLoop(ctx) })
	workers.Wait()
}
