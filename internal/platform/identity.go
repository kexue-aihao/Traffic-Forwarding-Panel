package platform

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

// tokenPrefixLen 是明文里留下来做「这是哪一把」标识的长度。8 位十六进制足够
// 分辨几十把钥匙，又短到不可能反推出凭据本身。
const tokenPrefixLen = 8

// permanentExpiry 是「永不过期」在数据库里的写法。0 与任何真实时间戳都不冲突：
// 有限期凭据的 expires_at 一定大于 0，所以老数据不需要迁移。
const permanentExpiry = int64(0)

type tokenRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at"`
	Permanent bool       `json:"permanent"`
	// GroupIDs 把凭据限定到这几个设备组，留空表示不限制（跟随账号自己的可见
	// 范围）。范围只可能变窄：列进来的组必须是账号自己也能看到的。
	GroupIDs []string `json:"group_ids"`
}

// validate 要求调用方明确选择有效期：要么给一个一年以内的时刻，要么显式
// permanent。不能「两个都没填」就悄悄发一把永久钥匙 —— 那是最不该默认的一种。
func (t tokenRequest) validate(now time.Time) error {
	if strings.TrimSpace(t.Name) == "" || len(t.Name) > 190 {
		return errors.New("token 需要名字")
	}
	if t.Permanent == (t.ExpiresAt != nil) {
		return errors.New("有效期二选一：给出 expires_at，或声明 permanent")
	}
	if t.ExpiresAt != nil && (!t.ExpiresAt.After(now) || t.ExpiresAt.After(now.Add(contract.MaxTokenLifetime))) {
		return errors.New("有效期必须在将来，且不超过一年")
	}
	if len(t.GroupIDs) > 100 {
		return errors.New("设备组范围最多 100 个")
	}
	seen := map[string]bool{}
	for _, group := range t.GroupIDs {
		if strings.TrimSpace(group) == "" || len(group) > 64 || seen[group] {
			return errors.New("设备组范围里有重复或无效的组")
		}
		seen[group] = true
	}
	return nil
}

func (t tokenRequest) expiry(now time.Time) int64 {
	if t.Permanent {
		return permanentExpiry
	}
	return t.ExpiresAt.Unix()
}

// createToken 给自己发一把凭据。
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var in tokenRequest
	if !decode(w, r, &in) {
		return
	}
	if e := in.validate(time.Now()); e != nil {
		fail(w, 400, e.Error())
		return
	}
	u, _ := UserFromContext(r.Context())
	s.issueToken(w, r, u.ID, in, "token.create", true)
}

