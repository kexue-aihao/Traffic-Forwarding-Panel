package commerce

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/testdb"
)

func TestBackupRestoreRejectsPartialAndExistingData(t *testing.T) {
	ctx := context.Background()
	source := testdb.Open(t)
	makeService := func(st *storage.Store) *Service {
		s := New(st.DB, st.Dialect, func(c context.Context, f func(*sql.Tx) error) error { return st.Write(c, storage.Critical, f) })
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := makeService(source)
	if e := s.Write(ctx, func(tx *sql.Tx) error {
		return s.post(ctx, tx, "backup-user", 9007199254740993, "test", "exact-integer")
	}); e != nil {
		t.Fatal(e)
	}
	p, e := s.CreatePlan(ctx, Plan{Name: "backup", Price: 100, Quota: 1000, Months: 1})
	if e != nil {
		t.Fatal(e)
	}
	ent, e := s.Purchase(ctx, "backup-user", p.ID, "backup-buy", 0)
	if e != nil {
		t.Fatal(e)
	}
	var archive bytes.Buffer
	if e = source.Export(ctx, &archive); e != nil {
		t.Fatal(e)
	}
	target := testdb.Open(t)
	restored := makeService(target)
	raw := archive.Bytes()
	cut := bytes.LastIndex(raw, []byte(`{"end":true`))
	if cut < 0 {
		t.Fatal("end marker missing")
	}
	if e = target.Import(ctx, bytes.NewReader(raw[:cut])); e == nil {
		t.Fatal("incomplete backup committed")
	}
	var count int
	if e = target.DB.QueryRow("SELECT COUNT(*) FROM commerce_wallets").Scan(&count); e != nil || count != 0 {
		t.Fatal("failed import not rolled back", count, e)
	}
	if e = target.Import(ctx, bytes.NewReader(raw)); e != nil {
		t.Fatal(e)
	}
	w, e := restored.Wallet(ctx, "backup-user")
	if e != nil || w.Balance != 9007199254740893 {
		t.Fatal("integer precision lost", w, e)
	}
	got, e := restored.Purchase(ctx, "backup-user", p.ID, "backup-buy", 0)
	if e != nil || got.ID != ent.ID {
		t.Fatal("restored idempotency lost", got, e)
	}
	if e = target.Import(ctx, bytes.NewReader(raw)); e == nil {
		t.Fatal("existing data overwritten")
	}
}
