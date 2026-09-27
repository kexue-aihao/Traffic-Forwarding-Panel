package commerce

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestManualOrderAtomicRollbackAndReplay(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	_, err := s.CreateManualOrderAtomic(ctx, "alice", "telegram-trx", "rollback", 100, "address", func(*sql.Tx, Order) error { return errors.New("intent failed") })
	if err == nil {
		t.Fatal("intent failure accepted")
	}
	var n int
	if err = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM commerce_orders").Scan(&n); err != nil || n != 0 {
		t.Fatal("partial order committed", n, err)
	}
	first, err := s.CreateManualOrder(ctx, "alice", "telegram-trx", "same", 100, "address")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.CreateManualOrder(ctx, "alice", "telegram-trx", "same", 100, "address")
	if err != nil || first.ID != replay.ID {
		t.Fatal("replay changed order", err)
	}
	if _, err = s.CreateManualOrder(ctx, "alice", "telegram-trx", "same", 200, "address"); !errors.Is(err, ErrConflict) {
		t.Fatal("mismatched replay accepted", err)
	}
	s.CheckAccount = func(context.Context, *sql.Tx, string) error { return ErrAccountDisabled }
	if _, err = s.CreateManualOrder(ctx, "alice", "telegram-trx", "disabled", 100, "address"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatal("disabled user created order", err)
	}
}