// createUserToken 是管理员路径：先建账号，再把凭据发给对方。
//
// 「密钥只显示一次」是这里唯一的交付方式：面板存的是摘要，事后任何接口都取不
// 回明文。忘了或者弄丢了只能重置 —— 重置同样只显示这一次。
func (s *Server) createUserToken(w http.ResponseWriter, r *http.Request) {
	var in tokenRequest
	if !decode(w, r, &in) {
		return
	}
	if e := in.validate(time.Now()); e != nil {
		fail(w, 400, e.Error())
		return
	}
	target := r.PathValue("id")
	var exists int
	if e := s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT COUNT(*) FROM cp_users WHERE id=?`), target).Scan(&exists); e != nil || exists != 1 {
		fail(w, 404, "unknown user")
		return
	}
	s.issueToken(w, r, target, in, "token.issue", false)
}

func (s *Server) issueToken(w http.ResponseWriter, r *http.Request, user string, in tokenRequest, action string, self bool) {
	raw := token()
	tid := id()
	expiry := in.expiry(time.Now())
	actor, _ := UserFromContext(r.Context())
	created := time.Now().UTC()
	// 范围只能变窄：凭据能看到的组必须是账号自己也能看到的，否则等于绕开账号的
	// 授权另发一把钥匙。管理员给自己发的凭据同样按这条校验（管理员的可见范围
	// 本来就是全部）。
	if len(in.GroupIDs) > 0 {
		allowed, e := s.userGroups(r.Context(), user)
		if e != nil {
			fail(w, 500, "token creation failed")
			return
		}
		for _, group := range in.GroupIDs {
			if !allowed[group] {
				fail(w, 400, "凭据的设备组范围不能超出账号自己的授权")
				return
			}
		}
	}
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_tokens(id,token_hash,user_id,name,expires_at,prefix,created_at,last_used_at) VALUES(?,?,?,?,?,?,?,0)`), tid, digest(raw), user, in.Name, expiry, raw[:tokenPrefixLen], created.Unix())
		if e != nil {
			return e
		}
		for _, group := range in.GroupIDs {
			if _, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_token_groups(token_id,group_id) VALUES(?,?)`), tid, group); e != nil {
				return e
			}
		}
		return s.AuditTx(r.Context(), tx, actor.ID, action, tid)
	})
	if e != nil {
		fail(w, 500, "token creation failed")
		return
	}
	out := map[string]any{"id": tid, "token": raw, "name": in.Name, "prefix": raw[:tokenPrefixLen], "permanent": in.Permanent, "scope": contract.TokenScopeOwnerResources, "created_at": created}
	if in.Permanent {
		out["expires_at"] = nil
	} else {
		out["expires_at"] = in.ExpiresAt.UTC()
	}
	if !self {
		out["user_id"] = user
	}
	if len(in.GroupIDs) > 0 {
		out["group_ids"] = in.GroupIDs
	}
	reply(w, 201, out)
}

// resetToken 换一把新密钥，行本身保留（同一个 id、名字和有效期）。
//
// 这是凭据泄露或遗失后的唯一补救手段：明文既然取不回来，就只能作废重发。
func (s *Server) resetToken(w http.ResponseWriter, r *http.Request) {
	target, tid := r.PathValue("id"), r.PathValue("token_id")
	raw := token()
	actor, _ := UserFromContext(r.Context())
	var expires int64
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_tokens SET token_hash=?,prefix=?,created_at=?,last_used_at=0 WHERE id=? AND user_id=?`), digest(raw), raw[:tokenPrefixLen], time.Now().Unix(), tid, target)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errConflict
		}
		if e = tx.QueryRowContext(r.Context(), s.q(`SELECT expires_at FROM cp_tokens WHERE id=?`), tid).Scan(&expires); e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "token.reset", tid)
	})
	if e != nil {
		fail(w, 404, "token not found")
		return
	}
	out := map[string]any{"id": tid, "token": raw, "prefix": raw[:tokenPrefixLen], "permanent": expires == permanentExpiry}
	if expires != permanentExpiry {
		out["expires_at"] = time.Unix(expires, 0).UTC()
	} else {
		out["expires_at"] = nil
	}
	reply(w, 200, out)
}

