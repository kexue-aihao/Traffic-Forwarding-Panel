package platform

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func TestUsageBatchMixedReplayAtomicIsolation(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	r := cfg.Rules[0]
	u := contract.UsageRecord{ID: "batch-first", NodeID: n.NodeID, RuleID: r.ID, LeaseID: r.Lease.ID, EntitlementID: r.Lease.EntitlementID, StartedAt: time.Now().Add(-time.Second), EndedAt: time.Now(), UploadBytes: 10}
	read[map[string]any](t, f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u, u}}, n.Token), 200)
	bad := u
	bad.ID = "foreign"
	bad.RuleID = "other-user-rule"
	fresh := u
	fresh.ID = "fresh"
	if rr := f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u, fresh, bad}}, n.Token); rr.Code != 409 {
		t.Fatal("foreign rule accepted", rr.Code)
	}
	var count, used int64
	f.s.Store.DB.QueryRow(`SELECT COUNT(*) FROM cp_usage`).Scan(&count)
	f.s.Store.DB.QueryRow(f.s.q(`SELECT bytes_used FROM cp_rule_leases WHERE id=?`), r.Lease.ID).Scan(&used)
	if count != 1 || used != 10 {
		t.Fatal("batch failure partially committed", count, used)
	}
	read[map[string]any](t, f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u, fresh, fresh}}, n.Token), 200)
	f.s.Store.DB.QueryRow(`SELECT COUNT(*) FROM cp_usage`).Scan(&count)
	f.s.Store.DB.QueryRow(f.s.q(`SELECT bytes_used FROM cp_rule_leases WHERE id=?`), r.Lease.ID).Scan(&used)
	if count != 2 || used != 20 {
		t.Fatal("dedup failed", count, used)
	}
}

