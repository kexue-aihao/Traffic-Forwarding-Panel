package platform

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestIdentityGroupDrivesSharedDeviceAuthorization(t *testing.T) {
	f := setup(t)
	adminCookie := f.cookie
	identity := read[contract.IdentityGroup](t, f.req("POST", "/identity-groups", map[string]any{"id": "east", "name": "华东客户"}, ""), 201)
	alice := read[contract.UserCreated](t, f.req("POST", "/users", map[string]any{"username": "identity-alice", "role": "user", "identity_group_id": identity.ID}, ""), 201)
	bob := read[contract.UserCreated](t, f.req("POST", "/users", map[string]any{"username": "identity-bob", "role": "user", "identity_group_id": identity.ID}, ""), 201)
	charlie := read[contract.UserCreated](t, f.req("POST", "/users", map[string]any{"username": "identity-charlie", "role": "user"}, ""), 201)
	group := read[contract.Group](t, f.req("POST", "/groups", map[string]any{
		"name":               "华东入口",
		"identity_group_ids": []string{identity.ID},
		"port_min":           20000,
		"port_max":           21000,
		"multiplier":         "1",
	}, ""), 201)
	if len(group.IdentityGroupIDs) != 1 || group.IdentityGroupIDs[0] != identity.ID {
		t.Fatal("device group did not retain identity authorization", group.IdentityGroupIDs)
	}
	identities := read[struct {
		Items []contract.IdentityGroup `json:"items"`
		Total int                      `json:"total"`
	}](t, f.req("GET", "/identity-groups?q="+url.QueryEscape(identity.Name), nil, ""), 200)
	if identities.Total != 1 || len(identities.Items) != 1 || identities.Items[0].UserCount != 2 || identities.Items[0].DeviceGroupCount != 1 {
		t.Fatal("identity group counts or search incorrect", identities)
	}

	for _, user := range []contract.UserCreated{alice, bob} {
		login := f.req("POST", "/auth/login", map[string]any{"username": user.Username, "password": user.InitialPassword}, "")
		f.cookie = login.Result().Cookies()[0]
		if response := f.req("GET", "/identity-groups", nil, ""); response.Code != 403 {
			t.Fatal("ordinary user accessed identity administration", response.Code)
		}
		page := read[struct {
			Items []contract.Group `json:"items"`
		}](t, f.req("GET", "/groups", nil, ""), 200)
		if len(page.Items) != 1 || page.Items[0].ID != group.ID {
			t.Fatalf("shared identity did not grant %s access", user.Username)
		}
		if len(page.Items[0].IdentityGroupIDs) != 0 || len(page.Items[0].UserIDs) != 0 {
			t.Fatal("ordinary user received authorization membership")
		}
	}

	login := f.req("POST", "/auth/login", map[string]any{"username": charlie.Username, "password": charlie.InitialPassword}, "")
	f.cookie = login.Result().Cookies()[0]
	page := read[struct {
		Items []contract.Group `json:"items"`
	}](t, f.req("GET", "/groups", nil, ""), 200)
	if len(page.Items) != 0 {
		t.Fatal("unrelated identity gained device access")
	}

	f.cookie = adminCookie
	if response := f.req("PUT", "/users/"+charlie.ID+"/identity-group", map[string]any{"identity_group_id": "missing"}, ""); response.Code != 409 {
		t.Fatal("nonexistent identity group was accepted", response.Code)
	}
	if response := f.req("PUT", "/users/missing/identity-group", map[string]any{"identity_group_id": identity.ID}, ""); response.Code != 404 {
		t.Fatal("missing user should return 404", response.Code)
	}
	if response := f.req("PUT", "/users/"+charlie.ID+"/identity-group", map[string]any{"identity_group_id": identity.ID}, ""); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	f.cookie = login.Result().Cookies()[0]
	page = read[struct {
		Items []contract.Group `json:"items"`
	}](t, f.req("GET", "/groups", nil, ""), 200)
	if len(page.Items) != 1 || page.Items[0].ID != group.ID {
		t.Fatal("identity reassignment did not update access")
	}

	f.cookie = adminCookie
	if response := f.req("DELETE", "/identity-groups/"+identity.ID, nil, ""); response.Code != 409 {
		t.Fatal("referenced identity group was deleted", response.Code)
	}
	empty := read[contract.IdentityGroup](t, f.req("POST", "/identity-groups", map[string]any{"id": "empty", "name": "待删除"}, ""), 201)
	if response := f.req("DELETE", "/identity-groups/"+empty.ID, nil, ""); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	if response := f.req("DELETE", "/identity-groups/"+empty.ID, nil, ""); response.Code != 404 {
		t.Fatal("deleted identity group should return 404", response.Code)
	}
}

