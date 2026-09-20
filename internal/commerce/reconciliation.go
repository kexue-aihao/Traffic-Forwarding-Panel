package commerce

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

const (
	reconciliationInterval = 10 * time.Second
	reconciliationTimeout  = 15 * time.Second
	reconciliationDelay    = 30 * time.Second
	reconciliationMaxDelay = time.Hour
	reconciliationBatch    = 20
	reconciliationMaxBatch = 100
)

func (s *Service) migrateReconciliation(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS commerce_reconciliation(order_id VARCHAR(64) PRIMARY KEY,attempts BIGINT NOT NULL,next_at BIGINT NOT NULL)`); err != nil {
		return err
	}
	if err := storage.EnsureIndex(ctx, conn, s.Dialect, "commerce_reconciliation", "commerce_reconciliation_due", "next_at,order_id", false); err != nil {
		return err
	}
	// Restartable after MySQL DDL auto-commit, without resetting an existing
	// schedule. Include legacy orders and attempts left creating by a crash.
	_, err := conn.ExecContext(ctx, `INSERT INTO commerce_reconciliation(order_id,attempts,next_at) SELECT o.id,0,0 FROM commerce_orders o LEFT JOIN commerce_reconciliation r ON r.order_id=o.id WHERE o.status='pending' AND r.order_id IS NULL`)
	return err
}

func (s *Service) scheduleReconciliation(ctx context.Context, tx *sql.Tx, orderID string) error {
	_, err := tx.ExecContext(ctx, s.q("INSERT INTO commerce_reconciliation(order_id,attempts,next_at) VALUES(?,0,?)"), orderID, s.Now().Add(reconciliationDelay).UnixMilli())
	return err
}

func reconciliationBackoff(attempts int64) time.Duration {
	delay := reconciliationDelay
	for attempts > 1 && delay < reconciliationMaxDelay {
		delay *= 2
		attempts--
	}
	if delay > reconciliationMaxDelay {
		return reconciliationMaxDelay
	}
	return delay
}

// ReconcilePending queries a bounded batch of due orders. The return count is
// the number of durable claims, including queries that fail or remain pending.
// Such results retain their pending financial state and persisted retry time.
// channels must remain immutable while this method or RunReconciliation runs.
func (s *Service) ReconcilePending(ctx context.Context, channels map[string]Channel, limit int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Overlapping ticks in a control plane skip instead of building a queue.
	if !s.reconcileMu.TryLock() {
		return 0, nil
	}
	defer s.reconcileMu.Unlock()
	if limit <= 0 {
		limit = reconciliationBatch
	}
	if limit > reconciliationMaxBatch {
		limit = reconciliationMaxBatch
	}
	var names []string
	for name, channel := range channels {
		if channel.Adapter != nil && channel.Adapter.Capabilities().Query {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return 0, nil
	}
	sort.Strings(names)
	// Old backup imports can populate orders after schema migration. Repair
	// a bounded number each tick instead of relying only on v2's backfill.
	if err := s.repairReconciliation(ctx, names, limit); err != nil {
		return 0, err
	}
	args := []any{s.Now().UnixMilli()}
	for _, name := range names {
		args = append(args, name)
	}
	args = append(args, limit)
	query := "SELECT r.order_id,o.user_id,r.attempts FROM commerce_reconciliation r JOIN commerce_orders o ON o.id=r.order_id WHERE r.next_at<=? AND o.status='pending' AND o.channel IN (" + strings.TrimSuffix(strings.Repeat("?,", len(names)), ",") + ") ORDER BY r.next_at,r.order_id LIMIT ?"
	rows, err := s.DB.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return 0, err
	}
	type dueOrder struct {
		id, user string
		attempts int64
	}
	var due []dueOrder
	for rows.Next() {
		var order dueOrder
		if err = rows.Scan(&order.id, &order.user, &order.attempts); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, order)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	var failures []error
	for _, order := range due {
		if err = ctx.Err(); err != nil {
			return count, errors.Join(append(failures, err)...)
		}
		claimed := false
		err = s.Write(ctx, func(tx *sql.Tx) error {
			// Match ConfirmPayment's lock order (order, then schedule) so a
			// callback and a retry claim cannot deadlock each other.
			query := "SELECT status FROM commerce_orders WHERE id=?"
			if s.Dialect != "sqlite" {
				query += " FOR UPDATE"
			}
			var status string
			if claimErr := tx.QueryRowContext(ctx, s.q(query), order.id).Scan(&status); claimErr != nil {
				return claimErr
			}
			if status != "pending" {
				return nil
			}
			now := s.Now()
			attempts := order.attempts + 1
			// Saturate the counter rather than overflowing after long uptime.
			if order.attempts >= 2147483647 {
				attempts = 2147483647
			}
			result, claimErr := tx.ExecContext(ctx, s.q("UPDATE commerce_reconciliation SET attempts=?,next_at=? WHERE order_id=? AND attempts=? AND next_at<=?"), attempts, now.Add(reconciliationBackoff(attempts)).UnixMilli(), order.id, order.attempts, now.UnixMilli())
			if claimErr != nil {
				return claimErr
			}
			n, claimErr := result.RowsAffected()
			claimed = n == 1
			return claimErr
		})
		if err != nil {
			return count, errors.Join(append(failures, err)...)
		}
		if !claimed {
			continue
		}
		count++
		// No SQL transaction or result set remains open during gateway I/O.
		if _, err = s.ReconcileOrder(ctx, order.user, order.id, channels); err != nil {
			failures = append(failures, fmt.Errorf("reconcile order %s: %w", order.id, err))
		}
	}
	return count, errors.Join(failures...)
}

func (s *Service) repairReconciliation(ctx context.Context, channels []string, limit int) error {
	args := make([]any, 0, len(channels)+1)
	for _, channel := range channels {
		args = append(args, channel)
	}
	args = append(args, limit)
	query := "SELECT o.id FROM commerce_orders o LEFT JOIN commerce_reconciliation r ON r.order_id=o.id WHERE o.status='pending' AND r.order_id IS NULL AND o.channel IN (" + strings.TrimSuffix(strings.Repeat("?,", len(channels)), ",") + ") ORDER BY o.created_at,o.id LIMIT ?"
	rows, err := s.DB.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return err
	}
	var missing []string
	for rows.Next() {
		var order string
		if err = rows.Scan(&order); err != nil {
			rows.Close()
			return err
		}
		missing = append(missing, order)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(missing) == 0 {
		return err
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		for _, order := range missing {
			query := "SELECT status FROM commerce_orders WHERE id=?"
			if s.Dialect != "sqlite" {
				query += " FOR UPDATE"
			}
			var status string
			if err := tx.QueryRowContext(ctx, s.q(query), order).Scan(&status); err != nil {
				return err
			}
			if status != "pending" {
				continue
			}
			insert := "INSERT INTO commerce_reconciliation(order_id,attempts,next_at) VALUES(?,0,0) ON CONFLICT(order_id) DO NOTHING"
			if s.Dialect == "mysql" {
				insert = "INSERT INTO commerce_reconciliation(order_id,attempts,next_at) VALUES(?,0,0) ON DUPLICATE KEY UPDATE order_id=order_id"
			}
			if _, err := tx.ExecContext(ctx, s.q(insert), order); err != nil {
				return err
			}
		}
		return nil
	})
}

// RunReconciliation runs immediately on startup, then periodically until ctx
// is canceled. Transient query/database errors leave durable retries intact.
func (s *Service) RunReconciliation(ctx context.Context, channels map[string]Channel) error {
	ticker := time.NewTicker(reconciliationInterval)
	defer ticker.Stop()
	for {
		count, err := s.ReconcilePending(ctx, channels, reconciliationBatch)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// Gateway errors may contain credential-bearing URLs. Do not log
			// their raw strings; subsequent ticks use the persisted schedule.
			slog.WarnContext(ctx, "payment reconciliation batch will retry", "attempted", count)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
