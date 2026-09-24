package platform

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func groupDeleteURL(g contract.Group) string {
	return fmt.Sprintf("/groups/%s?version=%d", g.ID, g.Version)
}

func TestDeleteGroupDetachesDevicesAndRevokesEnrollment(t *testing.T) {
	f := setup(t)
	identity := read[contract.IdentityGroup](t, f.req("POST", "/identity-groups", map[string]any{"id": "delete-test", "name": "delete-test"}, ""), 201)
	group := read[contract.Group](t, f.req("POST", "/groups", map[string]any{"name": "remove", "type": "monitor", "identity_group_ids": []string{identity.ID}}, ""), 201)
	other := read[contract.Group](t, f.req("POST", "/groups", map[string]any{"name": "retain", "type": "monitor"}, ""), 201)
	key := read[map[string]string](t, f.req("GET", "/groups/"+group.ID+"/join-key", nil, ""), 200)["join_key"]
	enroll := func() string {
		return read[map[string]string](t, f.req("POST", "/nodes/enrollment", map[string]any{"name": "shared", "group_ids": []string{group.ID, other.ID}}, ""), 201)["token"]
	}
	node := read[contract.Registered](t, f.req("POST", "/agent/register", contract.Registration{Token: enroll(), Name: "shared"}, ""), 201)
	unusedToken := enroll()
	if response := f.req("DELETE", groupDeleteURL(group), nil, ""); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	page := read[struct{ Items []contract.Node }](t, f.req("GET", "/nodes", nil, ""), 200)
	if len(page.Items) != 1 || len(page.Items[0].GroupIDs) != 1 || page.Items[0].GroupIDs[0] != other.ID {
		t.Fatal("device or other membership lost", page)
	}
	config := read[contract.Config](t, f.req("GET", "/agent/config", nil, node.Token), 200)
	if config.Version < 2 {
		t.Fatal("device configuration was not updated")
	}
	for _, credential := range []string{key, unusedToken} {
		if response := f.req("POST", "/agent/register", contract.Registration{Token: credential, Name: "stale"}, ""); response.Code != 401 {
			t.Fatal("deleted group credential still accepted", response.Code)
		}
	}
	if response := f.req("DELETE", "/identity-groups/"+identity.ID, nil, ""); response.Code != 204 {
		t.Fatal("group grant not removed", response.Code, response.Body.String())
	}
	var audits int
	if err := f.s.Store.DB.QueryRow(f.s.q("SELECT COUNT(*) FROM cp_audit WHERE action='group.delete' AND target=?"), group.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("missing deletion audit", audits, err)
	}
	if response := f.req("DELETE", groupDeleteURL(group), nil, ""); response.Code != 404 {
		t.Fatal("missing group not reported", response.Code)
	}
}

