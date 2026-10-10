package platform

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agentdist"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

const autoUpgradeActor = "system:agent-update"
const maxAutoUpgrades = 2

type agentUpdates struct {
	releases *agentdist.Releases
	key      ed25519.PrivateKey
	keyHash  string
	version  string
	reason   string
}

type AgentUpdateSettings struct {
	Enabled         bool   `json:"enabled"`
	Version         int64  `json:"version"`
	ReleaseVersion  string `json:"release_version"`
	Available       bool   `json:"available"`
	Reason          string `json:"reason"`
	PublicKeySHA256 string `json:"public_key_sha256"`
}

type AgentUpdateStatus struct {
	State         string            `json:"state"`
	TargetVersion string            `json:"target_version"`
	Release       *contract.Upgrade `json:"release,omitempty"`
}

// Each installation signs its bundled artifacts using a key kept in the DB.
// Image/container replacement never rotates the key already trusted by Agents.
func (s *Server) ConfigureAgentUpdates(ctx context.Context, dir, version string) error {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	insert := "INSERT INTO cp_agent_update_settings(id,enabled,version,private_key) VALUES(1,1,1,?) ON CONFLICT(id) DO NOTHING"
	if s.Store.Dialect == "mysql" {
		insert = "INSERT IGNORE INTO cp_agent_update_settings(id,enabled,version,private_key) VALUES(1,1,1,?)"
	}
	err = s.Store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, s.q(insert), base64.StdEncoding.EncodeToString(key))
		return e
	})
	if err != nil {
		return err
	}
	var raw string
	if err = s.Store.DB.QueryRowContext(ctx, "SELECT private_key FROM cp_agent_update_settings WHERE id=1").Scan(&raw); err != nil {
		return err
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(data) != ed25519.PrivateKeySize {
		return errors.New("invalid persisted Agent signing key")
	}
	key = ed25519.PrivateKey(data)
	if !key.Equal(ed25519.NewKeyFromSeed(key.Seed())) {
		return errors.New("invalid persisted Agent signing key")
	}
	h := sha256.Sum256(key.Public().(ed25519.PublicKey))
	a := &agentUpdates{key: key, keyHash: hex.EncodeToString(h[:]), version: version}
	a.releases, err = agentdist.LoadReleases(dir, version)
	if errors.Is(err, os.ErrNotExist) {
		a.reason = "本版本未携带双架构 Agent 发布清单"
	} else if err != nil {
		a.reason = "Agent 发布清单与程序或校验值不匹配"
	}
	if a.releases != nil && !strings.HasPrefix(s.opts.Origin, "https://") {
		a.reason = "自动升级需要配置面板公开 HTTPS 地址"
	}
	s.agentUpdates = a
	return nil
}

func (s *Server) agentUpdateSettings(ctx context.Context, q operationQuery) (AgentUpdateSettings, error) {
	var enabled int
	u := AgentUpdateSettings{}
	if s.agentUpdates == nil {
		u.Reason = "自动升级尚未初始化"
		return u, nil
	}
	err := q.QueryRowContext(ctx, "SELECT enabled,version FROM cp_agent_update_settings WHERE id=1").Scan(&enabled, &u.Version)
	u.Enabled = enabled != 0
	u.ReleaseVersion = s.agentUpdates.version
	u.Available = s.agentUpdates.reason == "" && s.agentUpdates.releases != nil
	u.Reason = s.agentUpdates.reason
	u.PublicKeySHA256 = s.agentUpdates.keyHash
	return u, err
}

func (s *Server) getAgentUpdateSettings(w http.ResponseWriter, r *http.Request) {
	u, err := s.agentUpdateSettings(r.Context(), s.Store.DB)
	if err != nil {
		fail(w, 500, "Agent update settings unavailable")
		return
	}
	reply(w, 200, u)
}

func (s *Server) putAgentUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled *bool `json:"enabled"`
		Version int64 `json:"version"`
	}
	if !decode(w, r, &in) {
		return
	}
	if s.agentUpdates == nil || in.Enabled == nil {
		fail(w, 400, "Agent update settings required")
		return
	}
	u, _ := UserFromContext(r.Context())
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		enabled := 0
		if *in.Enabled {
			enabled = 1
		}
		result, err := tx.ExecContext(r.Context(), s.q("UPDATE cp_agent_update_settings SET enabled=?,version=version+1 WHERE id=1 AND version=?"), enabled, in.Version)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errConflict
		}
		return s.AuditTx(r.Context(), tx, u.ID, "agent.auto_update.settings", "")
	})
	if err != nil {
		fail(w, 409, "Agent update settings changed; refresh and retry")
		return
	}
	s.getAgentUpdateSettings(w, r)
}

