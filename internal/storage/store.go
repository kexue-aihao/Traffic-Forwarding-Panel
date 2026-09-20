// Package storage supplies the shared SQL and transactional write boundary.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type Priority int

const (
	Critical Priority = iota
	Normal
	Background
)

type request struct {
	ctx  context.Context
	fn   func(*sql.Tx) error
	done chan error
}
type Store struct {
	DB      *sql.DB
	Dialect string
	queues  [3]chan request
	stop    chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func Open(ctx context.Context, dialect, dsn string) (*Store, error) {
	driver := dialect
	switch dialect {
	case "sqlite":
		if dsn == "" {
			return nil, errors.New("sqlite path is required")
		}
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		dsn += separator + "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	case "postgres":
		driver = "pgx"
	case "mysql":
	default:
		return nil, fmt.Errorf("unsupported database %q", dialect)
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	if dialect == "sqlite" {
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(8)
	}
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{DB: db, Dialect: dialect, stop: make(chan struct{}), closed: make(chan struct{})}
	if err = s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	for i := range s.queues {
		s.queues[i] = make(chan request, 256)
	}
	go s.writer()
	return s, nil
}

// Rebind converts placeholders in queries written by this application; queries
// must not contain question marks in SQL literals or comments.
func (s *Store) Rebind(q string) string {
	if s.Dialect != "postgres" {
		return q
	}
	n := 0
	return strings.Map(func(r rune) rune { return r }, replacePlaceholders(q, &n))
}
func replacePlaceholders(q string, n *int) string {
	parts := strings.Split(q, "?")
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			*n++
			fmt.Fprintf(&b, "$%d", *n)
		}
		b.WriteString(p)
	}
	return b.String()
}

// Write does not acknowledge work until its transaction has committed. Callbacks
// must perform only database work. Context cancellation can race a commit; callers
// that retry mutating operations must use durable idempotency keys.
func (s *Store) Write(ctx context.Context, p Priority, fn func(*sql.Tx) error) error {
	if p < Critical || p > Background {
		return errors.New("invalid write priority")
	}
	select {
	case <-s.stop:
		return errors.New("store closed")
	default:
	}
	if s.Dialect != "sqlite" {
		return s.transaction(ctx, fn)
	}
	r := request{ctx: ctx, fn: fn, done: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stop:
		return errors.New("store closed")
	case s.queues[p] <- r:
	}
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stop:
		return errors.New("store closed")
	}
}
func (s *Store) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) writer() {
	defer close(s.closed)
	// Weighted rounds ensure background work cannot starve indefinitely.
	schedule := []int{0, 0, 0, 0, 1, 1, 2}
	cursor := 0
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		var r request
		found := false
		for i := 0; i < len(schedule); i++ {
			p := schedule[cursor]
			cursor = (cursor + 1) % len(schedule)
			select {
			case r = <-s.queues[p]:
				found = true
			default:
			}
			if found {
				break
			}
		}
		if !found {
			select {
			case <-s.stop:
				return
			case r = <-s.queues[0]:
			case r = <-s.queues[1]:
			case r = <-s.queues[2]:
			}
		}
		if err := r.ctx.Err(); err != nil {
			r.done <- err
			continue
		}
		r.done <- s.transaction(r.ctx, r.fn)
	}
}
func (s *Store) Close() error { s.once.Do(func() { close(s.stop) }); <-s.closed; return s.DB.Close() }

