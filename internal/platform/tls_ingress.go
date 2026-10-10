package platform

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"golang.org/x/net/idna"
)

func managedTLS(r contract.Rule) bool { return r.SharedTLS != nil && r.SharedTLS.IngressID != "" }
func sharesPort(r contract.Rule) bool { return managedTLS(r) || sharedParent(r) != "" }
func ingressID(group, node string) string {
	h := sha256.Sum256([]byte(group + "\x00" + node))
	return hex.EncodeToString(h[:16])
}
func ingressSettings(g contract.Group) *contract.SharedTLSIngressSettings {
	if g.CanEnter() && activeAdvanced(g) {
		return g.Advanced.SharedTLSIngress
	}
	return nil
}

func hydrateTLSListen(r *contract.Rule, g contract.Group) {
	if managedTLS(*r) && g.Advanced != nil && g.Advanced.SharedTLSIngress != nil {
		v := g.Advanced.SharedTLSIngress
		r.Listen = net.JoinHostPort(v.ListenIP, strconv.Itoa(v.Port))
	}
}
func validateIngressSettings(a *contract.GroupAdvanced) error {
	if a == nil || a.SharedTLSIngress == nil {
		return nil
	}
	v := a.SharedTLSIngress
	if v.ListenIP == "" {
		v.ListenIP = "0.0.0.0"
	}
	if v.Port == 0 {
		v.Port = 443
	}
	ip := net.ParseIP(v.ListenIP)
	if ip == nil || v.Port < 1 || v.Port > 65535 {
		return errors.New("shared TLS ingress requires an IP and port 1–65535")
	}
	v.ListenIP = ip.String()
	return nil
}

