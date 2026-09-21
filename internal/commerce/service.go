package commerce

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

type Plan struct {
	Limits  contract.ResourceLimits `json:"limits"`
	ID      string                  `json:"id"`
	Name    string                  `json:"name"`
	Price   int64                   `json:"price_cents,string"`
	Quota   int64                   `json:"quota_bytes,string"`
	Months  int                     `json:"months"`
	Active  bool                    `json:"active"`
	Version int64                   `json:"version"`
	Kind    string                  `json:"kind"`
}
type Wallet struct {
	Currency string `json:"currency"`
	Balance  int64  `json:"balance_cents,string"`
}
type Entitlement struct {
	Limits    contract.ResourceLimits `json:"limits"`
	ID        string                  `json:"id"`
	UserID    string                  `json:"user_id"`
	PlanID    string                  `json:"plan_id"`
	Version   int64                   `json:"version"`
	StartsAt  time.Time               `json:"starts_at"`
	ExpiresAt time.Time               `json:"expires_at"`
	Quota     int64                   `json:"quota_bytes,string"`
	Used      int64                   `json:"used_bytes,string"`
}
type Ledger struct {
	ID        string    `json:"id"`
	Amount    int64     `json:"amount_cents,string"`
	Balance   int64     `json:"balance_cents,string"`
	Kind      string    `json:"kind"`
	Reference string    `json:"reference"`
	CreatedAt time.Time `json:"created_at"`
}
type Order struct {
	ID         string    `json:"id"`
	Channel    string    `json:"channel"`
	Amount     int64     `json:"amount_cents,string"`
	Currency   string    `json:"currency"`
	Status     string    `json:"status"`
	PaymentURL string    `json:"payment_url,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}
type Writer func(context.Context, func(*sql.Tx) error) error
type Service struct {
	PaymentAllowed func(context.Context, int64) error
	DB             *sql.DB
	Dialect        string
	Write          Writer
	Now            func() time.Time
	reconcileMu    sync.Mutex
	CheckAccount   func(context.Context, *sql.Tx, string) error
	webhookClient  *http.Client
	EventVisible   func(context.Context, *sql.Tx, string, string, string, bool) (bool, error)
}

var ErrAccountDisabled = errors.New("account disabled")
var ErrConflict = errors.New("state conflict")
var ErrFunds = errors.New("insufficient available balance")

func New(db *sql.DB, dialect string, write Writer) *Service {
	return &Service{DB: db, Dialect: dialect, Write: write, Now: time.Now}
}
func id() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func (s *Service) q(q string) string {
	if s.Dialect != "postgres" && s.Dialect != "pg" {
		return q
	}
	n := 0
	var b strings.Builder
	for _, c := range q {
		if c == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}
func (s *Service) Migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS commerce_plans(id VARCHAR(64) PRIMARY KEY,name VARCHAR(200) NOT NULL,price BIGINT NOT NULL,quota BIGINT NOT NULL,months INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_wallets(user_id VARCHAR(64) PRIMARY KEY,balance BIGINT NOT NULL,version BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_ledger(id VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,amount BIGINT NOT NULL,balance BIGINT NOT NULL,kind VARCHAR(32) NOT NULL,reference_id VARCHAR(128) NOT NULL UNIQUE,created_at VARCHAR(40) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_entitlements(id VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,plan_id VARCHAR(64) NOT NULL,version BIGINT NOT NULL,starts_at VARCHAR(40) NOT NULL,expires_at VARCHAR(40) NOT NULL,quota BIGINT NOT NULL,used BIGINT NOT NULL,allocated BIGINT NOT NULL,UNIQUE(user_id,version))`,
		`CREATE TABLE IF NOT EXISTS commerce_purchases(user_id VARCHAR(64) NOT NULL,idempotency_key VARCHAR(128) NOT NULL,plan_id VARCHAR(64) NOT NULL,expected_version BIGINT NOT NULL,entitlement_id VARCHAR(64) NOT NULL,PRIMARY KEY(user_id,idempotency_key))`,
		`CREATE TABLE IF NOT EXISTS commerce_orders(id VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,channel VARCHAR(32) NOT NULL,amount BIGINT NOT NULL,status VARCHAR(32) NOT NULL,payment_url TEXT NOT NULL,created_at VARCHAR(40) NOT NULL,idempotency_key VARCHAR(128) NOT NULL,provider_tx VARCHAR(128),UNIQUE(user_id,idempotency_key))`,
		`CREATE TABLE IF NOT EXISTS commerce_leases(id VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,node_id VARCHAR(64) NOT NULL,rule_id VARCHAR(64) NOT NULL,entitlement_id VARCHAR(64) NOT NULL,expires_at VARCHAR(40) NOT NULL,bytes BIGINT NOT NULL,used BIGINT NOT NULL,multiplier VARCHAR(80) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_usage(id VARCHAR(128) PRIMARY KEY,lease_id VARCHAR(64) NOT NULL,payload TEXT NOT NULL,charged BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_lease_reservations(lease_id VARCHAR(64) PRIMARY KEY,budget BIGINT NOT NULL,closed INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS commerce_attempts(order_id VARCHAR(64) PRIMARY KEY,state VARCHAR(32) NOT NULL,provider_id VARCHAR(128) NOT NULL,updated_at VARCHAR(40) NOT NULL)`,
	}
	err := storage.MigrateNamespace(ctx, s.DB, s.Dialect, "commerce", 6, func(conn *sql.Conn) error {
		var current int
		if err := conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM commerce_schema").Scan(&current); err != nil {
			return err
		}
		// Preserve the already released v1 migration. A fresh database applies
		// it before v2; an existing v1 database only receives the new table.
		if current < 1 {
			for _, q := range statements {
				if s.Dialect == "mysql" {
					q = strings.ReplaceAll(q, " TEXT", " LONGTEXT")
				}
				if _, e := conn.ExecContext(ctx, q); e != nil {
					return e
				}
			}
			for _, idx := range []struct {
				name, table, columns string
				unique               bool
			}{
				{"commerce_ledger_user", "commerce_ledger", "user_id,created_at,id", false},
				{"commerce_order_user", "commerce_orders", "user_id,created_at,id", false},
				{"commerce_order_transaction", "commerce_orders", "channel,provider_tx", true},
				{"commerce_usage_lease", "commerce_usage", "lease_id", false},
				{"commerce_lease_rule", "commerce_leases", "rule_id,expires_at", false},
			} {
				if e := storage.EnsureIndex(ctx, conn, s.Dialect, idx.table, idx.name, idx.columns, idx.unique); e != nil {
					return e
				}
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO commerce_schema(version) VALUES(1)"); err != nil {
				return err
			}
		}
		if current < 2 {
			if err := s.migrateReconciliation(ctx, conn); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO commerce_schema(version) VALUES(2)"); err != nil {
				return err
			}
		}
		if current < 3 {
			if err := s.migrateCommerceV3(ctx, conn); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO commerce_schema(version) VALUES(3)"); err != nil {
				return err
			}
		}
		if current < 4 {
			if err := s.migrateLimits(ctx, conn); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO commerce_schema(version) VALUES(4)"); err != nil {
				return err
			}
		}
		if current < 5 {
			if err := s.migrateWebhookSettings(ctx, conn); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO commerce_schema(version) VALUES(5)"); err != nil {
				return err
			}
		}
		return s.migrateFunding(ctx, conn)
	})
	if err != nil {
		return err
	}
	// Backup import keeps target schema markers. On restart, backfill only data
	// omitted by old backups; no DDL is performed outside the migration lock.
	return s.Write(ctx, func(tx *sql.Tx) error {
		q := "INSERT INTO commerce_plan_states(plan_id,active,updated_at) SELECT p.id,1,? FROM commerce_plans p WHERE NOT EXISTS(SELECT 1 FROM commerce_plan_states st WHERE st.plan_id=p.id) ON CONFLICT(plan_id) DO NOTHING"
		if s.Dialect == "mysql" {
			q = "INSERT INTO commerce_plan_states(plan_id,active,updated_at) SELECT p.id,1,? FROM commerce_plans p WHERE NOT EXISTS(SELECT 1 FROM commerce_plan_states st WHERE st.plan_id=p.id) ON DUPLICATE KEY UPDATE plan_id=commerce_plan_states.plan_id"
		}
		_, err := tx.ExecContext(ctx, s.q(q), stamp(s.Now()))
		return err
	})
}

