package platform

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/http"
	"reflect"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func (s *Server) exits(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	n, o := pages(r)
	where := ""
	args := []any{}
	if actor.Role != "admin" {
		where = " WHERE EXISTS(SELECT 1 FROM cp_group_identity_groups gig JOIN cp_users iu ON iu.identity_group_id=gig.identity_group_id WHERE gig.group_id=e.group_id AND iu.id=?)"
		args = append(args, actor.ID)
	}
	var total int
	if err := s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_exits e"+where), args...).Scan(&total); err != nil {
		fail(w, 500, "exit query failed")
		return
	}
	args = append(args, n, o)
	rows, err := s.Store.DB.QueryContext(r.Context(), s.q("SELECT e.payload,n.last_seen FROM cp_exits e JOIN cp_nodes n ON n.id=e.node_id"+where+" ORDER BY e.id LIMIT ? OFFSET ?"), args...)
	if err != nil {
		fail(w, 500, "exit query failed")
		return
	}
	defer rows.Close()
	items := []contract.Exit{}
	for rows.Next() {
		var raw string
		var seen int64
		var e contract.Exit
		if rows.Scan(&raw, &seen) != nil || json.Unmarshal([]byte(raw), &e) != nil {
			fail(w, 500, "exit query failed")
			return
		}
		e.Online = seen > 0 && time.Now().Unix()-seen <= 90
		e.Tunnel.Token = ""
		for i := range e.Tunnel.Chain {
			e.Tunnel.Chain[i].Token = ""
		}
		if actor.Role != "admin" {
			e.Tunnel = contract.Tunnel{}
		}
		items = append(items, e)
	}
	if rows.Err() != nil {
		fail(w, 500, "exit query failed")
		return
	}
	reply(w, 200, map[string]any{"items": items, "total": total})
}
func (s *Server) saveExit(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	var e contract.Exit
	if !decode(w, r, &e) {
		return
	}
	if e.Name == "" || len(e.Name) > 190 || e.Weight < 1 || e.Weight > 100 {
		fail(w, 400, "name and weight 1-100 required")
		return
	}
	e.ID = r.PathValue("id")
	create := e.ID == ""
	if create {
		e.ID = id()
	}
	expected := e.Version
	e.Version++
	if create {
		e.Version = 1
	}
	e.Online = false
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		group, err := s.groupTx(r.Context(), tx, e.GroupID)
		if err != nil {
			return err
		}
		if !group.CanHostExit() {
			return errors.New("出口节点只能关联出口类型的设备组")
		}
		if group.Type == contract.GroupExit && len(e.Tunnel.Chain) > 0 {
			return errors.New("多跳路由请配置链式出口设备组")
		}
		var member int
		if err := tx.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_node_groups WHERE node_id=? AND group_id=?"), e.NodeID, e.GroupID).Scan(&member); err != nil {
			return err
		}
		if member != 1 {
			return errors.New("exit node must belong to exit group")
		}
		if !create {
			var raw string
			if err := tx.QueryRowContext(r.Context(), s.q("SELECT payload FROM cp_exits WHERE id=?"), e.ID).Scan(&raw); err != nil {
				return err
			}
			var old contract.Exit
			if err := json.Unmarshal([]byte(raw), &old); err != nil {
				return err
			}
			if e.Tunnel.Token == "" && e.Tunnel.Endpoint == old.Tunnel.Endpoint && e.Tunnel.ServerName == old.Tunnel.ServerName {
				e.Tunnel.Token = old.Tunnel.Token
			}
			for i, h := range e.Tunnel.Chain {
				if i < len(old.Tunnel.Chain) && h.Token == "" && h.Endpoint == old.Tunnel.Chain[i].Endpoint && h.ServerName == old.Tunnel.Chain[i].ServerName {
					e.Tunnel.Chain[i].Token = old.Tunnel.Chain[i].Token
				}
			}
		}
		if err := tunnel.ValidateChain(contract.TunnelHop{Transport: e.Transport, Endpoint: e.Tunnel.Endpoint, ServerName: e.Tunnel.ServerName, Token: e.Tunnel.Token}, e.Tunnel.Chain); err != nil {
			return err
		}
		if err := (contract.Rule{Network: "tcp", Tunnel: &e.Tunnel}).ValidateAdvanced(); err != nil {
			return err
		}
		if create {
			if _, err := tx.ExecContext(r.Context(), s.q("INSERT INTO cp_exits(id,group_id,node_id,payload,version) VALUES(?,?,?,?,?)"), e.ID, e.GroupID, e.NodeID, strJSON(e), e.Version); err != nil {
				return err
			}
		} else {
			res, err := tx.ExecContext(r.Context(), s.q("UPDATE cp_exits SET group_id=?,node_id=?,payload=?,version=? WHERE id=? AND version=?"), e.GroupID, e.NodeID, strJSON(e), e.Version, e.ID, expected)
			if err != nil {
				return err
			}
			count, _ := res.RowsAffected()
			if count != 1 {
				return errConflict
			}
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "exit.save", e.ID)
	})
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	e.Tunnel.Token = ""
	for i := range e.Tunnel.Chain {
		e.Tunnel.Chain[i].Token = ""
	}
	status := 200
	if create {
		status = 201
	}
	reply(w, status, e)
}

