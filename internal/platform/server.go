package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/httporigin"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

type Entitlements interface {
	Allocate(context.Context, *sql.Tx, string, string, string) (*contract.Lease, error)
}
type Options struct {
	Origin               string
	SecureCookies        bool
	TrustProxy           bool
	OfflineNodeTime      time.Duration
	OfflineNodeRetention time.Duration
	UserRateLimit        *RateLimit
	DefaultRateLimit     *RateLimit
	AdminTestBytes       int64
	Entitlements         Entitlements
	ResourceLimits       func(context.Context, *sql.Tx, string) (contract.ResourceLimits, error)
	// ActiveEntitlement 报告用户是否有未过期的权益。探针是付费能力，普通用户
	// 要有它才能看 —— 用函数字段而不是扩充 Entitlements 接口，与旁边的
	// LeaseCurrent / RetireLease 保持同一种写法。
	ActiveEntitlement func(context.Context, string) (bool, error)
	LeaseCurrent      func(context.Context, *sql.Tx, string, *contract.Lease) (bool, error)
	RetireLease       func(context.Context, *sql.Tx, string, string, int64) error
}
type Server struct {
	Store       *storage.Store
	opts        Options
	mu          sync.RWMutex
	probes      map[string]contract.Probe
	lastContact map[string]time.Time
	limits      map[string]limit
	history     *probeHistory
	taskMu      sync.Mutex
	terminalMu  sync.Mutex
	terminals   map[string]*terminalBridge
	geo         *geoCache
}
type limit struct {
	since  time.Time
	count  int
	period time.Duration
}
type userKey struct{}

func New(s *storage.Store, o Options) *Server {
	return &Server{Store: s, opts: o, probes: map[string]contract.Probe{}, lastContact: map[string]time.Time{}, limits: map[string]limit{}, history: newProbeHistory(time.Now()), terminals: map[string]*terminalBridge{}, geo: newGeoCache()}
}
func UserFromContext(ctx context.Context) (contract.User, bool) {
	u, ok := ctx.Value(userKey{}).(contract.User)
	return u, ok
}
func token() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}

func generateUserPassword() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const segmentLength = 8
	const segmentCount = 4
	const unbiasedLimit = 256 - 256%len(alphabet)

	password := make([]byte, segmentCount*segmentLength+segmentCount-1)
	random := make([]byte, 1)
	position := 0
	for segment := 0; segment < segmentCount; segment++ {
		if segment > 0 {
			password[position] = '-'
			position++
		}
		for i := 0; i < segmentLength; i++ {
			for {
				if _, err := rand.Read(random); err != nil {
					return "", err
				}
				if int(random[0]) < unbiasedLimit {
					password[position] = alphabet[int(random[0])%len(alphabet)]
					position++
					break
				}
			}
		}
	}
	return string(password), nil
}

