package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
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
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func run() error {
	addr := flag.String("addr", env("TFP_ADDR", "127.0.0.1:8080"), "HTTP listen address")
	driver := flag.String("database", env("TFP_DATABASE", "sqlite"), "sqlite, postgres or mysql")
	dsn := flag.String("dsn", env("TFP_DSN", "data/panel.db"), "database DSN (prefer TFP_DSN for server credentials)")
	origin := flag.String("origin", env("TFP_ORIGIN", ""), "public scheme://host for reverse proxy and CSRF checks")
	initAdmin := flag.String("init-admin", "", "create first administrator then exit; reads password from stdin")
	resetPassword := flag.String("reset-password", "", "reset local account password and revoke sessions; reads password from stdin")
	paymentsFile := flag.String("payments", os.Getenv("TFP_PAYMENTS_FILE"), "operator-owned payment configuration JSON file")
	backupPath := flag.String("backup", "", "export a consistent sensitive database snapshot to a new JSONL file, then exit")
	restorePath := flag.String("restore", "", "restore JSONL into an empty database, then exit; stop panel before use")
	flag.Parse()
	commands := 0
	for _, v := range []string{*initAdmin, *resetPassword, *backupPath, *restorePath} {
		if v != "" {
			commands++
		}
	}
	if commands > 1 {
		return errors.New("select one local administrative command")
	}
	if *origin != "" {
		u, e := url.Parse(*origin)
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("origin must be a public http(s) scheme://host")
		}
		*origin = strings.TrimRight(*origin, "/")
	}
	if *driver == "sqlite" && !strings.Contains(*dsn, "?") && !strings.HasPrefix(*dsn, "file:") {
		if err := os.MkdirAll(filepath.Dir(*dsn), 0700); err != nil {
			return err
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store, err := storage.Open(ctx, *driver, *dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()
	gateway := payment.EPay{Gateway: os.Getenv("TFP_EPAY_GATEWAY"), PID: os.Getenv("TFP_EPAY_PID"), Key: os.Getenv("TFP_EPAY_KEY"), NotifyURL: os.Getenv("TFP_EPAY_NOTIFY_URL"), ReturnURL: os.Getenv("TFP_EPAY_RETURN_URL")}
	if gateway.NotifyURL == "" && *origin != "" {
		gateway.NotifyURL = *origin + "/api/v1/payments/epay/notify"
	}
	if gateway.ReturnURL == "" && *origin != "" {
		gateway.ReturnURL = *origin + "/#/wallet"
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
	application, err := app.New(ctx, store, app.Options{Origin: *origin, SecureCookies: strings.HasPrefix(*origin, "https://"), EPay: gateway, Channels: channels})
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
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		server.Shutdown(shutdownCtx)
	}()
	log.Printf("面板监听 %s；数据库=%s，用户入口 /，管理员入口 /admin", *addr, *driver)
	err = server.ListenAndServe()
	cancel()
	<-stopped
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
