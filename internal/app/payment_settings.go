package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/platform"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

// PaymentSettings 是面板上保存的支付通道配置。
//
// 启动参数里的配置文件（--payments-file）与环境变量只在第一次生效：那份配置会
// 被写进这里，之后以数据库为准。两边都能改的话，「面板上看到的到底是哪一份」
// 就永远说不清了。
type PaymentSettings struct {
	Version  int64                           `json:"version"`
	Channels map[string]PaymentConfiguration `json:"channels"`
}

// PaymentChannelView 是回给界面的通道配置。密钥只回一个「有没有」，绝不回明文；
// 保存时留空表示沿用已经存下来的那一把。
type PaymentChannelView struct {
	PaymentConfiguration
	Configured bool `json:"configured"`
	KeySet     bool `json:"key_set"`
}

type PaymentSettingsView struct {
	Version  int64                         `json:"version"`
	Channels map[string]PaymentChannelView `json:"channels"`
}

func migratePaymentSettings(ctx context.Context, store *storage.Store) error {
	return storage.MigrateNamespace(ctx, store.DB, store.Dialect, "payment_settings", 1, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cp_payment_settings(id BIGINT PRIMARY KEY,payload TEXT NOT NULL,version BIGINT NOT NULL)`)
		return err
	})
}

// loadPaymentSettings 读回保存过的通道配置。没有那一行就返回空的一份 —— 空表示
// 「面板上还没配过」，与「配了但一条通道都没有」是同一件事，不额外区分。
func loadPaymentSettings(ctx context.Context, store *storage.Store) (PaymentSettings, error) {
	settings := PaymentSettings{Channels: map[string]PaymentConfiguration{}}
	var raw string
	err := store.DB.QueryRowContext(ctx, store.Rebind(`SELECT payload,version FROM cp_payment_settings WHERE id=1`)).Scan(&raw, &settings.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return PaymentSettings{Channels: map[string]PaymentConfiguration{}}, nil
	}
	if err != nil {
		return settings, err
	}
	if err = json.Unmarshal([]byte(raw), &settings.Channels); err != nil {
		return settings, err
	}
	if settings.Channels == nil {
		settings.Channels = map[string]PaymentConfiguration{}
	}
	return settings, nil
}

// savePaymentSettings 落盘并返回新的版本号。版本号是乐观锁：两个管理员同时在改，
// 后一个会拿到冲突而不是把人家的改动覆盖掉。audit 与写入在同一个事务里 ——
// 改了通道却查不到是谁改的，比不改更糟。
func savePaymentSettings(ctx context.Context, store *storage.Store, expected int64, channels map[string]PaymentConfiguration, audit func(context.Context, *sql.Tx) error) (int64, error) {
	payload, err := json.Marshal(channels)
	if err != nil {
		return expected, err
	}
	next := expected + 1
	err = store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
		if expected == 0 {
			if _, err := tx.ExecContext(ctx, store.Rebind(`INSERT INTO cp_payment_settings(id,payload,version) VALUES(1,?,?)`), string(payload), next); err != nil {
				return err
			}
		} else {
			res, err := tx.ExecContext(ctx, store.Rebind(`UPDATE cp_payment_settings SET payload=?,version=? WHERE id=1 AND version=?`), string(payload), next, expected)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("payment settings changed")
			}
		}
		if audit == nil {
			return nil
		}
		return audit(ctx, tx)
	})
	if err != nil {
		return expected, err
	}
	return next, nil
}

// applyPaymentChannels 让新配置立刻生效：报价、下单与后台核对读的都是同一份。
// 构建失败时不动现有的通道 —— 存一半的配置比不存更糟。
func (a *App) applyPaymentChannels(configs map[string]PaymentConfiguration) error {
	channels, err := BuildPaymentChannels(configs, a.origin)
	if err != nil {
		return err
	}
	a.Commerce.SetChannels(channels)
	return nil
}

// paymentChannelNames 是面板支持的五种协议。名字会进回调路径与适配器工厂，所以
// 这里写死一份，不接受请求体自定义。
var paymentChannelNames = []string{"epay", "epusdt", "bepusdt", "tokenpay", "cryptomus"}

func (a *App) paymentSettingsView(ctx context.Context) (PaymentSettingsView, error) {
	stored, err := loadPaymentSettings(ctx, a.store)
	if err != nil {
		return PaymentSettingsView{}, err
	}
	view := PaymentSettingsView{Version: stored.Version, Channels: map[string]PaymentChannelView{}}
	// 五种协议一个不少地列出来：调用方不必自己维护这份清单，界面上没配过的那几条
	// 也能直接渲染成空表单。
	for _, name := range paymentChannelNames {
		cfg := stored.Channels[name]
		entry := PaymentChannelView{PaymentConfiguration: cfg, Configured: cfg.Gateway != "", KeySet: cfg.Key != ""}
		// 密钥不回明文，界面上只需要知道「已经有一把」。
		entry.Key = ""
		view.Channels[name] = entry
	}
	return view, nil
}

func (a *App) getPaymentSettings(w http.ResponseWriter, r *http.Request) {
	view, err := a.paymentSettingsView(r.Context())
	if err != nil {
		replyError(w, 503, "payment settings unavailable")
		return
	}
	replyJSON(w, 200, view)
}

func (a *App) putPaymentSettings(w http.ResponseWriter, r *http.Request) {
	var in PaymentSettingsView
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		replyError(w, 400, "invalid payment settings")
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		replyError(w, 400, "payment settings requires one JSON object")
		return
	}
	user, _ := platform.UserFromContext(r.Context())
	actor := user.ID
	stored, err := loadPaymentSettings(r.Context(), a.store)
	if err != nil {
		replyError(w, 503, "payment settings unavailable")
		return
	}
	next := map[string]PaymentConfiguration{}
	for name, cfg := range in.Channels {
		name = strings.TrimSpace(name)
		if !validChannelName(name) {
			replyError(w, 400, "unknown payment channel "+name)
			return
		}
		cfg.Key = strings.TrimSpace(cfg.Key)
		// 留空表示沿用已存下来的密钥：密文只在服务端，界面上根本拿不到它。
		if cfg.Key == "" {
			cfg.Key = stored.Channels[name].Key
		}
		cfg.Gateway = strings.TrimSpace(cfg.Gateway)
		cfg.MerchantID = strings.TrimSpace(cfg.MerchantID)
		cfg.CryptoCurrency = strings.TrimSpace(cfg.CryptoCurrency)
		cfg.SignatureAlgorithm = strings.TrimSpace(cfg.SignatureAlgorithm)
		cfg.EPayMode = strings.TrimSpace(cfg.EPayMode)
		cfg.Method = strings.TrimSpace(cfg.Method)
		cfg.NotifyURL = strings.TrimSpace(cfg.NotifyURL)
		cfg.ReturnURL = strings.TrimSpace(cfg.ReturnURL)
		cfg.FeePercent = strings.TrimSpace(cfg.FeePercent)
		cfg.FeeFixed = strings.TrimSpace(cfg.FeeFixed)
		cfg.Rate = strings.TrimSpace(cfg.Rate)
		if cfg.Gateway == "" {
			// 一条什么都没有的通道按「删掉」处理，而不是报错：界面上清空一条
			// 就等于不再使用它。
			continue
		}
		if cfg.Key == "" {
			replyError(w, 400, "payment key required for "+name)
			return
		}
		next[name] = cfg.PaymentConfiguration
	}
	// 先建一遍再存：校验不过就什么都不写，生效的还是原来那一份。
	if err := a.applyPaymentChannels(next); err != nil {
		replyError(w, 400, err.Error())
		return
	}
	_, err = savePaymentSettings(r.Context(), a.store, in.Version, next, func(ctx context.Context, tx *sql.Tx) error {
		return a.Platform.AuditTx(ctx, tx, actor, "payment.update", "payments")
	})
	if err != nil {
		// 版本冲突时把刚生效的那份收回去，内存与库里不能各说各话。
		_ = a.applyPaymentChannels(stored.Channels)
		replyError(w, 409, "settings changed; reload before saving")
		return
	}
	view, err := a.paymentSettingsView(r.Context())
	if err != nil {
		replyError(w, 503, "payment settings unavailable")
		return
	}
	replyJSON(w, 200, view)
}

// validChannelName 只认面板列出来的那几种协议：名字会进回调路径与适配器工厂，
// 不能让请求体自己决定。
func validChannelName(name string) bool {
	for _, known := range paymentChannelNames {
		if name == known {
			return true
		}
	}
	return false
}

func replyJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func replyError(w http.ResponseWriter, status int, message string) {
	replyJSON(w, status, contract.APIError{Code: http.StatusText(status), Error: message})
}
