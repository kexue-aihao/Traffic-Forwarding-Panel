package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

type Task struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Result    string    `json:"result,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type exportPayload struct {
	RuleIDs []string `json:"rule_ids"`
	Admin   bool     `json:"admin"`
}
type importPayload struct {
	Mode  string          `json:"mode,omitempty"`
	Rules []contract.Rule `json:"rules"`
	Admin bool            `json:"admin"`
}
type taskItem struct {
	Index int            `json:"index"`
	Rule  *contract.Rule `json:"rule,omitempty"`
	Error string         `json:"error,omitempty"`
}

func (s *Server) createExportTask(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	var in struct {
		RuleIDs []string `json:"rule_ids"`
		Key     string   `json:"idempotency_key"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.RuleIDs) > 500 {
		fail(w, 400, "select at most 500 rules or export all")
		return
	}
	payload, _ := json.Marshal(exportPayload{in.RuleIDs, actor.Role == "admin"})
	s.createTask(w, r, actor, "rules.export", in.Key, string(payload))
}
func (s *Server) createImportTask(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	var in ImportRequest
	if !decode(w, r, &in) {
		return
	}
	if len(in.Rules) < 1 || len(in.Rules) > 1000 || !validImportMode(in.Mode) {
		fail(w, 400, "import requires 1-1000 rules")
		return
	}
	payload, err := json.Marshal(importPayload{Rules: in.Rules, Admin: actor.Role == "admin", Mode: in.Mode})
	if err != nil || len(payload) > 1<<20 {
		fail(w, 400, "import too large")
		return
	}
	s.createTask(w, r, actor, "rules.import", in.Key, string(payload))
}
func (s *Server) createTask(w http.ResponseWriter, r *http.Request, actor contract.User, kind, key, payload string) {
	if len(key) < 1 || len(key) > 128 {
		fail(w, 400, "idempotency_key required (1-128 characters)")
		return
	}
	now := time.Now().UTC()
	task := Task{ID: id(), Kind: kind, Status: "pending", CreatedAt: now, UpdatedAt: now}
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		q := "SELECT disabled FROM cp_users WHERE id=?"
		if s.Store.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		var disabled int
		if err := tx.QueryRowContext(r.Context(), s.q(q), actor.ID).Scan(&disabled); err != nil {
			return err
		}
		if disabled != 0 {
			return errConflict
		}
		var oldKind, hash string
		err := tx.QueryRowContext(r.Context(), s.q("SELECT id,kind,status,payload_hash FROM cp_tasks WHERE user_id=? AND idempotency_key=?"), actor.ID, key).Scan(&task.ID, &oldKind, &task.Status, &hash)
		if err == nil {
			if oldKind != kind || hash != digest(payload) {
				return errConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var pending int
		if err := tx.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_tasks WHERE user_id=? AND status IN ('pending','running')"), actor.ID).Scan(&pending); err != nil {
			return err
		}
		if pending >= 10 {
			return errors.New("too many active tasks")
		}
		_, err = tx.ExecContext(r.Context(), s.q("INSERT INTO cp_tasks(id,user_id,kind,status,payload,payload_hash,result,error,created_at,updated_at,cancel_requested,claim_token,idempotency_key,requires_admin) VALUES(?,?,?,'pending',?,?,'','',?,?,0,'',?,?)"), task.ID, actor.ID, kind, payload, digest(payload), now.Unix(), now.Unix(), key, map[bool]int{true: 1, false: 0}[actor.Role == "admin"])
		return err
	})
	if err != nil {
		fail(w, 409, "task creation conflict or limit reached")
		return
	}
	reply(w, 202, task)
}
func (s *Server) task(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	var t Task
	var owner string
	var created, updated int64
	var admin int
	err := s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT user_id,kind,status,result,error,created_at,updated_at,requires_admin FROM cp_tasks WHERE id=?"), r.PathValue("id")).Scan(&owner, &t.Kind, &t.Status, &t.Result, &t.Error, &created, &updated, &admin)
	if err != nil || actor.Role != "admin" && (owner != actor.ID || admin != 0) {
		fail(w, 404, "task not found")
		return
	}
	t.ID = r.PathValue("id")
	t.CreatedAt = time.Unix(created, 0).UTC()
	t.UpdatedAt = time.Unix(updated, 0).UTC()
	if t.Kind == "rules.import" && (t.Status == "running" || t.Result == "") {
		result, _, err := s.importResult(r.Context(), t.ID)
		if err != nil {
			fail(w, 500, "task result unavailable")
			return
		}
		t.Result = result
	}
	reply(w, 200, t)
}
func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	rows, err := s.Store.DB.QueryContext(r.Context(), s.q("SELECT id,kind,status,created_at,updated_at FROM cp_tasks WHERE user_id=? AND (requires_admin=0 OR ?='admin') ORDER BY created_at DESC,id DESC LIMIT 100"), actor.ID, actor.Role)
	if err != nil {
		fail(w, 500, "task query failed")
		return
	}
	defer rows.Close()
	items := []Task{}
	for rows.Next() {
		var t Task
		var c, u int64
		if err = rows.Scan(&t.ID, &t.Kind, &t.Status, &c, &u); err != nil {
			fail(w, 500, "task query failed")
			return
		}
		t.CreatedAt = time.Unix(c, 0).UTC()
		t.UpdatedAt = time.Unix(u, 0).UTC()
		items = append(items, t)
	}
	if rows.Err() != nil {
		fail(w, 500, "task query failed")
		return
	}
	reply(w, 200, map[string]any{"items": items})
}
func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), s.q("UPDATE cp_tasks SET cancel_requested=1 WHERE id=? AND (user_id=? OR ?='admin') AND status IN ('pending','running')"), r.PathValue("id"), actor.ID, actor.Role)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
	if err != nil {
		fail(w, 404, "task not found or finished")
		return
	}
	reply(w, 202, map[string]any{"status": "cancel_requested"})
}