func AddMonths(t time.Time, months int) time.Time {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	t = t.In(loc)
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(months), 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
	last := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, loc).Day()
	if d > last {
		d = last
	}
	return time.Date(first.Year(), first.Month(), d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc).UTC()
}
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func parse(t string) time.Time { v, _ := time.Parse(time.RFC3339Nano, t); return v }

type scanner interface{ Scan(...any) error }

func scanEnt(r scanner) (Entitlement, error) {
	var e Entitlement
	var start, end string
	err := r.Scan(&e.ID, &e.UserID, &e.PlanID, &e.Version, &start, &end, &e.Quota, &e.Used)
	e.StartsAt = parse(start)
	e.ExpiresAt = parse(end)
	return e, err
}

const entFields = "id,user_id,plan_id,version,starts_at,expires_at,quota,used"

func (s *Service) Entitlement(ctx context.Context, user string) (Entitlement, error) {
	e, err := scanEnt(s.DB.QueryRowContext(ctx, s.q("SELECT "+entFields+" FROM commerce_entitlements WHERE user_id=? ORDER BY version DESC LIMIT 1"), user))
	if err == nil {
		e.Limits, err = s.entitlementLimits(ctx, s.DB, e.ID)
	}
	return e, err
}
func (s *Service) Plans(ctx context.Context) ([]Plan, error) {
	rows, e := s.DB.QueryContext(ctx, s.q("SELECT p.id,p.name,p.price,p.quota,p.months,COALESCE(st.active,1),COALESCE(st.version,1),COALESCE(st.kind,'period'),COALESCE(lim.payload,'{}') FROM commerce_plans p LEFT JOIN commerce_plan_states st ON st.plan_id=p.id LEFT JOIN commerce_plan_limits lim ON lim.plan_id=p.id ORDER BY p.id"))
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		var p Plan
		var rawLimits string
		if e = rows.Scan(&p.ID, &p.Name, &p.Price, &p.Quota, &p.Months, &p.Active, &p.Version, &p.Kind, &rawLimits); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(rawLimits), &p.Limits); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Service) CreatePlan(ctx context.Context, p Plan) (Plan, error) {
	if p.Kind == "" {
		p.Kind = "period"
	}
	if err := validatePlan(p); err != nil {
		return p, errors.New("invalid plan")
	}
	p.ID = id()
	p.Active = true
	p.Version = 1
	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, s.q("INSERT INTO commerce_plans(id,name,price,quota,months) VALUES(?,?,?,?,?)"), p.ID, p.Name, p.Price, p.Quota, p.Months)
		if e != nil {
			return e
		}
		if e = s.savePlanLimits(ctx, tx, p); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_plan_states(plan_id,active,version,kind,updated_at) VALUES(?,?,1,?,?)"), p.ID, 1, p.Kind, stamp(s.Now()))
		return e
	})
	return p, err
}
func (s *Service) Wallet(ctx context.Context, user string) (Wallet, error) {
	w := Wallet{Currency: "CNY"}
	e := s.DB.QueryRowContext(ctx, s.q("SELECT balance FROM commerce_wallets WHERE user_id=?"), user).Scan(&w.Balance)
	if errors.Is(e, sql.ErrNoRows) {
		e = nil
	}
	return w, e
}
func (s *Service) walletTx(ctx context.Context, tx *sql.Tx, user string) (int64, int64, error) {
	var balance, version int64
	insert := "INSERT INTO commerce_wallets(user_id,balance,version) VALUES(?,0,0) ON CONFLICT(user_id) DO NOTHING"
	if s.Dialect == "mysql" {
		insert = "INSERT INTO commerce_wallets(user_id,balance,version) VALUES(?,0,0) ON DUPLICATE KEY UPDATE user_id=user_id"
	}
	if _, e := tx.ExecContext(ctx, s.q(insert), user); e != nil {
		return 0, 0, e
	}
	query := "SELECT balance,version FROM commerce_wallets WHERE user_id=?"
	if s.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	e := tx.QueryRowContext(ctx, s.q(query), user).Scan(&balance, &version)
	return balance, version, e
}
func (s *Service) post(ctx context.Context, tx *sql.Tx, user string, delta int64, kind, reference string) error {
	balance, v, e := s.walletTx(ctx, tx, user)
	if e != nil {
		return e
	}
	if delta > 0 && balance > int64(^uint64(0)>>1)-delta {
		return errors.New("balance overflow")
	}
	if delta < 0 && balance < -delta {
		return ErrFunds
	}
	if e = s.trackFunds(ctx, tx, user, balance, delta, kind, reference); e != nil {
		return e
	}
	r, e := tx.ExecContext(ctx, s.q("UPDATE commerce_wallets SET balance=?,version=version+1 WHERE user_id=? AND version=?"), balance+delta, user, v)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	_, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_ledger(id,user_id,amount,balance,kind,reference_id,created_at) VALUES(?,?,?,?,?,?,?)"), id(), user, delta, balance+delta, kind, reference, stamp(s.Now()))
	if e != nil {
		return e
	}
	return s.emitEventTx(ctx, tx, user, "wallet."+kind, map[string]any{"amount_cents": strconv.FormatInt(delta, 10), "balance_cents": strconv.FormatInt(balance+delta, 10), "reference": reference})
}
func (s *Service) Purchase(ctx context.Context, user, plan, key string, expected int64) (Entitlement, error) {
	return s.PurchaseQuote(ctx, user, plan, key, expected, 0)
}
func (s *Service) PurchaseQuote(ctx context.Context, user, plan, key string, expected, planVersion int64) (Entitlement, error) {
	var result Entitlement
	if len(key) < 1 || len(key) > 128 {
		return result, errors.New("invalid idempotency key")
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.purchaseTx(ctx, tx, user, plan, key, expected, planVersion)
		return err
	})
	return result, err
}

