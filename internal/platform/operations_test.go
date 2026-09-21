package platform

import (
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestNodeOperationAuthorizationClaimAndResult(t *testing.T) {
	f := setup(t)
	_, registered := f.node()
	if rr := f.req("POST", "/agent/ack", contract.Ack{Capabilities: []string{"terminal-v1"}, Version: 1, AppliedVersion: 1}, registered.Token); rr.Code != 204 {
		t.Fatalf("capability ACK: %d %s", rr.Code, rr.Body.String())
	}
	access := read[map[string]any](f.t, f.req("POST", "/nodes/"+registered.NodeID+"/operation-access", map[string]any{"password": "test-password-long"}, ""), 201)
	raw, ok := access["token"].(string)
	if !ok || len(raw) != 64 {
		t.Fatal("missing short-lived operation token")
	}
	op := read[contract.NodeOperation](f.t, f.req("POST", "/nodes/"+registered.NodeID+"/terminal", map[string]any{"access_token": raw, "idempotency_key": "terminal-operation-1"}, ""), 201)
	control := read[contract.Control](f.t, f.req("POST", "/agent/control", nil, registered.Token), 200)
	if control.Operation == nil || control.Operation.ID != op.ID || control.Operation.Claim == "" {
		t.Fatal("operation was not claimed by node")
	}
	if rr := f.req("POST", "/agent/control/result", contract.OperationResult{ID: op.ID, Claim: control.Operation.Claim, Status: "succeeded"}, registered.Token); rr.Code != 204 {
		t.Fatalf("operation result: %d %s", rr.Code, rr.Body.String())
	}
	items := read[map[string]any](f.t, f.req("GET", "/nodes/"+registered.NodeID+"/operations", nil, ""), 200)
	if len(items["items"].([]any)) != 1 {
		t.Fatal("operation list did not retain completed task")
	}
}
