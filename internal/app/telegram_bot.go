package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/platform"
	"html"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

type telegramUpdate struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		From struct {
			ID    int64 `json:"id"`
			IsBot bool  `json:"is_bot"`
		} `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
}

type telegramUpdatesResponse struct {
	OK     bool             `json:"ok"`
	Result []telegramUpdate `json:"result"`
}

// RunTelegramBot keeps the configured bot alive. Configuration is read from
// the database each poll, so changing the token or addresses takes effect
// without restarting the panel.
func (a *App) RunTelegramBot(ctx context.Context) {
	client := &http.Client{Timeout: 35 * time.Second}
	offset := 0
	botID := ""
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		settings, err := loadPaymentSettings(ctx, a.store)
		if err != nil || settings.Telegram.Disabled || strings.TrimSpace(settings.Telegram.BotToken) == "" {
			if !sleepContext(ctx, 10*time.Second) {
				return
			}
			continue
		}
		currentBot := strings.SplitN(settings.Telegram.BotToken, ":", 2)[0]
		if botID != currentBot {
			var saved int
			err := a.store.DB.QueryRowContext(ctx, a.store.Rebind("SELECT update_offset FROM cp_telegram_offsets WHERE bot_id=?"), currentBot).Scan(&saved)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				if !sleepContext(ctx, time.Second) {
					return
				}
				continue
			}
			botID, offset = currentBot, saved
		}
		updates, err := telegramGetUpdates(ctx, client, settings.Telegram.BotToken, offset)
		if err != nil {
			if !sleepContext(ctx, 5*time.Second) {
				return
			}
			continue
		}
		for _, update := range updates {
			if update.UpdateID < offset {
				continue
			}
			if telegramPrivateUpdate(update) {
				a.handleTelegramMessage(ctx, client, settings.Telegram, strconv.FormatInt(update.Message.Chat.ID, 10), update.Message.Text, botID+"-"+strconv.Itoa(update.UpdateID))
			}
			next := update.UpdateID + 1
			err = a.store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
				query := "INSERT INTO cp_telegram_offsets(bot_id,update_offset) VALUES(?,?) ON CONFLICT(bot_id) DO UPDATE SET update_offset=excluded.update_offset"
				if a.store.Dialect == "mysql" {
					query = "INSERT INTO cp_telegram_offsets(bot_id,update_offset) VALUES(?,?) ON DUPLICATE KEY UPDATE update_offset=VALUES(update_offset)"
				}
				_, e := tx.ExecContext(ctx, a.store.Rebind(query), botID, next)
				return e
			})
			if err != nil {
				break
			}
			offset = next
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func telegramGetUpdates(ctx context.Context, client *http.Client, token string, offset int) ([]telegramUpdate, error) {
	u := "https://api.telegram.org/bot" + url.PathEscape(token) + "/getUpdates?timeout=25&allowed_updates=%5B%22message%22%5D&offset=" + strconv.Itoa(offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("telegram getUpdates status %s", resp.Status)
	}
	var out telegramUpdatesResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, errors.New("telegram getUpdates rejected")
	}
	return out.Result, nil
}

func telegramSend(ctx context.Context, client *http.Client, token, chatID, text string) error {
	body, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text, "disable_web_page_preview": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+url.PathEscape(token)+"/sendMessage", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("telegram sendMessage status %s", resp.Status)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result); err != nil || !result.OK {
		return errors.New("telegram sendMessage rejected")
	}
	return nil
}

func telegramTokenHash(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func (a *App) handleTelegramMessage(ctx context.Context, client *http.Client, settings TelegramSettings, chatID, text, updateKey string) {
	parts := strings.Fields(strings.TrimSpace(text))
	if len(parts) == 0 {
		return
	}
	command := strings.ToLower(strings.SplitN(parts[0], "@", 2)[0])
	send := func(message string) { _ = telegramSend(ctx, client, settings.BotToken, chatID, message) }
	switch command {
	case "/start", "/help":
		send("可用命令：\n/login 生成一次性登录链接并绑定账号\n/balance 查询账户余额\n/traffic 查询套餐流量\n/pay <网络> <金额> 发起充值\n支持：trc20-usdt、erc20-usdt、bep20-usdt、polygon-usdt、trx、pol、eth、bnb")
	case "/login":
		raw, err := storage.RandomKey()
		if err != nil {
			send("暂时无法生成登录链接，请稍后重试。")
			return
		}
		expires := time.Now().Add(10 * time.Minute)
		err = a.store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
			var user string
			e := tx.QueryRowContext(ctx, a.store.Rebind("SELECT user_id FROM cp_telegram_bindings WHERE chat_id=?"), chatID).Scan(&user)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if _, e = tx.ExecContext(ctx, a.store.Rebind("DELETE FROM cp_telegram_login_tokens WHERE chat_id=? OR expires_at<=?"), chatID, time.Now().Unix()); e != nil {
				return e
			}
			_, e = tx.ExecContext(ctx, a.store.Rebind("INSERT INTO cp_telegram_login_tokens(token_hash,chat_id,expires_at,used,user_id) VALUES(?,?,?,0,?)"), telegramTokenHash(raw), chatID, expires.Unix(), user)
			return e
		})
		if err != nil {
			send("暂时无法生成登录链接，请稍后重试。")
			return
		}
		link := strings.TrimRight(a.origin, "/") + "/api/v1/telegram/login?token=" + url.QueryEscape(raw)
		send("登录链接（10 分钟内有效且只能使用一次）：\n" + link)
	case "/pay":
		a.handleTelegramPay(ctx, client, settings, chatID, parts[1:], updateKey, send)
	case "/balance", "/traffic":
		if len(parts) != 1 {
			send("此命令不需要参数。")
			return
		}
		send(a.telegramAccountInfo(ctx, chatID, command))
	default:
		send("未知命令。发送 /help 查看用法。")
	}
}

func (a *App) telegramAccountInfo(ctx context.Context, chatID, command string) string {
	var user, username string
	err := a.store.DB.QueryRowContext(ctx, a.store.Rebind("SELECT u.id,u.username FROM cp_telegram_bindings b JOIN cp_users u ON u.id=b.user_id WHERE b.chat_id=? AND u.disabled=0"), chatID).Scan(&user, &username)
	if errors.Is(err, sql.ErrNoRows) {
		return "请先发送 /login 完成站点账号绑定。"
	}
	if err != nil {
		return "账户信息暂时不可用，请稍后重试。"
	}
	if command == "/balance" {
		wallet, err := a.Commerce.Wallet(ctx, user)
		if err != nil {
			return "余额暂时不可用，请稍后重试。"
		}
		return fmt.Sprintf("账号：%s\n账户余额：%s 元（CNY）", username, contract.FormatAmount(wallet.Balance))
	}
	entitlement, err := a.Commerce.Entitlement(ctx, user)
	if errors.Is(err, sql.ErrNoRows) {
		return "账号：" + username + "\n暂无套餐流量。"
	}
	if err != nil {
		return "流量信息暂时不可用，请稍后重试。"
	}
	status := "有效"
	if !time.Now().Before(entitlement.ExpiresAt) {
		status = "已到期"
	}
	quota := formatTelegramBytes(entitlement.Quota)
	if entitlement.Quota == 0 {
		quota = "不限量"
	}
	return fmt.Sprintf("账号：%s\n套餐状态：%s\n已用流量：%s\n套餐流量：%s\n到期时间：%s", username, status, formatTelegramBytes(entitlement.Used), quota, entitlement.ExpiresAt.Format("2006-01-02 15:04 MST"))
}

func formatTelegramBytes(value int64) string {
	const gib = 1024 * 1024 * 1024
	return fmt.Sprintf("%.2f GiB", float64(value)/gib)
}

func (a *App) telegramBoundUser(ctx context.Context, chatID string) (string, error) {
	var user string
	err := a.store.DB.QueryRowContext(ctx, a.store.Rebind(`SELECT user_id FROM cp_telegram_bindings WHERE chat_id=?`), chatID).Scan(&user)
	return user, err
}

func (a *App) handleTelegramPay(ctx context.Context, client *http.Client, settings TelegramSettings, chatID string, args []string, updateKey string, send func(string)) {
	if len(args) < 1 || len(args) > 2 {
		send("用法：/pay <网络> <金额>，例如 /pay trc20-usdt 100")
		return
	}
	network := strings.ToLower(strings.TrimSpace(args[0]))
	if !validTelegramNetwork(network) {
		send("不支持该网络。可选：" + strings.Join(TelegramNetworks, ", "))
		return
	}
	address := strings.TrimSpace(settings.WalletAddresses[network])
	if address == "" {
		send("管理员尚未配置 " + network + " 收款地址。")
		return
	}
	if len(args) == 1 {
		send("网络：" + network + "\n收款地址：\n" + address + "\n请继续发送 /pay " + network + " <金额> 创建充值订单。")
		return
	}
	amount, err := parseTelegramAmount(args[1])
	if err != nil || amount <= 0 || amount > contract.MaxAmountCents {
		send("金额无效，请输入 0.01 至 1000000.00 之间的金额。")
		return
	}
	user, err := a.telegramBoundUser(ctx, chatID)
	if err != nil {
		send("请先发送 /login 完成站点账号绑定。")
		return
	}
	coinAmount, err := telegramCoinAmount(amount, settings.Rates[network], network)
	if err != nil {
		send("管理员尚未配置该网络汇率，请联系管理员。金额单位为站点余额人民币元。")
		return
	}
	order, err := a.Commerce.CreateManualOrderAtomic(ctx, user, "telegram-"+network, "tg-"+updateKey, amount, address, func(tx *sql.Tx, order commerce.Order) error {
		_, e := tx.ExecContext(ctx, a.store.Rebind("INSERT INTO cp_telegram_payment_intents(id,user_id,chat_id,network,amount,address,status,created_at,transaction_id,coin_amount) VALUES(?,?,?,?,?,?,'pending',?,'',?)"), order.ID, user, chatID, network, amount, address, time.Now().Unix(), coinAmount)
		return e
	})
	if err != nil {
		send("充值订单创建失败，请稍后重试。")
		return
	}
	if err := a.store.DB.QueryRowContext(ctx, a.store.Rebind("SELECT coin_amount FROM cp_telegram_payment_intents WHERE id=?"), order.ID).Scan(&coinAmount); err != nil {
		send("订单已创建，但读取报价失败，请联系管理员，暂勿转账。")
		return
	}
	send(fmt.Sprintf(`充值订单 %s
