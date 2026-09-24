package integration

import (
	"net"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func TestIdentityReassignmentRevokesAndRestoresAppliedRules(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	target, _ := targets(t)
	rule := f.rule("tcp", "direct", target, nil)
	f.sync()
	transfer(t, rule, []byte("authorized identity"))
	renamed := decode[contract.IdentityGroup](t, f.request("PUT", "/identity-groups/"+f.owner.IdentityGroupID, map[string]any{"id": "client-1001", "name": "Client identity"}, f.admin, 200))
	f.owner.IdentityGroupID = renamed.ID
	f.sync()
	transfer(t, rule, []byte("renamed identity keeps access"))

	replacement := decode[contract.IdentityGroup](t, f.request("POST", "/identity-groups", map[string]any{"id": "no-device-access", "name": "no-device-access"}, f.admin, 201))
	f.request("PUT", "/users/"+f.owner.ID+"/identity-group", map[string]any{"identity_group_id": replacement.ID}, f.admin, 204)
	f.sync()
	if len(f.store.Config().Rules) != 0 {
		t.Fatal("old identity rules remained applied")
	}
	if conn, err := net.DialTimeout("tcp", rule.Listen, time.Second); err == nil {
		conn.Close()
		t.Fatal("old identity listener remained open")
	}
	page := decode[struct {
		Items []contract.Node `json:"items"`
	}](t, f.request("GET", "/nodes", nil, f.user, 200))
	if len(page.Items) != 0 {
		t.Fatal("existing session retained old node access")
	}
	f.request("PUT", "/users/"+f.owner.ID+"/identity-group", map[string]any{"identity_group_id": f.owner.IdentityGroupID}, f.admin, 204)
	f.sync()
	transfer(t, rule, []byte("restored identity"))
}