// Weighted rendezvous selection is stable for a rule until candidate health or
// configuration changes. Liveness is panel heartbeat, not target reachability.
func (s *Server) resolveExitTx(ctx context.Context, tx *sql.Tx, rule *contract.Rule) (string, error) {
	if rule.ExitGroupID == "" {
		rule.SelectedExitID = ""
		return "1", nil
	}
	var role string
	g, err := s.groupTx(ctx, tx, rule.ExitGroupID)
	if err != nil {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, s.q("SELECT role FROM cp_users WHERE id=?"), rule.UserID).Scan(&role); err != nil {
		return "", err
	}
	if role != "admin" {
		authorized, err := s.groupAuthorized(ctx, tx, rule.ExitGroupID, rule.UserID)
		if err != nil || !authorized {
			return "", errors.New("exit group not authorized")
		}
	}
	entry, err := s.groupTx(ctx, tx, rule.GroupID)
	if err != nil {
		return "", err
	}
	if !g.CanExit() || !entry.CanEnter() {
		return "", errors.New("请选择入口设备组和出口或链式出口设备组")
	}
	if g.Type == contract.GroupChainExit {
		if rule.ExitID != "" && rule.ExitID != "auto" {
			return "", errors.New("链式出口按每一跳的出口组自动选择节点")
		}
		if len(g.ChainGroupIDs) < 2 || len(g.ChainGroupIDs) > 3 {
			return "", errors.New("链式出口需要 2–3 跳")
		}
		parts := []contract.Rule{}
		seenGroups := map[string]bool{g.ID: true}
		seenNodes := map[string]bool{rule.NodeID: true}
		for _, gid := range g.ChainGroupIDs {
			hop, err := s.groupTx(ctx, tx, gid)
			if err != nil {
				return "", err
			}
			if !hop.CanHostExit() || seenGroups[gid] {
				return "", errors.New("链式出口必须由不重复的出口设备组组成")
			}
			seenGroups[gid] = true
			part := *rule
			part.ExitGroupID, part.ExitID = gid, "auto"
			if _, err := s.resolveExitTx(ctx, tx, &part); err != nil {
				return "", err
			}
			if part.Tunnel == nil || len(part.Tunnel.Chain) > 0 || part.Tunnel.Reverse != "" {
				return "", errors.New("链式出口的每一跳必须是普通正向出口")
			}
			var nodeID string
			if err := tx.QueryRowContext(ctx, s.q("SELECT node_id FROM cp_exits WHERE id=?"), part.SelectedExitID).Scan(&nodeID); err != nil {
				return "", err
			}
			if seenNodes[nodeID] {
				return "", errors.New("链式出口不能重复经过同一设备")
			}
			seenNodes[nodeID] = true
			parts = append(parts, part)
		}
		resolved := *parts[0].Tunnel
		resolved.Chain = nil
		for _, part := range parts[1:] {
			resolved.Chain = append(resolved.Chain, contract.TunnelHop{Transport: part.Transport, Endpoint: part.Tunnel.Endpoint, ServerName: part.Tunnel.ServerName, Token: part.Tunnel.Token})
		}
		candidate := *rule
		candidate.Transport, candidate.Tunnel = parts[0].Transport, &resolved
		if policyDenied(g, candidate) || entryPolicyDenied(entry, candidate) {
			return "", errors.New("group policy denied")
		}
		if err := tunnel.ValidateChain(contract.TunnelHop{Transport: candidate.Transport, Endpoint: resolved.Endpoint, ServerName: resolved.ServerName, Token: resolved.Token}, resolved.Chain); err != nil {
			return "", err
		}
		if err := candidate.ValidateAdvanced(); err != nil {
			return "", err
		}
		rule.Transport, rule.Tunnel, rule.SelectedExitID = candidate.Transport, candidate.Tunnel, parts[0].SelectedExitID
		return g.Multiplier, nil
	}
	rows, err := tx.QueryContext(ctx, s.q("SELECT e.payload,n.last_seen FROM cp_exits e JOIN cp_nodes n ON n.id=e.node_id JOIN cp_node_groups ng ON ng.node_id=e.node_id AND ng.group_id=e.group_id WHERE e.group_id=? ORDER BY e.id"), g.ID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var best *contract.Exit
	score := math.Inf(1)
	for rows.Next() {
		var raw string
		var seen int64
		var e contract.Exit
		if err = rows.Scan(&raw, &seen); err != nil {
			return "", err
		}
		if err = json.Unmarshal([]byte(raw), &e); err != nil {
			return "", err
		}
		if !e.Enabled || e.NodeID == rule.NodeID || seen == 0 || time.Now().Unix()-seen > 90 || rule.ExitID != "" && rule.ExitID != "auto" && rule.ExitID != e.ID {
			continue
		}
		candidate := *rule
		candidate.Transport = e.Transport
		candidate.Tunnel = &e.Tunnel
		if policyDenied(g, candidate) || entryPolicyDenied(entry, candidate) {
			continue
		}
		h := sha256.Sum256([]byte(rule.ID + ":" + e.ID))
		u := float64(binary.BigEndian.Uint64(h[:8])>>11) + 1
		rank := -math.Log(u/(float64(uint64(1)<<53)+1)) / float64(e.Weight)
		if rank < score {
			value := e
			best = &value
			score = rank
		}
	}
	if rows.Err() != nil {
		return "", rows.Err()
	}
	if best == nil {
		return "", errors.New("no authorized online exit")
	}
	rule.Transport = best.Transport
	rule.Tunnel = &best.Tunnel
	rule.SelectedExitID = best.ID
	return g.Multiplier, nil
}
func effectiveMultiplier(entry, exit string) (string, error) {
	a, ok := new(big.Rat).SetString(entry)
	if !ok || a.Sign() <= 0 {
		return "", errors.New("invalid entry multiplier")
	}
	b, ok := new(big.Rat).SetString(exit)
	if !ok || b.Sign() <= 0 {
		return "", errors.New("invalid exit multiplier")
	}
	a.Mul(a, b)
	if a.Num().BitLen() > 63 || a.Denom().BitLen() > 63 {
		return "", errors.New("multiplier exceeds precision limit")
	}
	return a.RatString(), nil
}

func (s *Server) refreshExits(ctx context.Context, node string) error {
	return s.Store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, s.q("SELECT payload FROM cp_rules WHERE node_id=? AND deleted=0"), node)
		if err != nil {
			return err
		}
		rules := []contract.Rule{}
		for rows.Next() {
			var raw string
			var rule contract.Rule
			if err = rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			if err = json.Unmarshal([]byte(raw), &rule); err != nil {
				rows.Close()
				return err
			}
			if rule.ExitGroupID != "" {
				rules = append(rules, rule)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		changed := false
		for _, old := range rules {
			rule := old
			exit, e := s.resolveExitTx(ctx, tx, &rule)
			rule.ExitUnavailable = e != nil
			if e == nil {
				var raw string
				var g contract.Group
				if err = tx.QueryRowContext(ctx, s.q("SELECT payload FROM cp_groups WHERE id=?"), rule.GroupID).Scan(&raw); err != nil {
					return err
				}
				if err = json.Unmarshal([]byte(raw), &g); err != nil {
					return err
				}
				rule.BillingMultiplier, err = effectiveMultiplier(g.Multiplier, exit)
				if err != nil {
					return err
				}
			}
			if reflect.DeepEqual(old, rule) {
				continue
			}
			rule.Lease = nil
			res, err := tx.ExecContext(ctx, s.q("UPDATE cp_rules SET payload=? WHERE id=? AND version=?"), strJSON(rule), rule.ID, rule.Version)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return errConflict
			}
			changed = true
		}
		if changed {
			_, err = tx.ExecContext(ctx, s.q("UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id=?"), node)
		}
		return err
	})
}
