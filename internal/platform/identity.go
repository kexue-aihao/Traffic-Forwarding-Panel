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

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string    `json:"name"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 190 || !in.ExpiresAt.After(time.Now()) || in.ExpiresAt.After(time.Now().Add(365*24*time.Hour)) {
		fail(w, 400, "token requires name and expiry within one year")
		return
	}
	u, _ := UserFromContext(r.Context())
	raw := token()
	tid := id()
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_tokens(id,token_hash,user_id,name,expires_at) VALUES(?,?,?,?,?)`), tid, digest(raw), u.ID, in.Name, in.ExpiresAt.Unix())
		if e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, u.ID, "token.create", tid)
	})
	if e != nil {
		fail(w, 500, "token creation failed")
		return
	}
	reply(w, 201, map[string]any{"id": tid, "token": raw, "expires_at": in.ExpiresAt, "scope": "owner-resources"})
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

// bearerUser deliberately constrains machine API keys to ordinary owner access;
// full administrator financial/operational keys need explicit future scopes.
func (s *Server) bearerUser(r *http.Request) (contract.User, error) {
	var u contract.User
	var disabled int
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	e := s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT u.id,u.username,u.role,u.disabled FROM cp_tokens t JOIN cp_users u ON u.id=t.user_id WHERE t.token_hash=? AND t.expires_at>?`), digest(raw), time.Now().Unix()).Scan(&u.ID, &u.Username, &u.Role, &disabled)
	if e != nil || disabled != 0 {
		return u, errors.New("authentication required")
	}
	u.Role = "user"
	return u, nil
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
