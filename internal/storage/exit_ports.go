package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func (s *Store) backfillExitPorts(ctx context.Context, conn interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}) error {
	rows, e := conn.QueryContext(ctx, "SELECT id,node_id,payload FROM cp_exits")
	if e != nil {
		return e
	}
	type reservation struct {
		node, network string
		port          int
		exit          string
	}
	items := []reservation{}
	for rows.Next() {
		var id, node, raw string
		var exit contract.Exit
		if e := rows.Scan(&id, &node, &raw); e != nil {
			rows.Close()
			return e
		}
		if e := json.Unmarshal([]byte(raw), &exit); e != nil {
			rows.Close()
			return fmt.Errorf("invalid legacy exit %s", id)
		}
		ports, e := contract.ExitListeningPorts(exit)
		if e != nil {
			rows.Close()
			return fmt.Errorf("legacy exit %s: %w", id, e)
		}
		for network, port := range ports {
			items = append(items, reservation{node, network, port, id})
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	query := "INSERT INTO cp_exit_ports(node_id,network,port,exit_id) VALUES(?,?,?,?) ON CONFLICT DO NOTHING"
	if s.Dialect == "mysql" {
		query = "INSERT IGNORE INTO cp_exit_ports(node_id,network,port,exit_id) VALUES(?,?,?,?)"
	}
	// Preserve all owners of legacy shared ports. Removing one exit must not
	// accidentally release another's reservation; new conflicts are refused.
	for _, item := range items {
		if _, e = conn.ExecContext(ctx, s.Rebind(query), item.node, item.network, item.port, item.exit); e != nil {
			return e
		}
	}
	return nil
}
