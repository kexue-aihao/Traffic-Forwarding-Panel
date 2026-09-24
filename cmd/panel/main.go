package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/app"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/webui"
)

// Version is injected at release build time using -ldflags -X.
var Version = "0.1.6"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	opts, err := readOptions(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if opts.ShowVersion {
		fmt.Println(Version)
		return nil
	}
	if opts.ExportHTML != "" {
		return webui.Export(opts.ExportHTML)
	}
	if opts.CheckHealth {
		return healthcheckTLS(opts.Listen, opts.TLSCert)
	}
	var certificate tls.Certificate
	if opts.TLSCert != "" {
		certificate, err = tls.LoadX509KeyPair(opts.TLSCert, opts.TLSKey)
		if err != nil {
			return errors.New("无法加载 tls-cert / tls-key，请检查证书和私钥")
		}
	}
	if opts.HTMLPath != "" {
		if err := webui.ValidateDirectory(opts.HTMLPath); err != nil {
			return err
		}
	}
	if opts.CheckConfig {
		fmt.Println("面板配置校验通过；未连接数据库。")
		return nil
	}
	addr, driver, dsn, origin, trustProxy := &opts.Listen, &opts.Driver, &opts.DSN, &opts.Origin, &opts.TrustProxy
	initAdmin, resetPassword, paymentsFile, agentDir := &opts.InitAdmin, &opts.ResetPassword, &opts.PaymentsFile, &opts.AgentDir
	backupPath, restorePath := &opts.BackupPath, &opts.RestorePath
	if *driver == "sqlite" && !strings.Contains(*dsn, "?") && !strings.HasPrefix(*dsn, "file:") {
		if err := os.MkdirAll(filepath.Dir(*dsn), 0700); err != nil {
			return err
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store, err := storage.OpenWithOptions(ctx, *driver, *dsn, storage.Options{MaxOpenConnections: opts.MaxOpenConnection, MaxIdleConnections: opts.MaxIdleConnection, DisableQueue: opts.DisableQueue})
	if err != nil {
		return errors.New("无法打开数据库，请检查数据库地址、连接权限与服务状态")
	}
	defer store.Close()
	gateway := payment.EPay{Gateway: os.Getenv("TFP_EPAY_GATEWAY"), PID: os.Getenv("TFP_EPAY_PID"), Key: os.Getenv("TFP_EPAY_KEY"), NotifyURL: os.Getenv("TFP_EPAY_NOTIFY_URL"), ReturnURL: os.Getenv("TFP_EPAY_RETURN_URL")}
	if gateway.NotifyURL == "" && *origin != "" {
		gateway.NotifyURL = *origin + "/api/v1/payments/epay/notify"
	}
	if gateway.ReturnURL == "" && *origin != "" {
		gateway.ReturnURL = *origin + "/#/commerce"
	}
	var channels map[string]commerce.Channel
	if *paymentsFile != "" {
		f, e := os.Open(*paymentsFile)
		if e != nil {
			return errors.New("cannot read payment configuration file")
		}
		channels, e = app.PaymentChannels(f, *origin)
		f.Close()
		if e != nil {
			return e
		}
	}
	application, err := app.New(ctx, store, app.Options{
		Origin: *origin, TrustProxy: *trustProxy, SecureCookies: strings.HasPrefix(*origin, "https://"), EPay: gateway, Channels: channels, AgentDir: *agentDir,
		HTMLPath: opts.HTMLPath, DisableGzip: opts.DisableGzip,
		OfflineNodeTime: time.Duration(opts.OfflineNodeTime) * time.Second, OfflineNodeRetention: time.Duration(opts.OfflineNodeRetentionTime) * time.Second,
		UserRateLimit: rateLimit(opts.UserRateLimit), DefaultRateLimit: rateLimit(opts.DefaultRateLimit),
	})
	if err != nil {
		return err
	}
	if *backupPath != "" {
		f, e := os.OpenFile(*backupPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		e = store.Export(ctx, f)
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Println("数据库快照已生成；恢复所需证书、支付配置和 Agent 状态需单独保管。")
		return nil
	}
	if *restorePath != "" {
		f, e := os.Open(*restorePath)
		if e != nil {
			return e
		}
		defer f.Close()
		if e = store.Import(ctx, f); e != nil {
			return e
		}
		fmt.Println("数据库已恢复。启动前核对节点状态、支付对账与配置版本。")
		return nil
	}
	if *initAdmin != "" || *resetPassword != "" {
		fmt.Fprintln(os.Stderr, "读取标准输入中的管理员密码（至少 12 字符），不写入配置或日志。")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return errors.New("administrator password is required on stdin")
		}
		if *resetPassword != "" {
			err = application.Platform.ResetPassword(ctx, *resetPassword, scanner.Text())
		} else {
			err = application.Platform.Bootstrap(ctx, *initAdmin, scanner.Text())
		}
		if err != nil {
			return err
		}
		fmt.Println("本机账号操作完成。")
		return nil
	}
	server := &http.Server{Addr: *addr, Handler: application.Handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	backgroundStopped := make(chan struct{})
	go func() {
		defer close(backgroundStopped)
		application.RunBackground(ctx)
	}()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		server.Shutdown(shutdownCtx)
	}()
	log.Printf("面板监听 %s；数据库=%s，用户入口 /，管理员入口 /admin", *addr, *driver)
	if opts.TLSCert != "" {
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
		err = server.ListenAndServeTLS("", "")
	} else {
		err = server.ListenAndServe()
	}
	cancel()
	<-stopped
	<-backgroundStopped
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