func TestIdentityGroupDeletionRequiresRemovingEveryReference(t *testing.T) {
	f := setup(t)
	identity := read[contract.IdentityGroup](t, f.req("POST", "/identity-groups", map[string]any{"id": "devices-only", "name": "devices-only"}, ""), 201)
	group := read[contract.Group](t, f.req("POST", "/groups", map[string]any{
		"name": "devices", "identity_group_ids": []string{identity.ID}, "port_min": 20000, "port_max": 21000, "multiplier": "1",
	}, ""), 201)
	if response := f.req("DELETE", "/identity-groups/"+identity.ID, nil, ""); response.Code != 409 {
		t.Fatal("device-only reference was ignored", response.Code)
	}
	group.IdentityGroupIDs = nil
	read[contract.Group](t, f.req("PUT", "/groups/"+group.ID, group, ""), 200)
	user := read[contract.UserCreated](t, f.req("POST", "/users", map[string]any{"username": "member", "role": "user", "identity_group_id": identity.ID}, ""), 201)
	if response := f.req("DELETE", "/identity-groups/"+identity.ID, nil, ""); response.Code != 409 {
		t.Fatal("user-only reference was ignored", response.Code)
	}
	replacement := read[contract.IdentityGroup](t, f.req("POST", "/identity-groups", map[string]any{"id": "replacement", "name": "replacement"}, ""), 201)
	if response := f.req("PUT", "/users/"+user.ID+"/identity-group", map[string]any{"identity_group_id": replacement.ID}, ""); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	if response := f.req("DELETE", "/identity-groups/"+identity.ID, nil, ""); response.Code != 204 {
		t.Fatal("unreferenced identity group could not be deleted", response.Code, response.Body.String())
	}
}

func TestIdentityGroupManualIDValidation(t *testing.T) {
	f := setup(t)
	for _, invalid := range []string{"", "  ", "a/b", "a b", "..", "a?b", strings.Repeat("a", 65)} {
		if response := f.req("POST", "/identity-groups", map[string]any{"id": invalid, "name": "invalid"}, ""); response.Code != 400 {
			t.Fatalf("invalid ID %q accepted: %d", invalid, response.Code)
		}
	}
	group := read[contract.IdentityGroup](t, f.req("POST", "/identity-groups", map[string]any{"id": " 001_VIP-A ", "name": " VIP "}, ""), 201)
	if group.ID != "001_VIP-A" || group.Name != "VIP" {
		t.Fatal("manual ID or trimmed name not preserved", group)
	}
	for _, body := range []map[string]any{
		{"id": group.ID, "name": "different"},
		{"id": "different", "name": group.Name},
	} {
		if response := f.req("POST", "/identity-groups", body, ""); response.Code != 409 {
			t.Fatal("duplicate ID/name accepted", response.Code)
		}
	}
	if response := f.req("PUT", "/identity-groups/"+group.ID, map[string]any{"id": "", "name": "name"}, ""); response.Code != 400 {
		t.Fatal("empty replacement ID accepted", response.Code)
	}
	if response := f.req("PUT", "/identity-groups/missing", map[string]any{"id": "valid", "name": "name"}, ""); response.Code != 404 {
		t.Fatal("missing identity edit should return 404", response.Code)
	}
	updated := read[contract.IdentityGroup](t, f.req("PUT", "/identity-groups/"+group.ID, map[string]any{"id": group.ID, "name": "renamed"}, ""), 200)
	if updated.ID != group.ID || updated.Name != "renamed" {
		t.Fatal("name-only edit failed", updated)
	}
}

