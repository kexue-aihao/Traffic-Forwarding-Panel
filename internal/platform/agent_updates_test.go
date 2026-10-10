package platform

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agentdist"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func updateFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := setup(t)
	dir := t.TempDir()
	for _, arch := range []string{"amd64", "arm64"} {
		b := make([]byte, 32)
		copy(b, "\x7fELF\x02\x01")
		b[18] = 62
		if arch == "arm64" {
			b[18] = 183
		}
		if err := os.WriteFile(filepath.Join(dir, "agent-linux-"+arch), b, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := agentdist.WriteReleaseManifest(dir, "0.1.42"); err != nil {
		t.Fatal(err)
	}
	f.s.opts.Origin = "https://panel.example.com"
	if err := f.s.ConfigureAgentUpdates(context.Background(), dir, "0.1.42"); err != nil {
		t.Fatal(err)
	}
	return f, dir
}

func updateNode(t *testing.T, f *fixture, arch string, capable bool) contract.Registered {
	t.Helper()
	_, n := f.node()
	var raw string
	if err := f.s.Store.DB.QueryRow(f.s.q("SELECT payload FROM cp_nodes WHERE id=?"), n.NodeID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var node contract.Node
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		t.Fatal(err)
	}
	node.OS, node.Arch, node.Version = "linux", arch, "0.1.41"
	if capable {
		node.Capabilities = []string{"upgrade-v1"}
		node.UpgradeKeySHA256 = f.s.agentUpdates.keyHash
	}
	if _, err := f.s.Store.DB.Exec(f.s.q("UPDATE cp_nodes SET payload=? WHERE id=?"), strJSON(node), node.ID); err != nil {
		t.Fatal(err)
	}
	return n
}

func reconcileUpdates(t *testing.T, f *fixture) {
	t.Helper()
	if err := f.s.ReconcileAgentUpdates(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func updateCount(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	if err := f.s.Store.DB.QueryRow("SELECT COUNT(*) FROM cp_node_operations WHERE user_id='system:agent-update'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAutomaticAgentUpgradeSignedClaimAndVersionACK(t *testing.T) {
	f, dir := updateFixture(t)
	n := updateNode(t, f, "arm64", true)
	reconcileUpdates(t, f)
	reconcileUpdates(t, f)
	if updateCount(t, f) != 1 {
		t.Fatal("automatic upgrade duplicated")
	}
	control := read[contract.Control](t, f.req("POST", "/agent/control", nil, n.Token), 200)
	if control.Operation == nil || control.Operation.Upgrade == nil {
		t.Fatal("automatic task expired without administrator session")
	}
	op := *control.Operation
	if op.Upgrade.Arch != "arm64" || op.Upgrade.Version != "0.1.42" {
		t.Fatal("wrong release platform/version")
	}
	sig, _ := base64.StdEncoding.DecodeString(op.Upgrade.Signature)
	if !ed25519.Verify(f.s.agentUpdates.key.Public().(ed25519.PublicKey), op.Upgrade.SignedMessage(), sig) {
		t.Fatal("automatic release signature invalid")
	}
	if strings.Contains(op.Upgrade.URL, "http://") || !strings.Contains(op.Upgrade.URL, "sha256=") {
		t.Fatal("release URL not pinned")
	}
	// A panel restart reuses the key, allowing the already queued claim.
	keyHash := f.s.agentUpdates.keyHash
	if err := f.s.ConfigureAgentUpdates(context.Background(), dir, "0.1.42"); err != nil {
		t.Fatal(err)
	}
	if f.s.agentUpdates.keyHash != keyHash {
		t.Fatal("panel restart rotated signing key")
	}
	for _, status := range []string{"staged", "succeeded"} {
		if rr := f.req("POST", "/agent/control/result", contract.OperationResult{ID: op.ID, Claim: op.Claim, Status: status}, n.Token); rr.Code != 204 {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	ack := contract.Ack{Version: 1, AppliedVersion: 1, AgentVersion: "0.1.42", Capabilities: []string{"upgrade-v1"}, UpgradeKeySHA256: keyHash}
	if rr := f.req("POST", "/agent/ack", ack, n.Token); rr.Code != 204 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	reconcileUpdates(t, f)
	if updateCount(t, f) != 1 {
		t.Fatal("up-to-date Agent upgraded again")
	}
	status := read[AgentUpdateStatus](t, f.req("GET", "/nodes/"+n.NodeID+"/agent-update", nil, ""), 200)
	if status.State != "current" {
		t.Fatal("upgraded version not visible", status.State)
	}
}

func TestAutomaticAgentUpgradeFleetBoundOfflineAndBootstrap(t *testing.T) {
	f, _ := updateFixture(t)
	var nodes []contract.Registered
	for i := 0; i < 5; i++ {
		nodes = append(nodes, updateNode(t, f, "amd64", true))
	}
	legacy := updateNode(t, f, "amd64", false)
	offline := updateNode(t, f, "arm64", true)
	if _, err := f.s.Store.DB.Exec(f.s.q("UPDATE cp_nodes SET last_seen=? WHERE id=?"), time.Now().Add(-time.Hour).Unix(), offline.NodeID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- f.s.ReconcileAgentUpdates(context.Background()) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if updateCount(t, f) != maxAutoUpgrades {
		t.Fatal("fleet bound exceeded or concurrent scheduling duplicated", updateCount(t, f))
	}
	status := read[AgentUpdateStatus](t, f.req("GET", "/nodes/"+legacy.NodeID+"/agent-update", nil, ""), 200)
	if status.State != "requires_setup" {
		t.Fatal("old Agent capability problem hidden")
	}
	// Drain the currently pending fleet and verify newly reconnected nodes join.
	if _, err := f.s.Store.DB.Exec("UPDATE cp_node_operations SET status='succeeded'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Store.DB.Exec(f.s.q("UPDATE cp_nodes SET last_seen=? WHERE id=?"), time.Now().Unix(), offline.NodeID); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		reconcileUpdates(t, f)
		if _, err := f.s.Store.DB.Exec("UPDATE cp_node_operations SET status='succeeded'"); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := f.s.Store.DB.QueryRow(f.s.q("SELECT COUNT(*) FROM cp_node_operations WHERE node_id=?"), offline.NodeID).Scan(&n); err != nil || n != 1 {
		t.Fatal("offline Agent did not catch up", n, err)
	}
	if updateCount(t, f) != 6 {
		t.Fatal("legacy node scheduled or fleet not drained", updateCount(t, f))
	}
}

func TestAutomaticAgentUpgradeClaimSurvivesRolloutStopAndPanelUpdate(t *testing.T) {
	f, dir := updateFixture(t)
	n := updateNode(t, f, "amd64", true)
	reconcileUpdates(t, f)
	control := read[contract.Control](t, f.req("POST", "/agent/control", nil, n.Token), 200)
	if control.Operation == nil {
		t.Fatal("missing automatic upgrade")
	}
	op := *control.Operation
	u := read[AgentUpdateSettings](t, f.req("GET", "/agent-update-settings", nil, ""), 200)
	_ = read[AgentUpdateSettings](t, f.req("PUT", "/agent-update-settings", map[string]any{"enabled": false, "version": u.Version}, ""), 200)
	// A subsequent panel version with unavailable artifacts must still retain
	// the already claimed signed operation until the Agent acknowledges it.
	if err := f.s.ConfigureAgentUpdates(context.Background(), dir, "0.1.43"); err != nil {
		t.Fatal(err)
	}
	control = read[contract.Control](t, f.req("POST", "/agent/control", nil, n.Token), 200)
	if !contains(control.Active, op.ID) {
		t.Fatal("claimed operation revoked during rollout stop")
	}
	for _, state := range []string{"staged", "succeeded"} {
		if rr := f.req("POST", "/agent/control/result", contract.OperationResult{ID: op.ID, Claim: op.Claim, Status: state}, n.Token); rr.Code != 204 {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	var state string
	if err := f.s.Store.DB.QueryRow(f.s.q("SELECT status FROM cp_node_operations WHERE id=?"), op.ID).Scan(&state); err != nil || state != "succeeded" {
		t.Fatal(state, err)
	}
}

func TestAutomaticAgentUpgradeDisableAndTamperedTaskCannotClaim(t *testing.T) {
	for _, mode := range []string{"disabled", "tampered", "untrusted-key", "no-https", "mismatched-release"} {
		t.Run(mode, func(t *testing.T) {
			f, dir := updateFixture(t)
			n := updateNode(t, f, "amd64", true)
			reconcileUpdates(t, f)
			switch mode {
			case "disabled":
				u := read[AgentUpdateSettings](t, f.req("GET", "/agent-update-settings", nil, ""), 200)
				_ = read[AgentUpdateSettings](t, f.req("PUT", "/agent-update-settings", map[string]any{"enabled": false, "version": u.Version}, ""), 200)
				if rr := f.req("PUT", "/agent-update-settings", map[string]any{"enabled": true, "version": u.Version}, ""); rr.Code != 409 {
					t.Fatal("stale settings update accepted")
				}
			case "tampered":
				var raw string
				if err := f.s.Store.DB.QueryRow("SELECT payload FROM cp_node_operations").Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var release contract.Upgrade
				if err := json.Unmarshal([]byte(raw), &release); err != nil {
					t.Fatal(err)
				}
				release.URL = "https://untrusted.example/agent"
				if _, err := f.s.Store.DB.Exec(f.s.q("UPDATE cp_node_operations SET payload=?"), strJSON(release)); err != nil {
					t.Fatal(err)
				}
			case "untrusted-key":
				f.s.agentUpdates.keyHash = strings.Repeat("f", 64)
				// Claims must re-check trust in case a node changes key after enqueue.
			case "no-https":
				f.s.opts.Origin = "http://panel.example.com"
				if err := f.s.ConfigureAgentUpdates(context.Background(), dir, "0.1.42"); err != nil {
					t.Fatal(err)
				}
			case "mismatched-release":
				if err := f.s.ConfigureAgentUpdates(context.Background(), dir, "0.1.43"); err != nil {
					t.Fatal(err)
				}
			}
			control := read[contract.Control](t, f.req("POST", "/agent/control", nil, n.Token), 200)
			if control.Operation != nil {
				t.Fatal("automatic authorization accepted invalid release/policy", mode)
			}
		})
	}
}

func TestAutomaticAgentUpgradeRetriesAreBoundedAndRollbackStops(t *testing.T) {
	for _, end := range []string{"failed", "rolled_back", "cancelled"} {
		t.Run(end, func(t *testing.T) {
			f, _ := updateFixture(t)
			n := updateNode(t, f, "amd64", true)
			reconcileUpdates(t, f)
			for attempt := 1; attempt <= 3; attempt++ {
				control := read[contract.Control](t, f.req("POST", "/agent/control", nil, n.Token), 200)
				if control.Operation == nil {
					t.Fatal("missing retry", attempt)
				}
				op := *control.Operation
				if end == "cancelled" {
					if rr := f.req("POST", "/node-operations/"+op.ID+"/cancel", nil, ""); rr.Code != 204 {
						t.Fatal(rr.Code, rr.Body.String())
					}
				} else {
					if end == "rolled_back" {
						if rr := f.req("POST", "/agent/control/result", contract.OperationResult{ID: op.ID, Claim: op.Claim, Status: "staged"}, n.Token); rr.Code != 204 {
							t.Fatal(rr.Code)
						}
					}
					if rr := f.req("POST", "/agent/control/result", contract.OperationResult{ID: op.ID, Claim: op.Claim, Status: end}, n.Token); rr.Code != 204 {
						t.Fatal(rr.Code, rr.Body.String())
					}
				}
				reconcileUpdates(t, f)
				if updateCount(t, f) != attempt {
					t.Fatal("retry ignored backoff or repeated rollback")
				}
				if _, err := f.s.Store.DB.Exec("UPDATE cp_node_operations SET updated_at=0"); err != nil {
					t.Fatal(err)
				}
				reconcileUpdates(t, f)
				if end != "failed" {
					if updateCount(t, f) != 1 {
						t.Fatal("rolled back/cancelled node automatically restarted again")
					}
					break
				}
			}
			if end == "failed" && updateCount(t, f) != 3 {
				t.Fatal("retry limit not enforced")
			}
		})
	}
}

func TestAutomaticAgentUpgradeKeyEndpointDoesNotExposePrivateKey(t *testing.T) {
	f, _ := updateFixture(t)
	w := httptest.NewRecorder()
	f.m.ServeHTTP(w, httptest.NewRequest("GET", "/download/agent-release-key", nil))
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(w.Body.String()))
	if w.Code != 200 || err != nil || len(data) != ed25519.PublicKeySize {
		t.Fatal("installer public key unavailable")
	}
	u := read[AgentUpdateSettings](t, f.req("GET", "/agent-update-settings", nil, ""), 200)
	if !u.Enabled || !u.Available {
		t.Fatal("automatic follow not enabled by default")
	}
	if strings.Contains(w.Body.String(), base64.StdEncoding.EncodeToString(f.s.agentUpdates.key)) {
		t.Fatal("private key exposed")
	}
	old := f.cookie
	f.cookie = nil
	if rr := f.req("PUT", "/agent-update-settings", map[string]any{"enabled": false, "version": u.Version}, ""); rr.Code != 401 {
		t.Fatal("unauthenticated update setting accepted", rr.Code)
	}
	f.cookie = old
}

func TestAgentVersionNeverAutomaticallyDowngrades(t *testing.T) {
	for _, tc := range []struct {
		current, target string
		older           bool
	}{
		{"0.1.41", "0.1.42", true}, {"v0.1.41", "0.1.42", true}, {"0.1.42-tcp-credit-test", "0.1.42", true}, {"0.1.42", "0.1.42", false}, {"0.1.43", "0.1.42", false}, {"0.2.0", "0.1.42", false}, {"dev", "0.1.42", false}, {"0.1.41", "0.1.42-test", false},
	} {
		if got := olderAgentVersion(tc.current, tc.target); got != tc.older {
			t.Fatal(tc, got)
		}
	}
}
