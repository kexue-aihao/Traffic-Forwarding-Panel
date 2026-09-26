package platform

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestUninstallRequiresEmptyAcknowledgedNodeAndHidesOnlyAfterResult(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	ack := func(version int64) {
		t.Helper()
		if r := f.req("POST", "/agent/ack", contract.Ack{Version: version, AppliedVersion: version, Capabilities: []string{"uninstall-v1", "shell-v1"}}, n.Token); r.Code != 204 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	ack(1)
	access := read[map[string]string](t, f.req("POST", "/nodes/"+n.NodeID+"/operation-access", map[string]string{"password": "test-password-long"}, ""), 201)
	input := map[string]string{"access_token": access["token"], "idempotency_key": "uninstall-test-node"}
	create := func() int { return f.req("POST", "/nodes/"+n.NodeID+"/uninstall", input, "").Code }
	rule := read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
	if create() != 409 {
		t.Fatal("uninstall accepted a live rule")
	}
	if r := f.req("DELETE", fmt.Sprintf("/rules/%s?version=%d", rule.ID, rule.Version), nil, ""); r.Code != 204 {
		t.Fatal(r.Body.String())
	}
	if create() != 409 {
		t.Fatal("uninstall accepted pending removal ACK")
	}
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	ack(cfg.Version)
	op := read[contract.NodeOperation](t, f.req("POST", "/nodes/"+n.NodeID+"/uninstall", input, ""), 201)
	if r := f.req("POST", "/rules", ruleFor(g, n), ""); r.Code != 409 {
		t.Fatal("rule raced uninstall", r.Code)
	}
	repeated := read[contract.NodeOperation](t, f.req("POST", "/nodes/"+n.NodeID+"/uninstall", input, ""), 201)
	if repeated.ID != op.ID {
		t.Fatal("non-idempotent uninstall")
	}
	control := read[contract.Control](t, f.req("POST", "/agent/control", nil, n.Token), 200)
	if control.Operation == nil {
		t.Fatal("uninstall not claimed")
	}
	if r := f.req("POST", "/node-operations/"+op.ID+"/cancel", nil, ""); r.Code != 409 {
		t.Fatal("running uninstall cancelled")
	}
	// Even after expiry, password-grant removal and a missed heartbeat, an
	// irreversible claimed uninstall is neither requeued nor reported complete.
	_, e := f.s.Store.DB.ExecContext(context.Background(), f.s.q("UPDATE cp_node_operations SET updated_at=?,expires_at=? WHERE id=?"), time.Now().Add(-time.Hour).Unix(), time.Now().Add(-time.Minute).Unix(), op.ID)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.s.Store.DB.ExecContext(context.Background(), "DELETE FROM cp_operation_access")
	if e != nil {
		t.Fatal(e)
	}
	next := read[contract.Control](t, f.req("POST", "/agent/control", nil, n.Token), 200)
	if next.Operation == nil || next.Operation.Claim != control.Operation.Claim || len(next.Active) != 1 {
		t.Fatal("uninstall requeued/expired")
	}
	list := read[struct{ Items []contract.Node }](t, f.req("GET", "/nodes", nil, ""), 200)
	if len(list.Items) != 1 {
		t.Fatal("hidden before completion")
	}
	result := contract.OperationResult{ID: op.ID, Claim: control.Operation.Claim, Status: "succeeded"}
	bad := result
	bad.Claim = "invalid"
	if r := f.req("POST", "/agent/control/result", bad, n.Token); r.Code != 409 {
		t.Fatal("wrong claim accepted")
	}
	for range 2 {
		if r := f.req("POST", "/agent/control/result", result, n.Token); r.Code != 204 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	list = read[struct{ Items []contract.Node }](t, f.req("GET", "/nodes", nil, ""), 200)
	if len(list.Items) != 0 {
		t.Fatal("completed node still visible")
	}
	if r := f.req("GET", "/agent/config", nil, n.Token); r.Code != 401 {
		t.Fatal("removed credential can still fetch configuration")
	}
	if r := f.req("POST", "/rules", ruleFor(g, n), ""); r.Code != 409 {
		t.Fatal("removed node can host rules")
	}
}

func TestUninstallPendingCanBeCancelled(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	f.req("POST", "/agent/ack", contract.Ack{Version: 1, AppliedVersion: 1, Capabilities: []string{"uninstall-v1"}}, n.Token)
	access := read[map[string]string](t, f.req("POST", "/nodes/"+n.NodeID+"/operation-access", map[string]string{"password": "test-password-long"}, ""), 201)
	op := read[contract.NodeOperation](t, f.req("POST", "/nodes/"+n.NodeID+"/uninstall", map[string]string{"access_token": access["token"], "idempotency_key": "cancel-uninstall"}, ""), 201)
	if r := f.req("POST", "/node-operations/"+op.ID+"/cancel", nil, ""); r.Code != 204 {
		t.Fatal(r.Code)
	}
	if r := f.req("POST", "/rules", ruleFor(g, n), ""); r.Code != 201 {
		t.Fatal("cancel did not release reservation", r.Code, r.Body.String())
	}
}

// WebSSH 换来的授权不带密码，所以它只够开终端。这一条守着「顺手把卸载也放行」
// 这类改动：卸载要的是密码换来的 sensitive 授权。
func TestShellScopeCannotUninstall(t *testing.T) {
	f := setup(t)
	_, n := f.node()
	if r := f.req("POST", "/agent/ack", contract.Ack{Version: 1, AppliedVersion: 1, Capabilities: []string{"uninstall-v1", "shell-v1"}}, n.Token); r.Code != 204 {
		t.Fatal(r.Code, r.Body.String())
	}
	access := read[map[string]string](t, f.req("POST", "/nodes/"+n.NodeID+"/operation-access", map[string]string{"scope": "shell"}, ""), 201)
	in := map[string]string{"access_token": access["token"], "idempotency_key": "shell-scope-uninstall"}
	if r := f.req("POST", "/nodes/"+n.NodeID+"/uninstall", in, ""); r.Code != 409 {
		t.Fatalf("scope=shell 的授权不该能卸载: %d %s", r.Code, r.Body.String())
	}
	if r := f.req("POST", "/nodes/"+n.NodeID+"/shell", in, ""); r.Code != 201 {
		t.Fatalf("scope=shell 的授权应当能开终端: %d %s", r.Code, r.Body.String())
	}
}
