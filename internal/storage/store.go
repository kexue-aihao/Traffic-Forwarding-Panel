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
	DB           *sql.DB
	Dialect      string
	queues       [3]chan request
	stop         chan struct{}
	closed       chan struct{}
	once         sync.Once
	disableQueue bool
}

type Options struct {
	MaxOpenConnections *int
	MaxIdleConnections *int
	DisableQueue       bool
}

func Open(ctx context.Context, dialect, dsn string) (*Store, error) {
	return OpenWithOptions(ctx, dialect, dsn, Options{})
}

func OpenWithOptions(ctx context.Context, dialect, dsn string, opts Options) (*Store, error) {
	if opts.MaxOpenConnections != nil && *opts.MaxOpenConnections < 1 || opts.MaxIdleConnections != nil && *opts.MaxIdleConnections < 0 {
		return nil, errors.New("invalid database connection pool options")
	}
	if opts.MaxOpenConnections != nil && opts.MaxIdleConnections != nil && *opts.MaxIdleConnections > *opts.MaxOpenConnections {
		return nil, errors.New("idle connections exceed open connections")
	}
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
	if opts.MaxOpenConnections != nil {
		db.SetMaxOpenConns(*opts.MaxOpenConnections)
	}
	if opts.MaxIdleConnections != nil {
		db.SetMaxIdleConns(*opts.MaxIdleConnections)
	}
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{DB: db, Dialect: dialect, stop: make(chan struct{}), closed: make(chan struct{}), disableQueue: opts.DisableQueue}
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
	return replacePlaceholders(q, &n)
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
	if s.Dialect != "sqlite" || s.disableQueue {
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
		lockName, e := mysqlMigrationLock(ctx, conn, "cp")
		if e != nil {
			return e
		}
		var locked int
		if err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 20)", lockName).Scan(&locked); err != nil {
			return err
		}
		if locked != 1 {
			return errors.New("migration lock unavailable")
		}
		defer conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", lockName)
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
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM cp_schema WHERE version=3").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		for _, statement := range []string{`CREATE TABLE IF NOT EXISTS cp_tasks(id VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,kind VARCHAR(32) NOT NULL,status VARCHAR(32) NOT NULL,payload TEXT NOT NULL,result TEXT NOT NULL,error TEXT NOT NULL,created_at BIGINT NOT NULL,updated_at BIGINT NOT NULL,cancel_requested INTEGER NOT NULL DEFAULT 0,claim_token VARCHAR(64) NOT NULL,payload_hash VARCHAR(64) NOT NULL,requires_admin INTEGER NOT NULL,idempotency_key VARCHAR(128) NOT NULL,UNIQUE(user_id,idempotency_key))`,
			`CREATE TABLE IF NOT EXISTS cp_task_items(task_id VARCHAR(64) NOT NULL,item_index INTEGER NOT NULL,result TEXT NOT NULL,PRIMARY KEY(task_id,item_index))`} {
			if s.Dialect == "mysql" {
				statement = strings.ReplaceAll(statement, " TEXT", " LONGTEXT")
			}
			if _, err = conn.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		if err = EnsureIndex(ctx, conn, s.Dialect, "cp_tasks", "cp_tasks_user", "user_id,status,created_at", false); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO cp_schema(version) VALUES(3)"); err != nil {
			return err
		}
	}

	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM cp_schema WHERE version=4").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		for _, idx := range []struct{ table, name, columns string }{
			{"cp_exits", "cp_exits_group", "group_id,id"},
			{"cp_diagnostics", "cp_diagnostics_due", "node_id,status,created_at"},
			{"cp_captchas", "cp_captcha_expiry", "expires_at"},
		} {
			if err = EnsureIndex(ctx, conn, s.Dialect, idx.table, idx.name, idx.columns, false); err != nil {
				return err
			}
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO cp_schema(version) VALUES(4)"); err != nil {
			return err
		}
	}

	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS cp_terminal_commands(id VARCHAR(64) PRIMARY KEY,operation_id VARCHAR(64) NOT NULL,command TEXT NOT NULL,created_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS cp_operation_access(token_hash VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,node_id VARCHAR(64) NOT NULL,session_hash VARCHAR(64) NOT NULL,expires_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS cp_node_operations(id VARCHAR(64) PRIMARY KEY,node_id VARCHAR(64) NOT NULL,user_id VARCHAR(64) NOT NULL,kind VARCHAR(32) NOT NULL,status VARCHAR(32) NOT NULL,payload TEXT NOT NULL,payload_hash VARCHAR(64) NOT NULL,claim_token VARCHAR(64) NOT NULL,access_hash VARCHAR(64) NOT NULL,idempotency_key VARCHAR(128) NOT NULL,error TEXT NOT NULL,created_at BIGINT NOT NULL,updated_at BIGINT NOT NULL,expires_at BIGINT NOT NULL,UNIQUE(user_id,idempotency_key))`,
	} {
		if s.Dialect == "mysql" {
			statement = strings.ReplaceAll(statement, " TEXT", " LONGTEXT")
		}
		if _, err = conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if err = EnsureIndex(ctx, conn, s.Dialect, "cp_node_operations", "cp_node_operations_pending", "node_id,status,created_at", false); err != nil {
		return err
	}
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM cp_schema WHERE version=4").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err = conn.ExecContext(ctx, "INSERT INTO cp_schema(version) VALUES(4)"); err != nil {
			return err
		}
	}
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM cp_schema WHERE version=5").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		// 设备组的固定接入密钥。明文存，与 commerce_webhook_subscriptions.secret
		// 同一理由：面板必须能把它再次展示给运营方，存哈希就取不回来了。
		// 它不是节点身份 —— 只是一张「允许接入本组」的共享口令，轮换一次
		// 已分发出去的命令全部失效。
		if _, err = conn.ExecContext(ctx, "ALTER TABLE cp_groups ADD COLUMN join_key VARCHAR(64) NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err = s.backfillGroupJoinKeys(ctx, conn); err != nil {
			return err
		}
		if err = EnsureIndex(ctx, conn, s.Dialect, "cp_groups", "cp_groups_join", "join_key", true); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO cp_schema(version) VALUES(5)"); err != nil {
			return err
		}
	}
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM cp_schema WHERE version=6").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		// 网络诊断（LookingGlass）。与 cp_diagnostics 同构：pending → running
		// → done/failed，带一次性认领令牌与抢占窗口。
		if _, err = conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS cp_looking_glass(id VARCHAR(64) PRIMARY KEY,node_id VARCHAR(64) NOT NULL,user_id VARCHAR(64) NOT NULL,method VARCHAR(16) NOT NULL,target VARCHAR(300) NOT NULL,status VARCHAR(16) NOT NULL,claim_token VARCHAR(64) NOT NULL,payload TEXT NOT NULL,created_at BIGINT NOT NULL,claimed_at BIGINT NOT NULL,finished_at BIGINT NOT NULL)"); err != nil {
			return err
		}
		if err = EnsureIndex(ctx, conn, s.Dialect, "cp_looking_glass", "cp_looking_glass_pending", "node_id,status,created_at", false); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO cp_schema(version) VALUES(6)"); err != nil {
			return err
		}
	}
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM cp_schema WHERE version=7").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		// API Token 的运营侧可见信息。凭据本身仍然只存摘要，这几列让管理员
		// 看得出「这把钥匙是谁的、什么时候发的、最近用过没有」，从而判断该
		// 重置哪一条 —— 没有它们，重置就只剩下「全部删掉重发」。
		for _, column := range []struct{ name, definition string }{
			{"prefix", "VARCHAR(16) NOT NULL DEFAULT ''"},
			{"created_at", "BIGINT NOT NULL DEFAULT 0"},
			{"last_used_at", "BIGINT NOT NULL DEFAULT 0"},
		} {
			if err = EnsureColumn(ctx, conn, s.Dialect, "cp_tokens", column.name, column.definition); err != nil {
				return err
			}
		}
		if err = EnsureIndex(ctx, conn, s.Dialect, "cp_tokens", "cp_tokens_user", "user_id,id", false); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO cp_schema(version) VALUES(7)"); err != nil {
			return err
		}
	}
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM cp_schema WHERE version=8").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if err = EnsureColumn(ctx, conn, s.Dialect, "cp_users", "identity_group_id", "VARCHAR(64) NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		for _, statement := range []string{
			`CREATE TABLE IF NOT EXISTS cp_identity_groups(id VARCHAR(64) PRIMARY KEY,name VARCHAR(190) NOT NULL UNIQUE)`,
			`CREATE TABLE IF NOT EXISTS cp_group_identity_groups(group_id VARCHAR(64) NOT NULL,identity_group_id VARCHAR(64) NOT NULL,PRIMARY KEY(group_id,identity_group_id),FOREIGN KEY(group_id) REFERENCES cp_groups(id),FOREIGN KEY(identity_group_id) REFERENCES cp_identity_groups(id))`,
		} {
			if _, err = conn.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		if err = s.backfillIdentityGroups(ctx, conn); err != nil {
			return err
		}
		if err = EnsureIndex(ctx, conn, s.Dialect, "cp_users", "cp_users_identity_group", "identity_group_id,id", false); err != nil {
			return err
		}
		if err = EnsureIndex(ctx, conn, s.Dialect, "cp_group_identity_groups", "cp_gig_identity", "identity_group_id,group_id", false); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO cp_schema(version) VALUES(8)"); err != nil {
			return err
		}
	}
	if s.Dialect == "sqlite" {
		_, err = conn.ExecContext(ctx, "COMMIT")
	}
	return err
}

// backfillIdentityGroups preserves the old per-user grants by assigning each
// existing user a deterministic identity group and copying every grant to it.
func (s *Store) backfillIdentityGroups(ctx context.Context, conn interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}) error {
	rows, err := conn.QueryContext(ctx, "SELECT id,username,identity_group_id FROM cp_users")
	if err != nil {
		return err
	}
	type legacyUser struct{ id, username, group string }
	users := []legacyUser{}
	byUser := map[string]string{}
	for rows.Next() {
		var user legacyUser
		if err = rows.Scan(&user.id, &user.username, &user.group); err != nil {
			rows.Close()
			return err
		}
		if user.group == "" {
			user.group = "legacy-" + user.id
		}
		users = append(users, user)
		byUser[user.id] = user.group
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, user := range users {
		var exists int
		if err = conn.QueryRowContext(ctx, s.Rebind("SELECT COUNT(*) FROM cp_identity_groups WHERE id=?"), user.group).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err = conn.ExecContext(ctx, s.Rebind("INSERT INTO cp_identity_groups(id,name) VALUES(?,?)"), user.group, user.username); err != nil {
				return err
			}
		}
		if _, err = conn.ExecContext(ctx, s.Rebind("UPDATE cp_users SET identity_group_id=? WHERE id=?"), user.group, user.id); err != nil {
			return err
		}
	}
	rows, err = conn.QueryContext(ctx, "SELECT group_id,user_id FROM cp_group_users")
	if err != nil {
		return err
	}
	type legacyGrant struct{ deviceGroup, user string }
	grants := []legacyGrant{}
	for rows.Next() {
		var grant legacyGrant
		if err = rows.Scan(&grant.deviceGroup, &grant.user); err != nil {
			rows.Close()
			return err
		}
		grants = append(grants, grant)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, grant := range grants {
		identity := byUser[grant.user]
		if identity == "" {
			continue
		}
		var exists int
		if err = conn.QueryRowContext(ctx, s.Rebind("SELECT COUNT(*) FROM cp_group_identity_groups WHERE group_id=? AND identity_group_id=?"), grant.deviceGroup, identity).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err = conn.ExecContext(ctx, s.Rebind("INSERT INTO cp_group_identity_groups(group_id,identity_group_id) VALUES(?,?)"), grant.deviceGroup, identity); err != nil {
				return err
			}
		}
	}
	return nil
}

// backfillGroupJoinKeys 给升级前就存在的设备组各补一把接入密钥。少了这一步，
// 那些组在控制台上就没有可复制的接入命令。
func (s *Store) backfillGroupJoinKeys(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, "SELECT id FROM cp_groups WHERE join_key='' OR join_key IS NULL")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var group string
		if err = rows.Scan(&group); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, group)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	// SQLite 在同一个连接上不能一边开着 rows 一边写，先把 id 收完再更新。
	rows.Close()
	for _, group := range ids {
		key, err := RandomKey()
		if err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, s.Rebind("UPDATE cp_groups SET join_key=? WHERE id=?"), key, group); err != nil {
			return err
		}
	}
	return nil
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS cp_diagnostics(id VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,node_id VARCHAR(64) NOT NULL,rule_id VARCHAR(64) NOT NULL,payload TEXT NOT NULL,status VARCHAR(16) NOT NULL,claim_token VARCHAR(64) NOT NULL,created_at BIGINT NOT NULL,claimed_at BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cp_site_settings(id INTEGER PRIMARY KEY,payload TEXT NOT NULL,version BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cp_captchas(id VARCHAR(64) PRIMARY KEY,answer_hash VARCHAR(64) NOT NULL,expires_at BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cp_registration_invites(token_hash VARCHAR(64) PRIMARY KEY,expires_at BIGINT NOT NULL,used_by VARCHAR(64) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cp_exits(id VARCHAR(64) PRIMARY KEY,group_id VARCHAR(64) NOT NULL,node_id VARCHAR(64) NOT NULL,payload TEXT NOT NULL,version BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cp_schema(version INTEGER PRIMARY KEY)`,
	`CREATE TABLE IF NOT EXISTS cp_identity_groups(id VARCHAR(64) PRIMARY KEY,name VARCHAR(190) NOT NULL UNIQUE)`,
	`CREATE TABLE IF NOT EXISTS cp_users(id VARCHAR(64) PRIMARY KEY, username VARCHAR(190) NOT NULL UNIQUE, password_hash VARCHAR(190) NOT NULL, role VARCHAR(32) NOT NULL, identity_group_id VARCHAR(64) NOT NULL DEFAULT '', disabled INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS cp_sessions(token_hash VARCHAR(64) PRIMARY KEY, user_id VARCHAR(64) NOT NULL, expires_at BIGINT NOT NULL, FOREIGN KEY(user_id) REFERENCES cp_users(id))`,
	`CREATE TABLE IF NOT EXISTS cp_tokens(id VARCHAR(64) PRIMARY KEY,token_hash VARCHAR(64) NOT NULL UNIQUE,user_id VARCHAR(64) NOT NULL,name VARCHAR(190) NOT NULL,expires_at BIGINT NOT NULL,prefix VARCHAR(16) NOT NULL DEFAULT '',created_at BIGINT NOT NULL DEFAULT 0,last_used_at BIGINT NOT NULL DEFAULT 0,FOREIGN KEY(user_id) REFERENCES cp_users(id))`,
	`CREATE TABLE IF NOT EXISTS cp_groups(id VARCHAR(64) PRIMARY KEY, name VARCHAR(190) NOT NULL, payload TEXT NOT NULL, version BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cp_group_users(group_id VARCHAR(64) NOT NULL, user_id VARCHAR(64) NOT NULL, PRIMARY KEY(group_id,user_id), FOREIGN KEY(group_id) REFERENCES cp_groups(id), FOREIGN KEY(user_id) REFERENCES cp_users(id))`,
	`CREATE TABLE IF NOT EXISTS cp_group_identity_groups(group_id VARCHAR(64) NOT NULL,identity_group_id VARCHAR(64) NOT NULL,PRIMARY KEY(group_id,identity_group_id),FOREIGN KEY(group_id) REFERENCES cp_groups(id),FOREIGN KEY(identity_group_id) REFERENCES cp_identity_groups(id))`,
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
	{"cp_gig_identity", "cp_group_identity_groups", "identity_group_id,group_id"},
	{"cp_ng_group", "cp_node_groups", "group_id,node_id"},
	{"cp_ports_rule", "cp_ports", "rule_id"},
	{"cp_usage_rule", "cp_usage", "rule_id,received_at"},
	{"cp_usage_settled", "cp_usage", "settled,received_at"},
	{"cp_audit_time", "cp_audit", "created_at,id"},
	{"cp_leases_rule", "cp_rule_leases", "rule_id,expires_at"},
}