// Every item holds this fenced claim through its resource writes and checkpoint.
func (s *Server) taskGuard(ctx context.Context, tx *sql.Tx, taskID, claim, owner string, requiresAdmin bool) error {
	q := "SELECT role,disabled FROM cp_users WHERE id=?"
	if s.Store.Dialect != "sqlite" {
		q += " FOR UPDATE"
	}
	var role string
	var disabled int
	if err := tx.QueryRowContext(ctx, s.q(q), owner).Scan(&role, &disabled); err != nil {
		return err
	}
	if disabled != 0 || requiresAdmin && role != "admin" {
		return errors.New("task permission revoked")
	}
	q = "SELECT claim_token,cancel_requested FROM cp_tasks WHERE id=? AND status='running'"
	if s.Store.Dialect != "sqlite" {
		q += " FOR UPDATE"
	}
	var current string
	var cancel int
	if err := tx.QueryRowContext(ctx, s.q(q), taskID).Scan(&current, &cancel); err != nil {
		return err
	}
	if current != claim {
		return errConflict
	}
	if cancel != 0 {
		return errTaskCanceled
	}
	return nil
}

var errTaskCanceled = errors.New("task canceled")

func (s *Server) runTask(ctx context.Context, taskID, claim string) error {
	var owner, kind, payload string
	var admin int
	if err := s.Store.DB.QueryRowContext(ctx, s.q("SELECT user_id,kind,payload,requires_admin FROM cp_tasks WHERE id=? AND claim_token=?"), taskID, claim).Scan(&owner, &kind, &payload, &admin); err != nil {
		return err
	}
	if kind == "rules.import" {
		return s.runImportTask(ctx, taskID, claim, owner, payload, admin != 0)
	}
	if kind != "rules.export" {
		return s.finishTask(ctx, taskID, claim, "failed", "", "unsupported task")
	}
	var in exportPayload
	if json.Unmarshal([]byte(payload), &in) != nil {
		return errors.New("invalid payload")
	}
	items := []contract.Rule{}
	err := s.Store.Write(ctx, storage.Background, func(tx *sql.Tx) error {
		if err := s.taskGuard(ctx, tx, taskID, claim, owner, admin != 0); err != nil {
			return err
		}
		where := " WHERE deleted=0"
		args := []any{}
		if !in.Admin {
			where += " AND user_id=?"
			args = append(args, owner)
		}
		if len(in.RuleIDs) > 0 {
			where += " AND id IN ("
			for i, ruleID := range in.RuleIDs {
				if i > 0 {
					where += ","
				}
				where += "?"
				args = append(args, ruleID)
			}
			where += ")"
		}
		rows, err := tx.QueryContext(ctx, s.q("SELECT payload FROM cp_rules"+where+" ORDER BY id LIMIT 10001"), args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var raw string
			var rule contract.Rule
			if err = rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			if err = json.Unmarshal([]byte(raw), &rule); err != nil {
				rows.Close()
				return err
			}
			redact(&rule)
			items = append(items, rule)
			if len(items) > 10000 {
				rows.Close()
				return errors.New("export exceeds 10000 rules")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	result, err := json.Marshal(map[string]any{"version": 1, "items": items})
	if err != nil {
		return err
	}
	return s.finishTask(ctx, taskID, claim, "completed", string(result), "")
}
func (s *Server) runImportTask(ctx context.Context, taskID, claim, owner, payload string, admin bool) error {
	var in importPayload
	if json.Unmarshal([]byte(payload), &in) != nil {
		return errors.New("invalid payload")
	}
	for i, input := range in.Rules {
		err := s.Store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
			if err := s.taskGuard(ctx, tx, taskID, claim, owner, admin); err != nil {
				return err
			}
			var prior string
			err := tx.QueryRowContext(ctx, s.q("SELECT result FROM cp_task_items WHERE task_id=? AND item_index=?"), taskID, i).Scan(&prior)
			if err == nil {
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err = tx.ExecContext(ctx, "SAVEPOINT import_item"); err != nil {
				return err
			}
			output, importErr := s.importInputTx(ctx, tx, owner, in.Admin, input, in.Mode, false)
			item := taskItem{Index: i}
			if importErr != nil {
				if _, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT import_item"); err != nil {
					return err
				}
				item.Error = "rule rejected; check permissions, policy, entitlement and port availability"
			} else {
				redact(&output)
				item.Rule = &output
			}
			if _, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT import_item"); err != nil {
				return err
			}
			raw, err := json.Marshal(item)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, s.q("INSERT INTO cp_task_items(task_id,item_index,result) VALUES(?,?,?)"), taskID, i, string(raw)); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, s.q("UPDATE cp_tasks SET updated_at=? WHERE id=? AND claim_token=?"), time.Now().Unix(), taskID, claim)
			return err
		})
		if err != nil {
			return err
		}
	}
	result, failed, err := s.importResult(ctx, taskID)
	if err != nil {
		return err
	}
	status := "completed"
	if failed {
		status = "completed_with_errors"
	}
	return s.finishTask(ctx, taskID, claim, status, result, "")
}
func (s *Server) importResult(ctx context.Context, taskID string) (string, bool, error) {
	rows, err := s.Store.DB.QueryContext(ctx, s.q("SELECT result FROM cp_task_items WHERE task_id=? ORDER BY item_index"), taskID)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	items := []taskItem{}
	failed := false
	for rows.Next() {
		var raw string
		var item taskItem
		if err = rows.Scan(&raw); err != nil {
			return "", false, err
		}
		if err = json.Unmarshal([]byte(raw), &item); err != nil {
			return "", false, err
		}
		failed = failed || item.Error != ""
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return "", false, err
	}
	raw, err := json.Marshal(map[string]any{"items": items})
	return string(raw), failed, err
}
func (s *Server) finishTask(ctx context.Context, taskID, claim, status, result, message string) error {
	return s.Store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
		var cancel int
		var token, kind string
		q := "SELECT cancel_requested,claim_token,kind FROM cp_tasks WHERE id=? AND status='running'"
		if s.Store.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		if err := tx.QueryRowContext(ctx, s.q(q), taskID).Scan(&cancel, &token, &kind); err != nil {
			return err
		}
		if token != claim {
			return errConflict
		}
		if cancel != 0 {
			if kind == "rules.export" {
				result = ""
			}
			status = "canceled"
		}
		_, err := tx.ExecContext(ctx, s.q("UPDATE cp_tasks SET status=?,result=?,error=?,payload='',updated_at=? WHERE id=? AND claim_token=?"), status, result, message, time.Now().Unix(), taskID, claim)
		return err
	})
}
func (s *Server) RunTasks(ctx context.Context, limit int) (int, error) {
	if !s.taskMu.TryLock() {
		return 0, nil
	}
	defer s.taskMu.Unlock()
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	if err := s.Store.Write(ctx, storage.Background, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.q("UPDATE cp_tasks SET status='pending',claim_token='' WHERE status='running' AND updated_at<?"), time.Now().Add(-5*time.Minute).Unix())
		return err
	}); err != nil {
		return 0, err
	}
	rows, err := s.Store.DB.QueryContext(ctx, s.q("SELECT id FROM cp_tasks WHERE status='pending' ORDER BY created_at,id LIMIT ?"), limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var taskID string
		if err = rows.Scan(&taskID); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, taskID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, taskID := range ids {
		claim := id()
		claimed := false
		err = s.Store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, s.q("UPDATE cp_tasks SET status='running',claim_token=?,updated_at=? WHERE id=? AND status='pending'"), claim, time.Now().Unix(), taskID)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			claimed = n == 1
			return err
		})
		if err != nil {
			return count, err
		}
		if !claimed {
			continue
		}
		count++
		taskCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		runErr := s.runTask(taskCtx, taskID, claim)
		cancel()
		if ctx.Err() != nil {
			return count, ctx.Err()
		}
		if runErr != nil {
			status := "failed"
			message := "task failed; check resource access and configuration"
			if errors.Is(runErr, errTaskCanceled) {
				status = "canceled"
				message = ""
			}
			result, _, e := s.importResult(ctx, taskID)
			if e != nil {
				return count, e
			}
			if e = s.finishTask(ctx, taskID, claim, status, result, message); e != nil {
				return count, e
			}
		}
	}
	return count, nil
}
func (s *Server) RunTaskLoop(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.PruneTaskResults(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "task retention will retry")
		}
		if _, err := s.RunTasks(ctx, 10); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "task worker will retry")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (s *Server) importRuleTx(ctx context.Context, tx *sql.Tx, owner string, admin bool, rule contract.Rule) (contract.Rule, error) {
	actor := contract.User{ID: owner, Role: "user"}
	if admin {
		actor.Role = "admin"
	}
	if !admin || rule.UserID == "" {
		rule.UserID = owner
	}
	rule.ID, rule.Version, rule.Lease = id(), 0, nil
	if rule.SharedTLS != nil {
		return rule, errors.New("shared TLS rules must be created individually after their parent")
	}
	err := s.saveRuleTx(ctx, tx, actor, &rule, true)
	return rule, err
}

// Keep task idempotency tombstones; remove bulky results after seven days.
func (s *Server) PruneTaskResults(ctx context.Context) error {
	rows, err := s.Store.DB.QueryContext(ctx, s.q("SELECT id FROM cp_tasks WHERE status NOT IN ('pending','running') AND result<>'' AND updated_at<? ORDER BY updated_at,id LIMIT 100"), time.Now().Add(-7*24*time.Hour).Unix())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(ids) == 0 {
		return err
	}
	return s.Store.Write(ctx, storage.Background, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, s.q("DELETE FROM cp_task_items WHERE task_id=?"), id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, s.q("UPDATE cp_tasks SET result='' WHERE id=?"), id); err != nil {
				return err
			}
		}
		return nil
	})
}
