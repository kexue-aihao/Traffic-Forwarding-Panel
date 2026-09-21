package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func TestImportCheckpointRecoveryAndIdempotency(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	g, n := f.node()
	first := ruleFor(g, n)
	second := first
	second.Listen = ":20002"
	input := map[string]any{"rules": []contract.Rule{first, second}, "idempotency_key": "import-restart"}
	task := read[Task](t, f.req("POST", "/tasks/rules/import", input, ""), 202)
	replay := read[Task](t, f.req("POST", "/tasks/rules/import", input, ""), 202)
	if task.ID != replay.ID {
		t.Fatal("duplicate task")
	}
	// Simulate a process dying after the first item and its checkpoint committed.
	err := f.s.Store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
		var owner string
		if err := tx.QueryRowContext(ctx, f.s.q("SELECT user_id FROM cp_tasks WHERE id=?"), task.ID).Scan(&owner); err != nil {
			return err
		}
		output, err := f.s.importRuleTx(ctx, tx, owner, true, first)
		if err != nil {
			return err
		}
		redact(&output)
		raw, _ := json.Marshal(taskItem{Index: 0, Rule: &output})
		if _, err = tx.ExecContext(ctx, f.s.q("INSERT INTO cp_task_items(task_id,item_index,result) VALUES(?,0,?)"), task.ID, string(raw)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, f.s.q("UPDATE cp_tasks SET status='running',claim_token='dead-process',updated_at=? WHERE id=?"), time.Now().Add(-6*time.Minute).Unix(), task.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(f.s.Store, f.s.opts)
	if count, err := restarted.RunTasks(ctx, 10); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	got := read[Task](t, f.req("GET", "/tasks/"+task.ID, nil, ""), 200)
	if got.Status != "completed" {
		t.Fatal(got)
	}
	var count int
	if err = f.s.Store.DB.QueryRow("SELECT COUNT(*) FROM cp_rules").Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	var items struct {
		Items []taskItem `json:"items"`
	}
	if err = json.Unmarshal([]byte(got.Result), &items); err != nil || len(items.Items) != 2 || items.Items[0].Error != "" {
		t.Fatal(items, err)
	}
	var payload string
	if err = f.s.Store.DB.QueryRow(f.s.q("SELECT payload FROM cp_tasks WHERE id=?"), task.ID).Scan(&payload); err != nil || payload != "" {
		t.Fatal("completed import retained credentials", err)
	}
	replay = read[Task](t, f.req("POST", "/tasks/rules/import", input, ""), 202)
	if replay.ID != task.ID {
		t.Fatal(replay)
	}
}

func TestTaskCancelRecoveryAndRevokedAdmin(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	task := read[Task](t, f.req("POST", "/tasks/rules/export", map[string]any{"idempotency_key": "cancel"}, ""), 202)
	if _, err := f.s.Store.DB.Exec(f.s.q("UPDATE cp_tasks SET status='running',claim_token='old',cancel_requested=1,updated_at=? WHERE id=?"), time.Now().Add(-6*time.Minute).Unix(), task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RunTasks(ctx, 10); err != nil {
		t.Fatal(err)
	}
	got := read[Task](t, f.req("GET", "/tasks/"+task.ID, nil, ""), 200)
	if got.Status != "canceled" {
		t.Fatal(got)
	}
	task = read[Task](t, f.req("POST", "/tasks/rules/export", map[string]any{"idempotency_key": "revoke"}, ""), 202)
	if _, err := f.s.Store.DB.Exec("UPDATE cp_users SET role='user'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RunTasks(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := f.s.Store.DB.QueryRow(f.s.q("SELECT status FROM cp_tasks WHERE id=?"), task.ID).Scan(&status); err != nil || status != "failed" {
		t.Fatal(status, err)
	}
	if rr := f.req("GET", "/tasks/"+task.ID, nil, ""); rr.Code != 404 {
		t.Fatal("revoked admin accessed privileged task", rr.Code)
	}
}

func TestGroupRuleLimitConcurrentCreateAndImport(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	g.MaxRules = 1
	g = read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	var wg sync.WaitGroup
	codes := make(chan int, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rule := ruleFor(g, n)
			rule.Listen = fmt.Sprintf(":%d", 20010+i)
			codes <- f.req("POST", "/rules", rule, "").Code
		}(i)
	}
	wg.Wait()
	close(codes)
	success := 0
	for code := range codes {
		if code == 201 {
			success++
		}
	}
	if success != 1 {
		t.Fatal("limit not serialized", success)
	}
	rule := ruleFor(g, n)
	rule.Listen = ":20050"
	task := read[Task](t, f.req("POST", "/tasks/rules/import", map[string]any{"rules": []contract.Rule{rule}, "idempotency_key": "limit"}, ""), 202)
	if _, err := f.s.RunTasks(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	got := read[Task](t, f.req("GET", "/tasks/"+task.ID, nil, ""), 200)
	if got.Status != "completed_with_errors" {
		t.Fatal(got)
	}
}