func digest(s string) string        { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func id() string                    { return token()[:32] }
func (s *Server) q(q string) string { return s.Store.Rebind(q) }
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}
func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, contract.APIError{Code: http.StatusText(status), Error: msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		fail(w, 400, "invalid JSON")
		return false
	}
	if d.Decode(&struct{}{}) != io.EOF {
		fail(w, 400, "one JSON value required")
		return false
	}
	return true
}
func (s *Server) csrf(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return true
	}
	if r.Method == "GET" || r.Method == "HEAD" {
		return true
	}
	if r.Header.Get("X-Requested-With") != "fetch" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	} // non-browser CLI with explicit custom header
	expected := s.opts.Origin
	if expected == "" {
		expected = httporigin.Scheme(r, s.opts.TrustProxy) + "://" + r.Host
	}
	u, e := url.Parse(origin)
	return e == nil && u.Scheme != "" && strings.TrimRight(origin, "/") == strings.TrimRight(expected, "/")
}
func (s *Server) Authenticate(r *http.Request) (contract.User, error) {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return s.bearerUser(r)
	}
	var u contract.User
	c, e := r.Cookie("tfp_session")
	if e != nil {
		return u, errors.New("authentication required")
	}
	var disabled int
	e = s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT u.id,u.username,u.role,u.identity_group_id,u.disabled FROM cp_users u JOIN cp_sessions a ON a.user_id=u.id WHERE a.token_hash=? AND a.expires_at>?`), digest(c.Value), time.Now().Unix()).Scan(&u.ID, &u.Username, &u.Role, &u.IdentityGroupID, &disabled)
	u.Disabled = disabled != 0
	if e != nil || u.Disabled {
		return contract.User{}, errors.New("authentication required")
	}
	return u, nil
}
func (s *Server) RequireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, e := s.Authenticate(r)
		if e != nil {
			fail(w, 401, "authentication required")
			return
		}
		if !s.csrf(r) {
			fail(w, 403, "invalid request origin or CSRF header")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	}
}
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return s.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFromContext(r.Context())
		if u.Role != "admin" {
			fail(w, 403, "administrator required")
			return
		}
		next(w, r)
	})
}
func (s *Server) AuditTx(ctx context.Context, tx *sql.Tx, user, action, target string) error {
	_, e := tx.ExecContext(ctx, s.q(`INSERT INTO cp_audit(id,user_id,action,target,created_at) VALUES(?,?,?,?,?)`), id(), user, action, target, time.Now().Unix())
	return e
}
func (s *Server) Bootstrap(ctx context.Context, username, password string) error {
	if len(password) < 12 || len(password) > 72 || strings.TrimSpace(username) == "" {
		return errors.New("username and 12-72 byte password required")
	}
	h, e := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if e != nil {
		return e
	}
	username = strings.ToLower(strings.TrimSpace(username))
	return s.Store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
		var n int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM cp_users").Scan(&n); e != nil {
			return e
		}
		if n != 0 {
			return errors.New("already initialized")
		}
		identityGroupID := id()
		if _, e := tx.ExecContext(ctx, s.q(`INSERT INTO cp_identity_groups(id,name) VALUES(?,?)`), identityGroupID, username+" 默认组"); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, s.q(`INSERT INTO cp_users(id,username,password_hash,role,identity_group_id,disabled) VALUES(?,?,?,?,?,0)`), "bootstrap-admin", username, string(h), "admin", identityGroupID)
		return e
	})
}
func (s *Server) allow(key string, max int) bool {
	return s.allowWindow(key, max, time.Minute)
}

func (s *Server) allowWindow(key string, max int, period time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.limits) > 10000 {
		for k, v := range s.limits {
			if now.Sub(v.since) > v.period {
				delete(s.limits, k)
			}
		}
		if len(s.limits) > 10000 {
			return false
		}
	}
	l := s.limits[key]
	if now.Sub(l.since) >= period {
		l = limit{since: now, period: period}
	}
	l.count++
	s.limits[key] = l
	return l.count <= max
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.csrf(r) {
		fail(w, 403, "invalid request origin or CSRF header")
		return
	}
	ip := httporigin.ClientIP(r, s.opts.TrustProxy)
	if !s.allow("login:"+ip, 15) {
		fail(w, 429, "login rate limited")
		return
	}
	var in struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		CaptchaID     string `json:"captcha_id"`
		CaptchaAnswer string `json:"captcha_answer"`
	}
	if !decode(w, r, &in) {
		return
	}
	settings, err := s.SiteSettings(r.Context())
	if err != nil {
		fail(w, 503, "site settings unavailable")
		return
	}
	if settings.Captcha && !s.verifyCaptcha(r.Context(), in.CaptchaID, in.CaptchaAnswer) {
		fail(w, 400, "invalid or expired captcha")
		return
	}
	var u contract.User
	var hash string
	var disabled int
	e := s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT id,username,role,identity_group_id,disabled,password_hash FROM cp_users WHERE username=?`), strings.ToLower(strings.TrimSpace(in.Username))).Scan(&u.ID, &u.Username, &u.Role, &u.IdentityGroupID, &disabled, &hash)
	if e != nil || disabled != 0 || bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
		fail(w, 401, "invalid credentials")
		return
	}
	t := token()
	exp := time.Now().Add(12 * time.Hour)
	e = s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_sessions(token_hash,user_id,expires_at) VALUES(?,?,?)`), digest(t), u.ID, exp.Unix())
		return e
	})
	if e != nil {
		fail(w, 500, "session unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "tfp_session", Value: t, Path: "/", HttpOnly: true, Secure: s.opts.SecureCookies || httporigin.Scheme(r, s.opts.TrustProxy) == "https", SameSite: http.SameSiteStrictMode, Expires: exp})
	reply(w, 200, map[string]any{"user": u})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie("tfp_session")
	if c == nil {
		fail(w, 400, "cookie session required; revoke API token separately")
		return
	}
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), s.q(`DELETE FROM cp_sessions WHERE token_hash=?`), digest(c.Value))
		return e
	})
	if e != nil {
		fail(w, 500, "logout unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "tfp_session", Value: "", Path: "/", HttpOnly: true, MaxAge: -1, Secure: s.opts.SecureCookies || httporigin.Scheme(r, s.opts.TrustProxy) == "https", SameSite: http.SameSiteStrictMode})
	w.WriteHeader(204)
}
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/rules/{id}/network-diagnostic", s.RequireUser(s.createDiagnostic))
	mux.HandleFunc("GET /api/v1/diagnostics/{id}", s.RequireUser(s.diagnostic))
	mux.HandleFunc("POST /api/v1/agent/diagnostics", s.agent(s.claimDiagnostic))
	mux.HandleFunc("POST /api/v1/agent/diagnostics/result", s.agent(s.finishDiagnostic))
	mux.HandleFunc("GET /api/v1/site", s.site)
	mux.HandleFunc("PUT /api/v1/site", s.admin(s.saveSite))
	mux.HandleFunc("GET /api/v1/auth/captcha", s.captcha)
	mux.HandleFunc("POST /api/v1/auth/register", s.registerUser)
	mux.HandleFunc("POST /api/v1/registration-invites", s.admin(s.registrationInvite))
	mux.HandleFunc("GET /api/v1/exits", s.RequireUser(s.exits))
	mux.HandleFunc("POST /api/v1/exits", s.admin(s.saveExit))
	mux.HandleFunc("PUT /api/v1/exits/{id}", s.admin(s.saveExit))
	mux.HandleFunc("GET /api/v1/auth/tokens", s.RequireUser(s.listTokens))
	mux.HandleFunc("POST /api/v1/nodes/{id}/rotate-token", s.admin(s.rotateNodeToken))
	mux.HandleFunc("POST /api/v1/agent/leases/retire", s.agent(s.retireLease))
	mux.HandleFunc("POST /api/v1/auth/tokens", s.RequireUser(s.createToken))
	mux.HandleFunc("DELETE /api/v1/auth/tokens/{id}", s.RequireUser(s.revokeToken))
	// 管理员给账号发凭据：建完账号拿到密钥交给用户，事后能看、能重置、能撤。
	mux.HandleFunc("GET /api/v1/users/{id}/tokens", s.admin(s.listUserTokens))
	mux.HandleFunc("POST /api/v1/users/{id}/tokens", s.admin(s.createUserToken))
	mux.HandleFunc("POST /api/v1/users/{id}/tokens/{token_id}/reset", s.admin(s.resetToken))
	mux.HandleFunc("DELETE /api/v1/users/{id}/tokens/{token_id}", s.admin(s.revokeUserToken))
	// 探针页面归属组的设备地址。带 /api/v1 前缀的是面板自身的接口；不带前缀
	// 的两个是给客户脚本用的稳定入口，路径按约定写死，不随版本变化。
	mux.HandleFunc("GET /api/v1/online/device/ip", s.RequireUser(s.deviceIP))
	mux.HandleFunc("GET /api/v1/online/device/ip/list", s.RequireUser(s.deviceIPList))
	mux.HandleFunc("GET /online/device/ip", s.RequireUser(s.deviceIP))
	mux.HandleFunc("GET /online/device/ip/list", s.RequireUser(s.deviceIPList))
	mux.HandleFunc("POST /api/v1/auth/password", s.RequireUser(s.changePassword))
	mux.HandleFunc("POST /api/v1/users/{id}/reset-password", s.admin(s.resetUserPassword))
	mux.HandleFunc("PUT /api/v1/users/{id}/status", s.admin(s.disableUser))
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("GET /api/v1/auth/session", s.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFromContext(r.Context())
		reply(w, 200, map[string]any{"user": u})
	}))
	mux.HandleFunc("POST /api/v1/auth/logout", s.RequireUser(s.logout))
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if s.Store.DB.PingContext(ctx) != nil {
			fail(w, 503, "database unavailable")
			return
		}
		reply(w, 200, map[string]any{"status": "ok", "database": s.Store.Dialect, "version": contract.Version})
	})
	mux.HandleFunc("GET /api/v1/users", s.admin(s.users))
	mux.HandleFunc("POST /api/v1/users", s.admin(s.createUser))
	mux.HandleFunc("PUT /api/v1/users/{id}/identity-group", s.admin(s.setUserIdentityGroup))
	mux.HandleFunc("GET /api/v1/identity-groups", s.admin(s.identityGroups))
	mux.HandleFunc("POST /api/v1/identity-groups", s.admin(s.createIdentityGroup))
	mux.HandleFunc("PUT /api/v1/identity-groups/{id}", s.admin(s.updateIdentityGroup))
	mux.HandleFunc("DELETE /api/v1/identity-groups/{id}", s.admin(s.deleteIdentityGroup))
	mux.HandleFunc("GET /api/v1/groups", s.RequireUser(s.groups))
	mux.HandleFunc("POST /api/v1/groups", s.admin(s.saveGroup))
	mux.HandleFunc("PUT /api/v1/groups/{id}", s.admin(s.saveGroup))
	mux.HandleFunc("GET /api/v1/groups/{id}/join-key", s.admin(s.groupJoinKey))
	mux.HandleFunc("POST /api/v1/groups/{id}/join-key", s.admin(s.rotateGroupJoinKey))
	mux.HandleFunc("GET /api/v1/nodes", s.RequireUser(s.nodes))
	mux.HandleFunc("POST /api/v1/nodes/enrollment", s.admin(s.enroll))
	mux.HandleFunc("GET /api/v1/rules", s.RequireUser(s.rules))
	mux.HandleFunc("POST /api/v1/rules", s.RequireUser(s.saveRule))
	mux.HandleFunc("PUT /api/v1/rules/{id}", s.RequireUser(s.saveRule))
	mux.HandleFunc("DELETE /api/v1/rules/{id}", s.RequireUser(s.deleteRule))
	mux.HandleFunc("GET /api/v1/rules/{id}/diagnose", s.RequireUser(s.diagnoseRule))
	mux.HandleFunc("GET /api/v1/probes", s.RequireUser(s.probeList))
	mux.HandleFunc("GET /api/v1/probes/{node_id}/history", s.RequireUser(s.probeHistoryList))
	mux.HandleFunc("GET /api/v1/probes/events", s.RequireUser(s.probeEvents))
	mux.HandleFunc("GET /api/v1/audit", s.admin(s.audit))
	mux.HandleFunc("POST /api/v1/tasks/rules/export", s.RequireUser(s.createExportTask))
	mux.HandleFunc("POST /api/v1/tasks/rules/preview", s.RequireUser(s.previewImport))
	mux.HandleFunc("POST /api/v1/tasks/rules/import", s.RequireUser(s.createImportTask))
	mux.HandleFunc("GET /api/v1/tasks", s.RequireUser(s.tasks))
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.RequireUser(s.task))
	mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.RequireUser(s.cancelTask))
	mux.HandleFunc("POST /api/v1/agent/register", s.registerNode)
	mux.HandleFunc("GET /api/v1/agent/config", s.agent(s.config))
	mux.HandleFunc("POST /api/v1/agent/ack", s.agent(s.ack))
	mux.HandleFunc("POST /api/v1/agent/probe", s.agent(s.probe))
	mux.HandleFunc("POST /api/v1/agent/usage", s.agent(s.usage))
	mux.HandleFunc("POST /api/v1/agent/control", s.agent(s.agentControl))
	mux.HandleFunc("POST /api/v1/agent/control/result", s.agent(s.agentControlResult))
	mux.HandleFunc("POST /api/v1/nodes/{id}/operation-access", s.operationsAdmin(s.operationAccess))
	mux.HandleFunc("POST /api/v1/nodes/{id}/terminal", s.operationsAdmin(s.createTerminal))
	mux.HandleFunc("POST /api/v1/nodes/{id}/looking-glass", s.operationsAdmin(s.createLookingGlass))
	mux.HandleFunc("GET /api/v1/looking-glass/{id}", s.RequireUser(s.lookingGlass))
	mux.HandleFunc("POST /api/v1/agent/looking-glass", s.agent(s.claimLookingGlass))
	mux.HandleFunc("POST /api/v1/agent/looking-glass/result", s.agent(s.finishLookingGlass))
	mux.HandleFunc("POST /api/v1/nodes/{id}/upgrade", s.operationsAdmin(s.createUpgrade))
	mux.HandleFunc("GET /api/v1/nodes/{id}/operations", s.operationsAdmin(s.nodeOperations))
	mux.HandleFunc("POST /api/v1/node-operations/{id}/cancel", s.operationsAdmin(s.cancelOperation))
	mux.HandleFunc("GET /api/v1/node-operations/{id}/terminal", s.operationsAdmin(s.browserTerminal))
	mux.HandleFunc("GET /api/v1/node-operations/{id}/commands", s.operationsAdmin(s.terminalCommands))
	mux.HandleFunc("GET /api/v1/agent/control/{id}/terminal", s.agent(s.agentTerminal))
}
func pages(r *http.Request) (int, int) {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	n, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if p < 1 {
		p = 1
	}
	if n < 1 {
		n = 20
	}
	if n > 100 {
		n = 100
	}
	return n, (p - 1) * n
}
func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	n, o := pages(r)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.q(`SELECT u.id,u.username,u.role,u.identity_group_id,COALESCE(ig.name,''),u.disabled FROM cp_users u LEFT JOIN cp_identity_groups ig ON ig.id=u.identity_group_id ORDER BY u.id LIMIT ? OFFSET ?`), n, o)
	if e != nil {
		fail(w, 500, "query failed")
		return
	}
	defer rows.Close()
	items := []contract.User{}
	for rows.Next() {
		var u contract.User
		var d int
		if rows.Scan(&u.ID, &u.Username, &u.Role, &u.IdentityGroupID, &u.IdentityGroupName, &d) != nil {
			fail(w, 500, "query failed")
			return
		}
		u.Disabled = d != 0
		items = append(items, u)
	}
	var total int
	s.Store.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM cp_users").Scan(&total)
	reply(w, 200, map[string]any{"items": items, "total": total})
}
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username        string `json:"username"`
		Role            string `json:"role"`
		IdentityGroupID string `json:"identity_group_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Username = strings.ToLower(strings.TrimSpace(in.Username))
	if len(in.Username) < 1 || len(in.Username) > 100 || (in.Role != "user" && in.Role != "admin") {
		fail(w, 400, "invalid username or role")
		return
	}
	password, e := generateUserPassword()
	if e != nil {
		fail(w, 500, "password generation failed")
		return
	}
	h, e := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if e != nil {
		fail(w, 400, "invalid password")
		return
	}
	u := contract.User{ID: id(), Username: in.Username, Role: in.Role, IdentityGroupID: strings.TrimSpace(in.IdentityGroupID)}
	if u.IdentityGroupID == "" {
		u.IdentityGroupID = id()
		u.IdentityGroupName = u.Username + " 默认组"
	}
	actor, _ := UserFromContext(r.Context())
	e = s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		if u.IdentityGroupName != "" {
			if _, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_identity_groups(id,name) VALUES(?,?)`), u.IdentityGroupID, u.IdentityGroupName); e != nil {
				return e
			}
		} else {
			identity, err := s.identityGroupForUpdate(r.Context(), tx, u.IdentityGroupID)
			if err != nil {
				return err
			}
			u.IdentityGroupID, u.IdentityGroupName = identity.ID, identity.Name
		}
		_, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_users(id,username,password_hash,role,identity_group_id,disabled) VALUES(?,?,?,?,?,0)`), u.ID, u.Username, string(h), u.Role, u.IdentityGroupID)
		if e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "user.create", u.ID)
	})
	if e != nil {
		fail(w, 409, "user creation conflict")
		return
	}
	reply(w, 201, contract.UserCreated{
		ID:                u.ID,
		Username:          u.Username,
		Role:              u.Role,
		IdentityGroupID:   u.IdentityGroupID,
		IdentityGroupName: u.IdentityGroupName,
		Disabled:          u.Disabled,
		InitialPassword:   password,
	})
}
func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	n, o := pages(r)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.q(`SELECT id,user_id,action,target,created_at FROM cp_audit ORDER BY created_at DESC,id LIMIT ? OFFSET ?`), n, o)
	if e != nil {
		fail(w, 500, "query failed")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var i, u, a, t string
		var at int64
		if rows.Scan(&i, &u, &a, &t, &at) != nil {
			fail(w, 500, "query failed")
			return
		}
		items = append(items, map[string]any{"id": i, "user_id": u, "action": a, "target": t, "created_at": time.Unix(at, 0).UTC()})
	}
	var total int
	s.Store.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM cp_audit").Scan(&total)
	reply(w, 200, map[string]any{"items": items, "total": total})
}
func strJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

var errConflict = fmt.Errorf("conflict")

// errEnrollment 覆盖两种接入凭据的失败：一次性令牌无效/过期/已用，或设备组
// 接入密钥不认识。对外只回一句笼统的话，不区分是哪一种。
var errEnrollment = fmt.Errorf("invalid enrollment")
