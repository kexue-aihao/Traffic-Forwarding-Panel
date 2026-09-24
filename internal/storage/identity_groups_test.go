package storage_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func TestIdentityGroupMigrationPreservesGrantsAndSurvivesRestore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	// The pre-identity schema has users and individual device-group grants.
	for _, statement := range []string{
		`CREATE TABLE cp_schema(version INTEGER PRIMARY KEY)`,
		`INSERT INTO cp_schema(version) VALUES(1),(2),(3),(4),(5),(6),(7)`,
		`CREATE TABLE cp_users(id VARCHAR(64) PRIMARY KEY,username VARCHAR(190) NOT NULL UNIQUE,password_hash VARCHAR(190) NOT NULL,role VARCHAR(32) NOT NULL,disabled INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE cp_groups(id VARCHAR(64) PRIMARY KEY,name VARCHAR(190) NOT NULL,payload TEXT NOT NULL,version BIGINT NOT NULL,join_key VARCHAR(64) NOT NULL DEFAULT '')`,
		`CREATE TABLE cp_group_users(group_id VARCHAR(64) NOT NULL,user_id VARCHAR(64) NOT NULL,PRIMARY KEY(group_id,user_id),FOREIGN KEY(group_id) REFERENCES cp_groups(id),FOREIGN KEY(user_id) REFERENCES cp_users(id))`,
		`INSERT INTO cp_users(id,username,password_hash,role) VALUES('alice','alice','test-hash','user'),('bob','bob','test-hash','user')`,
		`INSERT INTO cp_groups(id,name,payload,version,join_key) VALUES('private','private','{}',1,'private-test-key'),('shared','shared','{}',1,'shared-test-key')`,
		`INSERT INTO cp_group_users(group_id,user_id) VALUES('private','alice'),('shared','alice'),('shared','bob')`,
	} {
		if _, err = legacy.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, "sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var alice, bob string
	if err = store.DB.QueryRow(`SELECT identity_group_id FROM cp_users WHERE id='alice'`).Scan(&alice); err != nil {
		t.Fatal(err)
	}
	if err = store.DB.QueryRow(`SELECT identity_group_id FROM cp_users WHERE id='bob'`).Scan(&bob); err != nil {
		t.Fatal(err)
	}
	if alice == "" || bob == "" || alice == bob {
		t.Fatal("migration merged distinct authorization identities")
	}
	var count int
	if err = store.DB.QueryRow(`SELECT COUNT(*) FROM cp_group_identity_groups`).Scan(&count); err != nil || count != 3 {
		t.Fatal("migration lost or added grants", count, err)
	}
	if err = store.DB.QueryRow(`SELECT COUNT(*) FROM cp_group_identity_groups gig JOIN cp_users u ON u.identity_group_id=gig.identity_group_id WHERE gig.group_id='private' AND u.id='bob'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("migration broadened private access", count, err)
	}
	if _, err = store.DB.Exec(`DELETE FROM cp_group_identity_groups WHERE group_id='private'`); err != nil {
		t.Fatal(err)
	}
	var backup bytes.Buffer
	if err = store.Export(ctx, &backup); err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	restored, err := storage.Open(ctx, "sqlite", restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = restored.Import(ctx, &backup); err != nil {
		t.Fatal(err)
	}
	if err = restored.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(ctx, "sqlite", restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.DB.QueryRow(`SELECT COUNT(*) FROM cp_group_identity_groups`).Scan(&count); err != nil || count != 2 {
		t.Fatal("restore/restart replayed revoked legacy grants", count, err)
	}
	var restoredIdentity string
	if err = reopened.DB.QueryRow(`SELECT identity_group_id FROM cp_users WHERE id='alice'`).Scan(&restoredIdentity); err != nil || restoredIdentity != alice {
		t.Fatal("restore changed identity group ID", restoredIdentity, err)
	}
}

func TestRestoreLegacyBackupMigratesIdentityGrants(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, "sqlite", filepath.Join(t.TempDir(), "restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var backup bytes.Buffer
	encoder := json.NewEncoder(&backup)
	for _, record := range []map[string]any{
		{"format": 1, "dialect": "sqlite"},
		{"table": "cp_users", "columns": []string{"id", "username", "password_hash", "role", "disabled"}},
		{"table": "cp_users", "values": []any{"alice", "alice", "test-hash", "user", 0}},
		{"table": "cp_groups", "columns": []string{"id", "name", "payload", "version", "join_key"}},
		{"table": "cp_groups", "values": []any{"device", "device", "{}", 1, "test-key"}},
		{"table": "cp_group_users", "columns": []string{"group_id", "user_id"}},
		{"table": "cp_group_users", "values": []any{"device", "alice"}},
		{"end": true, "rows": 3},
	} {
		if err = encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.Import(ctx, &backup); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = store.DB.QueryRow(`SELECT COUNT(*) FROM cp_users u JOIN cp_group_identity_groups gig ON gig.identity_group_id=u.identity_group_id WHERE u.id='alice' AND gig.group_id='device'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("restoring a legacy backup lost authorization", count, err)
	}
}