// revokeUserToken 让管理员能撤掉发给用户的钥匙。
func (s *Server) revokeUserToken(w http.ResponseWriter, r *http.Request) {
	target, tid := r.PathValue("id"), r.PathValue("token_id")
	actor, _ := UserFromContext(r.Context())
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(r.Context(), s.q(`DELETE FROM cp_tokens WHERE id=? AND user_id=?`), tid, target)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errConflict
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "token.revoke", tid)
	})
	if e != nil {
		fail(w, 404, "token not found")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), s.q(`DELETE FROM cp_tokens WHERE id=? AND user_id=?`), r.PathValue("id"), u.ID)
		if e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, u.ID, "token.revoke", r.PathValue("id"))
	})
	if e != nil {
		fail(w, 500, "token revocation failed")
		return
	}
	w.WriteHeader(204)
}
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current  string `json:"current_password"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Password) < 12 || len(in.Password) > 72 {
		fail(w, 400, "password must contain 12-72 bytes")
		return
	}
	u, _ := UserFromContext(r.Context())
	var hash string
	if s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT password_hash FROM cp_users WHERE id=?`), u.ID).Scan(&hash) != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Current)) != nil {
		fail(w, 403, "current password incorrect")
		return
	}
	next, _ := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_users SET password_hash=? WHERE id=? AND password_hash=?`), string(next), u.ID, hash)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errConflict
		}
		for _, table := range []string{"cp_sessions", "cp_tokens"} {
			if _, e = tx.ExecContext(r.Context(), s.q(`DELETE FROM `+table+` WHERE user_id=?`), u.ID); e != nil {
				return e
			}
		}
		return s.AuditTx(r.Context(), tx, u.ID, "user.password", u.ID)
	})
	if e != nil {
		fail(w, 409, "password change conflict")
		return
	}
	w.WriteHeader(204)
}
func (s *Server) disableUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Disabled bool `json:"disabled"`
	}
	if !decode(w, r, &in) {
		return
	}
	actor, _ := UserFromContext(r.Context())
	target := r.PathValue("id")
	if target == actor.ID {
		fail(w, 409, "cannot disable your own administrator account")
		return
	}
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		var role string
		if e := tx.QueryRowContext(r.Context(), s.q(`SELECT role FROM cp_users WHERE id=?`), target).Scan(&role); e != nil {
			return e
		}
		if role == "admin" {
			return errors.New("administrator disabling requires recovery workflow")
		}
		disabled := 0
		if in.Disabled {
			disabled = 1
		}
		if _, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_users SET disabled=? WHERE id=?`), disabled, target); e != nil {
			return e
		}
		if in.Disabled {
			for _, table := range []string{"cp_sessions", "cp_tokens"} {
				if _, e := tx.ExecContext(r.Context(), s.q(`DELETE FROM `+table+` WHERE user_id=?`), target); e != nil {
					return e
				}
			}
		}
		if _, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id IN(SELECT node_id FROM cp_rules WHERE user_id=?)`), target); e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "user.status", target)
	})
	if e != nil {
		fail(w, 409, "account change conflict")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	target := r.PathValue("id")
	password, err := generateUserPassword()
	if err != nil {
		fail(w, 500, "password generation failed")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, "password reset failed")
		return
	}
	err = s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(r.Context(), s.q(`UPDATE cp_users SET password_hash=? WHERE id=?`), string(hash), target)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil || updated != 1 {
			return errConflict
		}
		for _, table := range []string{"cp_sessions", "cp_tokens"} {
			if _, err = tx.ExecContext(r.Context(), s.q(`DELETE FROM `+table+` WHERE user_id=?`), target); err != nil {
				return err
			}
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "user.password.reset", target)
	})
	if err != nil {
		fail(w, 409, "password reset conflict")
		return
	}
	reply(w, 200, contract.UserPasswordReset{UserID: target, Password: password})
}

// bearerUser deliberately constrains machine API keys to ordinary owner access;
// full administrator financial/operational keys need explicit future scopes.
//
// 这同时是探针页面设备地址接口的鉴权路径：客户脚本带的就是这种密钥。有效期
// 在 SQL 里判定 —— 永久凭据写的是 expires_at=0，正好绕开时间比较。
func (s *Server) bearerUser(r *http.Request) (contract.User, error) {
	var u contract.User
	var disabled int
	var tokenID string
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	e := s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT u.id,u.username,u.role,u.identity_group_id,u.disabled,t.id FROM cp_tokens t JOIN cp_users u ON u.id=t.user_id WHERE t.token_hash=? AND (t.expires_at=? OR t.expires_at>?)`), digest(raw), permanentExpiry, time.Now().Unix()).Scan(&u.ID, &u.Username, &u.Role, &u.IdentityGroupID, &disabled, &tokenID)
	if e != nil || disabled != 0 {
		return u, errors.New("authentication required")
	}
	// 最近使用时间按分钟节流：脚本可能每秒调一次接口，而它只是一条给人看的
	// 线索，不值得为每次请求排一次写。
	if s.allow("token-use:"+tokenID, 1) {
		_ = s.Store.Write(r.Context(), storage.Background, func(tx *sql.Tx) error {
			_, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_tokens SET last_used_at=? WHERE id=?`), time.Now().Unix(), tokenID)
			return e
		})
	}
	u.Role = "user"
	// 凭据自带的设备组范围：读一次，后续所有可见性判断都按它收窄。
	rows, e := s.Store.DB.QueryContext(r.Context(), s.q(`SELECT group_id FROM cp_token_groups WHERE token_id=?`), tokenID)
	if e != nil {
		return contract.User{}, errors.New("authentication required")
	}
	defer rows.Close()
	for rows.Next() {
		var group string
		if e = rows.Scan(&group); e != nil {
			return contract.User{}, errors.New("authentication required")
		}
		u.TokenGroups = append(u.TokenGroups, group)
	}
	return u, rows.Err()
}

// queryRower 让可见性判断既能在连接上跑，也能在事务里跑（诊断的鉴权在事务内）。
type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// groupVisible 判断这个身份能不能看到某个设备组：先是账号自己的可见范围，再
// 收窄到本次请求所用凭据自带的那些组。管理员不受限。
func (s *Server) groupVisible(ctx context.Context, q queryRower, u contract.User, group string) (bool, error) {
	if u.Role == "admin" {
		return true, nil
	}
	args := []any{group, u.ID}
	query := `SELECT COUNT(*) FROM cp_group_identity_groups gig JOIN cp_users iu ON iu.identity_group_id=gig.identity_group_id WHERE gig.group_id=? AND iu.id=?` + tokenGroupScope(u, "gig.group_id", &args)
	var n int
	if err := q.QueryRowContext(ctx, s.q(query), args...).Scan(&n); err != nil {
		return false, err
	}
	return n == 1, nil
}

// tokenGroupScope 把「这个身份能看到哪些设备组」再收窄到凭据自己带的那些组。
//
// column 是引用设备组 id 的 SQL 片段（如 gig.group_id）。没有范围的凭据返回
// 空串，SQL 一个字都不变 —— 绝大多数凭据都是这种。
func tokenGroupScope(u contract.User, column string, args *[]any) string {
	if len(u.TokenGroups) == 0 {
		return ""
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(u.TokenGroups)), ",")
	for _, group := range u.TokenGroups {
		*args = append(*args, group)
	}
	return " AND " + column + " IN (" + placeholders + ")"
}

// ResetPassword is an operator-local recovery API, never an unauthenticated HTTP
// route. Existing sessions and API keys are revoked in the same transaction.
func (s *Server) ResetPassword(ctx context.Context, username, password string) error {
	if len(password) < 12 || len(password) > 72 {
		return errors.New("password must contain 12-72 bytes")
	}
	hash, e := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if e != nil {
		return e
	}
	return s.Store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
		var uid string
		if e := tx.QueryRowContext(ctx, s.q(`SELECT id FROM cp_users WHERE username=?`), strings.ToLower(strings.TrimSpace(username))).Scan(&uid); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, s.q(`UPDATE cp_users SET password_hash=? WHERE id=?`), string(hash), uid); e != nil {
			return e
		}
		for _, table := range []string{"cp_sessions", "cp_tokens"} {
			if _, e := tx.ExecContext(ctx, s.q(`DELETE FROM `+table+` WHERE user_id=?`), uid); e != nil {
				return e
			}
		}
		return s.AuditTx(ctx, tx, "local-operator", "user.recover", uid)
	})
}
func (s *Server) rotateNodeToken(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("id")
	raw := token()
	u, _ := UserFromContext(r.Context())
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_nodes SET token_hash=? WHERE id=?`), digest(raw), node)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errConflict
		}
		return s.AuditTx(r.Context(), tx, u.ID, "node.rotate", node)
	})
	if e != nil {
		fail(w, 409, "node credential rotation failed")
		return
	}
	reply(w, 200, map[string]any{"node_id": node, "token": raw})
}

