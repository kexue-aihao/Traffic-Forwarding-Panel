package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// splitGroupPolicy upgrades legacy mixed policies without changing their meaning.
// Bare "http" in the legacy top-level field disabled the HTTP tunnel; the
// reference advanced field uses bare "http" for application traffic instead.
func splitGroupPolicy(g contract.Group) contract.Group {
	networks := append([]string{}, g.DisabledNetworks...)
	transports := append([]string{}, g.DisabledTransports...)
	apps := []string{}
	add := func(list *[]string, value string) {
		if !contains(*list, value) {
			*list = append(*list, value)
		}
	}
	for _, p := range g.BlockedProtocols {
		switch {
		case p == "tcp" || p == "udp" || strings.HasPrefix(p, "network:"):
			add(&networks, strings.TrimPrefix(p, "network:"))
		case contains([]string{"direct", "tls", "ws", "wss", "http"}, p) || strings.HasPrefix(p, "transport:"):
			add(&transports, strings.TrimPrefix(p, "transport:"))
		case p == "socks":
			add(&apps, "app:socks")
		default:
			add(&apps, p)
		}
	}
	if g.Advanced != nil {
		apps = nil
		for _, p := range g.Advanced.BlockedProtocol {
			switch p {
			case "http", "app:http":
				add(&apps, "app:http")
			case "socks", "app:socks":
				add(&apps, "app:socks")
			default:
				add(&apps, p)
			}
		}
	}
	g.BlockedProtocols = apps
	if g.Advanced != nil {
		// Keep reference names in the editable config and namespaced values in
		// the internal policy. Do not mutate the caller's advanced settings.
		advanced := *g.Advanced
		advanced.BlockedProtocol = make([]string, 0, len(apps))
		for _, p := range apps {
			advanced.BlockedProtocol = append(advanced.BlockedProtocol, strings.TrimPrefix(p, "app:"))
		}
		g.Advanced = &advanced
	}
	g.DisabledNetworks = networks
	g.DisabledTransports = transports
	return g
}

func cleanGroupList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func validateGroupAdvanced(a *contract.GroupAdvanced) error {
	if a == nil {
		return nil
	}
	a.AllowedHost = cleanGroupList(a.AllowedHost)
	a.BlockedHost = cleanGroupList(a.BlockedHost)
	a.BlockedPath = cleanGroupList(a.BlockedPath)
	a.BlockedProtocol = cleanGroupList(a.BlockedProtocol)
	a.IPv6Group = cleanGroupList(a.IPv6Group)
	a.ReverseGroup = cleanGroupList(a.ReverseGroup)
	if a.TLSInboundPolicy < 0 || a.TLSInboundPolicy > 2 {
		return errors.New("TLS 入站策略必须是 0、1 或 2")
	}
	if a.DisableUDP && a.UDPOverTCP {
		return errors.New("禁用 UDP 时不能同时启用 UDP over TCP")
	}
	if a.MaxFail < 0 || a.MaxFail > 1000 || a.FailTimeoutSec < 0 || a.FailTimeoutSec > 86400 {
		return errors.New("故障转移参数超出范围")
	}
	if a.Protocol != "" && !contains([]string{"tls", "tls_simple", "ws", "http"}, a.Protocol) {
		return errors.New("不支持的反向隧道协议")
	}
	if len(a.AllowedHost) > 0 && (len(a.BlockedHost) > 0 || len(a.BlockedPath) > 0 || len(a.BlockedProtocol) > 0) {
		return errors.New("allowed_host 不能与其他入站屏蔽选项同时使用")
	}
	for _, protocol := range a.BlockedProtocol {
		if !contains([]string{"http", "socks", "app:http", "app:socks"}, protocol) {
			return errors.New("不支持的屏蔽协议")
		}
	}
	return nil
}

