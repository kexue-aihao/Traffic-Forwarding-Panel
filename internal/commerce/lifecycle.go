package commerce

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

type RedeemCode struct {
	ID        string     `json:"id"`
	CodeHint  string     `json:"code_hint"`
	Amount    int64      `json:"amount_cents,string"`
	PlanID    string     `json:"plan_id,omitempty"`
	MaxUses   int64      `json:"max_uses"`
	Used      int64      `json:"used"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Enabled   bool       `json:"enabled"`
	CreatedAt time.Time  `json:"created_at"`
}

type AutoRenew struct {
	Enabled   bool   `json:"enabled"`
	PlanID    string `json:"plan_id,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

type ReferralCode struct {
	CodeHint  string    `json:"code_hint"`
	CreatedAt time.Time `json:"created_at"`
}

type Commission struct {
	Refunded  int64     `json:"refunded_cents,string"`
	ID        string    `json:"id"`
	InviterID string    `json:"inviter_id"`
	InviteeID string    `json:"invitee_id"`
	SourceID  string    `json:"source_id"`
	Amount    int64     `json:"amount_cents,string"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func digestCode(code string) string {
	v := strings.ToUpper(strings.TrimSpace(code))
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

func (s *Service) migrateCommerceV3(ctx context.Context, conn *sql.Conn) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS commerce_plan_states(plan_id VARCHAR(64) PRIMARY KEY,active INTEGER NOT NULL,version BIGINT NOT NULL DEFAULT 1,kind VARCHAR(16) NOT NULL DEFAULT 'period',updated_at VARCHAR(40) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_purchase_snapshots(entitlement_id VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,plan_version BIGINT NOT NULL,payload TEXT NOT NULL,created_at VARCHAR(40) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_addon_purchases(id VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,idempotency_key VARCHAR(128) NOT NULL,plan_id VARCHAR(64) NOT NULL,expected_version BIGINT NOT NULL,plan_version BIGINT NOT NULL,entitlement_id VARCHAR(64) NOT NULL,payload TEXT NOT NULL,result TEXT NOT NULL,created_at VARCHAR(40) NOT NULL,UNIQUE(user_id,idempotency_key))`,
		`CREATE TABLE IF NOT EXISTS commerce_refunds(id VARCHAR(64) PRIMARY KEY,order_id VARCHAR(64) NOT NULL,user_id VARCHAR(64) NOT NULL,amount BIGINT NOT NULL,status VARCHAR(32) NOT NULL,reason VARCHAR(500) NOT NULL,actor_id VARCHAR(64) NOT NULL,idempotency_key VARCHAR(128) NOT NULL,evidence VARCHAR(500) NOT NULL,resolved_by VARCHAR(64) NOT NULL,created_at VARCHAR(40) NOT NULL,updated_at VARCHAR(40) NOT NULL,UNIQUE(order_id,idempotency_key))`,
		`CREATE TABLE IF NOT EXISTS commerce_redeem_codes(code_hash VARCHAR(64) PRIMARY KEY,code_hint VARCHAR(32) NOT NULL,amount BIGINT NOT NULL,plan_id VARCHAR(64) NOT NULL,max_uses BIGINT NOT NULL,used BIGINT NOT NULL,expires_at VARCHAR(40),enabled INTEGER NOT NULL,created_at VARCHAR(40) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_redeem_claims(code_hash VARCHAR(64) NOT NULL,user_id VARCHAR(64) NOT NULL,result TEXT NOT NULL,created_at VARCHAR(40) NOT NULL,PRIMARY KEY(code_hash,user_id))`,
		`CREATE TABLE IF NOT EXISTS commerce_auto_renew(user_id VARCHAR(64) PRIMARY KEY,enabled INTEGER NOT NULL,plan_id VARCHAR(64) NOT NULL,next_at BIGINT NOT NULL,last_error VARCHAR(500) NOT NULL,updated_at VARCHAR(40) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_referral_policy(id INTEGER PRIMARY KEY,rate_bps INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_referral_codes(code_hash VARCHAR(64) PRIMARY KEY,owner_id VARCHAR(64) NOT NULL UNIQUE,code_hint VARCHAR(32) NOT NULL,created_at VARCHAR(40) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_referral_bindings(invitee_id VARCHAR(64) PRIMARY KEY,inviter_id VARCHAR(64) NOT NULL,code_hash VARCHAR(64) NOT NULL,created_at VARCHAR(40) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_commission_adjustments(id VARCHAR(64) PRIMARY KEY,commission_id VARCHAR(64) NOT NULL,action VARCHAR(32) NOT NULL,actor_id VARCHAR(64) NOT NULL,reason VARCHAR(500) NOT NULL,created_at VARCHAR(40) NOT NULL,UNIQUE(commission_id,action))`,
		`CREATE TABLE IF NOT EXISTS commerce_commissions(id VARCHAR(64) PRIMARY KEY,inviter_id VARCHAR(64) NOT NULL,invitee_id VARCHAR(64) NOT NULL,source_id VARCHAR(128) NOT NULL UNIQUE,amount BIGINT NOT NULL,status VARCHAR(32) NOT NULL,created_at VARCHAR(40) NOT NULL)`,
	}
	for _, q := range statements {
		if s.Dialect == "mysql" {
			q = strings.ReplaceAll(q, " TEXT", " LONGTEXT")
		}
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	// Existing plans predate plan state. Backfill them as available, while
	// keeping explicit state rows authoritative for future changes.
	now := stamp(s.Now())
	if _, err := conn.ExecContext(ctx, s.q("INSERT INTO commerce_plan_states(plan_id,active,updated_at) SELECT p.id,1,? FROM commerce_plans p LEFT JOIN commerce_plan_states st ON st.plan_id=p.id WHERE st.plan_id IS NULL"), now); err != nil {
		return err
	}
	for _, idx := range []struct {
		name, table, columns string
		unique               bool
	}{
		{"commerce_refund_order", "commerce_refunds", "order_id", false},
		{"commerce_redeem_owner", "commerce_redeem_codes", "enabled,expires_at", false},
		{"commerce_commission_inviter", "commerce_commissions", "inviter_id,created_at", false},
	} {
		if err := storage.EnsureIndex(ctx, conn, s.Dialect, idx.table, idx.name, idx.columns, idx.unique); err != nil {
			return err
		}
	}
	return s.migrateEvents(ctx, conn)
}

func (s *Service) SetPlanActive(ctx context.Context, planID string, active bool) error {
	if strings.TrimSpace(planID) == "" {
		return errors.New("plan id required")
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		v := 0
		if active {
			v = 1
		}
		res, err := tx.ExecContext(ctx, s.q("UPDATE commerce_plan_states SET active=?,version=version+1,updated_at=? WHERE plan_id=?"), v, stamp(s.Now()), planID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func (s *Service) CloseOrder(ctx context.Context, user, orderID string) (Order, error) {
	var out Order
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var owner, created string
		var status string
		var amount int64
		q := "SELECT user_id,channel,amount,status,payment_url,created_at FROM commerce_orders WHERE id=?"
		if s.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		if err := tx.QueryRowContext(ctx, s.q(q), orderID).Scan(&owner, &out.Channel, &amount, &status, &out.PaymentURL, &created); err != nil {
			return err
		}
		if user != "" && owner != user {
			return sql.ErrNoRows
		}
		out = Order{ID: orderID, Channel: out.Channel, Amount: amount, Currency: "CNY", Status: status, PaymentURL: out.PaymentURL, CreatedAt: parse(created)}
		if status == "pending" {
			if _, err := tx.ExecContext(ctx, s.q("UPDATE commerce_orders SET status='closed' WHERE id=? AND status='pending'"), orderID); err != nil {
				return err
			}
			out.Status = "closed"
		}
		return nil
	})
	return out, err
}

func (s *Service) SetAutoRenew(ctx context.Context, user, planID string, enabled bool) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if s.CheckAccount != nil {
			if err := s.CheckAccount(ctx, tx, user); err != nil {
				return err
			}
		}
		if _, _, err := s.walletTx(ctx, tx, user); err != nil {
			return err
		}
		v := 0
		if enabled {
			var active int
			if err := tx.QueryRowContext(ctx, s.q("SELECT active FROM commerce_plan_states WHERE plan_id=? AND kind='period'"), planID).Scan(&active); err != nil {
				return err
			}
			if active != 1 {
				return errors.New("plan unavailable")
			}
			v = 1
		} else {
			planID = ""
		}
		q := "INSERT INTO commerce_auto_renew(user_id,enabled,plan_id,next_at,last_error,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET enabled=excluded.enabled,plan_id=excluded.plan_id,next_at=excluded.next_at,last_error='',updated_at=excluded.updated_at"
		if s.Dialect == "mysql" {
			q = "INSERT INTO commerce_auto_renew(user_id,enabled,plan_id,next_at,last_error,updated_at) VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE enabled=VALUES(enabled),plan_id=VALUES(plan_id),next_at=VALUES(next_at),last_error='',updated_at=VALUES(updated_at)"
		}
		_, err := tx.ExecContext(ctx, s.q(q), user, v, planID, s.Now().UnixMilli(), "", stamp(s.Now()))
		return err
	})
}
func (s *Service) AutoRenewStatus(ctx context.Context, user string) (AutoRenew, error) {
	var v AutoRenew
	var enabled int
	err := s.DB.QueryRowContext(ctx, s.q("SELECT enabled,plan_id,last_error FROM commerce_auto_renew WHERE user_id=?"), user).Scan(&enabled, &v.PlanID, &v.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	v.Enabled = enabled != 0
	return v, err
}

// Renewal waits until expiry because every purchase immediately resets the period.
// The account/wallet lock serializes purchase, redemption and preference changes.
func (s *Service) RunAutoRenew(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, s.q("SELECT user_id FROM commerce_auto_renew WHERE enabled=1 AND next_at<=? ORDER BY next_at,user_id LIMIT ?"), s.Now().UnixMilli(), limit)
	if err != nil {
		return 0, err
	}
	var users []string
	for rows.Next() {
		var user string
		if err = rows.Scan(&user); err != nil {
			rows.Close()
			return 0, err
		}
		users = append(users, user)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, user := range users {
		attempted := false
		err = s.Write(ctx, func(tx *sql.Tx) error {
			now := s.Now()
			accountErr := error(nil)
			if s.CheckAccount != nil {
				accountErr = s.CheckAccount(ctx, tx, user)
				if accountErr != nil && !errors.Is(accountErr, ErrAccountDisabled) {
					return accountErr
				}
			}
			if _, _, err := s.walletTx(ctx, tx, user); err != nil {
				return err
			}
			var enabled int
			var plan string
			var next int64
			if err := tx.QueryRowContext(ctx, s.q("SELECT enabled,plan_id,next_at FROM commerce_auto_renew WHERE user_id=?"), user).Scan(&enabled, &plan, &next); err != nil {
				return err
			}
			if enabled == 0 || next > now.UnixMilli() {
				return nil
			}
			update := func(at time.Time, message string) error {
				_, err := tx.ExecContext(ctx, s.q("UPDATE commerce_auto_renew SET next_at=?,last_error=?,updated_at=? WHERE user_id=?"), at.UnixMilli(), message, stamp(now), user)
				return err
			}
			if accountErr != nil {
				return update(now.Add(time.Hour), "account_disabled")
			}
			ent, err := scanEnt(tx.QueryRowContext(ctx, s.q("SELECT "+entFields+" FROM commerce_entitlements WHERE user_id=? ORDER BY version DESC LIMIT 1"), user))
			if errors.Is(err, sql.ErrNoRows) {
				return update(now.Add(time.Hour), "")
			}
			if err != nil {
				return err
			}
			if ent.ExpiresAt.After(now) {
				return update(ent.ExpiresAt, "")
			}
			attempted = true
			if _, err = tx.ExecContext(ctx, "SAVEPOINT auto_renew_purchase"); err != nil {
				return err
			}
			renewed, purchaseErr := s.purchaseTx(ctx, tx, user, plan, "autorenew:"+ent.ID, ent.Version, 0)
			if purchaseErr != nil {
				if _, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT auto_renew_purchase"); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT auto_renew_purchase"); err != nil {
					return err
				}
				reason := "renewal_failed"
				if errors.Is(purchaseErr, ErrFunds) {
					reason = "insufficient_funds"
				}
				return update(now.Add(time.Hour), reason)
			}
			if _, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT auto_renew_purchase"); err != nil {
				return err
			}
			return update(renewed.ExpiresAt, "")
		})
		if err != nil {
			return count, err
		}
		if attempted {
			count++
		}
	}
	return count, nil
}
func (s *Service) RunAutoRenewLoop(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if _, err := s.RunAutoRenew(ctx, 20); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "automatic renewal batch will retry")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) CreateRedeemCode(ctx context.Context, amount int64, planID string, maxUses int64, expiresAt *time.Time) (RedeemCode, string, error) {
	if amount < 0 || amount > 100000000 || strings.TrimSpace(planID) == "" && amount == 0 || maxUses < 1 || maxUses > 1000000 {
		return RedeemCode{}, "", errors.New("invalid redeem code")
	}
	plain := "TFP-" + strings.ToUpper(id())
	hash := digestCode(plain)
	v := RedeemCode{ID: hash, CodeHint: plain[:8], Amount: amount, PlanID: planID, MaxUses: maxUses, Enabled: true, CreatedAt: s.Now().UTC()}
	if expiresAt != nil {
		if !expiresAt.After(s.Now()) {
			return v, "", errors.New("redeem expiry must be in the future")
		}
		v.ExpiresAt = expiresAt
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if planID != "" {
			var active int
			if err := tx.QueryRowContext(ctx, s.q("SELECT active FROM commerce_plan_states WHERE plan_id=? AND kind='period'"), planID).Scan(&active); err != nil {
				return err
			}
			if active != 1 {
				return errors.New("plan unavailable")
			}
		}
		_, e := tx.ExecContext(ctx, s.q("INSERT INTO commerce_redeem_codes(code_hash,code_hint,amount,plan_id,max_uses,used,expires_at,enabled,created_at) VALUES(?,?,?,?,?,0,?,?,?)"), hash, v.CodeHint, amount, planID, maxUses, nullableStamp(expiresAt), 1, stamp(v.CreatedAt))
		return e
	})
	return v, plain, err
}

func nullableStamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return stamp(*t)
}

func (s *Service) RedeemCode(ctx context.Context, user, plain string) (map[string]any, error) {
	hash := digestCode(plain)
	if strings.TrimSpace(plain) == "" || len(plain) > 128 {
		return nil, errors.New("redeem code required")
	}
	var result map[string]any
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if s.CheckAccount != nil {
			if err := s.CheckAccount(ctx, tx, user); err != nil {
				return err
			}
		}
		if _, _, err := s.walletTx(ctx, tx, user); err != nil {
			return err
		}
		var replay string
		replayErr := tx.QueryRowContext(ctx, s.q("SELECT result FROM commerce_redeem_claims WHERE code_hash=? AND user_id=?"), hash, user).Scan(&replay)
		if replayErr == nil {
			return json.Unmarshal([]byte(replay), &result)
		}
		if !errors.Is(replayErr, sql.ErrNoRows) {
			return replayErr
		}
		var amount, maxUses, used int64
		var planID string
		var expires sql.NullString
		var enabled int
		q := "SELECT amount,plan_id,max_uses,used,expires_at,enabled FROM commerce_redeem_codes WHERE code_hash=?"
		if s.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		if err := tx.QueryRowContext(ctx, s.q(q), hash).Scan(&amount, &planID, &maxUses, &used, &expires, &enabled); err != nil {
			return err
		}
		if enabled == 0 || used >= maxUses || expires.Valid && expires.String != "" && !s.Now().Before(parse(expires.String)) {
			return errors.New("redeem code unavailable")
		}
		res, err := tx.ExecContext(ctx, s.q("UPDATE commerce_redeem_codes SET used=used+1 WHERE code_hash=? AND used=? AND enabled=1"), hash, used)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		result = map[string]any{"amount_cents": strconv.FormatInt(amount, 10), "plan_id": planID}
		if amount > 0 {
			if err := s.post(ctx, tx, user, amount, "redeem", "redeem:"+digestCode(hash+":"+user)); err != nil {
				return err
			}
		}
		if planID != "" {
			p, err := s.planTx(ctx, tx, planID)
			if err != nil {
				return err
			}
			if !p.Active || p.Kind != "period" {
				return errors.New("plan is not available")
			}
			quota, months := p.Quota, p.Months
			var current int64
			if err := tx.QueryRowContext(ctx, s.q("SELECT COALESCE(MAX(version),0) FROM commerce_entitlements WHERE user_id=?"), user).Scan(&current); err != nil {
				return err
			}
			start := s.Now().UTC()
			end := AddMonths(start, months)
			entID := id()
			if _, err := tx.ExecContext(ctx, s.q("INSERT INTO commerce_entitlements(id,user_id,plan_id,version,starts_at,expires_at,quota,used,allocated) VALUES(?,?,?,?,?,?,?,0,0)"), entID, user, planID, current+1, stamp(start), stamp(end), quota); err != nil {
				return err
			}
			if err := s.snapshotLimits(ctx, tx, entID, p.Limits); err != nil {
				return err
			}
			snapshot, err := json.Marshal(p)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, s.q("INSERT INTO commerce_purchase_snapshots(entitlement_id,user_id,plan_version,payload,created_at) VALUES(?,?,?,?,?)"), entID, user, p.Version, string(snapshot), stamp(s.Now())); err != nil {
				return err
			}
			result["entitlement_id"] = entID
			if err := s.emitEventTx(ctx, tx, user, "entitlement.redeemed", map[string]any{"entitlement_id": entID, "plan_id": planID}); err != nil {
				return err
			}
		}
		payload, err := json.Marshal(result)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_redeem_claims(code_hash,user_id,result,created_at) VALUES(?,?,?,?)"), hash, user, string(payload), stamp(s.Now()))
		return err
	})
	return result, err
}

