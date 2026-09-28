package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

const resetMessage = "如果账号已绑定 Telegram 且机器人可用，验证码将发送到绑定的私聊。"

func (a *App) resetClient() *http.Client {
	if a.telegramClient != nil {
		return a.telegramClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (a *App) resetRequestAllowed(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if !a.Platform.CheckCSRF(r) || strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		replyError(w, http.StatusForbidden, "invalid request origin")
		return false
	}
	return true
}

func (a *App) requestPasswordReset(w http.ResponseWriter, r *http.Request) {
	if !a.resetRequestAllowed(w, r) {
		return
	}
	var in struct {
		Username string `json:"username"`
	}
	if !decodeJSONBody(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || len(in.Username) > 64 {
		replyError(w, 400, "invalid username")
		return
	}
	settings, err := loadPaymentSettings(r.Context(), a.store)
	if err != nil {
		replyError(w, 503, "password reset unavailable")
		return
	}
	if settings.Telegram.Disabled || settings.Telegram.BotToken == "" {
		replyJSON(w, 200, map[string]string{"message": resetMessage})
		return
	}
	var user, chat string
	err = a.store.DB.QueryRowContext(r.Context(), a.store.Rebind(`SELECT u.id,b.chat_id FROM cp_users u JOIN cp_telegram_bindings b ON b.user_id=u.id WHERE u.username=? AND u.disabled=0 ORDER BY b.updated_at DESC,b.chat_id LIMIT 1`), in.Username).Scan(&user, &chat)
	if errors.Is(err, sql.ErrNoRows) {
		replyJSON(w, 200, map[string]string{"message": resetMessage})
		return
	}
	if err != nil {
		replyError(w, 503, "password reset unavailable")
		return
	}
	now := time.Now().Unix()
	var last, window int64
	var sends int
	err = a.store.DB.QueryRowContext(r.Context(), a.store.Rebind("SELECT last_sent_at,window_started,sends FROM cp_telegram_password_resets WHERE user_id=?"), user).Scan(&last, &window, &sends)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		replyError(w, 503, "password reset unavailable")
		return
	}
	if now-last < 60 || (now-window < 3600 && sends >= 5) {
		replyJSON(w, 200, map[string]string{"message": resetMessage})
		return
	}
	n, err := rand.Int(rand.Reader, big.NewInt(100000000))
	if err != nil {
		replyError(w, 503, "password reset unavailable")
		return
	}
	code := fmt.Sprintf("%08d", n.Int64())
	hash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		replyError(w, 503, "password reset unavailable")
		return
	}
	issued := false
	err = a.store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		var last, window int64
		var sends int
		query := "SELECT last_sent_at,window_started,sends FROM cp_telegram_password_resets WHERE user_id=?"
		if a.store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		e := tx.QueryRowContext(r.Context(), a.store.Rebind(query), user).Scan(&last, &window, &sends)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if now-last < 60 || (now-window < 3600 && sends >= 5) {
			return nil
		}
		if now-window >= 3600 {
			window, sends = now, 0
		}
		if errors.Is(e, sql.ErrNoRows) {
			_, e = tx.ExecContext(r.Context(), a.store.Rebind("INSERT INTO cp_telegram_password_resets(user_id,code_hash,expires_at,last_sent_at,attempts,window_started,sends) VALUES(?,?,?,?,0,?,1)"), user, string(hash), now+600, now, now)
		} else {
			_, e = tx.ExecContext(r.Context(), a.store.Rebind("UPDATE cp_telegram_password_resets SET code_hash=?,expires_at=?,last_sent_at=?,attempts=0,window_started=?,sends=? WHERE user_id=?"), string(hash), now+600, now, window, sends+1, user)
		}
		issued = e == nil
		return e
	})
	if err != nil {
		replyError(w, 503, "password reset unavailable")
		return
	}
	if issued {
		message := "站点忘记密码验证码：" + code + "\n10 分钟内有效，仅供网站重置密码。若非本人操作，请忽略。"
		if telegramSend(r.Context(), a.resetClient(), settings.Telegram.BotToken, chat, message) != nil {
			_ = a.store.Write(context.WithoutCancel(r.Context()), storage.Critical, func(tx *sql.Tx) error {
				_, e := tx.ExecContext(context.WithoutCancel(r.Context()), a.store.Rebind("UPDATE cp_telegram_password_resets SET code_hash='' WHERE user_id=? AND code_hash=?"), user, string(hash))
				return e
			})
		}
	}
	replyJSON(w, 200, map[string]string{"message": resetMessage})
}

func (a *App) confirmPasswordReset(w http.ResponseWriter, r *http.Request) {
	if !a.resetRequestAllowed(w, r) {
		return
	}
	var in struct {
		Username string `json:"username"`
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if !decodeJSONBody(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || len(in.Username) > 64 || len(in.Code) != 8 || len(in.Password) < 12 || len(in.Password) > 72 {
		replyError(w, 400, "invalid reset details or password length")
		return
	}
	for _, digit := range in.Code {
		if digit < '0' || digit > '9' {
			replyError(w, 400, "invalid verification code")
			return
		}
	}
	settings, err := loadPaymentSettings(r.Context(), a.store)
	if err != nil {
		replyError(w, 503, "password reset unavailable")
		return
	}
	if settings.Telegram.Disabled || settings.Telegram.BotToken == "" {
		replyError(w, 400, "invalid or expired verification code")
		return
	}
	valid := false
	err = a.store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		var user, hash string
		var expires int64
		var attempts int
		query := `SELECT u.id,p.code_hash,p.expires_at,p.attempts FROM cp_users u JOIN cp_telegram_bindings b ON b.user_id=u.id JOIN cp_telegram_password_resets p ON p.user_id=u.id WHERE u.username=? AND u.disabled=0`
		if a.store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		e := tx.QueryRowContext(r.Context(), a.store.Rebind(query), in.Username).Scan(&user, &hash, &expires, &attempts)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if hash == "" || expires <= time.Now().Unix() || attempts >= 5 {
			return nil
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Code)) != nil {
			_, e = tx.ExecContext(r.Context(), a.store.Rebind("UPDATE cp_telegram_password_resets SET attempts=attempts+1 WHERE user_id=?"), user)
			return e
		}
		next, e := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
		if e != nil {
			return e
		}
		result, e := tx.ExecContext(r.Context(), a.store.Rebind("UPDATE cp_users SET password_hash=? WHERE id=? AND disabled=0"), string(next), user)
		if e != nil {
			return e
		}
		rows, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if rows != 1 {
			return errors.New("account changed")
		}
		for _, table := range []string{"cp_sessions", "cp_tokens", "cp_telegram_login_tokens"} {
			if _, e = tx.ExecContext(r.Context(), a.store.Rebind("DELETE FROM "+table+" WHERE user_id=?"), user); e != nil {
				return e
			}
		}
		if _, e = tx.ExecContext(r.Context(), a.store.Rebind("UPDATE cp_telegram_password_resets SET code_hash='',expires_at=0,attempts=0 WHERE user_id=?"), user); e != nil {
			return e
		}
		if e = a.Platform.AuditTx(r.Context(), tx, user, "user.password.reset.telegram", user); e != nil {
			return e
		}
		valid = true
		return nil
	})
	if err != nil {
		replyError(w, 503, "password reset unavailable")
		return
	}
	if !valid {
		replyError(w, 400, "invalid or expired verification code")
		return
	}
	w.WriteHeader(204)
}