func policyDenied(g contract.Group, rule contract.Rule) bool {
	g = splitGroupPolicy(g)
	if g.Advanced != nil && g.Advanced.DisableUDP && rule.Network == "udp" {
		return true
	}
	if contains(g.DisabledNetworks, rule.Network) || contains(g.DisabledTransports, rule.Transport) {
		return true
	}
	if rule.Tunnel != nil {
		for _, hop := range rule.Tunnel.Chain {
			if contains(g.DisabledTransports, hop.Transport) {
				return true
			}
		}
	}
	return false
}

func applicationBlocks(rule, group []string) []string {
	out := []string{}
	add := func(p string) {
		if !contains(out, p) {
			out = append(out, p)
		}
	}
	for _, p := range rule {
		if p == "http" || p == "socks" {
			add(p)
		} else if p == "app:http" || p == "app:socks" {
			add(strings.TrimPrefix(p, "app:"))
		}
	}
	for _, p := range group {
		if p == "app:http" || p == "app:socks" {
			add(strings.TrimPrefix(p, "app:"))
		} else if p == "socks" {
			add(p)
		}
	}
	return out
}
func (s *Server) groups(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	where := ""
	args := []any{}
	if u.Role != "admin" {
		where = " WHERE EXISTS(SELECT 1 FROM cp_group_identity_groups gig JOIN cp_users iu ON iu.identity_group_id=gig.identity_group_id WHERE gig.group_id=g.id AND iu.id=?"
		args = append(args, u.ID)
		where += tokenGroupScope(u, "gig.group_id", &args) + ")"
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
	items := []contract.Group{}
	for rows.Next() {
		var p string
		var g contract.Group
		if rows.Scan(&p) != nil || json.Unmarshal([]byte(p), &g) != nil {
			rows.Close()
			fail(w, 500, "query failed")
			return
		}
		g = splitGroupPolicy(g)
		g.UserIDs = nil
		if u.Role != "admin" {
			g.IdentityGroupIDs = nil
		}
		items = append(items, g)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		fail(w, 500, "query failed")
		return
	}
	rows.Close()
	if u.Role == "admin" && len(items) > 0 {
		groupArgs := make([]any, len(items))
		for i, group := range items {
			groupArgs[i] = group.ID
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(items)), ",")
		links, err := s.Store.DB.QueryContext(r.Context(), s.q(`SELECT group_id,identity_group_id FROM cp_group_identity_groups WHERE group_id IN (`+placeholders+`) ORDER BY group_id,identity_group_id`), groupArgs...)
		if err != nil {
			fail(w, 500, "query failed")
			return
		}
		grants := map[string][]string{}
		for links.Next() {
			var groupID, identityGroupID string
			if err = links.Scan(&groupID, &identityGroupID); err != nil {
				links.Close()
				fail(w, 500, "query failed")
				return
			}
			grants[groupID] = append(grants[groupID], identityGroupID)
		}
		if err = links.Err(); err != nil {
			links.Close()
			fail(w, 500, "query failed")
			return
		}
		links.Close()
		for index := range items {
			items[index].IdentityGroupIDs = grants[items[index].ID]
		}
	}
	reply(w, 200, map[string]any{"items": items, "total": total})
}

// groupJoinKey 返回设备组的固定接入密钥。
//
// 明文存储，与 commerce_webhook_subscriptions.secret 同一理由：面板必须能把它
// **再次展示**给运营方，存哈希就取不回来了。它不同于节点身份凭据 —— 那是一次性
// 下发、只存哈希的；这把密钥的用途就是被反复复制。只有管理员能取，而且不进
// GET /groups 的列表响应（那个接口普通用户也能调）。
func (s *Server) groupJoinKey(w http.ResponseWriter, r *http.Request) {
	group := r.PathValue("id")
	var key, raw string
	if e := s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT join_key,payload FROM cp_groups WHERE id=?`), group).Scan(&key, &raw); e != nil {
		fail(w, 404, "unknown group")
		return
	}
	var g contract.Group
	if json.Unmarshal([]byte(raw), &g) != nil || !g.CanEnroll() {
		fail(w, 409, "链式出口通过已有出口组组成，不接入设备")
		return
	}
	reply(w, 200, map[string]any{"group_id": group, "join_key": key})
}

// rotateGroupJoinKey 换一把新密钥。已分发出去的命令会立刻失效 —— 这是撤销的
// 唯一手段，界面上必须把这句话说清楚。
func (s *Server) rotateGroupJoinKey(w http.ResponseWriter, r *http.Request) {
	group := r.PathValue("id")
	key, e := storage.RandomKey()
	if e != nil {
		fail(w, 500, "access key generation failed")
		return
	}
	actor, _ := UserFromContext(r.Context())
	e = s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		g, err := s.groupTx(r.Context(), tx, group)
		if err != nil {
			return err
		}
		if !g.CanEnroll() {
			return errors.New("链式出口不接入设备")
		}
		res, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_groups SET join_key=? WHERE id=?`), key, group)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errConflict
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "group.join-key", group)
	})
	if e != nil {
		fail(w, 409, "access key rotation failed")
		return
	}
	reply(w, 200, map[string]any{"group_id": group, "join_key": key})
}

