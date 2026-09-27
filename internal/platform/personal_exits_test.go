package platform

import (
	"context"
	"database/sql"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"testing"
)

func TestPersonalExitIsolatedWithinSharedIdentity(t *testing.T) {
	f := setup(t)
	identity := read[contract.IdentityGroup](t, f.req("POST", "/identity-groups", map[string]any{"id": "shared-exit-test", "name": "Shared exit test"}, ""), 201)
	alice := read[contract.UserCreated](t, f.req("POST", "/users", map[string]any{"username": "exit-alice", "role": "user", "identity_group_id": identity.ID}, ""), 201)
	bob := read[contract.UserCreated](t, f.req("POST", "/users", map[string]any{"username": "exit-bob", "role": "user", "identity_group_id": identity.ID}, ""), 201)
	login := f.req("POST", "/auth/login", map[string]any{"username": alice.Username, "password": alice.InitialPassword}, "")
	f.cookie = login.Result().Cookies()[0]
	group := read[contract.Group](t, f.req("POST", "/my-exit-groups", map[string]any{"name": "private exit", "type": "exit", "multiplier": "999"}, ""), 201)
	if group.OwnerID != alice.ID || group.Multiplier != "1" {
		t.Fatal("owner or billing multiplier", group)
	}
	key := read[map[string]string](t, f.req("GET", "/groups/"+group.ID+"/join-key", nil, ""), 200)["join_key"]
	node := read[contract.Registered](t, f.req("POST", "/agent/register", contract.Registration{Token: key, Name: "private exit"}, ""), 201)
	var count int
	if err := f.s.Store.DB.QueryRow(f.s.q("SELECT COUNT(*) FROM cp_node_groups WHERE node_id=? AND group_id=?"), node.NodeID, group.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal(err, count)
	}
	login = f.req("POST", "/auth/login", map[string]any{"username": bob.Username, "password": bob.InitialPassword}, "")
	f.cookie = login.Result().Cookies()[0]
	for _, path := range []string{"/groups", "/nodes", "/exits"} {
		page := read[struct{ Total int }](t, f.req("GET", path, nil, ""), 200)
		if page.Total != 0 {
			t.Fatal("another identity member sees private resources", path, page.Total)
		}
	}
	if w := f.req("GET", "/groups/"+group.ID+"/join-key", nil, ""); w.Code != 403 {
		t.Fatal("join key disclosed", w.Code)
	}
	allowed, err := f.s.groupAuthorized(context.Background(), f.s.Store.DB, group.ID, bob.ID)
	if err != nil || allowed {
		t.Fatal("group authorized to non-owner", allowed, err)
	}
	tx, err := f.s.Store.DB.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = f.s.resolveExitTx(context.Background(), tx, &contract.Rule{UserID: bob.ID, ExitGroupID: group.ID}); err == nil {
		t.Fatal("foreign personal exit resolved")
	}
}
