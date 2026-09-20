package commerce

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

type Plan struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Price  int64  `json:"price_cents,string"`
	Quota  int64  `json:"quota_bytes,string"`
	Months int    `json:"months"`
}
type Wallet struct {
	Currency string `json:"currency"`
	Balance  int64  `json:"balance_cents,string"`
}
type Entitlement struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	PlanID    string    `json:"plan_id"`
	Version   int64     `json:"version"`
	StartsAt  time.Time `json:"starts_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Quota     int64     `json:"quota_bytes,string"`
	Used      int64     `json:"used_bytes,string"`
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
	DB          *sql.DB
	Dialect     string
	Write       Writer
	Now         func() time.Time
	reconcileMu sync.Mutex
}

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
	return storage.MigrateNamespace(ctx, s.DB, s.Dialect, "commerce", 2, func(conn *sql.Conn) error {
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
		return s.migrateReconciliation(ctx, conn)
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
	return scanEnt(s.DB.QueryRowContext(ctx, s.q("SELECT "+entFields+" FROM commerce_entitlements WHERE user_id=? ORDER BY version DESC LIMIT 1"), user))
}
func (s *Service) Plans(ctx context.Context) ([]Plan, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT id,name,price,quota,months FROM commerce_plans ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		var p Plan
		if e = rows.Scan(&p.ID, &p.Name, &p.Price, &p.Quota, &p.Months); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Service) CreatePlan(ctx context.Context, p Plan) (Plan, error) {
	if p.Name == "" || len(p.Name) > 200 || p.Price <= 0 || p.Quota <= 0 || p.Months < 1 || p.Months > 120 {
		return p, errors.New("invalid plan")
	}
	p.ID = id()
	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, s.q("INSERT INTO commerce_plans(id,name,price,quota,months) VALUES(?,?,?,?,?)"), p.ID, p.Name, p.Price, p.Quota, p.Months)
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
	r, e := tx.ExecContext(ctx, s.q("UPDATE commerce_wallets SET balance=?,version=version+1 WHERE user_id=? AND version=?"), balance+delta, user, v)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	_, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_ledger(id,user_id,amount,balance,kind,reference_id,created_at) VALUES(?,?,?,?,?,?,?)"), id(), user, delta, balance+delta, kind, reference, stamp(s.Now()))
	return e
}
func (s *Service) Purchase(ctx context.Context, user, plan, key string, expected int64) (Entitlement, error) {
	var result Entitlement
	if len(key) < 1 || len(key) > 128 {
		return result, errors.New("invalid idempotency key")
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, _, err := s.walletTx(ctx, tx, user); err != nil {
			return err
		}
		var prev, p string
		var v int64
		e := tx.QueryRowContext(ctx, s.q("SELECT entitlement_id,plan_id,expected_version FROM commerce_purchases WHERE user_id=? AND idempotency_key=?"), user, key).Scan(&prev, &p, &v)
		if e == nil {
			if p != plan || v != expected {
				return ErrConflict
			}
			result, e = scanEnt(tx.QueryRowContext(ctx, s.q("SELECT "+entFields+" FROM commerce_entitlements WHERE id=?"), prev))
			return e
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		var current int64
		e = tx.QueryRowContext(ctx, s.q("SELECT version FROM commerce_entitlements WHERE user_id=? ORDER BY version DESC LIMIT 1"), user).Scan(&current)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if current != expected {
			return ErrConflict
		}
		var price, quota int64
		var months int
		e = tx.QueryRowContext(ctx, s.q("SELECT price,quota,months FROM commerce_plans WHERE id=?"), plan).Scan(&price, &quota, &months)
		if e != nil {
			return e
		}
		result = Entitlement{ID: id(), UserID: user, PlanID: plan, Version: current + 1, StartsAt: s.Now().UTC(), Quota: quota}
		result.ExpiresAt = AddMonths(result.StartsAt, months)
		if e = s.post(ctx, tx, user, -price, "purchase", result.ID); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_entitlements(id,user_id,plan_id,version,starts_at,expires_at,quota,used,allocated) VALUES(?,?,?,?,?,?,?,0,0)"), result.ID, user, plan, result.Version, stamp(result.StartsAt), stamp(result.ExpiresAt), quota)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_purchases(user_id,idempotency_key,plan_id,expected_version,entitlement_id) VALUES(?,?,?,?,?)"), user, key, plan, expected, result.ID)
		return e
	})
	return result, err
}
