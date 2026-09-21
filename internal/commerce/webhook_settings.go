package commerce

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const webhookEvents = "*,wallet.recharge,wallet.purchase,wallet.purchase_refund,purchase.refunded,commission.refunded,wallet.addon,wallet.redeem,wallet.commission,wallet.commission_reversal,wallet.refund_reserve,wallet.refund_release,entitlement.purchased,entitlement.addon,entitlement.redeemed,refund.completed,refund.canceled,node.offline,node.recovered,entitlement.expiring,entitlement.expired,entitlement.low_quota,alerts.policy_updated"

type WebhookSettings struct {
	Events     []string   `json:"events"`
	Enabled    bool       `json:"enabled"`
	MutedUntil *time.Time `json:"muted_until"`
	Version    int64      `json:"version"`
}

func validateWebhookEvents(events []string) error {
	if len(events) == 0 || len(events) > 32 {
		return errors.New("invalid webhook events")
	}
	seen := map[string]bool{}
	for _, e := range events {
		if seen[e] || !eventAllowed(webhookEvents, e) {
			return errors.New("invalid webhook event")
		}
		seen[e] = true
	}
	if seen["*"] && len(events) != 1 {
		return errors.New("wildcard cannot be combined with named events")
	}
	return nil
}

func (s *Service) migrateWebhookSettings(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS commerce_webhook_settings(subscription_id VARCHAR(64) PRIMARY KEY,muted_until BIGINT NOT NULL,version BIGINT NOT NULL,admin_scope INTEGER NOT NULL DEFAULT 0)`)
	return err
}

func (s *Service) UpdateWebhook(ctx context.Context, user, sub string, in WebhookSettings) error {
	if err := validateWebhookEvents(in.Events); err != nil {
		return err
	}
	muted := int64(0)
	if in.MutedUntil != nil && in.MutedUntil.After(s.Now()) {
		if in.MutedUntil.After(s.Now().Add(30 * 24 * time.Hour)) {
			return errors.New("mute exceeds 30 days")
		}
		muted = in.MutedUntil.UnixMilli()
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		q := "SELECT id FROM commerce_webhook_subscriptions WHERE id=? AND user_id=?"
		if s.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		var found string
		if err := tx.QueryRowContext(ctx, s.q(q), sub, user).Scan(&found); err != nil {
			return err
		}
		version := int64(1)
		err := tx.QueryRowContext(ctx, s.q("SELECT version FROM commerce_webhook_settings WHERE subscription_id=?"), sub).Scan(&version)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if version != in.Version {
			return ErrConflict
		}
		q = "INSERT INTO commerce_webhook_settings(subscription_id,muted_until,version) VALUES(?,?,?) ON CONFLICT(subscription_id) DO UPDATE SET muted_until=excluded.muted_until,version=excluded.version"
		if s.Dialect == "mysql" {
			q = "INSERT INTO commerce_webhook_settings(subscription_id,muted_until,version) VALUES(?,?,?) ON DUPLICATE KEY UPDATE muted_until=VALUES(muted_until),version=VALUES(version)"
		}
		if _, err = tx.ExecContext(ctx, s.q(q), sub, muted, version+1); err != nil {
			return err
		}
		enabled := 0
		if in.Enabled {
			enabled = 1
		}
		if _, err = tx.ExecContext(ctx, s.q("UPDATE commerce_webhook_subscriptions SET enabled=?,events=? WHERE id=?"), enabled, strings.Join(in.Events, ","), sub); err != nil {
			return err
		}
		// Suppression is terminal: unmuting does not send a backlog of old alerts.
		q = "UPDATE commerce_event_deliveries SET attempts=12,last_error='suppressed' WHERE subscription_id=? AND delivered_at IS NULL AND attempts<12"
		args := []any{sub}
		if in.Enabled && muted == 0 {
			if len(in.Events) == 1 && in.Events[0] == "*" {
				return nil
			}
			q += " AND event_id IN(SELECT id FROM commerce_events WHERE kind NOT IN (" + strings.TrimRight(strings.Repeat("?,", len(in.Events)), ",") + "))"
			for _, e := range in.Events {
				args = append(args, e)
			}
		}
		_, err = tx.ExecContext(ctx, s.q(q), args...)
		return err
	})
}

// EmitEventTx lets the lifecycle monitor publish with the same commit as its
// transition/checkpoint; retries cannot create an alert without its dedupe state.
func (s *Service) EmitEventTx(ctx context.Context, tx *sql.Tx, user, kind string, data map[string]any) error {
	return s.emitEventTx(ctx, tx, user, kind, data)
}