func (s *Server) saveGroup(w http.ResponseWriter, r *http.Request) {
	var g contract.Group
	if !decode(w, r, &g) {
		return
	}
	// Older clients submitted individual users. Resolve them to their current
	// identity groups, then persist only the identity-group authorization.
	for _, userID := range g.UserIDs {
		var identityGroupID string
		if err := s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT identity_group_id FROM cp_users WHERE id=?`), userID).Scan(&identityGroupID); err != nil || identityGroupID == "" {
			fail(w, 400, "授权用户尚未分配身份用户组")
			return
		}
		if !contains(g.IdentityGroupIDs, identityGroupID) {
			g.IdentityGroupIDs = append(g.IdentityGroupIDs, identityGroupID)
		}
	}
	g.UserIDs = nil
	g.IdentityGroupIDs = cleanGroupList(g.IdentityGroupIDs)
	if !contains([]string{"", contract.GroupMonitor, contract.GroupEntry, contract.GroupExit, contract.GroupChainExit}, g.Type) {
		fail(w, 400, "不支持的设备类型")
		return
	}
	if !contains([]string{"", "forbid", "allow", "force"}, g.DirectPolicy) || !g.CanEnter() && g.DirectPolicy != "" {
		fail(w, 400, "直出策略只适用于入口设备组")
		return
	}
	if g.Type == contract.GroupChainExit && (len(g.ChainGroupIDs) < 2 || len(g.ChainGroupIDs) > 3) || g.Type != contract.GroupChainExit && len(g.ChainGroupIDs) > 0 {
		fail(w, 400, "链式出口需要按顺序配置 2–3 个出口设备组")
		return
	}
	if err := validateGroupAdvanced(g.Advanced); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if !g.CanEnter() && g.PortMin == 0 && g.PortMax == 0 {
		g.PortMin, g.PortMax = 10000, 60000
	}
	if strings.TrimSpace(g.Name) == "" || len(g.Name) > 190 || g.PortMin < 1 || g.PortMax > 65535 || g.PortMax < g.PortMin || g.MaxRules < 0 {
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
	g = splitGroupPolicy(g)
	for _, p := range g.BlockedProtocols {
		if !contains([]string{"app:http", "app:socks"}, p) {
			fail(w, 400, "unsupported blocked protocol")
			return
		}
	}
	for _, network := range g.DisabledNetworks {
		if !contains([]string{"tcp", "udp"}, network) {
			fail(w, 400, "unsupported disabled network")
			return
		}
	}
	for _, transport := range g.DisabledTransports {
		if !contains([]string{"direct", "direct-tls", "tls", "ws", "wss", "http"}, transport) {
			fail(w, 400, "unsupported disabled transport")
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
		for _, identityGroupID := range g.IdentityGroupIDs {
			if _, err := s.identityGroupForUpdate(r.Context(), tx, identityGroupID); err != nil {
				return errors.New("身份用户组不存在")
			}
		}
		if err := s.validateGroupTypeTx(r.Context(), tx, g, create); err != nil {
			return err
		}
		if g.Advanced != nil {
			for _, groupID := range append(append([]string{}, g.Advanced.IPv6Group...), g.Advanced.ReverseGroup...) {
				if groupID != g.ID {
					// Legacy advanced settings can contain unresolved IDs. Lock
					// existing references without making those settings invalid.
					if _, err := s.groupTx(r.Context(), tx, groupID); err != nil && !errors.Is(err, sql.ErrNoRows) {
						return err
					}
				}
			}
		}
		if create {
			// 接入密钥在设备组诞生时就有，之后固定不变 —— 运营方复制一次命令
			// 就能反复使用，装失败不必回控制台重新生成。
			key, e := storage.RandomKey()
			if e != nil {
				return e
			}
			_, e = tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_groups(id,name,payload,version,join_key) VALUES(?,?,?,?,?)`), g.ID, g.Name, strJSON(g), g.Version, key)
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
			if _, e = tx.ExecContext(r.Context(), s.q(`DELETE FROM cp_group_identity_groups WHERE group_id=?`), g.ID); e != nil {
				return e
			}
		}
		for _, identityGroupID := range g.IdentityGroupIDs {
			if _, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_group_identity_groups(group_id,identity_group_id) VALUES(?,?)`), g.ID, identityGroupID); e != nil {
				return e
			}
		}
		if _, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id IN(SELECT node_id FROM cp_node_groups WHERE group_id=?)`), g.ID); e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "group.save", g.ID)
	})
	if e != nil {
		fail(w, 409, "设备组保存失败："+e.Error())
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
	// 分类筛选在 SQL 里做：它是一列，不是 payload 里的字段。（未分类用
	// uncategorized=true 表达，省得给「空」编一个会和真实分类撞车的取值。）
	if category := strings.TrimSpace(r.URL.Query().Get("category")); category != "" {
		where += " AND category=?"
		args = append(args, category)
	} else if r.URL.Query().Get("uncategorized") == "true" {
		where += " AND category=''"
	}
	n, o := pages(r)
	var total int
	if s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_rules"+where), args...).Scan(&total) != nil {
		fail(w, 500, "query failed")
		return
	}
	args = append(args, n, o)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.q("SELECT payload,category FROM cp_rules"+where+" ORDER BY id LIMIT ? OFFSET ?"), args...)
	if e != nil {
		fail(w, 500, "query failed")
		return
	}
	defer rows.Close()
	items := []contract.Rule{}
	for rows.Next() {
		var p, category string
		var rule contract.Rule
		if rows.Scan(&p, &category) != nil || json.Unmarshal([]byte(p), &rule) != nil {
			fail(w, 500, "query failed")
			return
		}
		// 分类来自那一列；payload 里不该有它，这里以列为准。
		rule.Category = category
		redact(&rule)
		items = append(items, rule)
	}
	page := map[string]any{"items": items, "total": total}
	// 分类清单随列表一起回：界面上的筛选器要用它，为这个单开一个接口不值当。
	if categories, e := s.ruleCategories(r.Context(), u); e == nil {
		page["categories"] = categories
	}
	reply(w, 200, page)
}