func (s *Server) agentReleaseKey(w http.ResponseWriter, r *http.Request) {
	if s.agentUpdates == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(s.agentUpdates.key.Public().(ed25519.PublicKey)) + "\n"))
}

var stableAgentVersion = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

func olderAgentVersion(current, target string) bool {
	a, b := stableAgentVersion.FindStringSubmatch(current), stableAgentVersion.FindStringSubmatch(target)
	if a == nil || b == nil || b[4] != "" {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, err := strconv.ParseUint(a[i], 10, 64)
		if err != nil {
			return false
		}
		y, err := strconv.ParseUint(b[i], 10, 64)
		if err != nil {
			return false
		}
		if x != y {
			return x < y
		}
	}
	return a[4] != ""
}

func (s *Server) agentUpdateStatus(node contract.Node, u AgentUpdateSettings) AgentUpdateStatus {
	status := AgentUpdateStatus{State: "current", TargetVersion: u.ReleaseVersion}
	switch {
	case !u.Enabled:
		status.State = "disabled"
	case !u.Available:
		status.State = "unavailable"
	case node.OS != "linux" || node.Arch != "amd64" && node.Arch != "arm64":
		status.State = "unsupported"
	case !contains(node.Capabilities, "upgrade-v1") || node.UpgradeKeySHA256 != u.PublicKeySHA256:
		status.State = "requires_setup"
	case node.Version != u.ReleaseVersion && !olderAgentVersion(node.Version, u.ReleaseVersion):
		status.State = "version_skipped"
	case olderAgentVersion(node.Version, u.ReleaseVersion):
		status.State = "waiting"
	}
	if u.Available && node.OS == "linux" {
		if release, err := s.agentUpdates.releases.Upgrade(strings.TrimRight(s.opts.Origin, "/"), node.Arch, s.agentUpdates.key); err == nil {
			status.Release = &release
		}
	}
	return status
}

func (s *Server) getNodeAgentUpdate(w http.ResponseWriter, r *http.Request) {
	var raw string
	if err := s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT payload FROM cp_nodes WHERE id=?"), r.PathValue("id")).Scan(&raw); err != nil {
		fail(w, 404, "node unavailable")
		return
	}
	var node contract.Node
	if json.Unmarshal([]byte(raw), &node) != nil {
		fail(w, 500, "node unavailable")
		return
	}
	u, err := s.agentUpdateSettings(r.Context(), s.Store.DB)
	if err != nil {
		fail(w, 500, "update settings unavailable")
		return
	}
	status := s.agentUpdateStatus(node, u)
	if status.State == "waiting" {
		var state string
		fingerprint := digest(strJSON([]string{node.ID, "upgrade", strJSON(*status.Release)}))
		if s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT status FROM cp_node_operations WHERE node_id=? AND user_id=? AND payload_hash=? ORDER BY idempotency_key DESC LIMIT 1"), node.ID, autoUpgradeActor, fingerprint).Scan(&state) == nil {
			status.State = state
		}
	}
	reply(w, 200, status)
}

func (s *Server) automaticUpgradeAuthorized(ctx context.Context, tx *sql.Tx, op contract.NodeOperation, owner, access string) bool {
	if owner != autoUpgradeActor || op.Kind != "upgrade" || op.Upgrade == nil || s.agentUpdates == nil {
		return false
	}
	u, err := s.agentUpdateSettings(ctx, tx)
	if err != nil || access != digest("automatic:"+strJSON(*op.Upgrade)) {
		return false
	}
	signature, err := base64.StdEncoding.DecodeString(op.Upgrade.Signature)
	if err != nil || op.Upgrade.Validate() != nil || !ed25519.Verify(s.agentUpdates.key.Public().(ed25519.PublicKey), op.Upgrade.SignedMessage(), signature) {
		return false
	}
	var raw string
	if tx.QueryRowContext(ctx, s.q("SELECT payload FROM cp_nodes WHERE id=?"), op.NodeID).Scan(&raw) != nil {
		return false
	}
	var node contract.Node
	if json.Unmarshal([]byte(raw), &node) != nil || node.OS != op.Upgrade.OS || node.Arch != op.Upgrade.Arch || node.UpgradeKeySHA256 != u.PublicKeySHA256 || !contains(node.Capabilities, "upgrade-v1") {
		return false
	}
	// Once claimed, let the signed operation complete even if the administrator
	// stops the rollout or the panel is upgraded again during the download.
	if op.Status == "running" || op.Status == "staged" {
		return true
	}
	if !u.Enabled || !u.Available {
		return false
	}
	expected, err := s.agentUpdates.releases.Upgrade(strings.TrimRight(s.opts.Origin, "/"), node.Arch, s.agentUpdates.key)
	return err == nil && expected == *op.Upgrade && s.agentUpdateStatus(node, u).State == "waiting"
}

