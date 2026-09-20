package storage

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Backup contains sensitive credentials and financial facts. The operator must
// protect this output like the source database. No browser download is exposed.
type backupRecord struct {
	Format  int      `json:"format,omitempty"`
	Dialect string   `json:"dialect,omitempty"`
	Table   string   `json:"table,omitempty"`
	Columns []string `json:"columns,omitempty"`
	Values  []any    `json:"values,omitempty"`
	End     bool     `json:"end,omitempty"`
	Rows    int64    `json:"rows,omitempty"`
}

func (s *Store) tables(ctx context.Context) ([]string, error) {
	query := "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'"
	if s.Dialect == "postgres" {
		query = "SELECT tablename FROM pg_tables WHERE schemaname=current_schema()"
	}
	if s.Dialect == "mysql" {
		query = "SELECT table_name FROM information_schema.tables WHERE table_schema=DATABASE() AND table_type='BASE TABLE'"
	}
	rows, e := s.DB.QueryContext(ctx, query)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if e = rows.Scan(&name); e != nil {
			return nil, e
		}
		if !sqlIdentifier.MatchString(name) {
			return nil, errors.New("unsupported database identifier")
		}
		if strings.HasPrefix(name, "cp_") || strings.HasPrefix(name, "commerce_") {
			out = append(out, name)
		}
	}
	// Dependency order for foreign keys; remaining commerce tables use stable IDs.
	rank := map[string]int{"cp_users": 0, "cp_groups": 1, "cp_nodes": 2, "cp_rules": 3, "cp_group_users": 4, "cp_node_groups": 5, "cp_ports": 6, "cp_sessions": 7, "cp_tokens": 8}
	sort.Slice(out, func(i, j int) bool {
		a, ok := rank[out[i]]
		if !ok {
			a = 100
		}
		b, ok := rank[out[j]]
		if !ok {
			b = 100
		}
		if a == b {
			return out[i] < out[j]
		}
		return a < b
	})
	return out, rows.Err()
}

// Export streams a consistent SQL snapshot as JSON Lines with exact integers.
func (s *Store) Export(ctx context.Context, dst io.Writer) error {
	tables, err := s.tables(ctx)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	writer := bufio.NewWriter(dst)
	enc := json.NewEncoder(writer)
	if err = enc.Encode(backupRecord{Format: 1, Dialect: s.Dialect}); err != nil {
		return err
	}
	var count int64
	for _, table := range tables {
		if strings.HasSuffix(table, "_schema") {
			continue
		}
		rows, e := tx.QueryContext(ctx, "SELECT * FROM "+table)
		if e != nil {
			return e
		}
		columns, e := rows.Columns()
		if e != nil {
			rows.Close()
			return e
		}
		if e = enc.Encode(backupRecord{Table: table, Columns: columns}); e != nil {
			rows.Close()
			return e
		}
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if e = rows.Scan(dest...); e != nil {
				rows.Close()
				return e
			}
			for i, v := range values {
				if b, ok := v.([]byte); ok {
					values[i] = string(b)
				}
			}
			if e = enc.Encode(backupRecord{Table: table, Values: values}); e != nil {
				rows.Close()
				return e
			}
			count++
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = enc.Encode(backupRecord{End: true, Rows: count}); err != nil {
		return err
	}
	return writer.Flush()
}

// Import only accepts an initialized, empty target. Existing business records
// are never replaced. The complete restore is transactional on every backend.
func (s *Store) Import(ctx context.Context, src io.Reader) error {
	tables, err := s.tables(ctx)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, table := range tables {
		if !strings.HasSuffix(table, "_schema") {
			allowed[table] = true
		}
	}
	return s.Write(ctx, Critical, func(tx *sql.Tx) error {
		for table := range allowed {
			var n int64
			if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); e != nil {
				return e
			}
			if n != 0 {
				return errors.New("restore requires an empty initialized database")
			}
		}
		decoder := json.NewDecoder(bufio.NewReader(src))
		decoder.UseNumber()
		decoder.DisallowUnknownFields()
		var header backupRecord
		if e := decoder.Decode(&header); e != nil {
			return e
		}
		if header.Format != 1 {
			return errors.New("unsupported backup version")
		}
		var table string
		var columns []string
		var count int64
		seen := map[string]bool{}
		for {
			var r backupRecord
			if e := decoder.Decode(&r); e != nil {
				return fmt.Errorf("incomplete backup: %w", e)
			}
			if r.End {
				if r.Rows != count {
					return errors.New("backup record count mismatch")
				}
				if decoder.Decode(new(any)) != io.EOF {
					return errors.New("trailing backup data")
				}
				return nil
			}
			if !allowed[r.Table] {
				return errors.New("unknown backup table")
			}
			if len(r.Columns) > 0 {
				if seen[r.Table] {
					return errors.New("duplicate backup table header")
				}
				seen[r.Table] = true
				for _, col := range r.Columns {
					if !sqlIdentifier.MatchString(col) {
						return errors.New("invalid backup column")
					}
				}
				table = r.Table
				columns = r.Columns
				continue
			}
			if table != r.Table || len(r.Values) != len(columns) || len(columns) == 0 {
				return errors.New("invalid backup row")
			}
			for i, v := range r.Values {
				if n, ok := v.(json.Number); ok {
					number, e := strconv.ParseInt(string(n), 10, 64)
					if e != nil {
						return e
					}
					r.Values[i] = number
				} else {
					switch v.(type) {
					case nil, string, bool:
					default:
						return errors.New("unsupported backup value")
					}
				}
			}
			placeholders := strings.TrimRight(strings.Repeat("?,", len(columns)), ",")
			if _, e := tx.ExecContext(ctx, s.Rebind("INSERT INTO "+table+"("+strings.Join(columns, ",")+") VALUES("+placeholders+")"), r.Values...); e != nil {
				return e
			}
			count++
		}
	})
}
