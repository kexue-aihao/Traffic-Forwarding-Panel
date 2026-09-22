package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// nodeOnlineWindow 是「这台机器还在线吗」的判定窗口。Agent 的心跳写入是按
// 分钟节流的（见 probe 处理里的 heartbeat 限流），所以窗口要比一分钟宽出
// 余量，否则在线机器会周期性地显示成离线。
const nodeOnlineWindow = 150 * time.Second

var errProbeRequired = errors.New("entitlement required")

// deviceIP / deviceIPList 是探针页面对外预留的地址接口。
//
// 客户脚本要的是「我那组机器现在连哪个地址」。一组只有一台机器时看
// /online/device/ip，多台时看 /online/device/ip/list —— 两个接口读同一份
// 数据，区别只在单台时的形状：单台返回一台，多台仍然是列表。
//
// 机器被替换（node_id 变了）或只换了 IP（address/observed_at 变了）都体现在
// 同一份列表里，客户不需要为这两种情况分别写代码。
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
	query := `SELECT n.id,n.payload,n.last_seen,g.id,g.payload FROM cp_nodes n JOIN cp_node_groups ng ON ng.node_id=n.id JOIN cp_groups g ON g.id=ng.group_id`
	args := []any{}
	if u.Role != "admin" {
		query += ` WHERE EXISTS(SELECT 1 FROM cp_group_users gu WHERE gu.group_id=ng.group_id AND gu.user_id=?)`
		args = append(args, u.ID)
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
	items := []contract.DeviceIP{}
	seen := map[string]bool{}
	for rows.Next() {
		var nodeID, nodePayload, groupID, groupPayload string
		var lastSeen int64
		if e = rows.Scan(&nodeID, &nodePayload, &lastSeen, &groupID, &groupPayload); e != nil {
			return nil, e
		}
		// 一台机器可能同时属于多个组，而列表是按组展示的：用节点+组做键去重，
		// 保证每个分组里各出现一次。
		if seen[nodeID+"\x00"+groupID] {
			continue
		}
		seen[nodeID+"\x00"+groupID] = true
		var node contract.Node
		json.Unmarshal([]byte(nodePayload), &node)
		var g contract.Group
		json.Unmarshal([]byte(groupPayload), &g)
		item := contract.DeviceIP{NodeID: nodeID, NodeName: node.Name, GroupID: groupID, GroupName: g.Name}
		if lastSeen > 0 {
			at := time.Unix(lastSeen, 0).UTC()
			item.LastSeen = &at
			item.Online = time.Since(at) <= nodeOnlineWindow
		}
		if ip, family, source, at, ok := s.latestAddress(nodeID); ok {
			item.Address, item.Family, item.Source, item.ObservedAt = ip, family, source, at
			item.Location = s.locationFor(ctx, ip)
		}
		items = append(items, item)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	// 稳定顺序：先按分组，再按节点名。客户脚本按序取用不会因为 map 顺序抖动。
	sort.Slice(items, func(i, j int) bool {
		if items[i].GroupID != items[j].GroupID {
			return items[i].GroupID < items[j].GroupID
		}
		if items[i].NodeName != items[j].NodeName {
			return items[i].NodeName < items[j].NodeName
		}
		return items[i].NodeID < items[j].NodeID
	})
	return items, nil
}

// latestAddress 取一台机器最近一次观测到的对外地址。
//
// 优先 IPv4：客户拿这个地址去连服务，而多数入口只监听 v4；机器同时有 v6 时
// 也仍然把 v6 观测留在探针页面上。两者都没有就返回 false —— 那表示这台机器
// 还没有上报过地址，而不是「地址是空字符串」。
func (s *Server) latestAddress(node string) (string, string, string, time.Time, bool) {
	s.mu.RLock()
	probe, ok := s.probes[node]
	s.mu.RUnlock()
	if !ok || len(probe.PublicIPs) == 0 {
		return "", "", "", time.Time{}, false
	}
	best := contract.IPObservation{}
	found := false
	for _, ip := range probe.PublicIPs {
		if ip.Address == "" {
			continue
		}
		if !found || newerAddress(ip, best) {
			best, found = ip, true
		}
	}
	if !found {
		return "", "", "", time.Time{}, false
	}
	at := best.ObservedAt
	if at.IsZero() {
		at = probe.SampledAt
	}
	return best.Address, best.Family, best.Source, at.UTC(), true
}

// newerAddress 是地址的择优顺序：先看是不是 IPv4，再看观测时间。
func newerAddress(candidate, best contract.IPObservation) bool {
	if (candidate.Family == "ipv4") != (best.Family == "ipv4") {
		return candidate.Family == "ipv4"
	}
	return candidate.ObservedAt.After(best.ObservedAt)
}
