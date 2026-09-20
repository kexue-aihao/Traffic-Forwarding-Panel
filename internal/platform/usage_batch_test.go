package platform

import (
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"testing"
	"time"
)

func TestUsageBatchMixedReplayAtomicIsolation(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	r := cfg.Rules[0]
	u := contract.UsageRecord{ID: "batch-first", NodeID: n.NodeID, RuleID: r.ID, LeaseID: r.Lease.ID, EntitlementID: r.Lease.EntitlementID, StartedAt: time.Now().Add(-time.Second), EndedAt: time.Now(), UploadBytes: 10}
	read[map[string]any](t, f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u, u}}, n.Token), 200)
	bad := u
	bad.ID = "foreign"
	bad.RuleID = "other-user-rule"
	fresh := u
	fresh.ID = "fresh"
	if rr := f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u, fresh, bad}}, n.Token); rr.Code != 409 {
		t.Fatal("foreign rule accepted", rr.Code)
	}
	var count, used int64
	f.s.Store.DB.QueryRow(`SELECT COUNT(*) FROM cp_usage`).Scan(&count)
	f.s.Store.DB.QueryRow(f.s.q(`SELECT bytes_used FROM cp_rule_leases WHERE id=?`), r.Lease.ID).Scan(&used)
	if count != 1 || used != 10 {
		t.Fatal("batch failure partially committed", count, used)
	}
	read[map[string]any](t, f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u, fresh, fresh}}, n.Token), 200)
	f.s.Store.DB.QueryRow(`SELECT COUNT(*) FROM cp_usage`).Scan(&count)
	f.s.Store.DB.QueryRow(f.s.q(`SELECT bytes_used FROM cp_rule_leases WHERE id=?`), r.Lease.ID).Scan(&used)
	if count != 2 || used != 20 {
		t.Fatal("dedup failed", count, used)
	}
}
