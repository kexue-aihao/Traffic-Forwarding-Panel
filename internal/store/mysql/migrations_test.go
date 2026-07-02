package mysql

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSQLiteFreshMigration(t *testing.T) {
	requireCGO(t)
	ctx := context.Background()
	store := openTestSQLite(t)
	defer store.Close()

	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema second run: %v", err)
	}

	assertMigrationApplied(t, store.DB(), "000001_current_schema")
	assertSQLiteColumn(t, store.DB(), "users", "aff_balance_cents")
	assertSQLiteColumn(t, store.DB(), "tunnels", "listen_port")
	assertSQLiteColumn(t, store.DB(), "payment_orders", "metadata_json")
}

func TestSQLiteLegacyMigrationAddsColumnsAndMarksBaseline(t *testing.T) {
	requireCGO(t)
	ctx := context.Background()
	store := openTestSQLite(t)
	defer store.Close()

	mustExec(t, store.DB(), `CREATE TABLE admins (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, status TEXT NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`)
	mustExec(t, store.DB(), `CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, status TEXT NOT NULL, balance_cents INTEGER NOT NULL DEFAULT 0, flow_quota_mb INTEGER NOT NULL DEFAULT 0, traffic_used_bytes INTEGER NOT NULL DEFAULT 0, expires_at DATETIME NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`)
	mustExec(t, store.DB(), `CREATE TABLE nodes (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, host TEXT NOT NULL, port INTEGER NOT NULL, secret TEXT NOT NULL, status TEXT NOT NULL, last_seen_at DATETIME NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`)
	mustExec(t, store.DB(), `CREATE TABLE tunnels (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, node_id INTEGER NOT NULL, name TEXT NOT NULL, protocol TEXT NOT NULL, listen_addr TEXT NOT NULL, target_addr TEXT NOT NULL, max_conn INTEGER NOT NULL DEFAULT 0, speed_limit_kb INTEGER NOT NULL DEFAULT 0, quota_bytes INTEGER NOT NULL DEFAULT 0, used_bytes INTEGER NOT NULL DEFAULT 0, expires_at DATETIME NULL, auto_pause_on_limit INTEGER NOT NULL DEFAULT 1, status TEXT NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`)
	mustExec(t, store.DB(), `CREATE TABLE forward_services (id INTEGER PRIMARY KEY AUTOINCREMENT, tunnel_id INTEGER NOT NULL UNIQUE, user_id INTEGER NOT NULL, node_id INTEGER NOT NULL, service_key TEXT NOT NULL UNIQUE, protocol TEXT NOT NULL, listen_addr TEXT NOT NULL, target_addr TEXT NOT NULL, status TEXT NOT NULL, max_conn INTEGER NOT NULL DEFAULT 0, speed_limit_kb INTEGER NOT NULL DEFAULT 0, quota_bytes INTEGER NOT NULL DEFAULT 0, used_bytes INTEGER NOT NULL DEFAULT 0, bytes_in INTEGER NOT NULL DEFAULT 0, bytes_out INTEGER NOT NULL DEFAULT 0, active_conn INTEGER NOT NULL DEFAULT 0, paused_reason TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`)
	mustExec(t, store.DB(), `CREATE TABLE payment_channels (id INTEGER PRIMARY KEY AUTOINCREMENT, code TEXT NOT NULL UNIQUE, name TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, provider TEXT NOT NULL, config_json TEXT NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`)
	mustExec(t, store.DB(), `CREATE TABLE payment_orders (id INTEGER PRIMARY KEY AUTOINCREMENT, order_no TEXT NOT NULL UNIQUE, user_id INTEGER NOT NULL, channel TEXT NOT NULL, amount_cents INTEGER NOT NULL, status TEXT NOT NULL, pay_url TEXT NOT NULL DEFAULT '', trade_no TEXT NOT NULL DEFAULT '', raw_request TEXT NOT NULL, raw_notify TEXT NOT NULL, paid_at DATETIME NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`)
	mustExec(t, store.DB(), `CREATE TABLE sessions (id INTEGER PRIMARY KEY AUTOINCREMENT, actor_kind TEXT NOT NULL, actor_id INTEGER NOT NULL, token_hash TEXT NOT NULL UNIQUE, expires_at DATETIME NOT NULL, created_at DATETIME NOT NULL)`)
	mustExec(t, store.DB(), `CREATE TABLE node_commands (id INTEGER PRIMARY KEY AUTOINCREMENT, node_id INTEGER NOT NULL, type TEXT NOT NULL, payload_json TEXT NOT NULL, status TEXT NOT NULL, available_at DATETIME NOT NULL, consumed_at DATETIME NULL, created_at DATETIME NOT NULL)`)
	mustExec(t, store.DB(), `CREATE TABLE usage_reports (id INTEGER PRIMARY KEY AUTOINCREMENT, node_id INTEGER NOT NULL, service_key TEXT NOT NULL, bytes_in INTEGER NOT NULL, bytes_out INTEGER NOT NULL, active_conn INTEGER NOT NULL, reported_at DATETIME NOT NULL, created_at DATETIME NOT NULL)`)
	mustExec(t, store.DB(), `CREATE TABLE audit_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, actor_kind TEXT NOT NULL, actor_id INTEGER NOT NULL, action TEXT NOT NULL, target_kind TEXT NOT NULL, target_id INTEGER NOT NULL DEFAULT 0, detail_json TEXT NOT NULL, created_at DATETIME NOT NULL)`)

	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema legacy: %v", err)
	}

	assertMigrationApplied(t, store.DB(), "000001_current_schema")
	assertSQLiteColumn(t, store.DB(), "users", "aff_balance_cents")
	assertSQLiteColumn(t, store.DB(), "tunnels", "listen_port")
	assertSQLiteColumn(t, store.DB(), "forward_services", "config_json")
	assertSQLiteColumn(t, store.DB(), "payment_orders", "metadata_json")
	assertTableExists(t, store.DB(), "plans")
	assertTableExists(t, store.DB(), "device_groups")
}

func TestMySQLMigrationWhenConfigured(t *testing.T) {
	dsn := os.Getenv("TP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TP_TEST_MYSQL_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open MySQL: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema MySQL: %v", err)
	}
	assertMigrationApplied(t, store.DB(), "000001_current_schema")
}

func requireCGO(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" && os.Getenv("CGO_ENABLED") != "1" {
		t.Skip("go-sqlite3 needs CGO; set CGO_ENABLED=1 to run SQLite migration tests on Windows")
	}
}

func openTestSQLite(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trafficpanel.db")
	store, err := Open("sqlite:" + path)
	if err != nil {
		t.Fatalf("Open SQLite: %v", err)
	}
	return store
}

func mustExec(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("exec %s: %v", stmt, err)
	}
}

func assertMigrationApplied(t *testing.T, db *sql.DB, version string) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, version).Scan(&count); err != nil {
		t.Fatalf("query migration %s: %v", version, err)
	}
	if count != 1 {
		t.Fatalf("migration %s applied count = %d", version, count)
	}
}

func assertSQLiteColumn(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("pragma %s: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan pragma %s: %v", table, err)
		}
		if name == column {
			return
		}
	}
	t.Fatalf("missing column %s.%s", table, column)
}

func assertTableExists(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&count); err != nil {
		t.Fatalf("table exists %s: %v", table, err)
	}
	if count != 1 {
		t.Fatalf("missing table %s", table)
	}
}
