package platform

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) operationsAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.admin(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			fail(w, 403, "operations require an administrator cookie session")
			return
		}
		next(w, r)
	})
}

func (s *Server) operationAccess(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	if !s.allow("operations:"+u.ID, 5) {
		fail(w, 429, "rate limited")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	var hash string
	if s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT password_hash FROM cp_users WHERE id=?"), u.ID).Scan(&hash) != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
		fail(w, 403, "password incorrect")
		return
	}
	cookie, _ := r.Cookie("tfp_session")
	raw := token()
	expiry := time.Now().UTC().Add(15 * time.Minute)
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		var node string
		if e := tx.QueryRowContext(r.Context(), s.q("SELECT id FROM cp_nodes WHERE id=?"), r.PathValue("id")).Scan(&node); e != nil {
			return e
		}
		if _, e := tx.ExecContext(r.Context(), s.q("DELETE FROM cp_operation_access WHERE expires_at<=?"), time.Now().Unix()); e != nil {
			return e
		}
		if _, e := tx.ExecContext(r.Context(), s.q("INSERT INTO cp_operation_access(token_hash,user_id,node_id,session_hash,expires_at) VALUES(?,?,?,?,?)"), digest(raw), u.ID, node, digest(cookie.Value), expiry.Unix()); e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, u.ID, "node.operation_access", node)
	})
	if err != nil {
		fail(w, 409, "operation access unavailable")
		return
	}
	reply(w, 201, map[string]any{"token": raw, "expires_at": expiry})
}

func (s *Server) operationGrant(ctx context.Context, tx *sql.Tx, user, node, session, access string) (int64, error) {
	var expires int64
	e := tx.QueryRowContext(ctx, s.q(`SELECT a.expires_at FROM cp_operation_access a JOIN cp_sessions s ON s.token_hash=a.session_hash JOIN cp_users u ON u.id=a.user_id WHERE a.token_hash=? AND a.user_id=? AND a.node_id=? AND a.session_hash=? AND a.expires_at>? AND s.expires_at>? AND u.disabled=0 AND u.role='admin'`), access, user, node, session, time.Now().Unix(), time.Now().Unix()).Scan(&expires)
	return expires, e
}

