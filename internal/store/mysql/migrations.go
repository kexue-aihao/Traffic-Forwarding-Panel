package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type migration struct {
	Version     string
	Description string
	Statements  []string
}

var mysqlMigrations = []migration{
	{
		Version:     "000001_current_schema",
		Description: "current MySQL schema baseline",
		Statements:  append(append([]string{}, schemaStatements...), append([]string{}, migrationStatements...)...),
	},
}

var sqliteMigrations = []migration{
	{
		Version:     "000001_current_schema",
		Description: "current SQLite schema baseline",
		Statements:  append([]string{}, sqliteSchemaStatements...),
	},
}

func (s *Store) migrate(ctx context.Context) error {
	if err := s.ensureMigrationTable(ctx); err != nil {
		return err
	}
	if err := s.markLegacyBaseline(ctx); err != nil {
		return err
	}
	migrations := mysqlMigrations
	if s.driver == "sqlite3" {
		migrations = sqliteMigrations
	}
	for _, item := range migrations {
		applied, err := s.migrationApplied(ctx, item.Version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := s.applyMigration(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureMigrationTable(ctx context.Context) error {
	stmt := `CREATE TABLE IF NOT EXISTS schema_migrations (
		version VARCHAR(64) PRIMARY KEY,
		description VARCHAR(255) NOT NULL,
		applied_at DATETIME(6) NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if s.driver == "sqlite3" {
		stmt = `CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			description TEXT NOT NULL,
			applied_at DATETIME NOT NULL
		)`
	}
	_, err := s.db.ExecContext(ctx, stmt)
	return err
}

func (s *Store) markLegacyBaseline(ctx context.Context) error {
	empty, err := s.noMigrationsApplied(ctx)
	if err != nil || !empty {
		return err
	}
	exists, err := s.tableExists(ctx, "admins")
	if err != nil || !exists {
		return err
	}
	if err := s.runLegacySchemaUpgrade(ctx); err != nil {
		return err
	}
	return s.recordMigration(ctx, "000001_current_schema", "legacy schema baseline")
}

func (s *Store) noMigrationsApplied(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations`).Scan(&count); err != nil {
		return false, err
	}
	return count == 0, nil
}

func (s *Store) tableExists(ctx context.Context, table string) (bool, error) {
	var count int
	var err error
	if s.driver == "sqlite3" {
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&count)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&count)
	}
	return count > 0, err
}

func (s *Store) migrationApplied(ctx context.Context, version string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, version).Scan(&count)
	return count > 0, err
}

func (s *Store) applyMigration(ctx context.Context, item migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	for _, stmt := range item.Statements {
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply migration %s: %w", item.Version, err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, description, applied_at) VALUES(?,?,?)`, item.Version, item.Description, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) recordMigration(ctx context.Context, version, description string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations(version, description, applied_at) VALUES(?,?,?)`, version, description, time.Now().UTC())
	return err
}

func (s *Store) runLegacySchemaUpgrade(ctx context.Context) error {
	if s.driver == "sqlite3" {
		return s.runSQLiteLegacySchemaUpgrade(ctx)
	}
	for _, stmt := range schemaStatements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("schema statement failed: %w", err)
		}
	}
	for _, migration := range columnMigrations {
		if err := s.ensureColumn(ctx, migration); err != nil {
			return err
		}
	}
	for _, stmt := range migrationStatements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration statement failed: %w", err)
		}
	}
	return nil
}

func (s *Store) runSQLiteLegacySchemaUpgrade(ctx context.Context) error {
	for _, stmt := range sqliteSchemaStatements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite schema statement failed: %w", err)
		}
	}
	for _, migration := range sqliteColumnMigrations {
		if err := s.ensureSQLiteColumn(ctx, migration); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureSQLiteColumn(ctx context.Context, migration columnMigration) error {
	exists, err := s.sqliteColumnExists(ctx, migration.Table, migration.Column)
	if err != nil || exists {
		return err
	}
	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", migration.Table, migration.Column, migration.Definition)
	_, err = s.db.ExecContext(ctx, stmt)
	if err != nil {
		return fmt.Errorf("add sqlite column %s.%s: %w", migration.Table, migration.Column, err)
	}
	return nil
}

func (s *Store) sqliteColumnExists(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
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
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