func TestDeleteGroupAuthorizationAndVersion(t *testing.T) {
	f := setup(t)
	g, _ := f.node()
	admin := f.cookie
	user := read[contract.UserCreated](t, f.req("POST", "/users", map[string]string{"username": "ordinary", "role": "user"}, ""), 201)
	login := f.req("POST", "/auth/login", map[string]string{"username": user.Username, "password": user.InitialPassword}, "")
	f.cookie = login.Result().Cookies()[0]
	if response := f.req("DELETE", groupDeleteURL(g), nil, ""); response.Code != 403 {
		t.Fatal("ordinary user deleted group", response.Code)
	}
	f.cookie = nil
	if response := f.req("DELETE", groupDeleteURL(g), nil, ""); response.Code != 401 {
		t.Fatal("anonymous user deleted group", response.Code)
	}
	f.cookie = admin
	key := read[map[string]any](t, f.req("POST", "/auth/tokens", map[string]any{"name": "owner", "permanent": true}, ""), 201)["token"].(string)
	if response := f.req("DELETE", groupDeleteURL(g), nil, key); response.Code != 403 {
		t.Fatal("API token gained administrative deletion", response.Code)
	}
	for _, version := range []string{"", "0", "-1", "bad"} {
		if response := f.req("DELETE", "/groups/"+g.ID+"?version="+version, nil, ""); response.Code != 400 {
			t.Fatal("invalid version accepted", version, response.Code)
		}
	}
	updated := read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	if response := f.req("DELETE", groupDeleteURL(g), nil, ""); response.Code != 409 {
		t.Fatal("stale version deleted group", response.Code)
	}
	if response := f.req("DELETE", groupDeleteURL(updated), nil, ""); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestDeleteGroupWaitsForRuleACKAndPreservesLateUsage(t *testing.T) {
	f := setup(t)
	g, node := f.node()
	rule := read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, node), ""), 201)
	initial := read[contract.Config](t, f.req("GET", "/agent/config", nil, node.Token), 200)
	lease := initial.Rules[0].Lease
	usage := contract.UsageRecord{ID: "before-group-deletion", NodeID: node.NodeID, RuleID: rule.ID, LeaseID: lease.ID, EntitlementID: lease.EntitlementID, StartedAt: time.Now().Add(-time.Second), EndedAt: time.Now(), UploadBytes: 10}
	batch := contract.UsageBatch{Records: []contract.UsageRecord{usage}}
	read[map[string]any](t, f.req("POST", "/agent/usage", batch, node.Token), 200)
	for _, enabled := range []bool{true, false} {
		rule.Enabled = enabled
		rule = read[contract.Rule](t, f.req("PUT", "/rules/"+rule.ID, rule, ""), 200)
		if response := f.req("DELETE", groupDeleteURL(g), nil, ""); response.Code != 409 || !strings.Contains(response.Body.String(), "转发规则") {
			t.Fatal("group with live/disabled rule deleted", response.Code, response.Body.String())
		}
	}
	if response := f.req("DELETE", fmt.Sprintf("/rules/%s?version=%d", rule.ID, rule.Version), nil, ""); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	if response := f.req("DELETE", groupDeleteURL(g), nil, ""); response.Code != 409 || !strings.Contains(response.Body.String(), "等待节点") {
		t.Fatal("group removed before ACK", response.Code, response.Body.String())
	}
	config := read[contract.Config](t, f.req("GET", "/agent/config", nil, node.Token), 200)
	if response := f.req("POST", "/agent/ack", contract.Ack{Version: config.Version, AppliedVersion: config.Version}, node.Token); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	if response := f.req("DELETE", groupDeleteURL(g), nil, ""); response.Code != 204 {
		t.Fatal("acknowledged rule prevented deletion", response.Code, response.Body.String())
	}
	// Replay is still deduplicated and a delayed final WAL record still settles.
	read[map[string]any](t, f.req("POST", "/agent/usage", batch, node.Token), 200)
	usage.ID = "after-group-deletion"
	batch.Records = []contract.UsageRecord{usage}
	read[map[string]any](t, f.req("POST", "/agent/usage", batch, node.Token), 200)
	var used int64
	if err := f.s.Store.DB.QueryRow(f.s.q("SELECT bytes_used FROM cp_rule_leases WHERE id=?"), usage.LeaseID).Scan(&used); err != nil || used != 20 {
		t.Fatal("late usage lost or replay double charged", used, err)
	}
	if response := f.req("POST", "/agent/leases/retire", map[string]string{"lease_id": usage.LeaseID, "used_bytes": "20"}, node.Token); response.Code != 204 {
		t.Fatal("lease retirement after deletion failed", response.Code, response.Body.String())
	}
}