func (s *Server) createTerminal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Access string `json:"access_token"`
		Key    string `json:"idempotency_key"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.createOperation(w, r, "terminal", in.Access, in.Key, nil)
}
func (s *Server) createUpgrade(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Access  string           `json:"access_token"`
		Key     string           `json:"idempotency_key"`
		Upgrade contract.Upgrade `json:"upgrade"`
	}
	if !decode(w, r, &in) {
		return
	}
	if e := in.Upgrade.Validate(); e != nil {
		fail(w, 400, e.Error())
		return
	}
	s.createOperation(w, r, "upgrade", in.Access, in.Key, &in.Upgrade)
}
func (s *Server) createOperation(w http.ResponseWriter, r *http.Request, kind, access, key string, upgrade *contract.Upgrade) {
	if len(key) < 8 || len(key) > 128 || len(access) != 64 {
		fail(w, 400, "operation access and idempotency key required")
		return
	}
	u, _ := UserFromContext(r.Context())
	cookie, _ := r.Cookie("tfp_session")
	op := contract.NodeOperation{ID: id(), NodeID: r.PathValue("id"), Kind: kind, Status: "pending", Upgrade: upgrade, CreatedAt: time.Now().UTC()}
	payload := strJSON(upgrade)
	fingerprint := digest(strJSON([]string{op.NodeID, kind, payload}))
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		// Node row serializes active-operation creation and claiming on all databases.
		query := "SELECT payload FROM cp_nodes WHERE id=?"
		if s.Store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		var raw string
		if e := tx.QueryRowContext(r.Context(), s.q(query), op.NodeID).Scan(&raw); e != nil {
			return e
		}
		var node contract.Node
		if e := json.Unmarshal([]byte(raw), &node); e != nil {
			return e
		}
		expires, e := s.operationGrant(r.Context(), tx, u.ID, op.NodeID, digest(cookie.Value), digest(access))
		if e != nil {
			return errors.New("operation authorization expired")
		}
		var previousID, previousHash string
		e = tx.QueryRowContext(r.Context(), s.q("SELECT id,payload_hash FROM cp_node_operations WHERE user_id=? AND idempotency_key=?"), u.ID, key).Scan(&previousID, &previousHash)
		if e == nil {
			if previousHash != fingerprint {
				return errConflict
			}
			op, e = s.loadOperation(r.Context(), tx, previousID)
			return e
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if !contains(node.Capabilities, kind+"-v1") {
			return errors.New("Agent has not enabled this operation")
		}
		if upgrade != nil && (upgrade.OS != node.OS || upgrade.Arch != node.Arch) {
			return errors.New("release platform does not match node")
		}
		if _, e = tx.ExecContext(r.Context(), s.q("UPDATE cp_node_operations SET status='expired',updated_at=? WHERE node_id=? AND status IN ('pending','running') AND expires_at<=?"), time.Now().Unix(), op.NodeID, time.Now().Unix()); e != nil {
			return e
		}
		var active int
		if e = tx.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_node_operations WHERE node_id=? AND status IN ('pending','running')"), op.NodeID).Scan(&active); e != nil {
			return e
		}
		if active > 0 {
			return errors.New("node already has an active operation")
		}
		op.ExpiresAt = time.Now().UTC().Add(10 * time.Minute)
		if op.ExpiresAt.Unix() > expires {
			op.ExpiresAt = time.Unix(expires, 0).UTC()
		}
		_, e = tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_node_operations(id,node_id,user_id,kind,status,payload,payload_hash,claim_token,access_hash,idempotency_key,error,created_at,updated_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), op.ID, op.NodeID, u.ID, kind, op.Status, payload, fingerprint, "", digest(access), key, "", op.CreatedAt.Unix(), op.CreatedAt.Unix(), op.ExpiresAt.Unix())
		if e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, u.ID, "node."+kind+".create", op.ID)
	})
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	op.Claim = ""
	reply(w, 201, op)
}

type operationQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Server) loadOperation(ctx context.Context, q operationQuery, oid string) (contract.NodeOperation, error) {
	var op contract.NodeOperation
	var raw string
	var created, expires int64
	e := q.QueryRowContext(ctx, s.q("SELECT id,node_id,kind,status,payload,claim_token,error,created_at,expires_at FROM cp_node_operations WHERE id=?"), oid).Scan(&op.ID, &op.NodeID, &op.Kind, &op.Status, &raw, &op.Claim, &op.Error, &created, &expires)
	if e != nil {
		return op, e
	}
	op.CreatedAt = time.Unix(created, 0).UTC()
	op.ExpiresAt = time.Unix(expires, 0).UTC()
	e = json.Unmarshal([]byte(raw), &op.Upgrade)
	return op, e
}
func (s *Server) nodeOperations(w http.ResponseWriter, r *http.Request) {
	rows, e := s.Store.DB.QueryContext(r.Context(), s.q("SELECT id FROM cp_node_operations WHERE node_id=? ORDER BY created_at DESC,id DESC LIMIT 50"), r.PathValue("id"))
	if e != nil {
		fail(w, 500, "operations unavailable")
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
		ids = append(ids, id)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		fail(w, 500, "operations unavailable")
		return
	}
	ops := []contract.NodeOperation{}
	for _, id := range ids {
		op, e := s.loadOperation(r.Context(), s.Store.DB, id)
		if e != nil {
			fail(w, 500, "operations unavailable")
			return
		}
		op.Claim = ""
		if (op.Status == "pending" || op.Status == "running") && !op.ExpiresAt.After(time.Now()) {
			op.Status = "expired"
		}
		ops = append(ops, op)
	}
	reply(w, 200, map[string]any{"items": ops})
}
func (s *Server) cancelOperation(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		op, e := s.loadOperation(r.Context(), tx, r.PathValue("id"))
		if e != nil {
			return e
		}
		if op.Status != "pending" && op.Status != "running" {
			return nil
		}
		if _, e = tx.ExecContext(r.Context(), s.q("UPDATE cp_node_operations SET status='cancelled',updated_at=? WHERE id=? AND status IN ('pending','running')"), time.Now().Unix(), op.ID); e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, u.ID, "node.operation.cancel", op.ID)
	})
	if e != nil {
		fail(w, 409, "operation unavailable")
		return
	}
	s.closeTerminal(r.PathValue("id"))
	w.WriteHeader(204)
}
func (s *Server) agentControl(w http.ResponseWriter, r *http.Request) {
	node := r.Context().Value(nodeKey{}).(string)
	result := contract.Control{Active: []string{}}
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		query := "SELECT id FROM cp_nodes WHERE id=?"
		if s.Store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		var ignored string
		if e := tx.QueryRowContext(r.Context(), s.q(query), node).Scan(&ignored); e != nil {
			return e
		}
		// A node crash can leave a running claim behind. Agents poll every two
		// seconds, so a quiet running claim is safe to return to the queue after
		// this grace period; active terminal sessions refresh updated_at below.
		if _, e := tx.ExecContext(r.Context(), s.q("UPDATE cp_node_operations SET status='pending',claim_token='',updated_at=? WHERE node_id=? AND status='running' AND updated_at<?"), time.Now().Unix(), node, time.Now().Add(-30*time.Second).Unix()); e != nil {
			return e
		}
		rows, e := tx.QueryContext(r.Context(), s.q("SELECT id FROM cp_node_operations WHERE node_id=? AND status IN ('pending','running') ORDER BY created_at,id LIMIT 10"), node)
		if e != nil {
			return e
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				break
			}
			ids = append(ids, id)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return e
		}
		for _, oid := range ids {
			op, e := s.loadOperation(r.Context(), tx, oid)
			if e != nil {
				return e
			}
			var owner, access, session string
			e = tx.QueryRowContext(r.Context(), s.q("SELECT o.user_id,o.access_hash,COALESCE(a.session_hash,'') FROM cp_node_operations o LEFT JOIN cp_operation_access a ON a.token_hash=o.access_hash WHERE o.id=?"), oid).Scan(&owner, &access, &session)
			if e != nil {
				return e
			}
			_, grantErr := s.operationGrant(r.Context(), tx, owner, node, session, access)
			if !op.ExpiresAt.After(time.Now()) || grantErr != nil {
				if _, e = tx.ExecContext(r.Context(), s.q("UPDATE cp_node_operations SET status='expired',updated_at=? WHERE id=?"), time.Now().Unix(), oid); e != nil {
					return e
				}
				continue
			}
			result.Active = append(result.Active, oid)
			if op.Status == "running" {
				if _, e = tx.ExecContext(r.Context(), s.q("UPDATE cp_node_operations SET updated_at=? WHERE id=? AND status='running'"), time.Now().Unix(), oid); e != nil {
					return e
				}
			}
			if op.Status == "pending" && result.Operation == nil {
				op.Status = "running"
				op.Claim = token()
				if _, e = tx.ExecContext(r.Context(), s.q("UPDATE cp_node_operations SET status='running',claim_token=?,updated_at=? WHERE id=? AND status='pending'"), op.Claim, time.Now().Unix(), oid); e != nil {
					return e
				}
				if e = s.AuditTx(r.Context(), tx, owner, "node."+op.Kind+".claim", oid); e != nil {
					return e
				}
				result.Operation = &op
			}
		}
		return nil
	})
	if e != nil {
		fail(w, 500, "control unavailable")
		return
	}
	reply(w, 200, result)
}
func (s *Server) agentControlResult(w http.ResponseWriter, r *http.Request) {
	var in contract.OperationResult
	if !decode(w, r, &in) {
		return
	}
	if !contains([]string{"succeeded", "failed", "staged", "rolled_back"}, in.Status) || len(in.Error) > 256 {
		fail(w, 400, "invalid operation result")
		return
	}
	node := r.Context().Value(nodeKey{}).(string)
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		op, e := s.loadOperation(r.Context(), tx, in.ID)
		if e != nil {
			return e
		}
		if op.NodeID != node || op.Claim == "" || subtle.ConstantTimeCompare([]byte(op.Claim), []byte(in.Claim)) != 1 {
			return errConflict
		}
		if op.Status == in.Status {
			return nil
		}
		if op.Status == "cancelled" || op.Status == "expired" {
			return nil
		}
		if op.Status == "staged" && (in.Status == "succeeded" || in.Status == "rolled_back") {
			message := ""
			if in.Status == "rolled_back" {
				message = "replacement failed; previous Agent restored"
			}
			if _, e = tx.ExecContext(r.Context(), s.q("UPDATE cp_node_operations SET status=?,error=?,updated_at=? WHERE id=? AND status='staged'"), in.Status, message, time.Now().Unix(), op.ID); e != nil {
				return e
			}
			return s.AuditTx(r.Context(), tx, node, "node."+op.Kind+"."+in.Status, op.ID)
		}
		if op.Status != "running" {
			return errConflict
		}
		// The wire error is a short category; do not persist raw host paths or output.
		message := ""
		if in.Status != "succeeded" {
			message = "Agent reported " + in.Status
		}
		if _, e = tx.ExecContext(r.Context(), s.q("UPDATE cp_node_operations SET status=?,error=?,updated_at=? WHERE id=? AND status='running'"), in.Status, message, time.Now().Unix(), op.ID); e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, node, "node."+op.Kind+"."+in.Status, op.ID)
	})
	if e != nil {
		fail(w, 409, "operation result conflict")
		return
	}
	s.closeTerminal(in.ID)
	w.WriteHeader(204)
}

// A live session must retain its original cookie, password grant and task.
func (s *Server) terminalAuthorized(ctx context.Context, oid, user, session string) bool {
	var count int
	e := s.Store.DB.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM cp_node_operations o JOIN cp_operation_access a ON a.token_hash=o.access_hash JOIN cp_sessions s ON s.token_hash=a.session_hash JOIN cp_users u ON u.id=o.user_id WHERE o.id=? AND o.user_id=? AND o.kind='terminal' AND o.status IN ('pending','running') AND o.expires_at>? AND a.expires_at>? AND s.expires_at>? AND a.session_hash=? AND u.disabled=0 AND u.role='admin'`), oid, user, time.Now().Unix(), time.Now().Unix(), time.Now().Unix(), session).Scan(&count)
	return e == nil && count == 1
}

func validCommand(command string) bool {
	return len(command) > 0 && len(command) <= 4096 && !strings.ContainsRune(command, 0)
}
