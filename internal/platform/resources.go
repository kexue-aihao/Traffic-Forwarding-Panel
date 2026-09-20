package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
func (s *Server) groups(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	where := ""
	args := []any{}
	if u.Role != "admin" {
		where = " WHERE EXISTS(SELECT 1 FROM cp_group_users gu WHERE gu.group_id=g.id AND gu.user_id=?)"
		args = append(args, u.ID)
	}
	n, o := pages(r)
	var total int
	if s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_groups g"+where), args...).Scan(&total) != nil {
		fail(w, 500, "query failed")
		return
	}
	args = append(args, n, o)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.q("SELECT payload FROM cp_groups g"+where+" ORDER BY id LIMIT ? OFFSET ?"), args...)
	if e != nil {
		fail(w, 500, "query failed")
		return
	}
	defer rows.Close()
	items := []contract.Group{}
	for rows.Next() {
		var p string
		var g contract.Group
		if rows.Scan(&p) != nil || json.Unmarshal([]byte(p), &g) != nil {
			fail(w, 500, "query failed")
			return
		}
		if u.Role != "admin" {
			g.UserIDs = nil
		}
		items = append(items, g)
	}
	reply(w, 200, map[string]any{"items": items, "total": total})
}
func (s *Server) saveGroup(w http.ResponseWriter, r *http.Request) {
	var g contract.Group
	if !decode(w, r, &g) {
		return
	}
	if strings.TrimSpace(g.Name) == "" || len(g.Name) > 190 || g.PortMin < 1 || g.PortMax > 65535 || g.PortMax < g.PortMin {
		fail(w, 400, "invalid group name or port range")
		return
	}
	if g.Multiplier == "" {
		g.Multiplier = "1"
	}
	m, ok := new(big.Rat).SetString(g.Multiplier)
	if !ok || m.Sign() <= 0 {
		fail(w, 400, "invalid multiplier")
		return
	}
	for _, p := range g.BlockedProtocols {
		if !contains([]string{"tcp", "udp", "direct", "tls", "ws", "wss", "http"}, p) {
			fail(w, 400, "unsupported blocked protocol")
			return
		}
	}
	actor, _ := UserFromContext(r.Context())
	oldVersion := g.Version
	g.ID = r.PathValue("id")
	create := g.ID == ""
	if create {
		g.ID = id()
		g.Version = 1
	} else {
		g.Version++
	}
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		if create {
			_, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_groups(id,name,payload,version) VALUES(?,?,?,?)`), g.ID, g.Name, strJSON(g), g.Version)
			if e != nil {
				return e
			}
		} else {
			res, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_groups SET name=?,payload=?,version=? WHERE id=? AND version=?`), g.Name, strJSON(g), g.Version, g.ID, oldVersion)
			if e != nil {
				return e
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return errConflict
			}
			if _, e = tx.ExecContext(r.Context(), s.q(`DELETE FROM cp_group_users WHERE group_id=?`), g.ID); e != nil {
				return e
			}
		}
		for _, uid := range g.UserIDs {
			if _, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_group_users(group_id,user_id) VALUES(?,?)`), g.ID, uid); e != nil {
				return e
			}
		}
		if _, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id IN(SELECT node_id FROM cp_node_groups WHERE group_id=?)`), g.ID); e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "group.save", g.ID)
	})
	if e != nil {
		fail(w, 409, "group conflict or unknown member")
		return
	}
	status := 200
	if create {
		status = 201
	}
	reply(w, status, g)
}
func (s *Server) rules(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	where := " WHERE deleted=0"
	args := []any{}
	if u.Role != "admin" {
		where += " AND user_id=?"
		args = append(args, u.ID)
	}
	n, o := pages(r)
	var total int
	if s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_rules"+where), args...).Scan(&total) != nil {
		fail(w, 500, "query failed")
		return
	}
	args = append(args, n, o)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.q("SELECT payload FROM cp_rules"+where+" ORDER BY id LIMIT ? OFFSET ?"), args...)
	if e != nil {
		fail(w, 500, "query failed")
		return
	}
	defer rows.Close()
	items := []contract.Rule{}
	for rows.Next() {
		var p string
		var rule contract.Rule
		if rows.Scan(&p) != nil || json.Unmarshal([]byte(p), &rule) != nil {
			fail(w, 500, "query failed")
			return
		}
		redact(&rule)
		items = append(items, rule)
	}
	reply(w, 200, map[string]any{"items": items, "total": total})
}
func redact(rule *contract.Rule) {
	rule.Lease = nil
	if rule.Tunnel != nil {
		rule.Tunnel.Token = ""
	}
}
func validateRule(rule contract.Rule) (int, error) {
	if len(rule.Name) > 190 || strings.TrimSpace(rule.Name) == "" || !contains([]string{"tcp", "udp"}, rule.Network) || !contains([]string{"direct", "tls", "ws", "wss", "http"}, rule.Transport) {
		return 0, errors.New("invalid name, network or transport")
	}
	host, p, e := net.SplitHostPort(rule.Listen)
	if e != nil {
		return 0, errors.New("listen must be IP:port")
	}
	if host != "" && net.ParseIP(host) == nil {
		return 0, errors.New("listen must use an IP address")
	}
	port, e := strconv.Atoi(p)
	if e != nil || port < 1 || port > 65535 {
		return 0, errors.New("invalid port")
	}
	h, tp, e := net.SplitHostPort(rule.Target)
	if e != nil || h == "" {
		return 0, errors.New("target must be host:port")
	}
	pn, e := strconv.Atoi(tp)
	if e != nil || pn < 1 || pn > 65535 {
		return 0, errors.New("invalid target port")
	}
	if rule.Transport != "direct" && (rule.Tunnel == nil || rule.Tunnel.Endpoint == "" || rule.Tunnel.Token == "") {
		return 0, errors.New("tunnel endpoint and credential required")
	}
	return port, nil
}
func (s *Server) saveRule(w http.ResponseWriter, r *http.Request) {
	var rule contract.Rule
	if !decode(w, r, &rule) {
		return
	}
	actor, _ := UserFromContext(r.Context())
	rule.ID = r.PathValue("id")
	create := rule.ID == ""
	if create {
		rule.ID = id()
	}
	if actor.Role != "admin" || rule.UserID == "" {
		rule.UserID = actor.ID
	}
	rule.Lease = nil
	oldVersion := rule.Version
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		var old contract.Rule
		if !create {
			var payload string
			var deleted int
			if e := tx.QueryRowContext(r.Context(), s.q(`SELECT payload,deleted FROM cp_rules WHERE id=?`), rule.ID).Scan(&payload, &deleted); e != nil {
				return e
			}
			if e := json.Unmarshal([]byte(payload), &old); e != nil {
				return e
			}
			if deleted != 0 || old.Version != oldVersion || (actor.Role != "admin" && old.UserID != actor.ID) {
				return errConflict
			}
			if rule.NodeID != old.NodeID || rule.GroupID != old.GroupID || rule.UserID != old.UserID || rule.Listen != old.Listen || rule.Network != old.Network {
				return errors.New("listener, owner and placement are immutable; delete and recreate after ACK")
			}
			if rule.Tunnel != nil && rule.Tunnel.Token == "" && old.Tunnel != nil && rule.Tunnel.Endpoint == old.Tunnel.Endpoint {
				rule.Tunnel.Token = old.Tunnel.Token
			}
			rule.Lease = old.Lease
		}
		port, e := validateRule(rule)
		if e != nil {
			return e
		}
		var payload string
		if e = tx.QueryRowContext(r.Context(), s.q(`SELECT g.payload FROM cp_groups g JOIN cp_node_groups ng ON ng.group_id=g.id WHERE g.id=? AND ng.node_id=?`), rule.GroupID, rule.NodeID).Scan(&payload); e != nil {
			return errors.New("node not in group")
		}
		var g contract.Group
		if e = json.Unmarshal([]byte(payload), &g); e != nil {
			return e
		}
		if actor.Role != "admin" && !contains(g.UserIDs, actor.ID) {
			return errors.New("group not authorized")
		}
		if port < g.PortMin || port > g.PortMax || contains(g.BlockedProtocols, rule.Network) || contains(g.BlockedProtocols, rule.Transport) {
			return errors.New("group policy denied")
		}
		for _, p := range rule.BlockedProtocols {
			if !contains([]string{"tcp", "udp", "direct", "tls", "ws", "wss", "http"}, p) {
				return errors.New("unsupported rule protocol policy")
			}
		}
		rule.BlockedProtocols = append(rule.BlockedProtocols, g.BlockedProtocols...)
		if create {
			rule.Version = 1
		} else {
			rule.Version = oldVersion + 1
		}
		if rule.Enabled && rule.Lease == nil {
			if s.opts.Entitlements != nil {
				if a, ok := s.opts.Entitlements.(interface {
					AllocateWithMultiplier(context.Context, *sql.Tx, string, string, string, string) (*contract.Lease, error)
				}); ok {
					rule.Lease, e = a.AllocateWithMultiplier(r.Context(), tx, rule.UserID, rule.ID, rule.NodeID, g.Multiplier)
				} else {
					if g.Multiplier != "1" {
						return errors.New("multiplier allocator required")
					}
					rule.Lease, e = s.opts.Entitlements.Allocate(r.Context(), tx, rule.UserID, rule.ID, rule.NodeID)
				}
			} else if actor.Role == "admin" && s.opts.AdminTestBytes > 0 {
				rule.Lease = &contract.Lease{ID: id(), EntitlementID: "admin-test", ExpiresAt: time.Now().UTC().Add(24 * time.Hour), Bytes: s.opts.AdminTestBytes}
			} else {
				return errors.New("funded entitlement required")
			}
			if e != nil {
				return e
			}
			if rule.Lease == nil || rule.Lease.Bytes <= 0 || !rule.Lease.ExpiresAt.After(time.Now()) || rule.Lease.ExpiresAt.After(time.Now().Add(24*time.Hour+time.Second)) {
				return errors.New("invalid finite lease")
			}
			_, e = tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_rule_leases(id,rule_id,node_id,entitlement_id,bytes_allocated,bytes_used,expires_at) VALUES(?,?,?,?,?,0,?)`), rule.Lease.ID, rule.ID, rule.NodeID, rule.Lease.EntitlementID, rule.Lease.Bytes, rule.Lease.ExpiresAt.Unix())
			if e != nil {
				return e
			}
		}
		if create {
			_, e = tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_rules(id,user_id,node_id,group_id,payload,version,deleted,release_version) VALUES(?,?,?,?,?,?,0,0)`), rule.ID, rule.UserID, rule.NodeID, rule.GroupID, strJSON(rule), rule.Version)
			if e != nil {
				return e
			}
			_, e = tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_ports(node_id,network,port,rule_id) VALUES(?,?,?,?)`), rule.NodeID, rule.Network, port, rule.ID)
			if e != nil {
				return errors.New("physical machine port reserved (all IP addresses)")
			}
		} else {
			res, err := tx.ExecContext(r.Context(), s.q(`UPDATE cp_rules SET payload=?,version=? WHERE id=? AND version=? AND deleted=0`), strJSON(rule), rule.Version, rule.ID, oldVersion)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return errConflict
			}
		}
		if _, e = tx.ExecContext(r.Context(), s.q(`UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id=?`), rule.NodeID); e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "rule.save", rule.ID)
	})
	if e != nil {
		fail(w, 409, e.Error())
		return
	}
	redact(&rule)
	status := 200
	if create {
		status = 201
	}
	reply(w, status, rule)
}
func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	version, e := strconv.ParseInt(r.URL.Query().Get("version"), 10, 64)
	if e != nil {
		fail(w, 400, "version required")
		return
	}
	e = s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		var owner, node string
		if e := tx.QueryRowContext(r.Context(), s.q(`SELECT user_id,node_id FROM cp_rules WHERE id=? AND version=? AND deleted=0`), r.PathValue("id"), version).Scan(&owner, &node); e != nil {
			return e
		}
		if actor.Role != "admin" && actor.ID != owner {
			return errConflict
		}
		if _, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id=?`), node); e != nil {
			return e
		}
		var revision int64
		if e := tx.QueryRowContext(r.Context(), s.q(`SELECT desired_version FROM cp_nodes WHERE id=?`), node).Scan(&revision); e != nil {
			return e
		}
		res, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_rules SET deleted=1,version=version+1,release_version=? WHERE id=? AND version=? AND deleted=0`), revision, r.PathValue("id"), version)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errConflict
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "rule.delete", r.PathValue("id"))
	})
	if e != nil {
		fail(w, 409, "delete conflict")
		return
	}
	w.WriteHeader(204)
}
