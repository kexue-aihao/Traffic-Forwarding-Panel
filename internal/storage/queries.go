package storage

import (
	"context"
	"database/sql"
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
