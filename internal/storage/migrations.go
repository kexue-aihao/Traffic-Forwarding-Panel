package storage

import (
	"context"
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
