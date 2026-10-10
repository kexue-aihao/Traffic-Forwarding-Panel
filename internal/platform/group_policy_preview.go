package platform

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

type policyPreviewRule struct {
	RuleID     string                    `json:"rule_id"`
	NodeID     string                    `json:"node_id"`
	Hash       string                    `json:"policy_hash,omitempty"`
	Status     string                    `json:"status"`
	Reason     string                    `json:"reason,omitempty"`
	Inspection []policyPreviewInspection `json:"inspection,omitempty"`
}
type policyPreviewInspection struct {
	Protocol string `json:"protocol"`
	Network  string `json:"network"`
	Mode     string `json:"mode"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
}
type policyPreviewProfiles struct {
	NodeID   string                             `json:"node_id"`
	Name     string                             `json:"name"`
	Profiles []contract.InspectionProfileStatus `json:"profiles"`
}
type policyPreview struct {
	Version  int                     `json:"policy_version"`
	Rules    []policyPreviewRule     `json:"rules"`
	Notes    []string                `json:"notes"`
	Profiles []policyPreviewProfiles `json:"inspection_profiles"`
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
	g = splitGroupPolicy(g)
	top.groups[g.ID] = g
	out := policyPreview{Rules: []policyPreviewRule{}, Notes: []string{"Host/SNI 和逐请求 Path 作用于 TCP；HTTPS 的 Path 无法通过 TLS 透传读取。", "UDP over TCP 需要出口；直连 UDP 保持原生传输。", "IPv6 优先仅影响已授权组对端；单目标故障转移无法创造备用地址。", "反向路线需要本地证书 profile、托管 hub 和两端服务就绪。"}}
	out.Profiles = []policyPreviewProfiles{}
	for _, node := range top.nodes {
		if slices.Contains(node.GroupIDs, g.ID) {
			out.Profiles = append(out.Profiles, policyPreviewProfiles{NodeID: node.ID, Name: node.Name, Profiles: append([]contract.InspectionProfileStatus{}, node.InspectionProfiles...)})
		}
	}
	slices.SortFunc(out.Profiles, func(a, b policyPreviewProfiles) int { return strings.Compare(a.NodeID, b.NodeID) })
	out.Notes = append(out.Notes, "Shadowsocks/VMess 只确认所选本地凭据及支持版本；Trojan 需受控业务 TLS 终止，透传 TLS 内层不可见。", "strict 要求已声明范围的能力/profile 就绪，否则规则停止；observe 只观测，不承诺阻断。", "未知应用默认允许；unknown=deny 是独立的未知流量拒绝策略，会影响普通未知业务。", "SOCKS5 UDP 需受控 TCP UDP ASSOCIATE 关联或显式选择本地结构模式；结构首部不能当作认证确认，出口不能证明原客户端控制关联。", "手工或未托管出口只能确认入口检测；出口独立检测需托管服务与双方能力/profile 就绪。")
	if v := ingressSettings(g); v != nil && v.Enabled {
		out.Notes = append(out.Notes, "共享 TLS 入口按业务 SNI 路由；目标证书须覆盖该域名，不提供默认目标。修改监听地址会关闭共享连接，旧端口等待节点确认后释放。")
	}
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
		if layer := groupLayer(g); layer.Inspection != nil {
			for _, app := range layer.BlockedApps {
				v := policyPreviewInspection{Protocol: app, Network: rule.Network, Mode: layer.Inspection.Mode, Status: "declared_scope"}
				if v.Mode == "" {
					v.Mode = "strict"
				}
				copy := *layer.Inspection
				copy.Mode = "strict"
				probe := layer
				probe.BlockedApps = []string{app}
				probe.Inspection = &copy
				if err := inspectionCapabilities(probe, n, rule.Network); err != nil {
					v.Status = "unavailable"
					v.Reason = err.Error()
				}
				if v.Mode == "observe" {
					v.Status = "observe"
				}
				p.Inspection = append(p.Inspection, v)
			}
		}
		if err := compileGroupPolicy(&rule, role, top.groups, n.Capabilities, n.InspectionProfiles); err != nil {
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
