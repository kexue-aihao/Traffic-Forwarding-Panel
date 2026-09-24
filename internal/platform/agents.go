package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/httporigin"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

type nodeKey struct{}
type enrollment struct {
	Name     string   `json:"name"`
	GroupIDs []string `json:"group_ids"`
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var in enrollment
	if !decode(w, r, &in) {
		return
	}
	if len(in.GroupIDs) == 0 || len(in.GroupIDs) > 100 || len(in.Name) > 190 {
		fail(w, 400, "name and groups required")
		return
	}
	t := token()
	expires := time.Now().UTC().Add(15 * time.Minute)
	actor, _ := UserFromContext(r.Context())
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		for _, gid := range in.GroupIDs {
			g, e := s.groupTx(r.Context(), tx, gid)
			if e != nil {
				return e
			}
			if !g.CanEnroll() {
				return errors.New("链式出口通过已有出口组组成，不接入设备")
			}
		}
		_, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_enrollments(token_hash,payload,expires_at) VALUES(?,?,?)`), digest(t), strJSON(in), expires.Unix())
		if e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "node.enrollment", "")
	})
	if e != nil {
		fail(w, 400, "invalid enrollment")
		return
	}
	reply(w, 201, map[string]any{"token": t, "expires_at": expires})
}
func (s *Server) registerNode(w http.ResponseWriter, r *http.Request) {
	ip := httporigin.ClientIP(r, s.opts.TrustProxy)
	if !s.allow("register:"+ip, 30) {
		fail(w, 429, "rate limited")
		return
	}
	var in contract.Registration
	if !decode(w, r, &in) {
		return
	}
	if len(in.Name) > 190 || len(in.Capabilities) > 64 || len(in.Token) > 128 {
		fail(w, 400, "invalid registration")
		return
	}
	node := contract.Node{ID: id(), Name: in.Name, Version: in.Version, OS: in.OS, Arch: in.Arch, Capabilities: in.Capabilities, DesiredVersion: 1}
	credential := token()
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		// 两种凭据走同一条注册路径：一次性接入令牌，以及设备组的固定接入密钥。
		// 前者由面板指定名字与组、用完即废；后者可重复使用、名字由设备自报。
		var en enrollment
		var payload string
		err := tx.QueryRowContext(r.Context(), s.q(`SELECT payload FROM cp_enrollments WHERE token_hash=? AND expires_at>?`), digest(in.Token), time.Now().Unix()).Scan(&payload)
		switch {
		case err == nil:
			if e := json.Unmarshal([]byte(payload), &en); e != nil {
				return e
			}
			res, e := tx.ExecContext(r.Context(), s.q(`DELETE FROM cp_enrollments WHERE token_hash=?`), digest(in.Token))
			if e != nil {
				return e
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errConflict
			}
		case errors.Is(err, sql.ErrNoRows):
			// 空名字一律拒绝：那会在控制台上建出一个认不出来的节点。
			if strings.TrimSpace(in.Name) == "" {
				return errEnrollment
			}
			var group string
			if e := tx.QueryRowContext(r.Context(), s.q(`SELECT id FROM cp_groups WHERE join_key=? AND join_key<>''`), in.Token).Scan(&group); e != nil {
				return errEnrollment
			}
			en.GroupIDs = []string{group}
		default:
			return err
		}
		node.GroupIDs = en.GroupIDs
		for _, gid := range en.GroupIDs {
			g, err := s.groupTx(r.Context(), tx, gid)
			if err != nil || !g.CanEnroll() {
				return errEnrollment
			}
		}
		if en.Name != "" {
			node.Name = en.Name
		}
		if _, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_nodes(id,name,token_hash,payload,desired_version,applied_version,apply_error,last_seen) VALUES(?,?,?,?,1,0,'',?)`), node.ID, node.Name, digest(credential), strJSON(node), time.Now().Unix()); e != nil {
			return e
		}
		for _, g := range en.GroupIDs {
			if _, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_node_groups(node_id,group_id) VALUES(?,?)`), node.ID, g); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		fail(w, 401, "invalid enrollment token or group access key")
		return
	}
	reply(w, 201, contract.Registered{NodeID: node.ID, Token: credential})
}
func (s *Server) agent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			fail(w, 401, "node identity required")
			return
		}
		var node string
		if s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT id FROM cp_nodes WHERE token_hash=?`), digest(strings.TrimPrefix(h, "Bearer "))).Scan(&node) != nil {
			fail(w, 401, "node identity required")
			return
		}
		if r.URL.Path != "/api/v1/agent/control/result" {
			var count int
			if e := s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_node_operations WHERE node_id=? AND kind='uninstall' AND status='succeeded'"), node).Scan(&count); e != nil || count != 0 {
				fail(w, 401, "node removed")
				return
			}
		}
		s.mu.Lock()
		s.lastContact[node] = time.Now()
		s.mu.Unlock()
		next(w, r.WithContext(context.WithValue(r.Context(), nodeKey{}, node)))
	}
}
func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	// 可选按设备组收窄：探针页面的历史选择器要和探针本身看到同一批机器。
	group := r.URL.Query().Get("group_id")
	if group != "" && u.Role != "admin" {
		var member int
		if e := s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT COUNT(*) FROM cp_group_identity_groups gig JOIN cp_users iu ON iu.identity_group_id=gig.identity_group_id WHERE gig.group_id=? AND iu.id=?`), group, u.ID).Scan(&member); e != nil {
			fail(w, 500, "query failed")
			return
		}
		if member != 1 {
			fail(w, 403, "该设备组不在你的授权范围内")
			return
		}
	}
	where := ""
	args := []any{}
	if u.Role != "admin" {
		where = ` WHERE EXISTS(SELECT 1 FROM cp_node_groups ng JOIN cp_group_identity_groups gig ON gig.group_id=ng.group_id JOIN cp_users iu ON iu.identity_group_id=gig.identity_group_id WHERE ng.node_id=n.id AND iu.id=?)`
		args = append(args, u.ID)
	}
	if group != "" {
		clause := `EXISTS(SELECT 1 FROM cp_node_groups ng2 WHERE ng2.node_id=n.id AND ng2.group_id=?)`
		if where == "" {
			where = " WHERE " + clause
		} else {
			where += " AND " + clause
		}
		args = append(args, group)
	}
	if where == "" {
		where = " WHERE " + liveNodeSQL
	} else {
		where += " AND " + liveNodeSQL
	}
	n, o := pages(r)
	var total int
	if s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_nodes n"+where), args...).Scan(&total) != nil {
		fail(w, 500, "query failed")
		return
	}
	args = append(args, n, o)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.q(`SELECT payload,desired_version,applied_version,apply_error,last_seen FROM cp_nodes n`+where+` ORDER BY id LIMIT ? OFFSET ?`), args...)
	if e != nil {
		fail(w, 500, "query failed")
		return
	}
	defer rows.Close()
	items := []contract.Node{}
	for rows.Next() {
		var p string
		var d, a, seen int64
		var msg string
		if rows.Scan(&p, &d, &a, &msg, &seen) != nil {
			fail(w, 500, "query failed")
			return
		}
		var node contract.Node
		json.Unmarshal([]byte(p), &node)
		node.DesiredVersion = d
		node.AppliedVersion = a
		node.ApplyError = msg
		if seen > 0 {
			at := time.Unix(seen, 0).UTC()
			node.LastSeen = &at
		}
		if u.Role != "admin" {
			node.GroupIDs = nil
		}
		items = append(items, node)
	}
	reply(w, 200, map[string]any{"items": items, "total": total})
}
func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	node := r.Context().Value(nodeKey{}).(string)
	if e := s.refreshLeases(r.Context(), node); e != nil {
		fail(w, 409, "lease refresh unavailable or entitlement exhausted")
		return
	}
	var nodePayload string
	if err := s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT payload FROM cp_nodes WHERE id=?"), node).Scan(&nodePayload); err != nil {
		fail(w, 500, "config unavailable")
		return
	}
	var nodeInfo contract.Node
	if json.Unmarshal([]byte(nodePayload), &nodeInfo) != nil {
		fail(w, 500, "config unavailable")
		return
	}
	cfg := contract.Config{ContractVersion: contract.Version, NodeID: node, ValidUntil: time.Now().UTC().Add(24 * time.Hour), Rules: []contract.Rule{}}
	// Snapshot transaction prevents an old rule set being labeled with a newer version.
	tx, e := s.Store.DB.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelSerializable})
	if e != nil {
		fail(w, 500, "config unavailable")
		return
	}
	defer tx.Rollback()
	if e = tx.QueryRowContext(r.Context(), s.q(`SELECT desired_version FROM cp_nodes WHERE id=?`), node).Scan(&cfg.Version); e != nil {
		fail(w, 500, "config unavailable")
		return
	}
	rows, e := tx.QueryContext(r.Context(), s.q(`SELECT r.payload,g.payload,u.disabled,u.role,CASE WHEN EXISTS(SELECT 1 FROM cp_group_identity_groups gig WHERE gig.group_id=r.group_id AND gig.identity_group_id=u.identity_group_id) THEN 1 ELSE 0 END FROM cp_rules r JOIN cp_groups g ON g.id=r.group_id JOIN cp_users u ON u.id=r.user_id WHERE r.node_id=? AND r.deleted=0`), node)
	if e != nil {
		fail(w, 500, "config unavailable")
		return
	}
	for rows.Next() {
		var rp, gp, role string
		var disabled, authorized int
		if rows.Scan(&rp, &gp, &disabled, &role, &authorized) != nil {
			rows.Close()
			fail(w, 500, "config unavailable")
			return
		}
		var rule contract.Rule
		var g contract.Group
		if json.Unmarshal([]byte(rp), &rule) != nil || json.Unmarshal([]byte(gp), &g) != nil {
			rows.Close()
			fail(w, 500, "config unavailable")
			return
		}
		if rule.ExitUnavailable || disabled != 0 || !rule.Enabled || rule.Lease == nil || !rule.Lease.ExpiresAt.After(time.Now()) || entryPolicyDenied(g, rule) || (role != "admin" && authorized == 0) {
			continue
		}
		if rule.Lease.Limits != (contract.ResourceLimits{}) && !contains(nodeInfo.Capabilities, "resource-limits-v1") {
			continue
		}
		if rule.ProxyProtocol != nil && !contains(nodeInfo.Capabilities, "proxy-protocol-v1") {
			continue
		}
		if rule.Advanced() && !contains(nodeInfo.Capabilities, "advanced-routing-v1") {
			continue
		}
		rule.BlockedProtocols = applicationBlocks(rule.BlockedProtocols, g.BlockedProtocols)
		if rule.Lease.ExpiresAt.Before(cfg.ValidUntil) {
			cfg.ValidUntil = rule.Lease.ExpiresAt
		}
		cfg.Rules = append(cfg.Rules, rule)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		fail(w, 500, "config unavailable")
		return
	}
	rows.Close()
	if tx.Commit() != nil {
		fail(w, 500, "config unavailable")
		return
	}
	cfg.Rules = filterSharedChildren(cfg.Rules)
	reply(w, 200, cfg)
}
func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	var in contract.Ack
	if !decode(w, r, &in) {
		return
	}
	if len(in.Capabilities) > 64 || len(in.AgentVersion) > 64 {
		fail(w, 400, "too many capabilities")
		return
	}
	for _, capability := range in.Capabilities {
		if len(capability) > 64 {
			fail(w, 400, "invalid capability")
			return
		}
	}
	if len(in.Error) > 2000 || in.Version < 1 || in.AppliedVersion < 0 || in.AppliedVersion > in.Version || (in.Error == "" && in.AppliedVersion != in.Version) {
		fail(w, 400, "invalid ACK")
		return
	}
	node := r.Context().Value(nodeKey{}).(string)
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_nodes SET applied_version=?,apply_error=?,last_seen=? WHERE id=? AND desired_version=? AND applied_version<=?`), in.AppliedVersion, in.Error, time.Now().Unix(), node, in.Version, in.AppliedVersion)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			// MySQL reports changed rows: a same-second replay can match without
			// changing any value. Distinguish it from a stale/unmatched ACK.
			var matched int
			if err := tx.QueryRowContext(r.Context(), s.q(`SELECT COUNT(*) FROM cp_nodes WHERE id=? AND desired_version=? AND applied_version=? AND apply_error=?`), node, in.Version, in.AppliedVersion, in.Error).Scan(&matched); err != nil {
				return err
			}
			if matched != 1 {
				return errConflict
			}
		}
		if in.Capabilities != nil {
			var raw string
			if err := tx.QueryRowContext(r.Context(), s.q("SELECT payload FROM cp_nodes WHERE id=?"), node).Scan(&raw); err != nil {
				return err
			}
			var info contract.Node
			if err := json.Unmarshal([]byte(raw), &info); err != nil {
				return err
			}
			if !slices.Equal(info.Capabilities, in.Capabilities) || in.AgentVersion != "" && info.Version != in.AgentVersion {
				info.Capabilities = in.Capabilities
				if in.AgentVersion != "" {
					info.Version = in.AgentVersion
				}
				// Capability changes can add/remove eligible rules; publish a new
				// configuration revision so the previous ACK cannot cover them.
				if _, err := tx.ExecContext(r.Context(), s.q("UPDATE cp_nodes SET payload=?,desired_version=desired_version+1 WHERE id=?"), strJSON(info), node); err != nil {
					return err
				}
			}
		}
		if in.Error == "" {
			_, e = tx.ExecContext(r.Context(), s.q(`DELETE FROM cp_ports WHERE node_id=? AND rule_id IN(SELECT id FROM cp_rules WHERE node_id=? AND deleted=1 AND release_version<=?)`), node, node, in.AppliedVersion)
		}
		return e
	})
	if e != nil {
		fail(w, 409, "stale or invalid ACK")
		return
	}
	w.WriteHeader(204)
}
func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	var p contract.Probe
	if !decode(w, r, &p) {
		return
	}
	node := r.Context().Value(nodeKey{}).(string)
	if p.NodeID != "" && p.NodeID != node {
		fail(w, 403, "node mismatch")
		return
	}
	if p.SampledAt.IsZero() || p.SampledAt.After(time.Now().Add(time.Minute)) {
		fail(w, 400, "invalid sample time")
		return
	}
	p.NodeID = node
	if _, e := probeValues(p); e != nil {
		fail(w, 400, "invalid probe metric")
		return
	}
	if e := s.history.record(p, time.Now()); e != nil {
		fail(w, 503, e.Error())
		return
	}
	s.mu.Lock()
	p.Online = nil // Online is computed from server observations, never from an Agent claim.
	old, ok := s.probes[node]
	if !ok && len(s.probes) >= historyBuckets {
		s.mu.Unlock()
		fail(w, 503, "live probe capacity reached")
		return
	}
	if !ok || p.SampledAt.After(old.SampledAt) {
		s.probes[node] = p
	}
	s.lastContact[node] = time.Now()
	s.mu.Unlock() // live samples intentionally do not create per-sample DB writes
	if s.allow("heartbeat:"+node, 1) {
		_ = s.Store.Write(r.Context(), storage.Background, func(tx *sql.Tx) error {
			_, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_nodes SET last_seen=? WHERE id=?`), time.Now().Unix(), node)
			return e
		})
	}
	w.WriteHeader(204)
}

// errEntitlementRequired 表示这个用户没有有效权益，看不到探针。
var errEntitlementRequired = errors.New("entitlement required")

// errGroupForbidden 表示请求的设备组不在这个身份的授权范围内。
var errGroupForbidden = errors.New("group not authorized")

// probeNode 是一台机器的展示元数据：名字，以及它属于哪些设备组。
type probeNode struct {
	name     string
	groups   []string
	lastSeen time.Time
}

// visibleNodes 返回这个身份能看到哪些机器，以及每台机器归属的组。
//
// 可见性只有两条规则：管理员看全部；普通用户看自己所属设备组里的机器。
// group 非空时再按一个组收窄 —— 探针页面是「按组看机器」的，客户脚本也
// 只该拿到自己那一组的地址。
func (s *Server) visibleNodes(ctx context.Context, u contract.User, group string) (map[string]probeNode, error) {
	nodes := map[string]probeNode{}
	query := "SELECT n.id,n.payload,n.last_seen FROM cp_nodes n WHERE 1=1"
	args := []any{}
	if u.Role != "admin" {
		query = "SELECT n.id,n.payload,n.last_seen FROM cp_nodes n WHERE EXISTS(SELECT 1 FROM cp_node_groups ng JOIN cp_group_identity_groups gig ON gig.group_id=ng.group_id JOIN cp_users iu ON iu.identity_group_id=gig.identity_group_id WHERE ng.node_id=n.id AND iu.id=?)"
		args = append(args, u.ID)
	}
	query += " AND " + liveNodeSQL
	rows, e := s.Store.DB.QueryContext(ctx, s.q(query), args...)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var nodeID, payload string
		var lastSeen int64
		if e = rows.Scan(&nodeID, &payload, &lastSeen); e != nil {
			rows.Close()
			return nil, e
		}
		var node contract.Node
		json.Unmarshal([]byte(payload), &node)
		nodes[nodeID] = probeNode{name: node.Name, lastSeen: time.Unix(lastSeen, 0)}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	groupQuery := "SELECT node_id,group_id FROM cp_node_groups"
	args = []any{}
	filters := []string{}
	if u.Role != "admin" {
		filters = append(filters, "EXISTS(SELECT 1 FROM cp_group_identity_groups gig JOIN cp_users iu ON iu.identity_group_id=gig.identity_group_id WHERE gig.group_id=cp_node_groups.group_id AND iu.id=?)")
		args = append(args, u.ID)
	}
	if group != "" {
		filters = append(filters, "group_id=?")
		args = append(args, group)
	}
	if len(filters) > 0 {
		groupQuery += " WHERE " + strings.Join(filters, " AND ")
	}
	links, e := s.Store.DB.QueryContext(ctx, s.q(groupQuery), args...)
	if e != nil {
		return nil, e
	}
	defer links.Close()
	for links.Next() {
		var nodeID, groupID string
		if e = links.Scan(&nodeID, &groupID); e != nil {
			return nil, e
		}
		entry, ok := nodes[nodeID]
		if !ok {
			continue
		}
		entry.groups = append(entry.groups, groupID)
		nodes[nodeID] = entry
	}
	if e = links.Err(); e != nil {
		return nil, e
	}
	if group != "" {
		// 指定了组就只留下确实在这个组里的机器：上面那次 join 已经把范围收窄，
		// 但一台机器可能同时属于多个组，这里以组归属为准再确认一次。
		for nodeID, entry := range nodes {
			if !contains(entry.groups, group) {
				delete(nodes, nodeID)
			}
		}
	}
	return nodes, nil
}

func (s *Server) visibleProbes(ctx context.Context, u contract.User, group string) ([]contract.Probe, error) {
	// 探针是付费能力：普通用户要有未过期的权益才能看。管理员不受此限 ——
	// 他们本来就要排查所有人的节点。
	if u.Role != "admin" && s.opts.ActiveEntitlement != nil {
		ok, e := s.opts.ActiveEntitlement(ctx, u.ID)
		if e != nil {
			return nil, e
		}
		if !ok {
			return nil, errEntitlementRequired
		}
	}
	if group != "" && u.Role != "admin" {
		var member int
		if e := s.Store.DB.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM cp_group_identity_groups gig JOIN cp_users iu ON iu.identity_group_id=gig.identity_group_id WHERE gig.group_id=? AND iu.id=?`), group, u.ID).Scan(&member); e != nil {
			return nil, e
		}
		if member != 1 {
			return nil, errGroupForbidden
		}
	}
	nodes, e := s.visibleNodes(ctx, u, group)
	if e != nil {
		return nil, e
	}
	// 查询地址模板要在拿服务锁之前取好：它可能读一次站点设置，而服务锁
	// 是所有探针写入的必经之路。
	template := s.geoTemplate(ctx)
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := []contract.Probe{}
	for n, p := range s.probes {
		entry, ok := nodes[n]
		if !ok {
			continue
		}
		p.NodeName = entry.name
		p.GroupIDs = entry.groups
		lastSeen := entry.lastSeen
		if at := s.lastContact[n]; at.After(lastSeen) {
			lastSeen = at
		}
		if s.opts.OfflineNodeRetention > 0 && time.Since(lastSeen) > s.opts.OfflineNodeRetention {
			continue
		}
		if s.opts.OfflineNodeTime > 0 {
			online := time.Since(lastSeen) <= s.opts.OfflineNodeTime
			p.Online = &online
		}
		// 位置图标对所有人可见，机器地址不是：普通用户看得到「这台在哪里」，
		// 看不到它连哪个 IP。管理员两者都有，客户脚本走设备地址接口。
		if ip := probeAddress(p); ip != "" {
			p.Location = s.locationWith(ctx, ip, template)
		}
		if u.Role != "admin" {
			p.PublicIPs = nil
		}
		items = append(items, p)
	}
	// map 遍历顺序是随机的，而这一份要推给浏览器和客户脚本：按节点 id 定序。
	sort.Slice(items, func(i, j int) bool { return items[i].NodeID < items[j].NodeID })
	return items, nil
}

