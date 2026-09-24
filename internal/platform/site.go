package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/httporigin"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

// DefaultGeoLookupURL 是位置图标默认使用的地区查询服务：返回 JSON、无需密钥。
// 运营方可以在站点设置里换成自己的服务，或留空整个关掉。
const DefaultGeoLookupURL = "https://ipwho.is/{ip}"

func defaultSite() contract.SiteSettings {
	return contract.SiteSettings{Name: "流量控制台", Registration: "closed", Accent: "blue", PaymentsEnabled: true, Currency: contract.SettlementCurrency, MinimumRecharge: "1.00", MaximumRecharge: "1000000.00", DiagnosticsEnabled: true, DiagnosticsPerMinute: 6, GeoLookupURL: DefaultGeoLookupURL}
}
func (s *Server) SiteSettings(ctx context.Context) (contract.SiteSettings, error) {
	v := defaultSite()
	var raw string
	err := s.Store.DB.QueryRowContext(ctx, "SELECT payload FROM cp_site_settings WHERE id=1").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err == nil {
		err = json.Unmarshal([]byte(raw), &v)
	}
	// 旧文档里没有 currency，也可能只有按分写的老字段；统一在这里补齐，
	// 调用方拿到的永远是同一套单位。
	v.Normalize()
	return v, err
}
func (s *Server) site(w http.ResponseWriter, r *http.Request) {
	v, err := s.SiteSettings(r.Context())
	if err != nil {
		fail(w, 503, "site settings unavailable")
		return
	}
	reply(w, 200, v)
}
func (s *Server) saveSite(w http.ResponseWriter, r *http.Request) {
	var v contract.SiteSettings
	if !decode(w, r, &v) {
		return
	}
	actor, _ := UserFromContext(r.Context())
	v.Normalize()
	minimum, maximum, amountErr := v.RechargeRange()
	if strings.TrimSpace(v.Name) == "" || len(v.Name) > 100 || len(v.Announcement) > 8000 || !contains([]string{"closed", "open", "invite"}, v.Registration) || !contains([]string{"blue", "teal", "violet", "magenta", "amber", "graphite"}, v.Accent) || amountErr != nil || maximum > contract.MaxAmountCents || v.DiagnosticsPerMinute < 1 || v.DiagnosticsPerMinute > 30 || !contract.ValidGeoLookupURL(v.GeoLookupURL) {
		fail(w, 400, "invalid site settings")
		return
	}
	// 存回去的是规范化之后的写法：界面上「1」和「1.0」都会变成「1.00」。
	v.MinimumRecharge = contract.FormatAmount(minimum)
	v.MaximumRecharge = contract.FormatAmount(maximum)
	v.GeoLookupURL = strings.TrimSpace(v.GeoLookupURL)
	expected := v.Version
	v.Version++
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		if expected == 0 {
			if _, err := tx.ExecContext(r.Context(), s.q("INSERT INTO cp_site_settings(id,payload,version) VALUES(1,?,?)"), strJSON(v), v.Version); err != nil {
				return err
			}
		} else {
			res, err := tx.ExecContext(r.Context(), s.q("UPDATE cp_site_settings SET payload=?,version=? WHERE id=1 AND version=?"), strJSON(v), v.Version, expected)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return errConflict
			}
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "site.update", "site")
	})
	if err != nil {
		fail(w, 409, "settings changed; reload before saving")
		return
	}
	// 查询地址换了就立刻生效，不用等下一次回读。
	s.refreshGeo(v.GeoLookupURL)
	reply(w, 200, v)
}

// PaymentAllowed 判断一笔充值是否落在站点允许的区间里。amount 是整数分：
// 站点设置对外是元，闸门这里仍然是分。
func (s *Server) PaymentAllowed(ctx context.Context, amount int64) error {
	v, e := s.SiteSettings(ctx)
	if e != nil {
		return e
	}
	minimum, maximum, e := v.RechargeRange()
	if e != nil {
		return e
	}
	if !v.PaymentsEnabled || amount < minimum || amount > maximum {
		return errors.New("recharge disabled or outside configured limits")
	}
	return nil
}

var digitPixels = []string{"111101101101111", "010110010010111", "111001111100111", "111001111001111", "101101111001001", "111100111001111", "111100111101111", "111001001001001", "111101111101111", "111101111001111"}