// ruleCategories 列出这个身份能看到的规则分类（不含未分类）。
func (s *Server) ruleCategories(ctx context.Context, u contract.User) ([]string, error) {
	query := "SELECT DISTINCT category FROM cp_rules WHERE deleted=0 AND category<>''"
	args := []any{}
	if u.Role != "admin" {
		query += " AND user_id=?"
		args = append(args, u.ID)
	}
	query += " ORDER BY category"
	rows, e := s.Store.DB.QueryContext(ctx, s.q(query), args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []string{}
	for rows.Next() {
		var category string
		if e = rows.Scan(&category); e != nil {
			return nil, e
		}
		items = append(items, category)
	}
	return items, rows.Err()
}

// setRuleCategory 批量给规则归类。分类只影响控制台怎么分组：它不进发给 Agent 的
// 配置，所以既不 bump version 也不 bump desired_version —— 归个类不该惊动节点，
// 也因此不会和别处的编辑互相覆盖（payload 里没有它）。
func (s *Server) setRuleCategory(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	var in struct {
		IDs      []string `json:"ids"`
		Category string   `json:"category"`
	}
	if !decode(w, r, &in) {
		return
	}
	category := strings.TrimSpace(in.Category)
	if len(in.IDs) == 0 || len(in.IDs) > 500 || len(category) > 32 {
		fail(w, 400, "invalid rule category")
		return
	}
	// 归属交给 SQL：普通用户改不到别人的规则，改不到就是没改到。
	placeholders := strings.TrimRight(strings.Repeat("?,", len(in.IDs)), ",")
	args := []any{category}
	for _, ruleID := range in.IDs {
		args = append(args, ruleID)
	}
	query := "UPDATE cp_rules SET category=? WHERE deleted=0 AND id IN (" + placeholders + ")"
	if u.Role != "admin" {
		query += " AND user_id=?"
		args = append(args, u.ID)
	}
	var affected int64
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(r.Context(), s.q(query), args...)
		if e != nil {
			return e
		}
		affected, _ = res.RowsAffected()
		return s.AuditTx(r.Context(), tx, u.ID, "rule.category", category)
	})
	if err != nil {
		fail(w, 500, "rule category update failed")
		return
	}
	reply(w, 200, map[string]any{"updated": affected, "category": category})
}

