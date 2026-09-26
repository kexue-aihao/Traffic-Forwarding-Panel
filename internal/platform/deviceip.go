package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

var errProbeRequired = errors.New("entitlement required")

// deviceIP / deviceIPList 是探针页面对外预留的地址接口。
//
// 客户脚本要的是「我那组机器现在连哪个地址」。一台设备一条记录，记录里同时带
// IPv4 与 IPv6 —— 有哪一族给哪一族，两族都有就都给，因此调用方不必先判断这台
// 机器是双栈还是单栈。一组只有一台机器时看 /online/device/ip，多台时看
// /online/device/ip/list：两个接口读同一份数据，区别只在单台时的形状。
//
// 机器被替换或只换了 IP，都体现在同一份列表里，客户不需要为这两种情况分别写
// 代码。
func (s *Server) deviceIP(w http.ResponseWriter, r *http.Request) {
	s.deviceAddresses(w, r, false)
}
func (s *Server) deviceIPList(w http.ResponseWriter, r *http.Request) {
	s.deviceAddresses(w, r, true)
}
func (s *Server) deviceAddresses(w http.ResponseWriter, r *http.Request, list bool) {
	u, _ := UserFromContext(r.Context())
	items, e := s.ownerDeviceIPs(r.Context(), u, r.URL.Query().Get("group_id"))
	if errors.Is(e, errProbeRequired) {
		fail(w, 403, "需要有效的套餐权益才能查看设备地址")
		return
	}
	if e != nil {
		fail(w, 500, "query failed")
		return
	}
	if list {
		reply(w, 200, map[string]any{"items": items, "total": len(items)})
		return
	}
	// 单台接口要的是「唯一那台」。多于一台不是错误数据，而是用错了接口 ——
	// 明确让人改用列表，比随便挑一台返回要好。
	switch len(items) {
	case 0:
		fail(w, 404, "当前分组下没有可见设备")
	case 1:
		reply(w, 200, map[string]any{"device": items[0]})
	default:
		fail(w, 409, "当前分组有多台设备，请使用 /online/device/ip/list")
	}
}

// deviceRow 是排序用的中间形态：组名可能重名，定序的最后一位得靠节点 id，
// 而 id 不上接口。
type deviceRow struct {
	item            contract.DeviceIP
	groupID, nodeID string
}

// ownerDeviceIPs 列出这个身份能看到的设备及其最新地址。
//
// 与探针页面同一套可见性：普通用户必须在组里、且有未过期的权益；管理员不受
// 限制。机器凭据（Bearer）在这里被当作普通用户，所以客户脚本拿到的就是
// 他自己那一组。
func (s *Server) ownerDeviceIPs(ctx context.Context, u contract.User, group string) ([]contract.DeviceIP, error) {
	if u.Role != "admin" && s.opts.ActiveEntitlement != nil {
		ok, e := s.opts.ActiveEntitlement(ctx, u.ID)
		if e != nil {
			return nil, e
		}
		if !ok {
			return nil, errProbeRequired
		}
	}
	query := `SELECT n.id,n.payload,g.id,g.payload FROM cp_nodes n JOIN cp_node_groups ng ON ng.node_id=n.id JOIN cp_groups g ON g.id=ng.group_id`
	args := []any{}
	if u.Role != "admin" {
		query += ` WHERE EXISTS(SELECT 1 FROM cp_group_identity_groups gig JOIN cp_users iu ON iu.identity_group_id=gig.identity_group_id WHERE gig.group_id=ng.group_id AND iu.id=?`
		args = append(args, u.ID)
		query += tokenGroupScope(u, "gig.group_id", &args) + ")"
	}
	if group != "" {
		if len(args) == 0 {
			query += " WHERE g.id=?"
		} else {
			query += " AND g.id=?"
		}
		args = append(args, group)
	}
	rows, e := s.Store.DB.QueryContext(ctx, s.q(query), args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	table := []deviceRow{}
	seen := map[string]bool{}
	for rows.Next() {
		var nodeID, nodePayload, groupID, groupPayload string
		if e = rows.Scan(&nodeID, &nodePayload, &groupID, &groupPayload); e != nil {
			return nil, e
		}
		// 一台机器可能同时属于多个组，而列表是按组展示的：用节点+组做键去重，
		// 保证每个分组里各出现一次。
		if seen[nodeID+"\x00"+groupID] {
			continue
		}
		seen[nodeID+"\x00"+groupID] = true
		// 对外只给设备组名：客户脚本按组认机器，机器名是它自己报上来的主机名。
		var group contract.Group
		json.Unmarshal([]byte(groupPayload), &group)
		row := deviceRow{item: contract.DeviceIP{GroupName: group.Name}, groupID: groupID, nodeID: nodeID}
		// 地址取各家族最近一次的观测：两个都取，不做「优先 IPv4」的取舍 ——
		// 取舍留给调用方，接口把机器实际有的地址照实给出。
		s.mu.RLock()
		probe, ok := s.probes[nodeID]
		s.mu.RUnlock()
		if ok {
			row.item.IPv4 = probeAddressForFamily(probe, "ipv4")
			row.item.IPv6 = probeAddressForFamily(probe, "ipv6")
		}
		table = append(table, row)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	// 稳定顺序：先按分组，再按设备名。客户脚本按序取用不会因为 map 顺序抖动。
	sort.Slice(table, func(i, j int) bool {
		a, b := table[i], table[j]
		if a.groupID != b.groupID {
			return a.groupID < b.groupID
		}
		if a.item.GroupName != b.item.GroupName {
			return a.item.GroupName < b.item.GroupName
		}
		return a.nodeID < b.nodeID
	})
	items := make([]contract.DeviceIP, 0, len(table))
	for _, row := range table {
		items = append(items, row.item)
	}
	return items, nil
}