func (s *Store) migrate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Cross-process migration locks for server databases. SQLite BEGIN IMMEDIATE
	// serializes migrations before reading schema version.
	switch s.Dialect {
	case "postgres":
		if _, err = conn.ExecContext(ctx, "SELECT pg_advisory_lock(821736501)"); err != nil {
			return err
		}
		defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(821736501)")
	case "mysql":
		var locked int
		if err = conn.QueryRowContext(ctx, "SELECT GET_LOCK('tfp_schema', 20)").Scan(&locked); err != nil {
			return err
		}
		if locked != 1 {
			return errors.New("migration lock unavailable")
		}
		defer conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK('tfp_schema')")
	case "sqlite":
		if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		defer conn.ExecContext(context.Background(), "ROLLBACK")
	}
	for _, statement := range schema {
		if s.Dialect == "mysql" {
			statement = strings.ReplaceAll(statement, " TEXT", " LONGTEXT")
		}
		if _, err = conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("schema migration: %w", err)
		}
	}
	var count int
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM cp_schema WHERE version=1").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err = conn.ExecContext(ctx, "INSERT INTO cp_schema(version) VALUES(1)"); err != nil {
			return err
		}
	}
	// Migration 2 is restartable even on MySQL, whose DDL auto-commits.
	for _, idx := range indexes {
		if s.Dialect == "mysql" {
			var exists int
			if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=?", idx.table, idx.name).Scan(&exists); err != nil {
				return err
			}
			if exists > 0 {
				continue
			}
		}
		clause := "IF NOT EXISTS "
		if s.Dialect == "mysql" {
			clause = ""
		}
		if _, err = conn.ExecContext(ctx, "CREATE INDEX "+clause+idx.name+" ON "+idx.table+"("+idx.columns+")"); err != nil {
			return err
		}
	}
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM cp_schema WHERE version=2").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if s.Dialect == "postgres" {
			if _, err = conn.ExecContext(ctx, "ALTER TABLE cp_usage ALTER COLUMN id TYPE VARCHAR(128)"); err != nil {
				return err
			}
		}
		if s.Dialect == "mysql" {
			if _, err = conn.ExecContext(ctx, "ALTER TABLE cp_usage MODIFY id VARCHAR(128) NOT NULL"); err != nil {
				return err
			}
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO cp_schema(version) VALUES(2)"); err != nil {
			return err
		}
	}
	if s.Dialect == "sqlite" {
		_, err = conn.ExecContext(ctx, "COMMIT")
	}
	return err
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS cp_schema(version INTEGER PRIMARY KEY)`,
	`CREATE TABLE IF NOT EXISTS cp_users(id VARCHAR(64) PRIMARY KEY, username VARCHAR(190) NOT NULL UNIQUE, password_hash VARCHAR(190) NOT NULL, role VARCHAR(32) NOT NULL, disabled INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS cp_sessions(token_hash VARCHAR(64) PRIMARY KEY, user_id VARCHAR(64) NOT NULL, expires_at BIGINT NOT NULL, FOREIGN KEY(user_id) REFERENCES cp_users(id))`,
	`CREATE TABLE IF NOT EXISTS cp_tokens(id VARCHAR(64) PRIMARY KEY,token_hash VARCHAR(64) NOT NULL UNIQUE,user_id VARCHAR(64) NOT NULL,name VARCHAR(190) NOT NULL,expires_at BIGINT NOT NULL,FOREIGN KEY(user_id) REFERENCES cp_users(id))`,
	`CREATE TABLE IF NOT EXISTS cp_groups(id VARCHAR(64) PRIMARY KEY, name VARCHAR(190) NOT NULL, payload TEXT NOT NULL, version BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cp_group_users(group_id VARCHAR(64) NOT NULL, user_id VARCHAR(64) NOT NULL, PRIMARY KEY(group_id,user_id), FOREIGN KEY(group_id) REFERENCES cp_groups(id), FOREIGN KEY(user_id) REFERENCES cp_users(id))`,
	`CREATE TABLE IF NOT EXISTS cp_nodes(id VARCHAR(64) PRIMARY KEY, name VARCHAR(190) NOT NULL, token_hash VARCHAR(64) NOT NULL UNIQUE, payload TEXT NOT NULL, desired_version BIGINT NOT NULL, applied_version BIGINT NOT NULL, apply_error TEXT NOT NULL, last_seen BIGINT NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS cp_node_groups(node_id VARCHAR(64) NOT NULL, group_id VARCHAR(64) NOT NULL, PRIMARY KEY(node_id,group_id), FOREIGN KEY(node_id) REFERENCES cp_nodes(id), FOREIGN KEY(group_id) REFERENCES cp_groups(id))`,
	`CREATE TABLE IF NOT EXISTS cp_enrollments(token_hash VARCHAR(64) PRIMARY KEY, payload TEXT NOT NULL, expires_at BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cp_rules(id VARCHAR(64) PRIMARY KEY, user_id VARCHAR(64) NOT NULL, node_id VARCHAR(64) NOT NULL, group_id VARCHAR(64) NOT NULL, payload TEXT NOT NULL, version BIGINT NOT NULL, deleted INTEGER NOT NULL DEFAULT 0, release_version BIGINT NOT NULL DEFAULT 0, FOREIGN KEY(user_id) REFERENCES cp_users(id), FOREIGN KEY(node_id) REFERENCES cp_nodes(id), FOREIGN KEY(group_id) REFERENCES cp_groups(id))`,
	`CREATE TABLE IF NOT EXISTS cp_ports(node_id VARCHAR(64) NOT NULL, network VARCHAR(8) NOT NULL, port INTEGER NOT NULL, rule_id VARCHAR(64) NOT NULL, PRIMARY KEY(node_id,network,port), FOREIGN KEY(rule_id) REFERENCES cp_rules(id))`,
	`CREATE TABLE IF NOT EXISTS cp_usage(id VARCHAR(64) PRIMARY KEY, node_id VARCHAR(64) NOT NULL, rule_id VARCHAR(64) NOT NULL, lease_id VARCHAR(64) NOT NULL, payload TEXT NOT NULL, received_at BIGINT NOT NULL, settled INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS cp_audit(id VARCHAR(64) PRIMARY KEY, user_id VARCHAR(64) NOT NULL, action VARCHAR(64) NOT NULL, target VARCHAR(64) NOT NULL, created_at BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cp_rule_leases(id VARCHAR(64) PRIMARY KEY, rule_id VARCHAR(64) NOT NULL, node_id VARCHAR(64) NOT NULL, entitlement_id VARCHAR(64) NOT NULL, bytes_allocated BIGINT NOT NULL, bytes_used BIGINT NOT NULL, expires_at BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cp_lease_retirements(id VARCHAR(64) PRIMARY KEY,used_bytes BIGINT NOT NULL,retired_at BIGINT NOT NULL)`,
}

var indexes = []struct{ name, table, columns string }{
	{"cp_rules_node", "cp_rules", "node_id,deleted,id"},
	{"cp_rules_user", "cp_rules", "user_id,deleted,id"},
	{"cp_rules_group", "cp_rules", "group_id,deleted,id"},
	{"cp_rules_release", "cp_rules", "node_id,deleted,release_version"},
	{"cp_sessions_user", "cp_sessions", "user_id,expires_at"},
	{"cp_sessions_expiry", "cp_sessions", "expires_at"},
	{"cp_gu_user", "cp_group_users", "user_id,group_id"},
	{"cp_ng_group", "cp_node_groups", "group_id,node_id"},
	{"cp_ports_rule", "cp_ports", "rule_id"},
	{"cp_usage_rule", "cp_usage", "rule_id,received_at"},
	{"cp_usage_settled", "cp_usage", "settled,received_at"},
	{"cp_audit_time", "cp_audit", "created_at,id"},
	{"cp_leases_rule", "cp_rule_leases", "rule_id,expires_at"},
}
