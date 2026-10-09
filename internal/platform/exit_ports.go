package platform

import (
	"context"
	"database/sql"
	"errors"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func (s *Server) reserveExitPorts(ctx context.Context, tx *sql.Tx, e contract.Exit) error {
	// Serialize with rule allocation on the physical node (IP-independent).
	query := "SELECT id FROM cp_nodes WHERE id=?"
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var node string
	if err := tx.QueryRowContext(ctx, s.q(query), e.NodeID).Scan(&node); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.q("DELETE FROM cp_exit_ports WHERE exit_id=?"), e.ID); err != nil {
		return err
	}
	ports, err := contract.ExitListeningPorts(e)
	if err != nil {
		return err
	}
	for network, port := range ports {
		var count int
		if err = tx.QueryRowContext(ctx, s.q("SELECT (SELECT COUNT(*) FROM cp_ports WHERE node_id=? AND network=? AND port=?)+(SELECT COUNT(*) FROM cp_exit_ports WHERE node_id=? AND network=? AND port=?)"), e.NodeID, network, port, e.NodeID, network, port).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return errors.New("physical exit port already reserved")
		}
		if _, err = tx.ExecContext(ctx, s.q("INSERT INTO cp_exit_ports(node_id,network,port,exit_id) VALUES(?,?,?,?)"), e.NodeID, network, port, e.ID); err != nil {
			return errors.New("physical exit port already reserved")
		}
	}
	return nil
}