// Reconciliation also runs after offline nodes reconnect. Per-installation SQL
// locking caps fleet rollout across processes; idempotent attempts survive reboot.
func (s *Server) ReconcileAgentUpdates(ctx context.Context) error {
	if s.agentUpdates == nil {
		return nil
	}
	u, err := s.agentUpdateSettings(ctx, s.Store.DB)
	if err != nil || !u.Enabled || !u.Available {
		return err
	}
	offline := s.opts.OfflineNodeTime
	if offline <= 0 {
		offline = 20 * time.Second
	}
	rows, err := s.Store.DB.QueryContext(ctx, s.q("SELECT id FROM cp_nodes n WHERE last_seen>? AND "+liveNodeSQL+" ORDER BY id"), time.Now().Add(-offline).Unix())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, node := range ids {
		if err = s.enqueueAutomaticUpgrade(ctx, node); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) enqueueAutomaticUpgrade(ctx context.Context, nodeID string) error {
	return s.Store.Write(ctx, storage.Background, func(tx *sql.Tx) error {
		query := "SELECT enabled FROM cp_agent_update_settings WHERE id=1"
		if s.Store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		var enabled int
		if err := tx.QueryRowContext(ctx, query).Scan(&enabled); err != nil {
			return err
		}
		if enabled == 0 {
			return nil
		}
		if err := s.lockNodeTx(ctx, tx, nodeID); err != nil {
			return err
		}
		if err := s.nodeAvailableTx(ctx, tx, nodeID); err != nil {
			return nil
		}
		var raw string
		if err := tx.QueryRowContext(ctx, s.q("SELECT payload FROM cp_nodes WHERE id=?"), nodeID).Scan(&raw); err != nil {
			return err
		}
		var node contract.Node
		if err := json.Unmarshal([]byte(raw), &node); err != nil {
			return err
		}
		u, err := s.agentUpdateSettings(ctx, tx)
		if err != nil {
			return err
		}
		status := s.agentUpdateStatus(node, u)
		if status.State != "waiting" || status.Release == nil {
			return nil
		}
		release := *status.Release
		base := "auto:" + digest(nodeID+":"+strJSON(release))
		var attempts int
		var lastStatus string
		var lastAt int64
		rows, err := tx.QueryContext(ctx, s.q("SELECT status,updated_at FROM cp_node_operations WHERE user_id=? AND idempotency_key LIKE ? ORDER BY idempotency_key"), autoUpgradeActor, base+":%")
		if err != nil {
			return err
		}
		for rows.Next() {
			if err = rows.Scan(&lastStatus, &lastAt); err != nil {
				break
			}
			attempts++
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if attempts > 0 {
			if lastStatus != "failed" && lastStatus != "expired" {
				return nil
			}
			if attempts >= 3 || time.Now().Unix()-lastAt < int64(30*attempts*attempts) {
				return nil
			}
		}
		var active int
		if err = tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM cp_node_operations WHERE node_id=? AND status IN ('pending','running','staged') AND expires_at>?"), nodeID, time.Now().Unix()).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return nil
		}
		if err = tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM cp_node_operations WHERE user_id=? AND status IN ('pending','running','staged') AND expires_at>?"), autoUpgradeActor, time.Now().Unix()).Scan(&active); err != nil {
			return err
		}
		if active >= maxAutoUpgrades {
			return nil
		}
		op := contract.NodeOperation{ID: id(), NodeID: nodeID, Kind: "upgrade", Status: "pending", Upgrade: &release, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}
		_, err = tx.ExecContext(ctx, s.q(`INSERT INTO cp_node_operations(id,node_id,user_id,kind,status,payload,payload_hash,claim_token,access_hash,idempotency_key,error,created_at,updated_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), op.ID, nodeID, autoUpgradeActor, op.Kind, op.Status, strJSON(release), digest(strJSON([]string{nodeID, op.Kind, strJSON(release)})), "", digest("automatic:"+strJSON(release)), base+":"+strconv.Itoa(attempts+1), "", op.CreatedAt.Unix(), op.CreatedAt.Unix(), op.ExpiresAt.Unix())
		if err != nil {
			return err
		}
		return s.AuditTx(ctx, tx, autoUpgradeActor, "node.auto_upgrade.create", op.ID)
	})
}

func (s *Server) RunAgentUpdates(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		if err := s.ReconcileAgentUpdates(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "Agent automatic update reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
