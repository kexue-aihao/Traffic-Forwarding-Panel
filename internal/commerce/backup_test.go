package commerce

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
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
	p, e := s.CreatePlan(ctx, Plan{Name: "backup", Price: 100, Quota: 1000, DurationUnit: "week", DurationValue: 2})
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
	plans, e := restored.Plans(ctx)
	if e != nil || len(plans) != 1 || plans[0].DurationUnit != "week" || plans[0].DurationValue != 2 {
		t.Fatal("backup lost duration", plans, e)
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

func TestLegacyBackupDurationBackfill(t *testing.T) {
	ctx := context.Background()
	st := testdb.Open(t)
	s := New(st.DB, st.Dialect, func(c context.Context, f func(*sql.Tx) error) error { return st.Write(c, storage.Critical, f) })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// The target already has schema v8. An old archive has neither new column.
	archive := fmt.Sprintf("{\"format\":1,\"dialect\":%q}\n", st.Dialect) +
		"{\"table\":\"commerce_plans\",\"columns\":[\"id\",\"name\",\"price\",\"quota\",\"months\"]}\n" +
		"{\"table\":\"commerce_plans\",\"values\":[\"old-plan\",\"old monthly\",100,1000,6]}\n" +
		"{\"end\":true,\"rows\":1}\n"
	if err := st.Import(ctx, bytes.NewBufferString(archive)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		plans, err := s.Plans(ctx)
		if err != nil || len(plans) != 1 || plans[0].DurationUnit != "month" || plans[0].DurationValue != 6 || plans[0].Months != 6 {
			t.Fatal("legacy backup lost months", plans, err)
		}
	}
}

func TestLegacyBackupPlanStateBackfill(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	p, err := s.CreatePlan(ctx, Plan{Name: "legacy", Price: 100, Quota: 1000, Months: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("DELETE FROM commerce_plan_states"); err != nil {
		t.Fatal(err)
	}
	restarted := New(s.DB, s.Dialect, s.Write)
	if err = restarted.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "a", 100, "test", "fund-old") }); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.Purchase(ctx, "a", p.ID, "buy-old", 0); err != nil {
		t.Fatal(err)
	}
	if err = restarted.SetPlanActive(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	plans, err := restarted.Plans(ctx)
	if err != nil || plans[0].Active {
		t.Fatal(plans, err)
	}
}
