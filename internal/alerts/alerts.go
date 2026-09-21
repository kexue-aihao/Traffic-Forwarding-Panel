// Package alerts turns persisted health and entitlement state into durable,
// deduplicated notifications. It never probes destinations or changes billing.
package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

type Policy struct {
	Enabled          bool  `json:"enabled"`
	OfflineSeconds   int64 `json:"offline_seconds"`
	RecoverySeconds  int64 `json:"recovery_seconds"`
	ExpiryHours      int64 `json:"expiry_hours"`
	RemainingPercent int64 `json:"remaining_percent"`
	Version          int64 `json:"version"`
}

func defaultPolicy() Policy { return Policy{true, 90, 15, 72, 10, 0} }

type Manager struct {
	Store                         *storage.Store
	Commerce                      *commerce.Service
	Now                           func() time.Time
	mu                            sync.Mutex
	nodeCursor, entitlementCursor string
	startedAt                     int64
}

func New(st *storage.Store, billing *commerce.Service) *Manager {
	return &Manager{Store: st, Commerce: billing, Now: time.Now}
}
func (m *Manager) q(q string) string { return m.Store.Rebind(q) }
func (m *Manager) Migrate(ctx context.Context) error {
	return storage.MigrateNamespace(ctx, m.Store.DB, m.Store.Dialect, "cp_alert", 1, func(conn *sql.Conn) error {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS cp_alert_policy(id INTEGER PRIMARY KEY,payload TEXT NOT NULL,version BIGINT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS cp_alert_nodes(node_id VARCHAR(64) PRIMARY KEY,status VARCHAR(16) NOT NULL,observed_at BIGINT NOT NULL,recovery_since BIGINT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS cp_alert_entitlements(entitlement_id VARCHAR(64) NOT NULL,kind VARCHAR(64) NOT NULL,created_at BIGINT NOT NULL,PRIMARY KEY(entitlement_id,kind))`,
		} {
			if m.Store.Dialect == "mysql" {
				q = strings.ReplaceAll(q, " TEXT", " LONGTEXT")
			}
			if _, err := conn.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *Manager) Policy(ctx context.Context) (Policy, error) {
	p := defaultPolicy()
	var raw string
	err := m.Store.DB.QueryRowContext(ctx, "SELECT payload,version FROM cp_alert_policy WHERE id=1").Scan(&raw, &p.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal([]byte(raw), &p)
	return p, err
}

var ErrConflict = errors.New("alert policy version conflict")

func (m *Manager) SetPolicy(ctx context.Context, actor string, p Policy) (Policy, error) {
	if p.OfflineSeconds < 90 || p.OfflineSeconds > 3600 || p.RecoverySeconds < 5 || p.RecoverySeconds > 600 || p.ExpiryHours < 1 || p.ExpiryHours > 720 || p.RemainingPercent < 1 || p.RemainingPercent > 50 {
		return p, errors.New("invalid alert policy")
	}
	err := m.Store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
		base, _ := json.Marshal(defaultPolicy())
		q := "INSERT INTO cp_alert_policy(id,payload,version) VALUES(1,?,0) ON CONFLICT(id) DO NOTHING"
		if m.Store.Dialect == "mysql" {
			q = "INSERT INTO cp_alert_policy(id,payload,version) VALUES(1,?,0) ON DUPLICATE KEY UPDATE id=id"
		}
		if _, err := tx.ExecContext(ctx, m.q(q), string(base)); err != nil {
			return err
		}
		q = "SELECT version FROM cp_alert_policy WHERE id=1"
		if m.Store.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		var version int64
		if err := tx.QueryRowContext(ctx, q).Scan(&version); err != nil {
			return err
		}
		if p.Version != version {
			return ErrConflict
		}
		p.Version++
		raw, _ := json.Marshal(p)
		if _, err := tx.ExecContext(ctx, m.q("UPDATE cp_alert_policy SET payload=?,version=? WHERE id=1"), string(raw), p.Version); err != nil {
			return err
		}
		// The event ID doubles as the audit ID, with no mutable financial data.
		return m.Commerce.EmitEventTx(ctx, tx, actor, "alerts.policy_updated", map[string]any{"version": p.Version})
	})
	return p, err
}

type nodeState struct {
	Status             string
	Observed, Recovery int64
}

func transition(old nodeState, seen, now int64, p Policy) (nodeState, string) {
	next := old
	if next.Status == "" {
		next = nodeState{Status: "unknown", Observed: now}
		if seen > 0 {
			next.Status = "online"
		}
	}
	baseline := seen
	if baseline == 0 {
		baseline = next.Observed
	}
	online := seen > 0 && now-seen < p.OfflineSeconds
	if !online {
		next.Recovery = 0
		if next.Status != "offline" && now-baseline >= p.OfflineSeconds {
			next.Status = "offline"
			return next, "node.offline"
		}
		return next, ""
	}
	if next.Status == "unknown" {
		next.Status = "online"
	}
	if next.Status == "offline" {
		if next.Recovery == 0 {
			next.Recovery = now
		}
		// Require another fresh heartbeat after the candidate, not one isolated packet.
		if now-next.Recovery >= p.RecoverySeconds && seen > next.Recovery {
			next.Status = "online"
			next.Recovery = 0
			return next, "node.recovered"
		}
	}
	return next, ""
}

func (m *Manager) monitorNode(ctx context.Context, node string, p Policy) error {
	return m.Store.Write(ctx, storage.Background, func(tx *sql.Tx) error {
		var name string
		var seen int64
		q := "SELECT name,last_seen FROM cp_nodes WHERE id=?"
		if m.Store.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		if err := tx.QueryRowContext(ctx, m.q(q), node).Scan(&name, &seen); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		var old nodeState
		err := tx.QueryRowContext(ctx, m.q("SELECT status,observed_at,recovery_since FROM cp_alert_nodes WHERE node_id=?"), node).Scan(&old.Status, &old.Observed, &old.Recovery)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		next, event := transition(old, seen, m.Now().Unix(), p)
		if event == "node.offline" && m.Now().Unix()-m.startedAt < p.OfflineSeconds {
			return nil // Let Agents reconnect after a panel restart before declaring loss.
		}
		if next == old {
			return nil
		}
		q = "INSERT INTO cp_alert_nodes(node_id,status,observed_at,recovery_since) VALUES(?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET status=excluded.status,observed_at=excluded.observed_at,recovery_since=excluded.recovery_since"
		if m.Store.Dialect == "mysql" {
			q = "INSERT INTO cp_alert_nodes(node_id,status,observed_at,recovery_since) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE status=VALUES(status),observed_at=VALUES(observed_at),recovery_since=VALUES(recovery_since)"
		}
		if _, err = tx.ExecContext(ctx, m.q(q), node, next.Status, next.Observed, next.Recovery); err != nil {
			return err
		}
		if event == "" {
			return nil
		}
		rows, err := tx.QueryContext(ctx, m.q(`SELECT u.id FROM cp_users u WHERE u.disabled=0 AND (u.role='admin' OR EXISTS(SELECT 1 FROM cp_group_users gu JOIN cp_node_groups ng ON ng.group_id=gu.group_id WHERE gu.user_id=u.id AND ng.node_id=?))`), node)
		if err != nil {
			return err
		}
		var users []string
		for rows.Next() {
			var user string
			if err = rows.Scan(&user); err != nil {
				rows.Close()
				return err
			}
			users = append(users, user)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, user := range users {
			if err = m.Commerce.EmitEventTx(ctx, tx, user, event, map[string]any{"node_id": node, "name": name, "observed_at": m.Now().UTC().Format(time.RFC3339)}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *Manager) monitorEntitlement(ctx context.Context, ent string, p Policy) error {
	return m.Store.Write(ctx, storage.Background, func(tx *sql.Tx) error {
		var user, expires string
		var quota, used int64
		if err := tx.QueryRowContext(ctx, m.q("SELECT user_id FROM commerce_entitlements WHERE id=?"), ent).Scan(&user); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		lock := "SELECT disabled FROM cp_users WHERE id=?"
		if m.Store.Dialect != "sqlite" {
			lock += " FOR UPDATE"
		}
		var disabled int
		if err := tx.QueryRowContext(ctx, m.q(lock), user).Scan(&disabled); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		if disabled != 0 {
			return nil
		}
		q := `SELECT e.user_id,e.expires_at,e.quota,e.used FROM commerce_entitlements e JOIN cp_users u ON u.id=e.user_id WHERE e.id=? AND u.disabled=0 AND NOT EXISTS(SELECT 1 FROM commerce_entitlements newer WHERE newer.user_id=e.user_id AND newer.version>e.version)`
		if m.Store.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		if err := tx.QueryRowContext(ctx, m.q(q), ent).Scan(&user, &expires, &quota, &used); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		end, err := time.Parse(time.RFC3339Nano, expires)
		if err != nil {
			return err
		}
		remaining := max(int64(0), quota-used)
		threshold := quota/100*p.RemainingPercent + (quota%100*p.RemainingPercent)/100
		var kinds []string
		if !m.Now().Before(end) {
			kinds = append(kinds, "entitlement.expired")
		} else {
			if !end.After(m.Now().Add(time.Duration(p.ExpiryHours) * time.Hour)) {
				kinds = append(kinds, "entitlement.expiring")
			}
			if remaining <= threshold {
				kinds = append(kinds, "entitlement.low_quota")
			}
		}
		for _, kind := range kinds {
			var exists int
			if err = tx.QueryRowContext(ctx, m.q("SELECT COUNT(*) FROM cp_alert_entitlements WHERE entitlement_id=? AND kind=?"), ent, kind).Scan(&exists); err != nil {
				return err
			}
			if exists > 0 {
				continue
			}
			if _, err = tx.ExecContext(ctx, m.q("INSERT INTO cp_alert_entitlements(entitlement_id,kind,created_at) VALUES(?,?,?)"), ent, kind, m.Now().Unix()); err != nil {
				return err
			}
			if err = m.Commerce.EmitEventTx(ctx, tx, user, kind, map[string]any{"entitlement_id": ent, "expires_at": expires, "remaining_bytes": strconv.FormatInt(remaining, 10), "quota_bytes": strconv.FormatInt(quota, 10)}); err != nil {
				return err
			}
		}
		return nil
	})
}

// RunOnce inspects bounded pages. Stable node state causes no writes; entitlement
// candidates are filtered by deadline/remaining quota before entering a write transaction.
func (m *Manager) RunOnce(ctx context.Context) error {
	if !m.mu.TryLock() {
		return nil
	}
	defer m.mu.Unlock()
	if m.startedAt == 0 {
		m.startedAt = m.Now().Unix()
	}
	p, err := m.Policy(ctx)
	if err != nil || !p.Enabled {
		return err
	}
	rows, err := m.Store.DB.QueryContext(ctx, m.q(`SELECT n.id,n.last_seen,COALESCE(a.status,''),COALESCE(a.observed_at,0),COALESCE(a.recovery_since,0) FROM cp_nodes n LEFT JOIN cp_alert_nodes a ON a.node_id=n.id WHERE n.id>? ORDER BY n.id LIMIT 1000`), m.nodeCursor)
	if err != nil {
		return err
	}
	var nodes []string
	read := 0
	for rows.Next() {
		var node string
		var seen int64
		var old nodeState
		if err = rows.Scan(&node, &seen, &old.Status, &old.Observed, &old.Recovery); err != nil {
			rows.Close()
			return err
		}
		m.nodeCursor = node
		read++
		if next, event := transition(old, seen, m.Now().Unix(), p); next != old && !(event == "node.offline" && m.Now().Unix()-m.startedAt < p.OfflineSeconds) {
			nodes = append(nodes, node)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if read < 1000 {
		m.nodeCursor = ""
	}
	for _, node := range nodes {
		if err = m.monitorNode(ctx, node, p); err != nil {
			return err
		}
	}
	rows, err = m.Store.DB.QueryContext(ctx, m.q(`SELECT e.id,e.expires_at,e.quota,e.used,
        EXISTS(SELECT 1 FROM cp_alert_entitlements a WHERE a.entitlement_id=e.id AND a.kind='entitlement.expiring'),
        EXISTS(SELECT 1 FROM cp_alert_entitlements a WHERE a.entitlement_id=e.id AND a.kind='entitlement.expired'),
        EXISTS(SELECT 1 FROM cp_alert_entitlements a WHERE a.entitlement_id=e.id AND a.kind='entitlement.low_quota')
        FROM commerce_entitlements e JOIN cp_users u ON u.id=e.user_id WHERE e.id>? AND u.disabled=0 AND NOT EXISTS(SELECT 1 FROM commerce_entitlements newer WHERE newer.user_id=e.user_id AND newer.version>e.version) ORDER BY e.id LIMIT 1000`), m.entitlementCursor)
	if err != nil {
		return err
	}
	var candidates []string
	read = 0
	for rows.Next() {
		var ent, expires string
		var quota, used int64
		var expiringSent, expiredSent, quotaSent bool
		if err = rows.Scan(&ent, &expires, &quota, &used, &expiringSent, &expiredSent, &quotaSent); err != nil {
			rows.Close()
			return err
		}
		m.entitlementCursor = ent
		read++
		end, e := time.Parse(time.RFC3339Nano, expires)
		if e != nil {
			rows.Close()
			return e
		}
		expired := !m.Now().Before(end)
		if expired && !expiredSent || !expired && ((!expiringSent && !end.After(m.Now().Add(time.Duration(p.ExpiryHours)*time.Hour))) || (!quotaSent && quota-used <= quota/100*p.RemainingPercent+(quota%100*p.RemainingPercent)/100)) {
			candidates = append(candidates, ent)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if read < 1000 {
		m.entitlementCursor = ""
	}
	for _, ent := range candidates {
		if err = m.monitorEntitlement(ctx, ent, p); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if err := m.RunOnce(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "alert scan will retry")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Visible rechecks current node permissions both when listing and before sending.
func (m *Manager) Visible(ctx context.Context, tx *sql.Tx, user, kind, payload string, adminScope bool) (bool, error) {
	if !strings.HasPrefix(kind, "node.") {
		return true, nil
	}
	var event struct {
		Data struct {
			NodeID string `json:"node_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return false, err
	}
	var n int
	scope := 0
	if adminScope {
		scope = 1
	}
	err := tx.QueryRowContext(ctx, m.q(`SELECT COUNT(*) FROM cp_users u WHERE u.id=? AND u.disabled=0 AND ((u.role='admin' AND ?=1) OR EXISTS(SELECT 1 FROM cp_group_users gu JOIN cp_node_groups ng ON ng.group_id=gu.group_id WHERE gu.user_id=u.id AND ng.node_id=?))`), user, scope, event.Data.NodeID).Scan(&n)
	return n > 0, err
}
