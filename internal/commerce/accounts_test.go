package commerce

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestAdminBalanceAndRuleLimitAdjustments(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	for _, q := range []string{
		"INSERT INTO cp_users(id,username,password_hash,role,identity_group_id,disabled) VALUES('admin','admin','x','admin','',0),('alice','alice','x','user','',0),('bob','bob','x','user','',0)",
		"INSERT INTO cp_groups(id,name,payload,version) VALUES('group','group','{}',1)",
		"INSERT INTO cp_nodes(id,name,token_hash,payload,desired_version,applied_version,apply_error,last_seen) VALUES('node','node','node-token','{}',1,1,'',0)",
		"INSERT INTO cp_rules(id,user_id,node_id,group_id,payload,version,deleted) VALUES('r1','alice','node','group','{}',1,0),('r2','alice','node','group','{}',1,0),('deleted','alice','node','group','{}',1,1)",
	} {
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	plan, err := s.CreatePlan(ctx, Plan{Name: "three rules", Price: 100, Months: 1, Limits: contract.PlanLimits{MaxRules: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "alice", 10000, "test", "seed") }); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Purchase(ctx, "alice", plan.ID, "purchase", 0); err != nil {
		t.Fatal(err)
	}
	if account, err := s.UserAccount(ctx, "alice"); err != nil || account.Balance != 9900 || account.RuleCount != 2 || account.PlanMaxRules != 3 || account.MaxRules != 3 || account.RuleLimitOverride != nil {
		t.Fatal(account, err)
	}

	if err = s.AdjustUserBalance(ctx, "admin", "alice", "credit-once", "support credit", 250); err != nil {
		t.Fatal(err)
	}
	if err = s.AdjustUserBalance(ctx, "admin", "alice", "credit-once", "support credit", 250); err != nil {
		t.Fatal("same idempotency key replay failed", err)
	}
	if err = s.AdjustUserBalance(ctx, "admin", "alice", "credit-once", "different reason", 250); !errors.Is(err, ErrConflict) {
		t.Fatal("idempotency key reuse accepted", err)
	}
	if err = s.AdjustUserBalance(ctx, "admin", "alice", "too-much", "invalid debit", -10151); !errors.Is(err, ErrFunds) {
		t.Fatal("debit beyond balance accepted", err)
	}
	if wallet, err := s.Wallet(ctx, "alice"); err != nil || wallet.Balance != 10150 {
		t.Fatal(wallet, err)
	}

	maximum := 4
	if err = s.SetUserRuleLimit(ctx, "admin", "alice", &maximum, "temporary allowance"); err != nil {
		t.Fatal(err)
	}
	var limits contract.ResourceLimits
	if err = s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		limits, err = s.LimitsTx(ctx, tx, "alice")
		return err
	}); err != nil || limits.MaxRules != 4 || limits.MaxConnectionsPerNode != 0 {
		t.Fatal(limits, err)
	}
	if account, err := s.UserAccount(ctx, "alice"); err != nil || account.MaxRules != 4 || account.PlanMaxRules != 3 || account.RuleLimitOverride == nil || *account.RuleLimitOverride != 4 {
		t.Fatal(account, err)
	}
	var version int64
	if err = s.DB.QueryRowContext(ctx, "SELECT desired_version FROM cp_nodes WHERE id='node'").Scan(&version); err != nil || version != 2 {
		t.Fatal(version, err)
	}

	unlimited := 0
	if err = s.SetUserRuleLimit(ctx, "admin", "alice", &unlimited, "remove cap"); err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		limits, err = s.LimitsTx(ctx, tx, "alice")
		return err
	}); err != nil || limits.MaxRules != 0 {
		t.Fatal(limits, err)
	}
	if err = s.SetUserRuleLimit(ctx, "admin", "alice", nil, "follow plan again"); err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		limits, err = s.LimitsTx(ctx, tx, "alice")
		return err
	}); err != nil || limits.MaxRules != 3 {
		t.Fatal("clearing account override did not restore plan limit", limits, err)
	}

	maximum = 2
	if err = s.SetUserRuleLimit(ctx, "admin", "bob", &maximum, "standalone account cap"); err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		limits, err = s.LimitsTx(ctx, tx, "bob")
		return err
	}); err != nil || limits.MaxRules != 2 {
		t.Fatal("account override without a package was ignored", limits, err)
	}
}