func redact(rule *contract.Rule) {
	if rule.ExitGroupID != "" {
		rule.Tunnel = nil
	}
	rule.Lease = nil
	if rule.Tunnel != nil {
		rule.Tunnel.Token = ""
		for i := range rule.Tunnel.Chain {
			rule.Tunnel.Chain[i].Token = ""
		}
	}
}

// allocateListen 在设备组允许的端口范围里随机挑一个尚未预留的端口。
//
// 规则的监听地址留空时用它：让操作方从 20000–29999 里手挑一个是无谓的负担，
// 挑重复了还会撞上「物理机端口预留」。随机起点 + 环形扫描，既不会每次都挑到
// 同一个端口，也能在范围快满时找到空位。
func (s *Server) allocateListen(ctx context.Context, tx *sql.Tx, groupPayload, node, network string) (string, error) {
	var g contract.Group
	if e := json.Unmarshal([]byte(groupPayload), &g); e != nil {
		return "", e
	}
	if g.PortMax < g.PortMin || g.PortMin < 1 || g.PortMax > 65535 {
		return "", errors.New("该设备组没有可用的端口范围")
	}
	span := g.PortMax - g.PortMin + 1
	start := rand.IntN(span)
	for i := 0; i < span; i++ {
		port := g.PortMin + (start+i)%span
		var n int
		if e := tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM cp_ports WHERE node_id=? AND network=? AND port=?"), node, network, port).Scan(&n); e != nil {
			return "", e
		}
		if n == 0 {
			return fmt.Sprintf(":%d", port), nil
		}
	}
	return "", errors.New("该设备组的端口范围内已无空闲端口")
}