func (s *Service) CreateReferralCode(ctx context.Context, user string) (ReferralCode, string, error) {
	plain := "INV-" + strings.ToUpper(id())
	v := ReferralCode{CodeHint: plain[:8], CreatedAt: s.Now().UTC()}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, s.q("INSERT INTO commerce_referral_codes(code_hash,owner_id,code_hint,created_at) VALUES(?,?,?,?)"), digestCode(plain), user, v.CodeHint, stamp(v.CreatedAt))
		return e
	})
	return v, plain, err
}

// Policy is disabled until an administrator explicitly chooses a basis-point rate.
func (s *Service) SetCommissionRate(ctx context.Context, bps int64) error {
	if bps < 0 || bps > 10000 {
		return errors.New("invalid commission rate")
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		q := "INSERT INTO commerce_referral_policy(id,rate_bps) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET rate_bps=excluded.rate_bps"
		if s.Dialect == "mysql" {
			q = "INSERT INTO commerce_referral_policy(id,rate_bps) VALUES(1,?) ON DUPLICATE KEY UPDATE rate_bps=VALUES(rate_bps)"
		}
		_, err := tx.ExecContext(ctx, s.q(q), bps)
		return err
	})
}
func (s *Service) BindReferral(ctx context.Context, user, plain string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		insert := "INSERT INTO commerce_referral_policy(id,rate_bps) VALUES(1,0) ON CONFLICT(id) DO NOTHING"
		if s.Dialect == "mysql" {
			insert = "INSERT INTO commerce_referral_policy(id,rate_bps) VALUES(1,0) ON DUPLICATE KEY UPDATE id=id"
		}
		if _, err := tx.ExecContext(ctx, insert); err != nil {
			return err
		}
		// Serializes graph changes on all databases, including simultaneous A/B bindings.
		q := "SELECT rate_bps FROM commerce_referral_policy WHERE id=1"
		if s.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		var rate int
		if err := tx.QueryRowContext(ctx, q).Scan(&rate); err != nil {
			return err
		}
		hash := digestCode(plain)
		var inviter string
		if err := tx.QueryRowContext(ctx, s.q("SELECT owner_id FROM commerce_referral_codes WHERE code_hash=?"), hash).Scan(&inviter); err != nil {
			return err
		}
		current := inviter
		for depth := 0; current != ""; depth++ {
			if current == user || depth >= 1000 {
				return errors.New("referral cycle or depth limit")
			}
			var parent string
			err := tx.QueryRowContext(ctx, s.q("SELECT inviter_id FROM commerce_referral_bindings WHERE invitee_id=?"), current).Scan(&parent)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			current = parent
		}
		if _, _, err := s.walletTx(ctx, tx, user); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM commerce_purchases WHERE user_id=?"), user).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return errors.New("referral must be bound before first purchase")
		}
		_, err := tx.ExecContext(ctx, s.q("INSERT INTO commerce_referral_bindings(invitee_id,inviter_id,code_hash,created_at) VALUES(?,?,?,?)"), user, inviter, hash, stamp(s.Now()))
		return err
	})
}
func (s *Service) recordCommission(ctx context.Context, tx *sql.Tx, invitee, source string, price int64) error {
	if price <= 0 {
		return nil
	}
	var rate int64
	if err := tx.QueryRowContext(ctx, "SELECT rate_bps FROM commerce_referral_policy WHERE id=1").Scan(&rate); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if rate == 0 {
		return nil
	}
	var inviter string
	if err := tx.QueryRowContext(ctx, s.q("SELECT inviter_id FROM commerce_referral_bindings WHERE invitee_id=?"), invitee).Scan(&inviter); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	// Split the multiplication to avoid overflow at the int64 money boundary.
	amount := price/10000*rate + price%10000*rate/10000
	if amount == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, s.q("INSERT INTO commerce_commissions(id,inviter_id,invitee_id,source_id,amount,status,created_at) VALUES(?,?,?,?,?,'pending',?)"), id(), inviter, invitee, source, amount, stamp(s.Now()))
	return err
}

