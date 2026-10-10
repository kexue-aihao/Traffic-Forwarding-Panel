package platform

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

type policyPreviewRule struct {
	RuleID string `json:"rule_id"`
	NodeID string `json:"node_id"`
	Hash   string `json:"policy_hash,omitempty"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}
type policyPreview struct {
	Version int                 `json:"policy_version"`
	Rules   []policyPreviewRule `json:"rules"`
	Notes   []string            `json:"notes"`
}

func (s *Server) groupPolicyPreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Advanced *contract.GroupAdvanced `json:"advanced"`
	}
	if !decode(w, r, &in) {
		return
	}
	if e := validateGroupAdvanced(in.Advanced); e != nil {
		fail(w, 400, e.Error())
		return
	}
	tx, e := s.Store.DB.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelSerializable})
	if e != nil {
		fail(w, 500, "preview unavailable")
		return
	}
	defer tx.Rollback()
	top, e := s.managedTopology(r.Context(), tx)
	if e != nil {
		fail(w, 500, "preview unavailable")
		return
	}
	g, ok := top.groups[r.PathValue("id")]
	if !ok {
		fail(w, 404, "group not found")
		return
	}
	g.Advanced = in.Advanced
	top.groups[g.ID] = g
	out := policyPreview{Rules: []policyPreviewRule{}, Notes: []string{"Host/SNI 和逐请求 Path 作用于 TCP；HTTPS 的 Path 无法通过 TLS 透传读取。", "UDP over TCP 需要出口；直连 UDP 保持原生传输。", "IPv6 优先仅影响已授权组对端；单目标故障转移无法创造备用地址。", "反向路线需要本地证书 profile、托管 hub 和两端服务就绪。"}}
	if in.Advanced != nil {
		out.Version = in.Advanced.PolicyVersion
	}
	rows, e := tx.QueryContext(r.Context(), `SELECT r.payload,u.role FROM cp_rules r JOIN cp_users u ON u.id=r.user_id WHERE r.deleted=0 ORDER BY r.id`)
	if e != nil {
		fail(w, 500, "preview unavailable")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var raw, role string
		if rows.Scan(&raw, &role) != nil {
			fail(w, 500, "preview unavailable")
			return
		}
		var rule contract.Rule
		if json.Unmarshal([]byte(raw), &rule) != nil {
			fail(w, 500, "preview unavailable")
			return
		}
		exit := top.groups[rule.ExitGroupID]
		if rule.GroupID != g.ID && rule.ExitGroupID != g.ID && !slices.Contains(exit.ChainGroupIDs, g.ID) {
			continue
		}
		n := top.nodes[rule.NodeID]
		p := policyPreviewRule{RuleID: rule.ID, NodeID: rule.NodeID, Status: "preview"}
		if err := compileGroupPolicy(&rule, role, top.groups, n.Capabilities); err != nil {
			p.Status = "blocked"
			p.Reason = err.Error()
		} else if rule.EffectivePolicy != nil {
			p.Hash = rule.EffectivePolicy.Hash
			if err := previewManagedRoute(rule, top); err != nil {
				p.Status, p.Reason = "blocked", err.Error()
			}
		} else {
			p.Status = "legacy"
		}
		out.Rules = append(out.Rules, p)
	}
	if rows.Err() != nil {
		fail(w, 500, "preview unavailable")
		return
	}
	reply(w, 200, out)
}

func previewManagedRoute(rule contract.Rule, top managedTopology) error {
	ids := []string{rule.ExitGroupID}
	if top.groups[rule.ExitGroupID].Type == contract.GroupChainExit {
		ids = top.groups[rule.ExitGroupID].ChainGroupIDs
	}
	for _, groupID := range ids {
		if groupID == "" {
			continue
		}
		var reason error
		valid := false
		for _, x := range top.exits {
			if x.GroupID != groupID || !x.Enabled || x.ReverseHub || x.NodeID == rule.NodeID || len(ids) == 1 && rule.ExitID != "" && rule.ExitID != "auto" && rule.ExitID != x.ID {
				continue
			}
			if _, err := top.route(rule, x, false); err == nil {
				valid = true
				break
			} else {
				reason = err
			}
		}
		if !valid {
			if reason != nil {
				return reason
			}
			return errors.New("authorized_exit_resource_missing")
		}
	}
	return nil
}
