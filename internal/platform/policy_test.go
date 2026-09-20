package platform

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func TestProtocolLayersDoNotLeakCarriersToApplicationDetectors(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	g.BlockedProtocols = []string{"transport:ws", "app:socks", "app:http"}
	g = read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	r := read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
	c := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(c.Rules) != 1 || len(c.Rules[0].BlockedProtocols) != 2 || !contains(c.Rules[0].BlockedProtocols, "socks") || !contains(c.Rules[0].BlockedProtocols, "http") {
		t.Fatal(c)
	}
	g.BlockedProtocols = []string{"transport:direct"}
	read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	c = read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(c.Rules) != 0 {
		t.Fatalf("parent transport deny bypassed: %s", r.ID)
	}
}

type unavailableAllocator struct{}

func (unavailableAllocator) Allocate(context.Context, *sql.Tx, string, string, string) (*contract.Lease, error) {
	return nil, contract.ErrEntitlementUnavailable
}

func TestExhaustedEntitlementStillPublishesConfiguration(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
	c := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	r := c.Rules[0]
	r.Lease.ExpiresAt = time.Now().Add(-time.Minute)
	if err := f.s.Store.Write(context.Background(), storage.Critical, func(tx *sql.Tx) error {
		_, e := tx.Exec(f.s.q("UPDATE cp_rules SET payload=? WHERE id=?"), strJSON(r), r.ID)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	f.s.opts.Entitlements = unavailableAllocator{}
	next := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(next.Rules) != 0 || next.Version <= c.Version {
		t.Fatal("expired rule blocked config revocation", next)
	}
}