// accessTokens 读出一批凭据的公开字段。明文与摘要都不在这里 —— 这个函数的
// 返回值会被直接序列化给管理员看。
// userGroups 列出某个账号有权访问的设备组。只用于校验凭据范围 —— 这条判断问的
// 是「这个账号能不能看到」，与调用方自己的身份无关。
func (s *Server) userGroups(ctx context.Context, user string) (map[string]bool, error) {
	rows, e := s.Store.DB.QueryContext(ctx, s.q(`SELECT gig.group_id FROM cp_group_identity_groups gig JOIN cp_users u ON u.identity_group_id=gig.identity_group_id WHERE u.id=?`), user)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	allowed := map[string]bool{}
	for rows.Next() {
		var group string
		if e = rows.Scan(&group); e != nil {
			return nil, e
		}
		allowed[group] = true
	}
	return allowed, rows.Err()
}

func (s *Server) accessTokens(ctx context.Context, user string, n, o int) ([]contract.APIToken, int, error) {
	var total int
	if e := s.Store.DB.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM cp_tokens WHERE user_id=?`), user).Scan(&total); e != nil {
		return nil, 0, e
	}
	rows, e := s.Store.DB.QueryContext(ctx, s.q(`SELECT id,name,prefix,expires_at,created_at,last_used_at FROM cp_tokens WHERE user_id=? ORDER BY created_at DESC,id LIMIT ? OFFSET ?`), user, n, o)
	if e != nil {
		return nil, 0, e
	}
	defer rows.Close()
	items := []contract.APIToken{}
	for rows.Next() {
		var t contract.APIToken
		var expires, created, used int64
		if e = rows.Scan(&t.ID, &t.Name, &t.Prefix, &expires, &created, &used); e != nil {
			return nil, 0, e
		}
		t.UserID = user
		t.Scope = contract.TokenScopeOwnerResources
		t.Permanent = expires == permanentExpiry
		t.CreatedAt = time.Unix(created, 0).UTC()
		if !t.Permanent {
			at := time.Unix(expires, 0).UTC()
			t.ExpiresAt = &at
		}
		if used > 0 {
			at := time.Unix(used, 0).UTC()
			t.LastUsedAt = &at
		}
		// 凭据的设备组范围：一条小查询，一页最多几十把钥匙，不值得为它拼一个
		// 三库通吃的聚合。
		groupRows, e := s.Store.DB.QueryContext(ctx, s.q(`SELECT group_id FROM cp_token_groups WHERE token_id=? ORDER BY group_id`), t.ID)
		if e != nil {
			return nil, 0, e
		}
		for groupRows.Next() {
			var group string
			if e = groupRows.Scan(&group); e != nil {
				groupRows.Close()
				return nil, 0, e
			}
			t.GroupIDs = append(t.GroupIDs, group)
		}
		groupRows.Close()
		items = append(items, t)
	}
	return items, total, rows.Err()
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	n, o := pages(r)
	items, total, e := s.accessTokens(r.Context(), u.ID, n, o)
	if e != nil {
		fail(w, 500, "token query failed")
		return
	}
	reply(w, 200, map[string]any{"items": items, "total": total})
}

// listUserTokens 是管理员视角：账号列表里点开某个用户，看得到他手上有哪些
// 凭据、最近用过没有，从而决定重置哪一条 —— 明文取不回来，能管的就这些。
func (s *Server) listUserTokens(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("id")
	var username string
	if e := s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT username FROM cp_users WHERE id=?`), target).Scan(&username); e != nil {
		fail(w, 404, "unknown user")
		return
	}
	n, o := pages(r)
	items, total, e := s.accessTokens(r.Context(), target, n, o)
	if e != nil {
		fail(w, 500, "token query failed")
		return
	}
	for i := range items {
		items[i].Username = username
	}
	reply(w, 200, map[string]any{"items": items, "total": total, "user": map[string]string{"id": target, "username": username}})
}