func (s *Service) Commissions(ctx context.Context, user string) ([]Commission, error) {
	rows, err := s.DB.QueryContext(ctx, s.q("SELECT c.id,c.inviter_id,c.invitee_id,c.source_id,c.amount,c.status,c.created_at,COALESCE(r.amount,0) FROM commerce_commissions c LEFT JOIN commerce_commission_refunds r ON r.commission_id=c.id WHERE c.inviter_id=? ORDER BY c.created_at DESC LIMIT 100"), user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Commission
	for rows.Next() {
		var c Commission
		var created string
		if err := rows.Scan(&c.ID, &c.InviterID, &c.InviteeID, &c.SourceID, &c.Amount, &c.Status, &created, &c.Refunded); err != nil {
			return nil, err
		}
		c.CreatedAt = parse(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) String() string { return fmt.Sprintf("commerce:%s", s.Dialect) }

func (s *Service) CommissionRate(ctx context.Context) (int64, error) {
	var rate int64
	err := s.DB.QueryRowContext(ctx, "SELECT rate_bps FROM commerce_referral_policy WHERE id=1").Scan(&rate)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rate, err
}
func (s *Service) ResolveCommission(ctx context.Context, actor, commissionID, action, reason string) error {
	if actor == "" || (action != "settle" && action != "reverse") || strings.TrimSpace(reason) == "" || len(reason) > 500 {
		return errors.New("invalid commission resolution")
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		q := "SELECT inviter_id,amount,status FROM commerce_commissions WHERE id=?"
		if s.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		var owner, status string
		var amount int64
		if err := tx.QueryRowContext(ctx, s.q(q), commissionID).Scan(&owner, &amount, &status); err != nil {
			return err
		}
		reversed, err := s.reversedCommission(ctx, tx, commissionID)
		if err != nil {
			return err
		}
		amount -= reversed
		target := "paid"
		if action == "reverse" {
			target = "reversed"
		}
		if status == target {
			return nil
		}
		if status == "reversed" {
			return ErrConflict
		}
		if action == "settle" {
			if err := s.post(ctx, tx, owner, amount, "commission", "commission:"+commissionID); err != nil {
				return err
			}
		} else if status == "paid" {
			if err := s.post(ctx, tx, owner, -amount, "commission_reversal", "commission-reverse:"+commissionID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, s.q("UPDATE commerce_commissions SET status=? WHERE id=?"), target, commissionID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_commission_adjustments(id,commission_id,action,actor_id,reason,created_at) VALUES(?,?,?,?,?,?)"), id(), commissionID, action, actor, reason, stamp(s.Now()))
		return err
	})
}
func (s *Service) RedeemCodes(ctx context.Context) ([]RedeemCode, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT code_hash,code_hint,amount,plan_id,max_uses,used,expires_at,enabled,created_at FROM commerce_redeem_codes ORDER BY created_at DESC,code_hash DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RedeemCode{}
	for rows.Next() {
		var v RedeemCode
		var expires sql.NullString
		var enabled int
		var created string
		if err = rows.Scan(&v.ID, &v.CodeHint, &v.Amount, &v.PlanID, &v.MaxUses, &v.Used, &expires, &enabled, &created); err != nil {
			return nil, err
		}
		v.Enabled = enabled != 0
		v.CreatedAt = parse(created)
		if expires.Valid {
			t := parse(expires.String)
			v.ExpiresAt = &t
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Service) RevokeRedeemCode(ctx context.Context, codeID string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q("UPDATE commerce_redeem_codes SET enabled=0 WHERE code_hash=?"), codeID)
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
}
