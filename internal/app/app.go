// Package app composes independently tested control, commerce and UI modules.
package app

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agentdist"
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
	TrustProxy    bool
	EPay          payment.EPay
	// PaymentConfigs 是启动时从配置文件读到的通道配置。它只当作初始值：写进
	// 数据库之后就以数据库为准，面板上看到的就是生效的那一份。
	PaymentConfigs map[string]PaymentConfiguration
	// AgentDir 是发布给设备接入用的 Agent 产物目录，空值表示面板可执行文件
	// 所在目录 —— 容器镜像正是把 /agent 放在 /panel 旁边。
	AgentDir             string
	HTMLPath             string
	DisableGzip          bool
	OfflineNodeTime      time.Duration
	OfflineNodeRetention time.Duration
	UserRateLimit        *platform.RateLimit
	DefaultRateLimit     *platform.RateLimit
}

type App struct {
	Alerts   *alerts.Manager
	Handler  http.Handler
	Platform *platform.Server
	Commerce *commerce.Service
	// store 与 origin 留给支付通道的设置接口：它要读写自己的配置表，并按 origin
	// 补出回调地址。
	store  *storage.Store
	origin string
}

func New(ctx context.Context, store *storage.Store, opts Options) (*App, error) {
	if err := migratePaymentSettings(ctx, store); err != nil {
		return nil, err
	}
	// 支付通道以数据库为准：启动参数只是第一次的初始值，之后都在面板上改。
	configs := startupPaymentConfigs(opts)
	stored, err := loadPaymentSettings(ctx, store)
	if err != nil {
		return nil, err
	}
	if len(stored.Channels) > 0 {
		if len(configs) > 0 {
			slog.WarnContext(ctx, "启动参数里的支付通道配置被数据库里的设置覆盖：改配置请到面板的站点设置")
		}
		configs = stored.Channels
	} else if len(configs) > 0 {
		// 第一次启动：把配置文件/环境变量里的通道落库，面板上就能直接看到并接着改。
		if _, err := savePaymentSettings(ctx, store, 0, configs, nil); err != nil {
			return nil, err
		}
	}
	channels, err := BuildPaymentChannels(configs, opts.Origin)
	if err != nil {
		return nil, err
	}
	// 旧路径（下单时找不到对应通道适配器）读的还是 opts.EPay：把回调地址补成
	// 配置里的那一份，两条路给出的地址不会不一致。
	if cfg, ok := configs["epay"]; ok {
		opts.EPay.NotifyURL, opts.EPay.ReturnURL = cfg.NotifyURL, cfg.ReturnURL
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
	control := platform.New(store, platform.Options{Origin: opts.Origin, TrustProxy: opts.TrustProxy, SecureCookies: opts.SecureCookies, Entitlements: billing, LeaseCurrent: billing.LeaseCurrent, RetireLease: billing.RetireLease, ResourceLimits: billing.LimitsTx, ActiveEntitlement: billing.HasActiveEntitlement, OfflineNodeTime: opts.OfflineNodeTime, OfflineNodeRetention: opts.OfflineNodeRetention, UserRateLimit: opts.UserRateLimit, DefaultRateLimit: opts.DefaultRateLimit})
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
	billing.Register(mux, commerce.HTTPOptions{Authenticate: control.Authenticate, EPay: opts.EPay, PublicOrigin: opts.Origin, TrustProxy: opts.TrustProxy, Channels: channels})
	monitor.Register(mux, control)
	openapi.Register(mux)
	if err := webui.RegisterWithOptions(mux, webui.Options{HTMLPath: opts.HTMLPath, DisableGzip: opts.DisableGzip}); err != nil {
		return nil, err
	}
	agentdist.Register(mux, opts.AgentDir)
	application := &App{Alerts: monitor, Handler: webui.Security(control.RequestLimits(mux)), Platform: control, Commerce: billing, store: store, origin: opts.Origin}
	// 支付通道配置：面板上保存之后立刻生效，不必重启进程。
	mux.HandleFunc("GET /api/v1/payment-settings", control.Admin(application.getPaymentSettings))
	mux.HandleFunc("PUT /api/v1/payment-settings", control.Admin(application.putPaymentSettings))
	return application, nil
}

// RunBackground is started only in server mode, never during restore or local
// account commands. It returns after all workers stop, before closing SQL.
func (a *App) RunBackground(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Go(func() { a.Alerts.Run(ctx) })
	workers.Go(func() { a.Platform.RunProbeHistory(ctx) })
	workers.Go(func() { a.Platform.RunOperationLoop(ctx) })
	workers.Go(func() { _ = a.Platform.RunTaskLoop(ctx) })
	workers.Go(func() { _ = a.Commerce.RunReconciliation(ctx) })
	workers.Go(func() { _ = a.Commerce.RunAutoRenewLoop(ctx) })
	workers.Go(func() { _ = a.Commerce.RunWebhookLoop(ctx) })
	workers.Wait()
}