func (s *Server) captcha(w http.ResponseWriter, r *http.Request) {
	ip := httporigin.ClientIP(r, s.opts.TrustProxy)
	if !s.allow("captcha:"+ip, 15) {
		fail(w, 429, "rate limited")
		return
	}
	b := make([]byte, 6)
	if _, e := rand.Read(b); e != nil {
		fail(w, 500, "captcha unavailable")
		return
	}
	answer := ""
	canvas := image.NewRGBA(image.Rect(0, 0, 144, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 144; x++ {
			canvas.Set(x, y, color.RGBA{240, 244, 249, 255})
		}
	}
	for i, v := range b {
		d := int(v) % 10
		answer += string(rune('0' + d))
		pixels := digitPixels[d]
		for j, pixel := range pixels {
			if pixel == '1' {
				for y := 0; y < 5; y++ {
					for x := 0; x < 5; x++ {
						canvas.Set(5+i*23+(j%3)*5+x, 7+(j/3)*5+y, color.RGBA{27, 56, 88, 255})
					}
				}
			}
		}
	}
	key := token()
	err := s.Store.Write(r.Context(), storage.Normal, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(r.Context(), s.q("DELETE FROM cp_captchas WHERE expires_at<?"), time.Now().Unix()); e != nil {
			return e
		}
		var n int
		if e := tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM cp_captchas").Scan(&n); e != nil {
			return e
		}
		if n >= 10000 {
			return errors.New("captcha capacity reached")
		}
		_, e := tx.ExecContext(r.Context(), s.q("INSERT INTO cp_captchas(id,answer_hash,expires_at) VALUES(?,?,?)"), key, digest(key+":"+answer), time.Now().Add(3*time.Minute).Unix())
		return e
	})
	if err != nil {
		fail(w, 503, "captcha unavailable")
		return
	}
	var buf bytes.Buffer
	if png.Encode(&buf, canvas) != nil {
		fail(w, 500, "captcha unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	reply(w, 200, map[string]any{"id": key, "image": "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())})
}
func (s *Server) verifyCaptcha(ctx context.Context, key, answer string) bool {
	if len(key) != 64 || len(answer) != 6 {
		return false
	}
	valid := false
	err := s.Store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
		var hash string
		var expiry int64
		if e := tx.QueryRowContext(ctx, s.q("SELECT answer_hash,expires_at FROM cp_captchas WHERE id=?"), key).Scan(&hash, &expiry); e != nil {
			return e
		}
		res, e := tx.ExecContext(ctx, s.q("DELETE FROM cp_captchas WHERE id=?"), key)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		valid = n == 1 && expiry > time.Now().Unix() && hash == digest(key+":"+answer)
		return nil
	})
	return err == nil && valid
}
func (s *Server) registrationInvite(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	plain := token()
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(r.Context(), s.q("INSERT INTO cp_registration_invites(token_hash,expires_at,used_by) VALUES(?,?,'')"), digest(plain), time.Now().Add(7*24*time.Hour).Unix()); e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "registration.invite", digest(plain))
	})
	if err != nil {
		fail(w, 500, "invite unavailable")
		return
	}
	reply(w, 201, map[string]string{"code": plain})
}
func (s *Server) registerUser(w http.ResponseWriter, r *http.Request) {
	if !s.csrf(r) {
		fail(w, 403, "invalid request origin")
		return
	}
	ip := httporigin.ClientIP(r, s.opts.TrustProxy)
	if !s.allow("registration:"+ip, 5) {
		fail(w, 429, "rate limited")
		return
	}
	var in struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		Invite        string `json:"invite"`
		CaptchaID     string `json:"captcha_id"`
		CaptchaAnswer string `json:"captcha_answer"`
	}
	if !decode(w, r, &in) {
		return
	}
	settings, e := s.SiteSettings(r.Context())
	if e != nil || settings.Registration == "closed" {
		fail(w, 403, "registration closed")
		return
	}
	if settings.Captcha && !s.verifyCaptcha(r.Context(), in.CaptchaID, in.CaptchaAnswer) {
		fail(w, 400, "invalid or expired captcha")
		return
	}
	in.Username = strings.ToLower(strings.TrimSpace(in.Username))
	if len(in.Username) < 1 || len(in.Username) > 100 || len(in.Password) < 12 || len(in.Password) > 72 {
		fail(w, 400, "invalid username or password (12-72 bytes)")
		return
	}
	hash, e := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if e != nil {
		fail(w, 400, "invalid password")
		return
	}
	u := contract.User{ID: id(), Username: in.Username, Role: "user"}
	u.IdentityGroupID = id()
	e = s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		// Recheck policy after password hashing, including concurrent closure.
		q := "SELECT payload FROM cp_site_settings WHERE id=1"
		if s.Store.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		var raw string
		if err := tx.QueryRowContext(r.Context(), q).Scan(&raw); err != nil {
			return err
		}
		var current contract.SiteSettings
		if err := json.Unmarshal([]byte(raw), &current); err != nil {
			return err
		}
		if current.Version != settings.Version || current.Registration == "closed" {
			return errConflict
		}
		if current.Registration == "invite" {
			res, err := tx.ExecContext(r.Context(), s.q("UPDATE cp_registration_invites SET used_by=? WHERE token_hash=? AND used_by='' AND expires_at>?"), u.ID, digest(in.Invite), time.Now().Unix())
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return errors.New("invalid invitation")
			}
		}
		if _, err := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_identity_groups(id,name) VALUES(?,?)`), u.IdentityGroupID, u.Username+" 默认组"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(), s.q("INSERT INTO cp_users(id,username,password_hash,role,identity_group_id,disabled) VALUES(?,?,?,'user',?,0)"), u.ID, u.Username, string(hash), u.IdentityGroupID); err != nil {
			return err
		}
		return s.AuditTx(r.Context(), tx, u.ID, "user.register", u.ID)
	})
	if e != nil {
		fail(w, 409, "registration rejected; check username, invitation and current policy")
		return
	}
	reply(w, 201, u)
}
