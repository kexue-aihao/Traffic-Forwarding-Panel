// Package app composes independently tested control, commerce and UI modules.
package app

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/platform"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/webui"
)

type Options struct {
	Origin        string
	SecureCookies bool
	EPay          payment.EPay
}

type App struct {
	Handler  http.Handler
	Platform *platform.Server
	Commerce *commerce.Service
}

func New(ctx context.Context, store *storage.Store, opts Options) (*App, error) {
	billing := commerce.New(store.DB, store.Dialect, func(ctx context.Context, fn func(*sql.Tx) error) error { return store.Write(ctx, storage.Critical, fn) })
	if err := billing.Migrate(ctx); err != nil {
		return nil, err
	}
	control := platform.New(store, platform.Options{Origin: opts.Origin, SecureCookies: opts.SecureCookies, Entitlements: billing, LeaseCurrent: billing.LeaseCurrent, RetireLease: billing.RetireLease})
	mux := http.NewServeMux()
	control.Register(mux)
	billing.Register(mux, commerce.HTTPOptions{Authenticate: control.Authenticate, EPay: opts.EPay, PublicOrigin: opts.Origin})
	webui.Register(mux)
	return &App{Handler: webui.Security(mux), Platform: control, Commerce: billing}, nil
}
