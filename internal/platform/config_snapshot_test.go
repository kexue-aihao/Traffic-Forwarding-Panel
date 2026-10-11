package platform

import (
	"context"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestNodeConfigReadsCommittedSnapshotDuringOwnerUpdate(t *testing.T) {
	f := setup(t)
	g, node := f.node()
	rule := read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, node), ""), 201)
	before := read[contract.Config](t, f.req("GET", "/agent/config", nil, node.Token), 200)
	tx, err := f.s.Store.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(f.s.q("UPDATE cp_users SET disabled=1 WHERE id=?"), rule.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.s.q("UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id=?"), node.NodeID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	during, err := f.s.nodeConfig(ctx, node.NodeID)
	if err != nil {
		t.Fatal("committed snapshot was blocked by an uncommitted owner/node update", err)
	}
	if during.Version != before.Version || len(during.Rules) != 1 || during.Rules[0].ID != rule.ID {
		t.Fatal("configuration mixed committed and uncommitted policy", during)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	after, err := f.s.nodeConfig(context.Background(), node.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version+1 || len(after.Rules) != 0 {
		t.Fatal("new snapshot did not apply the committed revocation and version", after)
	}
}