// probeAddress 取探针里最适合拿来定位的地址：优先 IPv4。
func probeAddress(p contract.Probe) string {
	best := ""
	for _, ip := range p.PublicIPs {
		if ip.Address == "" {
			continue
		}
		if ip.Family == "ipv4" {
			return ip.Address
		}
		if best == "" {
			best = ip.Address
		}
	}
	return best
}
func (s *Server) probeList(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	items, e := s.visibleProbes(r.Context(), u, r.URL.Query().Get("group_id"))
	if errors.Is(e, errEntitlementRequired) {
		fail(w, 403, "需要有效的套餐权益才能查看探针")
		return
	}
	if errors.Is(e, errGroupForbidden) {
		fail(w, 403, "该设备组不在你的授权范围内")
		return
	}
	if e != nil {
		fail(w, 500, "query failed")
		return
	}
	reply(w, 200, map[string]any{"items": items})
}
func (s *Server) probeEvents(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "stream unsupported")
		return
	}
	group := r.URL.Query().Get("group_id")
	// 门槛要在写出事件流响应头之前判断 —— 一旦开始推送就只能以流中断收场，
	// 前端拿不到状态码，只能看到「连接断开」。
	u, e := s.Authenticate(r)
	if e != nil {
		fail(w, 401, "authentication required")
		return
	}
	if u.Role != "admin" && s.opts.ActiveEntitlement != nil {
		ok, e := s.opts.ActiveEntitlement(r.Context(), u.ID)
		if e != nil {
			fail(w, 500, "权益校验不可用")
			return
		}
		if !ok {
			fail(w, 403, "需要有效的套餐权益才能查看探针")
			return
		}
	}
	if group != "" && u.Role != "admin" {
		var member int
		if e := s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT COUNT(*) FROM cp_group_identity_groups gig JOIN cp_users iu ON iu.identity_group_id=gig.identity_group_id WHERE gig.group_id=? AND iu.id=?`), group, u.ID).Scan(&member); e != nil || member != 1 {
			fail(w, 403, "该设备组不在你的授权范围内")
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		u, e := s.Authenticate(r)
		if e != nil {
			return
		}
		items, e := s.visibleProbes(r.Context(), u, group)
		if e != nil {
			return
		}
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, e = fmt.Fprintf(w, "event: probes\ndata: %s\n\n", strJSON(map[string]any{"items": items})); e != nil {
			return
		}
		f.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	var batch contract.UsageBatch
	if !decode(w, r, &batch) {
		return
	}
	if len(batch.Records) > 500 {
		fail(w, 400, "maximum 500 records per batch")
		return
	}
	node := r.Context().Value(nodeKey{}).(string)
	e := s.Store.Write(r.Context(), storage.Normal, func(tx *sql.Tx) error { return s.persistUsageBatch(r.Context(), tx, node, batch.Records) })
	if e != nil {
		fail(w, 409, "usage validation or persistence failed")
		return
	}
	accepted := make([]string, 0, len(batch.Records))
	for _, u := range batch.Records {
		accepted = append(accepted, u.ID)
	}
	reply(w, 200, map[string]any{"accepted": accepted})
}