网络：%s
充值余额：%s 元（CNY）
应转币数量：%s
收款地址：
%s
请严格使用指定网络并提供交易哈希。管理员核实实际到账后入账，请勿重复转账。`, order.ID, network, contract.FormatAmount(amount), coinAmount, address))
}

var telegramBotTokenPattern = regexp.MustCompile("^[0-9]+:[A-Za-z0-9_-]{20,200}$")
var telegramRatePattern = regexp.MustCompile("^[0-9]{1,12}([.][0-9]{1,12})?$")
var telegramAmountPattern = regexp.MustCompile("^[0-9]{1,7}([.][0-9]{1,2})?$")

func telegramPrivateUpdate(u telegramUpdate) bool {
	return u.Message != nil && u.Message.Chat.Type == "private" && u.Message.Chat.ID > 0 && u.Message.From.ID == u.Message.Chat.ID && !u.Message.From.IsBot
}

// Rates are operator supplied CNY per coin. Round up to on-chain precision.
func telegramCoinAmount(amount int64, rate, network string) (string, error) {
	if !telegramRatePattern.MatchString(rate) || !validTelegramNetwork(network) || amount <= 0 {
		return "", errors.New("invalid rate")
	}
	r, ok := new(big.Rat).SetString(rate)
	if !ok || r.Sign() <= 0 {
		return "", errors.New("invalid rate")
	}
	precision := 18
	if network == "trc20-usdt" || network == "erc20-usdt" || network == "polygon-usdt" || network == "trx" {
		precision = 6
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(precision)), nil)
	coins := new(big.Rat).Quo(new(big.Rat).SetFrac64(amount, 100), r)
	coins.Mul(coins, new(big.Rat).SetInt(scale))
	q, rem := new(big.Int).QuoRem(coins.Num(), coins.Denom(), new(big.Int))
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return new(big.Rat).SetFrac(q, scale).FloatString(precision), nil
}

func parseTelegramAmount(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if !telegramAmountPattern.MatchString(value) {
		return 0, errors.New("invalid amount")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || len(parts[0]) > 12 || (len(parts) == 2 && len(parts[1]) > 2) {
		return 0, errors.New("invalid amount")
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > 1000000 {
		return 0, errors.New("invalid amount")
	}
	frac := int64(0)
	if len(parts) == 2 {
		frac, err = strconv.ParseInt(parts[1]+strings.Repeat("0", 2-len(parts[1])), 10, 64)
		if err != nil {
			return 0, err
		}
	}
	return whole*100 + frac, nil
}

// Confirmation, replay protection, audit and wallet accounting share one transaction.
func (a *App) confirmTelegramPayment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TransactionID string `json:"transaction_id"`
	}
	if !decodeJSONBody(w, r, &in) {
		return
	}
	in.TransactionID = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(in.TransactionID), "0x"))
	hash, err := hex.DecodeString(in.TransactionID)
	if err != nil || len(hash) != 32 {
		replyError(w, 400, "a 32-byte on-chain transaction hash is required")
		return
	}
	actor, _ := platform.UserFromContext(r.Context())
	var user, network string
	var amount int64
	err = a.store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		query := "SELECT user_id,network,amount,status,transaction_id FROM cp_telegram_payment_intents WHERE id=?"
		if a.store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		var status, previous string
		if e := tx.QueryRowContext(r.Context(), a.store.Rebind(query), r.PathValue("id")).Scan(&user, &network, &amount, &status, &previous); e != nil {
			return e
		}
		if status == "paid" {
			if previous == in.TransactionID {
				return nil
			}
			return errors.New("payment already finalized")
		}
		if status != "pending" {
			return errors.New("payment is not pending")
		}
		chain := map[string]string{"trc20-usdt": "tron", "trx": "tron", "erc20-usdt": "ethereum", "eth": "ethereum", "bep20-usdt": "bsc", "bnb": "bsc", "polygon-usdt": "polygon", "pol": "polygon"}[network]
		if _, e := tx.ExecContext(r.Context(), a.store.Rebind("INSERT INTO cp_telegram_receipts(receipt_id,order_id) VALUES(?,?)"), chain+":"+in.TransactionID, r.PathValue("id")); e != nil {
			return e
		}
		if e := a.Commerce.ConfirmPaymentTx(r.Context(), tx, "telegram-"+network, r.PathValue("id"), in.TransactionID, amount); e != nil {
			return e
		}
		if _, e := tx.ExecContext(r.Context(), a.store.Rebind("UPDATE cp_telegram_payment_intents SET status='paid',transaction_id=? WHERE id=?"), in.TransactionID, r.PathValue("id")); e != nil {
			return e
		}
		return a.Platform.AuditTx(r.Context(), tx, actor.ID, "telegram.payment.confirm", r.PathValue("id"))
	})
	if err != nil {
		replyError(w, 409, "payment confirmation failed or transaction already used")
		return
	}
	replyJSON(w, 200, map[string]any{"id": r.PathValue("id"), "user_id": user, "network": network, "amount_cents": amount, "status": "paid"})
}

func (a *App) listTelegramPayments(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.DB.QueryContext(r.Context(), "SELECT id,user_id,network,amount,address,status,created_at,transaction_id,coin_amount FROM cp_telegram_payment_intents ORDER BY created_at DESC,id LIMIT 100")
	if err != nil {
		replyError(w, 503, "payments unavailable")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, user, network, address, status, transaction, coin string
		var amount, created int64
		if err := rows.Scan(&id, &user, &network, &amount, &address, &status, &created, &transaction, &coin); err != nil {
			replyError(w, 503, "payments unavailable")
			return
		}
		items = append(items, map[string]any{"id": id, "user_id": user, "network": network, "address": address, "status": status, "amount_cents": amount, "created_at": created, "transaction_id": transaction, "coin_amount": coin})
	}
	if rows.Err() != nil {
		replyError(w, 503, "payments unavailable")
		return
	}
	replyJSON(w, 200, map[string]any{"items": items})
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || !errors.Is(d.Decode(new(any)), io.EOF) {
		replyError(w, 400, "one valid JSON object required")
		return false
	}
	return true
}

// First binding requires the regular authenticated session (including captcha).
// Later links issue a session for the snapshotted bound user, on POST only.
func (a *App) telegramLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	raw := strings.TrimSpace(r.URL.Query().Get("token"))
	if len(raw) != 64 {
		http.Error(w, "invalid login token", 410)
		return
	}
	var chat, user string
	var used int
	err := a.store.DB.QueryRowContext(r.Context(), a.store.Rebind("SELECT chat_id,user_id,used FROM cp_telegram_login_tokens WHERE token_hash=? AND expires_at>?"), telegramTokenHash(raw), time.Now().Unix()).Scan(&chat, &user, &used)
	if err != nil || used != 0 {
		http.Error(w, "login link expired or already used", 410)
		return
	}
	authenticated, authErr := a.Platform.Authenticate(r)
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><meta name=viewport content='width=device-width'><title>Telegram 登录</title><h1>Telegram 一次性登录</h1>")
		if user == "" && (authErr != nil || r.Header.Get("Authorization") != "") {
			fmt.Fprint(w, "<p>首次绑定请先在站点完成正常登录（含验证码），然后返回此页面刷新并确认绑定。不要转发本链接。</p><a href='/' target='_blank' rel='noreferrer'>打开站点登录</a>")
			return
		}
		label := "确认一次性登录"
		if user == "" {
			label = "确认绑定账号 " + authenticated.Username
		}
		fmt.Fprintf(w, "<form method='post'><input type='hidden' name='token' value='%s'><button>%s</button></form>", html.EscapeString(raw), html.EscapeString(label))
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if r.Header.Get("Origin") != strings.TrimRight(a.origin, "/") || a.origin == "" {
		http.Error(w, "invalid origin", 403)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.ParseForm() != nil || r.PostForm.Get("token") != raw {
		http.Error(w, "invalid form", 400)
		return
	}
	binding := user == ""
	if binding {
		if authErr != nil || r.Header.Get("Authorization") != "" {
			http.Error(w, "sign in to the site first", 401)
			return
		}
		user = authenticated.ID
	}
	var session string
	var expires time.Time
	err = a.store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(r.Context(), a.store.Rebind("UPDATE cp_telegram_login_tokens SET used=1 WHERE token_hash=? AND used=0 AND expires_at>?"), telegramTokenHash(raw), time.Now().Unix())
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil || n != 1 {
			return errors.New("token already consumed")
		}
		if binding {
			if _, e = tx.ExecContext(r.Context(), a.store.Rebind("INSERT INTO cp_telegram_bindings(chat_id,user_id,updated_at) VALUES(?,?,?)"), chat, user, time.Now().Unix()); e != nil {
				return e
			}
		} else {
			var bound string
			if e = tx.QueryRowContext(r.Context(), a.store.Rebind("SELECT user_id FROM cp_telegram_bindings WHERE chat_id=?"), chat).Scan(&bound); e != nil || bound != user {
				return errors.New("binding changed")
			}
		}
		session, expires, e = a.Platform.CreateSessionTx(r.Context(), tx, user)
		if e != nil {
			return e
		}
		return a.Platform.AuditTx(r.Context(), tx, user, "telegram.login", chat)
	})
	if err != nil {
		http.Error(w, "login link unavailable", 410)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "tfp_session", Value: session, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(a.origin, "https://"), SameSite: http.SameSiteStrictMode, Expires: expires})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
