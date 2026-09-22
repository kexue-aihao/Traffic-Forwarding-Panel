package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var sqlIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// MigrateNamespace executes restartable DDL on a dedicated connection with a
// cross-process lock. MySQL DDL commits implicitly: each migration must therefore
// be idempotent, and its version is recorded only after every statement succeeds.
func MigrateNamespace(ctx context.Context, db *sql.DB, dialect, namespace string, version int, apply func(*sql.Conn) error) error {
	if !sqlIdentifier.MatchString(namespace) || version < 1 {
		return errors.New("invalid migration namespace or version")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	switch dialect {
	case "sqlite":
		if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		defer conn.ExecContext(context.Background(), "ROLLBACK")
	case "postgres":
		if _, err = conn.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext($1))", "tfp_"+namespace); err != nil {
			return err
		}
		defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", "tfp_"+namespace)
		if _, err = conn.ExecContext(ctx, "BEGIN"); err != nil {
			return err
		}
		defer conn.ExecContext(context.Background(), "ROLLBACK")
	case "mysql":
		lockName, e := mysqlMigrationLock(ctx, conn, namespace)
		if e != nil {
			return e
		}
		var locked int
		if err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?,30)", lockName).Scan(&locked); err != nil {
			return err
		}
		if locked != 1 {
			return errors.New("migration lock unavailable")
		}
		defer conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", lockName)
	default:
		return errors.New("unsupported migration dialect")
	}
	if _, err = conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+namespace+"_schema(version INTEGER PRIMARY KEY)"); err != nil {
		return err
	}
	var current int
	if err = conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM "+namespace+"_schema").Scan(&current); err != nil {
		return err
	}
	if current > version {
		return errors.New("database schema is newer than this binary")
	}
	if current < version {
		if err = apply(conn); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO "+namespace+"_schema(version) VALUES("+strconv.Itoa(version)+")"); err != nil {
			return err
		}
	}
	if dialect != "mysql" {
		_, err = conn.ExecContext(ctx, "COMMIT")
	}
	return err
}

// RandomKey 生成 32 字节十六进制密钥 —— 与节点凭据、一次性接入令牌同一强度。
func RandomKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func EnsureIndex(ctx context.Context, conn *sql.Conn, dialect, table, name, columns string, unique bool) error {
	if !sqlIdentifier.MatchString(table) || !sqlIdentifier.MatchString(name) {
		return errors.New("invalid index identifier")
	}
	for _, column := range strings.Split(columns, ",") {
		if !sqlIdentifier.MatchString(column) {
			return errors.New("invalid index column")
		}
	}
	if dialect == "mysql" {
		var exists int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=?", table, name).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return nil
		}
	}
	query := "CREATE "
	if unique {
		query += "UNIQUE "
	}
	query += "INDEX "
	if dialect != "mysql" {
		query += "IF NOT EXISTS "
	}
	query += name + " ON " + table + "(" + columns + ")"
	_, err := conn.ExecContext(ctx, query)
	return err
}

// EnsureColumn 幂等地给已有表补一列。
//
// 与 EnsureIndex 同一理由：MySQL 的 DDL 自动提交，迁移中途失败后重跑会再撞上
// 同一条 ALTER。先查一遍 information_schema，重跑就是安全的。
func EnsureColumn(ctx context.Context, conn *sql.Conn, dialect, table, column, definition string) error {
	if !sqlIdentifier.MatchString(table) || !sqlIdentifier.MatchString(column) {
		return errors.New("invalid column identifier")
	}
	switch dialect {
	case "mysql":
		var exists int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?", table, column).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return nil
		}
	case "sqlite":
		// SQLite 不支持 ADD COLUMN IF NOT EXISTS。新建的库在建表语句里就已经
		// 带上这些列了，所以这一步在旧库上是补列，在新库上是空操作。
		rows, err := conn.QueryContext(ctx, "PRAGMA table_info("+table+")")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, kind string
			var defaultValue sql.NullString
			if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
				return err
			}
			if name == column {
				return nil
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
	}
	// Postgres 把 IF NOT EXISTS 放在 ADD COLUMN 之后，SQLite 则完全不支持 ——
	// 好在 SQLite 的迁移跑在 BEGIN IMMEDIATE 事务里，失败会整体回滚。
	query := "ALTER TABLE " + table + " ADD COLUMN "
	if dialect == "postgres" {
		query += "IF NOT EXISTS "
	}
	query += column + " " + definition
	_, err := conn.ExecContext(ctx, query)
	return err
}

// MySQL named locks are server-wide. Scope them to the selected database while
// keeping the 64-byte lock-name limit independent of database-name length.
func mysqlMigrationLock(ctx context.Context, conn *sql.Conn, namespace string) (string, error) {
	var database string
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(database + ":" + namespace))
	return "tfp_" + hex.EncodeToString(hash[:])[:56], nil
}
