package storage_test

import (
	"context"
	"database/sql"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/testdb"
	"sync"
	"testing"
)

func TestWriteRollbackAndCancelledQueue(t *testing.T) {
	s := testdb.Open(t)
	ctx := context.Background()
	sentinel := errors.New("rollback")
	e := s.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, s.Rebind(`INSERT INTO cp_users(id,username,password_hash,role,disabled) VALUES(?,?,?,?,0)`), "rollback", "rollback", "hash", "user")
		if e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
	var count int
	if e = s.DB.QueryRow(`SELECT COUNT(*) FROM cp_users WHERE id='rollback'`).Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	ran := false
	if e = s.Write(cancelled, storage.Normal, func(tx *sql.Tx) error { ran = true; return nil }); e == nil || ran {
		t.Fatal("cancelled transaction executed", e, ran)
	}
}
func TestConcurrentDurableWrites(t *testing.T) {
	s := testdb.Open(t)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- s.Write(context.Background(), storage.Priority(i%3), func(tx *sql.Tx) error {
				_, e := tx.Exec(`INSERT INTO cp_schema(version) VALUES(` + fmtInt(100+i) + `)`)
				return e
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var count int
	if e := s.DB.QueryRow(`SELECT COUNT(*) FROM cp_schema WHERE version>=100`).Scan(&count); e != nil || count != 40 {
		t.Fatal("successful writes not durable", count, e)
	}
}
func fmtInt(v int) string {
	if v == 0 {
		return "0"
	}
	b := []byte{}
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