func (s *Service) purchaseTx(ctx context.Context, tx *sql.Tx, user, plan, key string, expected, planVersion int64) (Entitlement, error) {
	var result Entitlement
	if s.CheckAccount != nil {
		if err := s.CheckAccount(ctx, tx, user); err != nil {
			return result, err
		}
	}
	if _, _, err := s.walletTx(ctx, tx, user); err != nil {
		return result, err
	}
	var prev, p string
	var v int64
	e := tx.QueryRowContext(ctx, s.q("SELECT entitlement_id,plan_id,expected_version FROM commerce_purchases WHERE user_id=? AND idempotency_key=?"), user, key).Scan(&prev, &p, &v)
	if e == nil {
		if planVersion != 0 {
			var quote int64
			if err := tx.QueryRowContext(ctx, s.q("SELECT plan_version FROM commerce_purchase_snapshots WHERE entitlement_id=?"), prev).Scan(&quote); err != nil {
				return result, err
			}
			if quote != planVersion {
				return result, ErrConflict
			}
		}
		if p != plan || v != expected {
			return result, ErrConflict
		}
		result, e = scanEnt(tx.QueryRowContext(ctx, s.q("SELECT "+entFields+" FROM commerce_entitlements WHERE id=?"), prev))
		if e == nil {
			result.Limits, e = s.entitlementLimits(ctx, tx, result.ID)
		}
		return result, e
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return result, e
	}
	var current int64
	e = tx.QueryRowContext(ctx, s.q("SELECT version FROM commerce_entitlements WHERE user_id=? ORDER BY version DESC LIMIT 1"), user).Scan(&current)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return result, e
	}
	if current != expected {
		return result, ErrConflict
	}
	purchasedPlan, e := s.planTx(ctx, tx, plan)
	if e != nil {
		return result, e
	}
	if !purchasedPlan.Active || purchasedPlan.Kind != "period" {
		return result, errors.New("plan unavailable")
	}
	if planVersion != 0 && purchasedPlan.Version != planVersion {
		return result, ErrConflict
	}
	price, quota, months := purchasedPlan.Price, purchasedPlan.Quota, purchasedPlan.Months
	result = Entitlement{ID: id(), UserID: user, PlanID: plan, Version: current + 1, StartsAt: s.Now().UTC(), Quota: quota, Limits: purchasedPlan.Limits}
	result.ExpiresAt = AddMonths(result.StartsAt, months)
	if e = s.post(ctx, tx, user, -price, "purchase", result.ID); e != nil {
		return result, e
	}
	_, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_entitlements(id,user_id,plan_id,version,starts_at,expires_at,quota,used,allocated) VALUES(?,?,?,?,?,?,?,0,0)"), result.ID, user, plan, result.Version, stamp(result.StartsAt), stamp(result.ExpiresAt), quota)
	if e != nil {
		return result, e
	}
	if e = s.snapshotLimits(ctx, tx, result.ID, result.Limits); e != nil {
		return result, e
	}
	snapshot, e := json.Marshal(purchasedPlan)
	if e != nil {
		return result, e
	}
	if _, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_purchase_snapshots(entitlement_id,user_id,plan_version,payload,created_at) VALUES(?,?,?,?,?)"), result.ID, user, purchasedPlan.Version, string(snapshot), stamp(s.Now())); e != nil {
		return result, e
	}
	if e = s.recordCommission(ctx, tx, user, result.ID, price); e != nil {
		return result, e
	}
	if e = s.emitEventTx(ctx, tx, user, "entitlement.purchased", map[string]any{"entitlement_id": result.ID, "plan_id": plan, "amount_cents": strconv.FormatInt(price, 10)}); e != nil {
		return result, e
	}
	_, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_purchases(user_id,idempotency_key,plan_id,expected_version,entitlement_id) VALUES(?,?,?,?,?)"), user, key, plan, expected, result.ID)
	return result, e
}
