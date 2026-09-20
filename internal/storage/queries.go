package storage

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
)

type Row interface{ Scan(...any) error }
type failedRow struct{ err error }

func (r failedRow) Scan(...any) error { return r.err }

// Queries prepares each distinct statement once per transaction. This avoids
// repeated SQL compilation in node batches without caching transactions globally.
type Queries struct {
	tx         *sql.Tx
	statements map[string]*sql.Stmt
}

func NewQueries(tx *sql.Tx) *Queries { return &Queries{tx: tx, statements: map[string]*sql.Stmt{}} }
func (q *Queries) prepare(ctx context.Context, query string) (*sql.Stmt, error) {
	if stmt, ok := q.statements[query]; ok {
		return stmt, nil
	}
	stmt, err := q.tx.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	q.statements[query] = stmt
	return stmt, nil
}
func (q *Queries) Row(ctx context.Context, query string, args ...any) Row {
	stmt, err := q.prepare(ctx, query)
	if err != nil {
		return failedRow{err}
	}
	return stmt.QueryRowContext(ctx, args...)
}
func (q *Queries) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	stmt, err := q.prepare(ctx, query)
	if err != nil {
		return nil, err
	}
	return stmt.ExecContext(ctx, args...)
}
func (q *Queries) Close() {
	for _, stmt := range q.statements {
		stmt.Close()
	}
}

// Bulk helpers cap placeholders below SQLite's historical 999 parameter limit.
// Identifiers and predicates must be compile-time application SQL, never input.
func Placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
func BulkInsert(ctx context.Context, tx *sql.Tx, rebind func(string) string, table, columns string, rows [][]any) error {
	for start := 0; start < len(rows); start += 100 {
		end := start + 100
		if end > len(rows) {
			end = len(rows)
		}
		width := len(rows[start])
		values := make([]string, 0, end-start)
		args := []any{}
		for _, row := range rows[start:end] {
			if len(row) != width {
				return errors.New("bulk row width mismatch")
			}
			values = append(values, "("+Placeholders(width)+")")
			args = append(args, row...)
		}
		if _, e := tx.ExecContext(ctx, rebind("INSERT INTO "+table+"("+columns+") VALUES "+strings.Join(values, ",")), args...); e != nil {
			return e
		}
	}
	return nil
}

type CounterChange struct {
	ID            string
	Before, Delta int64
}

// BulkCounter uses compare-and-swap when cas=true; every positive change must
// match, otherwise callers roll back the complete batch (including prior chunks).
func BulkCounter(ctx context.Context, tx *sql.Tx, rebind func(string) string, table, column, guard string, changes []CounterChange, cas bool) error {
	sort.Slice(changes, func(i, j int) bool { return changes[i].ID < changes[j].ID })
	for start := 0; start < len(changes); start += 100 {
		end := start + 100
		if end > len(changes) {
			end = len(changes)
		}
		args := []any{}
		var cases, where []string
		for _, c := range changes[start:end] {
			cases = append(cases, "WHEN ? THEN ?")
			args = append(args, c.ID, c.Delta)
		}
		for _, c := range changes[start:end] {
			if cas {
				where = append(where, "(id=? AND "+column+"=?)")
				args = append(args, c.ID, c.Before)
			} else {
				where = append(where, "id=?")
				args = append(args, c.ID)
			}
		}
		query := "UPDATE " + table + " SET " + column + "=" + column + "+CASE id " + strings.Join(cases, " ") + " ELSE 0 END WHERE (" + strings.Join(where, " OR ") + ")"
		if guard != "" {
			query += " AND (" + guard + ")"
		}
		res, e := tx.ExecContext(ctx, rebind(query), args...)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n != int64(end-start) {
			return errors.New("bulk counter conflict")
		}
	}
	return nil
}
