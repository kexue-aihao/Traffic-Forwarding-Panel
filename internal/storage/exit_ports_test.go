package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestExitPortMigrationPreservesLegacySharedOwners(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "migration.db")
	s, e := Open(ctx, "sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	if e := s.Write(ctx, Critical, func(tx *sql.Tx) error {
		if _, e := tx.Exec("DROP TABLE cp_exit_ports"); e != nil {
			return e
		}
		if _, e := tx.Exec("DELETE FROM cp_schema WHERE version=12"); e != nil {
			return e
		}
		for _, exit := range []contract.Exit{
			{ID: "a", NodeID: "node", Transport: "tls", Tunnel: contract.Tunnel{Endpoint: "exit.example:9443"}},
			{ID: "b", NodeID: "node", Transport: "wss", Tunnel: contract.Tunnel{Endpoint: "wss://exit.example:9443/tunnel"}},
			{ID: "c", NodeID: "node", Transport: "tls", Tunnel: contract.Tunnel{Endpoint: "exit.example:9445"}, UDP: &contract.UDPExit{Endpoint: "exit.example:9443"}},
			{ID: "reverse", NodeID: "node", Transport: "tls", Tunnel: contract.Tunnel{Endpoint: "relay.example:9446", Reverse: "reverse-id"}},
		} {
			payload, _ := json.Marshal(exit)
			if _, e := tx.Exec("INSERT INTO cp_exits(id,group_id,node_id,payload,version) VALUES(?,'group','node',?,1)", exit.ID, string(payload)); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		s.Close()
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(ctx, "sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var n int
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM cp_exit_ports").Scan(&n); e != nil || n != 4 {
		t.Fatal("migration lost physical reservations", n, e)
	}
	if _, e := s.DB.Exec("DELETE FROM cp_exit_ports WHERE exit_id='a'"); e != nil {
		t.Fatal(e)
	}
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM cp_exit_ports WHERE node_id='node' AND network='tcp' AND port=9443").Scan(&n); e != nil || n != 1 {
		t.Fatal("deleting one legacy owner freed another's port", n, e)
	}
}
