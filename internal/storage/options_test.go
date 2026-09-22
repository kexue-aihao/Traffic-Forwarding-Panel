package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestConfiguredPoolAndQueueCommitBoundary(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		ctx := context.Background()
		open, idle := 2, 0
		s, err := OpenWithOptions(ctx, "sqlite", filepath.Join(t.TempDir(), "pool.db"), Options{MaxOpenConnections: &open, MaxIdleConnections: &idle, DisableQueue: disabled})
		if err != nil {
			t.Fatal(err)
		}
		if s.DB.Stats().MaxOpenConnections != open {
			t.Fatal("pool size not applied")
		}
		if _, err := s.DB.Exec("CREATE TABLE queue_check (value INTEGER)"); err != nil {
			t.Fatal(err)
		}
		rollback := errors.New("rollback")
		for _, result := range []error{rollback, nil} {
			err := s.Write(ctx, Normal, func(tx *sql.Tx) error {
				if _, err := tx.Exec("INSERT INTO queue_check(value) VALUES(1)"); err != nil {
					return err
				}
				return result
			})
			if !errors.Is(err, result) {
				t.Fatalf("queue=%t: %v", disabled, err)
			}
		}
		var count int
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM queue_check").Scan(&count); err != nil || count != 1 {
			t.Fatal("commit/rollback changed", count, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Write(ctx, Normal, func(*sql.Tx) error { return nil }); err == nil {
			t.Fatal("closed store accepted write")
		}
	}
}
