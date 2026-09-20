package storage_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/testdb"
)

func TestBulkCounterBigIntAndLateConflictRollback(t *testing.T) {
	s := testdb.Open(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(`CREATE TABLE bulk_counters(id VARCHAR(64) PRIMARY KEY,used BIGINT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	rows := make([][]any, 125)
	changes := make([]storage.CounterChange, len(rows))
	for i := range rows {
		id := fmt.Sprintf("counter-%03d", i)
		rows[i] = []any{id, int64(0)}
		changes[i] = storage.CounterChange{ID: id, Delta: 1 << 32}
	}
	if err := s.Write(ctx, storage.Normal, func(tx *sql.Tx) error {
		return storage.BulkInsert(ctx, tx, s.Rebind, "bulk_counters", "id,used", rows)
	}); err != nil {
		t.Fatal(err)
	}
	apply := func(cas bool) error {
		return s.Write(ctx, storage.Normal, func(tx *sql.Tx) error {
			return storage.BulkCounter(ctx, tx, s.Rebind, "bulk_counters", "used", "", changes, cas)
		})
	}
	assertTotal := func(want int64) {
		t.Helper()
		var total int64
		if err := s.DB.QueryRow(`SELECT SUM(used) FROM bulk_counters`).Scan(&total); err != nil || total != want {
			t.Fatalf("total=%d want=%d: %v", total, want, err)
		}
	}
	changes[124].Before = 1
	if err := apply(true); err == nil {
		t.Fatal("late stale counter accepted")
	}
	assertTotal(0)
	changes[124].Before = 0
	if err := apply(true); err != nil {
		t.Fatal(err)
	}
	assertTotal(125 << 32)
	if err := apply(false); err != nil {
		t.Fatal(err)
	}
	assertTotal(125 << 33)
}