func validateRule(rule contract.Rule) (int, error) {
	if len(rule.Name) > 190 || strings.TrimSpace(rule.Name) == "" || !contains([]string{"tcp", "udp"}, rule.Network) || !contains([]string{"direct", "direct-tls", "tls", "ws", "wss", "http"}, rule.Transport) {
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
	// direct 与 direct-tls 都没有出口：前者明文直连目标，后者把到目标的那一段
	// 包进 TLS。两者的差别只在要不要校验名，所以隧道端点与凭据都不适用。
	if rule.Transport != "direct" && rule.Transport != "direct-tls" && (rule.Tunnel == nil || rule.Tunnel.Endpoint == "" || rule.Tunnel.Token == "") {
		return 0, errors.New("tunnel endpoint and credential required")
	}
	if rule.Transport == "direct" && rule.Tunnel != nil {
		return 0, errors.New("direct forwarding cannot contain a tunnel")
	}
	if rule.Transport == "direct-tls" && rule.Tunnel != nil && (rule.Tunnel.Endpoint != "" || rule.Tunnel.Token != "" || len(rule.Tunnel.Chain) > 0) {
		return 0, errors.New("direct-tls only accepts a verification name, not a tunnel")
	}
	if rule.Transport == "direct-tls" && rule.Network != "tcp" {
		return 0, errors.New("direct-tls supports tcp only")
	}
	if rule.Transport != "direct" && rule.Transport != "direct-tls" && rule.Tunnel != nil {
		if e := tunnel.ValidateChain(contract.TunnelHop{Transport: rule.Transport, Endpoint: rule.Tunnel.Endpoint, ServerName: rule.Tunnel.ServerName, Token: rule.Tunnel.Token}, rule.Tunnel.Chain); e != nil {
			return 0, e
		}
	}
	if err := rule.ValidateAdvanced(); err != nil {
		return 0, err
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
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		return s.saveRuleTx(r.Context(), tx, actor, &rule, create)
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
func (s *Server) saveRuleTx(ctx context.Context, tx *sql.Tx, actor contract.User, rule *contract.Rule, create bool) error {
	oldVersion := rule.Version
	// 分类存在列里，不进 payload —— 那是发给 Agent 的配置，归类和它无关。
	rule.Category = ""
	if err := s.lockRuleOwner(ctx, tx, rule.UserID); err != nil {
		return err
	}
	var old contract.Rule
	if !create {
		var payload string
		var deleted int
		if e := tx.QueryRowContext(ctx, s.q(`SELECT payload,deleted FROM cp_rules WHERE id=?`), rule.ID).Scan(&payload, &deleted); e != nil {
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
		if sharedParent(*rule) != sharedParent(old) {
			return errors.New("shared TLS parent is immutable")
		}
		if rule.ExitGroupID == "" && old.ExitGroupID == "" && rule.Tunnel != nil && old.Tunnel != nil {
			if rule.Tunnel.Token == "" && rule.Transport == old.Transport && rule.Tunnel.Endpoint == old.Tunnel.Endpoint && rule.Tunnel.ServerName == old.Tunnel.ServerName {
				rule.Tunnel.Token = old.Tunnel.Token
			}
			for i, hop := range rule.Tunnel.Chain {
				if i < len(old.Tunnel.Chain) {
					prior := old.Tunnel.Chain[i]
					if hop.Token == "" && hop.Transport == prior.Transport && hop.Endpoint == prior.Endpoint && hop.ServerName == prior.ServerName {
						rule.Tunnel.Chain[i].Token = prior.Token
					}
				}
			}
		}
		rule.Lease = old.Lease
	}
	exitMultiplier, e := s.resolveExitTx(ctx, tx, rule)
	if e != nil {
		return e
	}
	rule.ExitUnavailable = false
	var payload string
	groupQuery := `SELECT g.payload FROM cp_groups g WHERE g.id=? AND EXISTS(SELECT 1 FROM cp_node_groups ng WHERE ng.group_id=g.id AND ng.node_id=?)`
	if s.Store.Dialect != "sqlite" {
		groupQuery += " FOR UPDATE"
	}
	if e = tx.QueryRowContext(ctx, s.q(groupQuery), rule.GroupID, rule.NodeID).Scan(&payload); e != nil {
		return errors.New("node not in group")
	}
	if e = s.nodeAvailableTx(ctx, tx, rule.NodeID); e != nil {
		return e
	}
	// 监听地址留空 = 从设备组允许的范围里随机分配。
	if strings.TrimSpace(rule.Listen) == "" {
		listen, e := s.allocateListen(ctx, tx, payload, rule.NodeID, rule.Network)
		if e != nil {
			return e
		}
		rule.Listen = listen
	}
	port, e := validateRule(*rule)
	if e != nil {
		return e
	}
	var g contract.Group
	if e = json.Unmarshal([]byte(payload), &g); e != nil {
		return e
	}
	if actor.Role != "admin" {
		authorized, err := s.groupAuthorized(ctx, tx, rule.GroupID, actor.ID)
		if err != nil || !authorized {
			return errors.New("group not authorized")
		}
	}
	previousMultiplier := old.BillingMultiplier
	rule.BillingMultiplier, e = effectiveMultiplier(g.Multiplier, exitMultiplier)
	if e != nil {
		return e
	}
	if !create && (previousMultiplier != rule.BillingMultiplier || old.ExitGroupID != rule.ExitGroupID || old.SelectedExitID != rule.SelectedExitID) {
		rule.Lease = nil
	}
	if create {
		if e = s.checkRuleLimit(ctx, tx, rule.UserID, g); e != nil {
			return e
		}
	}
	if port < g.PortMin || port > g.PortMax || entryPolicyDenied(g, *rule) {
		return errors.New("group policy denied")
	}
	for _, p := range rule.BlockedProtocols {
		if !contains([]string{"http", "socks", "app:http", "app:socks"}, p) {
			return errors.New("unsupported rule protocol policy")
		}
	}
	rule.BlockedProtocols = applicationBlocks(rule.BlockedProtocols, nil)
	if e = s.validateSharedTx(ctx, tx, *rule); e != nil {
		return e
	}
	if rule.Enabled {
		supported, err := s.resourceCapabilities(ctx, tx, *rule)
		if err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade Agent to enforce plan limits before enabling this rule")
		}
	}
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
				rule.Lease, e = a.AllocateWithMultiplier(ctx, tx, rule.UserID, rule.ID, rule.NodeID, rule.BillingMultiplier)
			} else {
				if rule.BillingMultiplier != "1" {
					return errors.New("multiplier allocator required")
				}
				rule.Lease, e = s.opts.Entitlements.Allocate(ctx, tx, rule.UserID, rule.ID, rule.NodeID)
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
		_, e = tx.ExecContext(ctx, s.q(`INSERT INTO cp_rule_leases(id,rule_id,node_id,entitlement_id,bytes_allocated,bytes_used,expires_at) VALUES(?,?,?,?,?,0,?)`), rule.Lease.ID, rule.ID, rule.NodeID, rule.Lease.EntitlementID, rule.Lease.Bytes, rule.Lease.ExpiresAt.Unix())
		if e != nil {
			return e
		}
	}
	if create {
		_, e = tx.ExecContext(ctx, s.q(`INSERT INTO cp_rules(id,user_id,node_id,group_id,payload,version,deleted,release_version) VALUES(?,?,?,?,?,?,0,0)`), rule.ID, rule.UserID, rule.NodeID, rule.GroupID, strJSON(rule), rule.Version)
		if e != nil {
			return e
		}
		if sharedParent(*rule) == "" {
			_, e = tx.ExecContext(ctx, s.q(`INSERT INTO cp_ports(node_id,network,port,rule_id) VALUES(?,?,?,?)`), rule.NodeID, rule.Network, port, rule.ID)
			if e != nil {
				return errors.New("physical machine port reserved (all IP addresses)")
			}
		}
	} else {
		res, err := tx.ExecContext(ctx, s.q(`UPDATE cp_rules SET payload=?,version=? WHERE id=? AND version=? AND deleted=0`), strJSON(rule), rule.Version, rule.ID, oldVersion)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errConflict
		}
	}
	if _, e = tx.ExecContext(ctx, s.q(`UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id=?`), rule.NodeID); e != nil {
		return e
	}
	return s.AuditTx(ctx, tx, actor.ID, "rule.save", rule.ID)
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
		if err := s.validateSharedTx(r.Context(), tx, contract.Rule{ID: r.PathValue("id"), NodeID: node}); err != nil {
			return err
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

func (s *Server) diagnoseRule(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	var owner, nodeID, rulePayload, applyError string
	var desired, applied, lastSeen int64
	if err := s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT r.user_id,r.node_id,r.payload,n.desired_version,n.applied_version,n.apply_error,n.last_seen FROM cp_rules r JOIN cp_nodes n ON n.id=r.node_id WHERE r.id=? AND r.deleted=0`), r.PathValue("id")).Scan(&owner, &nodeID, &rulePayload, &desired, &applied, &applyError, &lastSeen); err != nil {
		fail(w, 404, "rule not found")
		return
	}
	if actor.Role != "admin" && actor.ID != owner {
		fail(w, 404, "rule not found")
		return
	}
	var rule contract.Rule
	if json.Unmarshal([]byte(rulePayload), &rule) != nil {
		fail(w, 500, "diagnostic unavailable")
		return
	}
	now := time.Now().UTC()
	checks := []map[string]any{}
	add := func(name string, ok bool, detail string) {
		checks = append(checks, map[string]any{"name": name, "ok": ok, "detail": detail})
	}
	var disabled, membership int
	if err := s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT disabled FROM cp_users WHERE id=?"), owner).Scan(&disabled); err != nil {
		fail(w, 500, "diagnostic unavailable")
		return
	}
	if err := s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT COUNT(*) FROM cp_group_identity_groups gig JOIN cp_users u ON u.identity_group_id=gig.identity_group_id WHERE u.id=? AND gig.group_id=?`), owner, rule.GroupID).Scan(&membership); err != nil {
		fail(w, 500, "diagnostic unavailable")
		return
	}
	add("account", disabled == 0, "account enabled")
	add("group_authorization", membership > 0 || actor.Role == "admin", "current group membership")
	add("enabled", rule.Enabled, "rule enabled")
	var supported, withinLimit bool
	err := s.Store.Write(r.Context(), storage.Background, func(tx *sql.Tx) error {
		var err error
		supported, err = s.resourceCapabilities(r.Context(), tx, rule)
		if err != nil {
			return err
		}
		limits, err := s.accountLimits(r.Context(), tx, owner)
		if err != nil {
			return err
		}
		withinLimit, err = s.withinPlanRuleLimit(r.Context(), tx, rule, limits.MaxRules)
		return err
	})
	if err != nil {
		fail(w, 500, "diagnostic unavailable")
		return
	}
	add("plan_rule_limit", withinLimit, "rule eligible within current plan limit")
	add("agent_limits_capability", supported, "Agent must support current plan limits")
	lastSeenAt := time.Unix(lastSeen, 0).UTC()
	add("node_online", lastSeen > 0 && now.Sub(lastSeenAt) <= 90*time.Second, lastSeenAt.Format(time.RFC3339))
	add("configuration_ack", desired == applied && applyError == "", map[bool]string{true: "applied", false: "pending or failed"}[desired == applied && applyError == ""])
	leaseOK := rule.Lease != nil && rule.Lease.ExpiresAt.After(now)
	leaseDetail := "missing or expired"
	if rule.Lease != nil {
		leaseDetail = rule.Lease.ExpiresAt.UTC().Format(time.RFC3339)
	}
	add("lease", leaseOK, leaseDetail)
	if actor.Role != "admin" && applyError != "" {
		applyError = "node reported a configuration error"
	}
	add("agent_error", applyError == "", applyError)
	reply(w, 200, map[string]any{"rule_id": rule.ID, "node_id": nodeID, "desired_version": desired, "applied_version": applied, "checks": checks, "generated_at": now})
}

// Callers hold the group row lock. A locking read avoids stale MySQL snapshots.
func (s *Server) checkRuleLimit(ctx context.Context, tx *sql.Tx, user string, group contract.Group) error {
	limits, err := s.accountLimits(ctx, tx, user)
	if err != nil {
		return err
	}
	if limits.MaxRules > 0 {
		q := "SELECT id FROM cp_rules WHERE user_id=? AND deleted=0"
		if s.Store.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		rows, err := tx.QueryContext(ctx, s.q(q), user)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if count >= limits.MaxRules {
			return errors.New("plan rule limit reached")
		}
	}
	if group.MaxRules <= 0 {
		return nil
	}
	q := "SELECT id FROM cp_rules WHERE user_id=? AND group_id=? AND deleted=0"
	if s.Store.Dialect != "sqlite" {
		q += " FOR UPDATE"
	}
	rows, err := tx.QueryContext(ctx, s.q(q), user, group.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count >= group.MaxRules {
			return errors.New("rule limit reached")
		}
	}
	return rows.Err()
}