func commercialBatchFixture(t *testing.T, leaseCount int) (*fixture, contract.Registered, []contract.UsageRecord) {
	t.Helper()
	f := setup(t)
	_, node := f.node()
	ctx := context.Background()
	billing := commerce.New(f.s.Store.DB, f.s.Store.Dialect, func(ctx context.Context, fn func(*sql.Tx) error) error {
		return f.s.Store.Write(ctx, storage.Critical, fn)
	})
	if err := billing.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f.s.opts.Entitlements = billing
	f.s.opts.RetireLease = billing.RetireLease
	now := time.Now().UTC()
	expiry := now.Add(time.Minute)
	records := make([]contract.UsageRecord, leaseCount*4)
	err := f.s.Store.Write(ctx, storage.Normal, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, f.s.q(`INSERT INTO commerce_entitlements(id,user_id,plan_id,version,starts_at,expires_at,quota,used,allocated) VALUES('batch-ent','batch-user','plan',1,?,?,?,0,?)`), now.Format(time.RFC3339Nano), expiry.Format(time.RFC3339Nano), 100*leaseCount, 100*leaseCount); err != nil {
			return err
		}
		for i := 0; i < leaseCount; i++ {
			id := fmt.Sprintf("batch-lease-%03d", i)
			rule := fmt.Sprintf("batch-rule-%03d", i)
			if _, err := tx.ExecContext(ctx, f.s.q(`INSERT INTO commerce_leases(id,user_id,node_id,rule_id,entitlement_id,expires_at,bytes,used,multiplier) VALUES(?,'batch-user',?,?,'batch-ent',?,300,0,'1/3')`), id, node.NodeID, rule, expiry.Format(time.RFC3339Nano)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, f.s.q(`INSERT INTO commerce_lease_reservations(lease_id,budget,closed) VALUES(?,100,0)`), id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, f.s.q(`INSERT INTO cp_rule_leases(id,rule_id,node_id,entitlement_id,bytes_allocated,bytes_used,expires_at) VALUES(?,?,?,'batch-ent',300,0,?)`), id, rule, node.NodeID, expiry.Unix()); err != nil {
				return err
			}
			for j := 0; j < 4; j++ {
				index := j*leaseCount + i
				records[index] = contract.UsageRecord{ID: fmt.Sprintf("bulk-%03d", index), NodeID: node.NodeID, RuleID: rule, LeaseID: id, EntitlementID: "batch-ent", StartedAt: now.Add(-time.Second), EndedAt: now, UploadBytes: 1}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, node, records
}

func assertBatchTotals(t *testing.T, f *fixture, facts, raw, charged int64) {
	t.Helper()
	for query, want := range map[string]int64{
		`SELECT COUNT(*) FROM cp_usage`:                           facts,
		`SELECT COUNT(*) FROM commerce_usage`:                     facts,
		`SELECT COALESCE(SUM(bytes_used),0) FROM cp_rule_leases`:  raw,
		`SELECT COALESCE(SUM(used),0) FROM commerce_leases`:       raw,
		`SELECT COALESCE(SUM(used),0) FROM commerce_entitlements`: charged,
		`SELECT COALESCE(SUM(charged),0) FROM commerce_usage`:     charged,
	} {
		var got int64
		if err := f.s.Store.DB.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, want %d: %v", query, got, want, err)
		}
	}
}

func TestUsageBatchSQLFailureRollsBackAllChunksAndLayers(t *testing.T) {
	for _, table := range []string{"commerce_usage", "cp_usage"} {
		t.Run(table, func(t *testing.T) {
			f, node, records := commercialBatchFixture(t, 125)
			var statements []string
			switch f.s.Store.Dialect {
			case "sqlite":
				statements = []string{"CREATE TRIGGER fail_usage BEFORE INSERT ON " + table + " WHEN NEW.id='bulk-250' BEGIN SELECT RAISE(ABORT,'injected usage failure'); END"}
			case "mysql":
				statements = []string{"CREATE TRIGGER fail_usage BEFORE INSERT ON " + table + " FOR EACH ROW BEGIN IF NEW.id='bulk-250' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected usage failure'; END IF; END"}
			case "postgres":
				statements = []string{
					"CREATE FUNCTION fail_usage_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='bulk-250' THEN RAISE EXCEPTION 'injected usage failure'; END IF; RETURN NEW; END; $$",
					"CREATE TRIGGER fail_usage BEFORE INSERT ON " + table + " FOR EACH ROW EXECUTE FUNCTION fail_usage_insert()",
				}
			}
			for _, query := range statements {
				if _, err := f.s.Store.DB.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			batch := contract.UsageBatch{Records: records}
			if rr := f.req("POST", "/agent/usage", batch, node.Token); rr.Code != 409 {
				t.Fatal("injected third-chunk failure accepted", rr.Code, rr.Body.String())
			}
			assertBatchTotals(t, f, 0, 0, 0)
			drop := "DROP TRIGGER fail_usage"
			if f.s.Store.Dialect == "postgres" {
				drop += " ON " + table
			}
			if _, err := f.s.Store.DB.Exec(drop); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				read[map[string]any](t, f.req("POST", "/agent/usage", batch, node.Token), 200)
				assertBatchTotals(t, f, 500, 500, 250)
			}
		})
	}
}

func TestUsageBatchConcurrentRetirement(t *testing.T) {
	for _, raw := range []int64{0, 1} {
		t.Run(fmt.Sprintf("bytes-%d", raw), func(t *testing.T) {
			f, node, records := commercialBatchFixture(t, 12)
			for i := 0; i < 12; i++ {
				u := records[i]
				u.UploadBytes = raw
				start := make(chan struct{})
				usage := make(chan *httptest.ResponseRecorder, 1)
				retire := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					<-start
					usage <- f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u}}, node.Token)
				}()
				go func() {
					<-start
					retire <- f.req("POST", "/agent/leases/retire", map[string]any{"lease_id": u.LeaseID, "used_bytes": "0"}, node.Token)
				}()
				close(start)
				ur, rr := <-usage, <-retire
				if ur.Code != 200 && ur.Code != 409 || rr.Code != 204 && rr.Code != 409 {
					t.Fatalf("unexpected concurrent responses usage=%d retire=%d", ur.Code, rr.Code)
				}
				if raw > 0 && ur.Code == 200 && rr.Code == 204 {
					t.Fatal("retirement refunded quota concurrently consumed by usage")
				}
				persisted := int64(0)
				if ur.Code == 200 {
					persisted = raw
				}
				if rr.Code != 204 {
					rr = f.req("POST", "/agent/leases/retire", map[string]any{"lease_id": u.LeaseID, "used_bytes": fmt.Sprint(persisted)}, node.Token)
					if rr.Code != 204 {
						t.Fatal("retry retirement", rr.Code, rr.Body.String())
					}
				}
				if ur.Code == 200 {
					read[map[string]any](t, f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u}}, node.Token), 200)
				}
				u.ID += "-after-retire"
				if result := f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u}}, node.Token); result.Code != 409 {
					t.Fatal("new fact admitted after retirement", result.Code)
				}
			}
			var used, allocated, leaseUsed, closed int64
			if err := f.s.Store.DB.QueryRow(`SELECT used,allocated FROM commerce_entitlements WHERE id='batch-ent'`).Scan(&used, &allocated); err != nil {
				t.Fatal(err)
			}
			if err := f.s.Store.DB.QueryRow(`SELECT SUM(used) FROM commerce_leases`).Scan(&leaseUsed); err != nil {
				t.Fatal(err)
			}
			if err := f.s.Store.DB.QueryRow(`SELECT SUM(closed) FROM commerce_lease_reservations`).Scan(&closed); err != nil {
				t.Fatal(err)
			}
			if used != allocated || used != leaseUsed || closed != 12 {
				t.Fatalf("refund diverged used=%d allocated=%d raw=%d closed=%d", used, allocated, leaseUsed, closed)
			}
		})
	}
}

func TestUsageBatchRejectsRetirementAfterTransactionSnapshot(t *testing.T) {
	for _, layer := range []string{"platform", "commerce"} {
		t.Run(layer, func(t *testing.T) {
			f, node, records := commercialBatchFixture(t, 1)
			if f.s.Store.Dialect == "sqlite" {
				t.Skip("SQLite serializes write callbacks; server databases allow overlapping transactions")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			tx, err := f.s.Store.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var before int64
			if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM cp_lease_retirements`).Scan(&before); err != nil || before != 0 {
				t.Fatal(before, err)
			}
			u := records[0]
			u.UploadBytes = 0
			if rr := f.req("POST", "/agent/leases/retire", map[string]any{"lease_id": u.LeaseID, "used_bytes": "0"}, node.Token); rr.Code != 204 {
				t.Fatal(rr.Code, rr.Body.String())
			}
			if layer == "platform" {
				err = f.s.persistUsageBatch(ctx, tx, node.NodeID, []contract.UsageRecord{u})
			} else {
				err = f.s.opts.Entitlements.(*commerce.Service).SettleUsageBatch(ctx, tx, []contract.UsageRecord{u})
			}
			if err == nil || err.Error() != "usage outside lease" {
				t.Fatalf("retired lease must be rejected after an older transaction snapshot: %v", err)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			assertBatchTotals(t, f, 0, 0, 0)
		})
	}
}