func TestIdentityGroupIDEditPreservesReferencesAndAuthorization(t *testing.T) {
	f := setup(t)
	admin := f.cookie
	identity := read[contract.IdentityGroup](t, f.req("POST", "/identity-groups", map[string]any{"id": strings.Repeat("1", 64), "name": "VIP"}, ""), 201)
	other := read[contract.IdentityGroup](t, f.req("POST", "/identity-groups", map[string]any{"id": "2001", "name": "Other"}, ""), 201)
	alice := read[contract.UserCreated](t, f.req("POST", "/users", map[string]any{"username": "alice", "role": "user", "identity_group_id": identity.ID}, ""), 201)
	read[contract.UserCreated](t, f.req("POST", "/users", map[string]any{"username": "bob", "role": "user", "identity_group_id": identity.ID}, ""), 201)
	groups := []contract.Group{}
	for _, name := range []string{"first", "second"} {
		groups = append(groups, read[contract.Group](t, f.req("POST", "/groups", map[string]any{
			"name": name, "identity_group_ids": []string{identity.ID, other.ID}, "port_min": 20000, "port_max": 21000, "multiplier": "1.5",
		}, ""), 201))
	}
	key := read[map[string]any](t, f.req("POST", "/users/"+alice.ID+"/tokens", map[string]any{"name": "existing key", "permanent": true}, ""), 201)["token"].(string)
	login := f.req("POST", "/auth/login", map[string]any{"username": alice.Username, "password": alice.InitialPassword}, "")
	f.cookie = login.Result().Cookies()[0]
	for _, method := range []string{"PUT", "DELETE"} {
		if response := f.req(method, "/identity-groups/"+identity.ID, map[string]any{"id": "3001", "name": "VIP"}, ""); response.Code != 403 {
			t.Fatal("ordinary user modified identity group", response.Code)
		}
	}
	f.cookie = admin
	if response := f.req("PUT", "/identity-groups/"+identity.ID, map[string]any{"id": "3001", "name": "VIP"}, key); response.Code != 403 {
		t.Fatal("owner token edited identity group", response.Code)
	}
	// Both uniqueness failures happen after links are detached; rollback must
	// preserve every user's assignment and every device-group grant.
	for _, body := range []map[string]any{
		{"id": other.ID, "name": "unused"},
		{"id": "3001", "name": other.Name},
	} {
		if response := f.req("PUT", "/identity-groups/"+identity.ID, body, ""); response.Code != 409 {
			t.Fatal("conflicting ID/name accepted", response.Code)
		}
		var grants, users int
		if err := f.s.Store.DB.QueryRow(f.s.q(`SELECT COUNT(*) FROM cp_group_identity_groups WHERE identity_group_id=?`), identity.ID).Scan(&grants); err != nil || grants != 2 {
			t.Fatal("failed edit lost device grants", grants, err)
		}
		if err := f.s.Store.DB.QueryRow(f.s.q(`SELECT COUNT(*) FROM cp_users WHERE identity_group_id=?`), identity.ID).Scan(&users); err != nil || users != 2 {
			t.Fatal("failed edit changed user assignments", users, err)
		}
	}
	replacementID := strings.Repeat("3", 64)
	updated := read[contract.IdentityGroup](t, f.req("PUT", "/identity-groups/"+identity.ID, map[string]any{"id": replacementID, "name": "VIP new"}, ""), 200)
	if updated.ID != replacementID || updated.Name != "VIP new" || updated.UserCount != 2 || updated.DeviceGroupCount != 2 {
		t.Fatal("identity edit response incorrect", updated)
	}
	var auditTarget string
	if err := f.s.Store.DB.QueryRow(f.s.q(`SELECT target FROM cp_audit WHERE action=?`), "identity-group.update").Scan(&auditTarget); err != nil || auditTarget != updated.ID {
		t.Fatal("identity edit audit must retain the target ID", auditTarget, err)
	}
	for _, group := range groups {
		var raw string
		if err := f.s.Store.DB.QueryRow(f.s.q(`SELECT payload FROM cp_groups WHERE id=?`), group.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var saved contract.Group
		if err := json.Unmarshal([]byte(raw), &saved); err != nil {
			t.Fatal(err)
		}
		if saved.Version != group.Version+1 || len(saved.IdentityGroupIDs) != 2 || !contains(saved.IdentityGroupIDs, updated.ID) || !contains(saved.IdentityGroupIDs, other.ID) || saved.Multiplier != group.Multiplier {
			t.Fatal("device-group payload lost references/settings", saved)
		}
		group.IdentityGroupIDs = saved.IdentityGroupIDs
		if response := f.req("PUT", "/groups/"+group.ID, group, ""); response.Code != 409 {
			t.Fatal("stale device-group editor was accepted", response.Code)
		}
	}
	if response := f.req("DELETE", "/identity-groups/"+identity.ID, nil, ""); response.Code != 404 {
		t.Fatal("old identity ID remained after edit", response.Code)
	}
	if response := f.req("DELETE", "/identity-groups/"+updated.ID, nil, ""); response.Code != 409 {
		t.Fatal("edited identity lost reference protection", response.Code)
	}
	f.cookie = login.Result().Cookies()[0]
	for _, bearer := range []string{"", key} {
		session := read[struct {
			User contract.User `json:"user"`
		}](t, f.req("GET", "/auth/session", nil, bearer), 200)
		if session.User.IdentityGroupID != updated.ID {
			t.Fatal("existing credential retained the old identity ID", session.User)
		}
		page := read[struct {
			Items []contract.Group `json:"items"`
		}](t, f.req("GET", "/groups", nil, bearer), 200)
		if len(page.Items) != 2 {
			t.Fatal("existing credential lost authorized device groups", page)
		}
	}
}