func TestDeleteGroupRejectsGroupReferences(t *testing.T) {
	for _, reference := range []string{"chain", "ipv6", "reverse"} {
		t.Run(reference, func(t *testing.T) {
			f := setup(t)
			create := func(name string) contract.Group {
				return read[contract.Group](t, f.req("POST", "/groups", map[string]string{"name": name, "type": "exit"}, ""), 201)
			}
			target, other := create("target"), create("other")
			in := contract.Group{Name: "referrer", Type: "monitor"}
			switch reference {
			case "chain":
				in.Type, in.ChainGroupIDs = "chain_exit", []string{target.ID, other.ID}
			case "ipv6":
				in.Advanced = &contract.GroupAdvanced{IPv6Group: []string{target.ID}}
			case "reverse":
				in.Advanced = &contract.GroupAdvanced{ReverseGroup: []string{target.ID}}
			}
			referrer := read[contract.Group](t, f.req("POST", "/groups", in, ""), 201)
			if response := f.req("DELETE", groupDeleteURL(target), nil, ""); response.Code != 409 || !strings.Contains(response.Body.String(), "referrer") {
				t.Fatal("referenced group deleted", response.Code, response.Body.String())
			}
			if response := f.req("DELETE", groupDeleteURL(referrer), nil, ""); response.Code != 204 {
				t.Fatal(response.Code, response.Body.String())
			}
			if response := f.req("DELETE", groupDeleteURL(target), nil, ""); response.Code != 204 {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
}

func TestDeleteGroupCleansUnusedExitAfterRuleACK(t *testing.T) {
	f := setup(t)
	entry, entryNode := f.node()
	exitGroup, exitNode := f.node()
	exit := read[contract.Exit](t, f.req("POST", "/exits", contract.Exit{Name: "managed-exit", GroupID: exitGroup.ID, NodeID: exitNode.NodeID, Transport: "tls", Tunnel: contract.Tunnel{Endpoint: "exit.example:443", Token: "secret-exit-token-long", ServerName: "exit.example"}, Weight: 1, Enabled: true}, ""), 201)
	input := ruleFor(entry, entryNode)
	input.ExitGroupID, input.ExitID = exitGroup.ID, exit.ID
	rule := read[contract.Rule](t, f.req("POST", "/rules", input, ""), 201)
	if response := f.req("DELETE", groupDeleteURL(exitGroup), nil, ""); response.Code != 409 {
		t.Fatal("managed exit was removed while referenced", response.Code)
	}
	if response := f.req("DELETE", fmt.Sprintf("/rules/%s?version=%d", rule.ID, rule.Version), nil, ""); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	if response := f.req("DELETE", groupDeleteURL(exitGroup), nil, ""); response.Code != 409 {
		t.Fatal("managed exit removed before entry ACK", response.Code)
	}
	config := read[contract.Config](t, f.req("GET", "/agent/config", nil, entryNode.Token), 200)
	if response := f.req("POST", "/agent/ack", contract.Ack{Version: config.Version, AppliedVersion: config.Version}, entryNode.Token); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	if response := f.req("DELETE", groupDeleteURL(exitGroup), nil, ""); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	var exits int
	if err := f.s.Store.DB.QueryRow(f.s.q("SELECT COUNT(*) FROM cp_exits WHERE id=?"), exit.ID).Scan(&exits); err != nil || exits != 0 {
		t.Fatal("orphan exit retained", exits, err)
	}
	read[contract.Config](t, f.req("GET", "/agent/config", nil, exitNode.Token), 200)
}

func TestDeleteGroupConcurrentReference(t *testing.T) {
	for _, kind := range []string{"rule", "chain", "exit", "registration"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			group, node := f.node()
			other, _ := f.node()
			var path string
			var input any
			switch kind {
			case "rule":
				path, input = "/rules", ruleFor(group, node)
			case "chain":
				path, input = "/groups", contract.Group{Name: "chain", Type: "chain_exit", ChainGroupIDs: []string{group.ID, other.ID}}
			case "exit":
				path, input = "/exits", contract.Exit{Name: "exit", GroupID: group.ID, NodeID: node.NodeID, Transport: "tls", Tunnel: contract.Tunnel{Endpoint: "exit.example:443", Token: "secret-exit-token-long", ServerName: "exit.example"}, Weight: 1, Enabled: true}
			case "registration":
				key := read[map[string]string](t, f.req("GET", "/groups/"+group.ID+"/join-key", nil, ""), 200)["join_key"]
				path, input = "/agent/register", contract.Registration{Token: key, Name: "concurrent"}
			}
			start := make(chan struct{})
			created, deleted := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
			go func() { <-start; created <- f.req("POST", path, input, "") }()
			go func() { <-start; deleted <- f.req("DELETE", groupDeleteURL(group), nil, "") }()
			close(start)
			creation, deletion := <-created, <-deleted
			if deletion.Code != 204 && deletion.Code != 409 {
				t.Fatal("unexpected deletion result", deletion.Code, deletion.Body.String())
			}
			if kind == "rule" || kind == "chain" {
				if deletion.Code == 204 && creation.Code == 201 {
					t.Fatal("concurrent reference points to deleted group")
				}
			}
			if deletion.Code == 204 {
				for _, table := range []string{"cp_rules", "cp_node_groups", "cp_exits"} {
					var count int
					if err := f.s.Store.DB.QueryRow(f.s.q("SELECT COUNT(*) FROM "+table+" WHERE group_id=?"), group.ID).Scan(&count); err != nil || count != 0 {
						t.Fatal("dangling reference", table, count, err)
					}
				}
			}
		})
	}
}