// All port owners use the same physical-node lock. Reservations remain until
// the node acknowledges removal, including while a listener has no rules.
func (s *Server) syncTLSIngresses(ctx context.Context, tx *sql.Tx) error {
	groups, err := loadPolicyGroups(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM cp_nodes ORDER BY id")
	if err != nil {
		return err
	}
	var nodes []string
	for rows.Next() {
		var n string
		if err = rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		nodes = append(nodes, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, node := range nodes {
		q := "SELECT id FROM cp_nodes WHERE id=?"
		if s.Store.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		var locked string
		if err = tx.QueryRowContext(ctx, s.q(q), node).Scan(&locked); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, s.q("SELECT group_id FROM cp_node_groups WHERE node_id=? ORDER BY group_id"), node)
		if err != nil {
			return err
		}
		wanted := map[string]string{}
		for rows.Next() {
			var gid string
			if err = rows.Scan(&gid); err != nil {
				rows.Close()
				return err
			}
			if v := ingressSettings(groups[gid]); v != nil && v.Enabled {
				wanted[ingressID(gid, node)] = gid
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, s.q("SELECT id,group_id,listen,enabled FROM cp_tls_ingresses WHERE node_id=?"), node)
		if err != nil {
			return err
		}
		type saved struct {
			id, group, listen string
			enabled           int
		}
		var old []saved
		for rows.Next() {
			var v saved
			if err = rows.Scan(&v.id, &v.group, &v.listen, &v.enabled); err != nil {
				rows.Close()
				return err
			}
			old = append(old, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		exists := map[string]bool{}
		for _, v := range old {
			exists[v.id] = true
			gid, keep := wanted[v.id]
			listen := ""
			if keep {
				conf := ingressSettings(groups[gid])
				listen = net.JoinHostPort(conf.ListenIP, strconv.Itoa(conf.Port))
			}
			if v.enabled != 0 && (!keep || listen != v.listen) {
				if _, err = tx.ExecContext(ctx, s.q("UPDATE cp_ingress_ports SET release_version=(SELECT desired_version+1 FROM cp_nodes WHERE id=?) WHERE ingress_id=? AND release_version=0"), node, v.id); err != nil {
					return err
				}
			}
			if !keep {
				if _, err = tx.ExecContext(ctx, s.q("UPDATE cp_tls_ingresses SET enabled=0 WHERE id=?"), v.id); err != nil {
					return err
				}
			}
		}
		// Sort physical port acquisition to make concurrent group writes deterministic.
		for _, gid := range sortedIngressGroups(wanted) {
			id := ingressID(gid, node)
			conf := ingressSettings(groups[gid])
			listen := net.JoinHostPort(conf.ListenIP, strconv.Itoa(conf.Port))
			var count int
			if err = tx.QueryRowContext(ctx, s.q("SELECT (SELECT COUNT(*) FROM cp_ports WHERE node_id=? AND network='tcp' AND port=?)+(SELECT COUNT(*) FROM cp_exit_ports WHERE node_id=? AND network='tcp' AND port=?)+(SELECT COUNT(*) FROM cp_ingress_ports WHERE node_id=? AND network='tcp' AND port=? AND ingress_id<>?)"), node, conf.Port, node, conf.Port, node, conf.Port, id).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("shared TLS ingress port already reserved on physical node")
			}
			var own int
			if err = tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM cp_ingress_ports WHERE node_id=? AND network='tcp' AND port=?"), node, conf.Port).Scan(&own); err != nil {
				return err
			}
			if own == 0 {
				_, err = tx.ExecContext(ctx, s.q("INSERT INTO cp_ingress_ports(node_id,network,port,ingress_id,release_version) VALUES(?,'tcp',?,?,0)"), node, conf.Port, id)
			} else {
				_, err = tx.ExecContext(ctx, s.q("UPDATE cp_ingress_ports SET release_version=0 WHERE node_id=? AND network='tcp' AND port=? AND ingress_id=?"), node, conf.Port, id)
			}
			if err != nil {
				return err
			}
			if exists[id] {
				_, err = tx.ExecContext(ctx, s.q("UPDATE cp_tls_ingresses SET listen=?,enabled=1 WHERE id=?"), listen, id)
			} else {
				_, err = tx.ExecContext(ctx, s.q("INSERT INTO cp_tls_ingresses(id,group_id,node_id,listen,enabled) VALUES(?,?,?,?,1)"), id, gid, node, listen)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedIngressGroups(wanted map[string]string) []string {
	out := make([]string, 0, len(wanted))
	for _, g := range wanted {
		out = append(out, g)
	}
	slices.Sort(out)
	return out
}

func (s *Server) bindTLSIngress(ctx context.Context, tx *sql.Tx, r *contract.Rule, g contract.Group) error {
	if !managedTLS(*r) {
		return nil
	}
	v := ingressSettings(g)
	if v == nil || !v.Enabled || r.Network != "tcp" || r.SharedTLS.ParentID != "" {
		return errors.New("shared TLS ingress unavailable")
	}
	id := ingressID(g.ID, r.NodeID)
	if r.SharedTLS.IngressID != "group" && r.SharedTLS.IngressID != id {
		return errors.New("shared TLS ingress belongs to another placement")
	}
	listen := net.JoinHostPort(v.ListenIP, strconv.Itoa(v.Port))
	if r.Listen != "" && r.Listen != listen {
		return errors.New("shared TLS listen address is generated by the platform")
	}
	var found string
	if err := tx.QueryRowContext(ctx, s.q("SELECT listen FROM cp_tls_ingresses WHERE id=? AND node_id=? AND group_id=? AND enabled=1"), id, r.NodeID, g.ID).Scan(&found); err != nil || found != listen {
		return errors.New("shared TLS ingress not reserved")
	}
	name, err := idna.Lookup.ToASCII(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.SharedTLS.ServerName)), "."))
	if err != nil || !contract.ValidServerName(name) {
		return errors.New("shared TLS requires an exact business SNI")
	}
	r.SharedTLS.IngressID, r.SharedTLS.ServerName, r.Listen = id, name, listen
	return nil
}

func (s *Server) compileTLSIngresses(ctx context.Context, tx *sql.Tx, cfg *contract.Config, caps []string) error {
	if !contains(caps, "shared-tls-ingress-v1") {
		return nil
	}
	rows, err := tx.QueryContext(ctx, s.q("SELECT i.id,i.group_id,i.listen FROM cp_tls_ingresses i JOIN cp_node_groups ng ON ng.node_id=i.node_id AND ng.group_id=i.group_id WHERE i.node_id=? AND i.enabled=1 ORDER BY i.id"), cfg.NodeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		v := contract.SharedTLSIngress{NodeID: cfg.NodeID}
		if err = rows.Scan(&v.ID, &v.GroupID, &v.Listen); err != nil {
			return err
		}
		cfg.TLSIngresses = append(cfg.TLSIngresses, v)
	}
	return rows.Err()
}
