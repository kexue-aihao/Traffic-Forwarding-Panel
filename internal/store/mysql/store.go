package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"trafficpanel/internal/domain"
	"trafficpanel/internal/security"

	_ "github.com/go-sql-driver/mysql"
)

type Store struct {
	db *sql.DB
}

type Summary struct {
	Admins       int64 `json:"admins"`
	Users        int64 `json:"users"`
	Nodes        int64 `json:"nodes"`
	Tunnels      int64 `json:"tunnels"`
	Services     int64 `json:"services"`
	Orders       int64 `json:"orders"`
	RevenueCents int64 `json:"revenue_cents"`
}

type UsageResult struct {
	Service domain.ForwardService `json:"service"`
	Tunnel  domain.Tunnel         `json:"tunnel"`
	User    domain.User           `json:"user"`
	Paused  bool                  `json:"paused"`
	Reason  string                `json:"reason"`
}

func Open(dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetConnMaxLifetime(2 * time.Hour)
	db.SetMaxIdleConns(10)
	db.SetMaxOpenConns(50)
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) EnqueueNodeCommand(ctx context.Context, nodeID int64, typ domain.CommandType, payload any, availableAt time.Time) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `INSERT INTO node_commands(node_id, type, payload_json, status, available_at, created_at) VALUES(?,?,?,?,?,?)`,
		nodeID, typ, string(raw), "pending", availableAt, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) PendingNodeCommands(ctx context.Context, nodeID int64, limit int) ([]domain.NodeCommand, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, node_id, type, payload_json, status, available_at, consumed_at, created_at FROM node_commands WHERE node_id = ? AND status = ? AND available_at <= ? ORDER BY id ASC LIMIT ?`,
		nodeID, "pending", time.Now().UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.NodeCommand, 0, limit)
	for rows.Next() {
		var item domain.NodeCommand
		if err := rows.Scan(&item.ID, &item.NodeID, &item.Type, &item.PayloadJSON, &item.Status, &item.AvailableAt, &item.ConsumedAt, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ConsumeNodeCommands(ctx context.Context, nodeID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now().UTC()
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		_ = id
	}
	stmt := fmt.Sprintf(`UPDATE node_commands SET status = ?, consumed_at = ? WHERE node_id = ? AND id IN (%s)`, strings.Join(placeholders, ","))
	args := make([]any, 0, len(ids)+3)
	args = append(args, "sent", now, nodeID)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, stmt, args...)
	return err
}

func (s *Store) EnsureSchema(ctx context.Context) error {
	for _, stmt := range schemaStatements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("schema statement failed: %w", err)
		}
	}
	return nil
}

func (s *Store) BootstrapAdmin(ctx context.Context, username, password string) error {
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM admins`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `INSERT INTO admins(username, password_hash, status, created_at, updated_at) VALUES(?,?,?,?,?)`, username, hash, domain.StatusEnabled, now, now)
	return err
}

func (s *Store) Stats(ctx context.Context) (Summary, error) {
	var out Summary
	query := `SELECT
		(SELECT COUNT(1) FROM admins),
		(SELECT COUNT(1) FROM users),
		(SELECT COUNT(1) FROM nodes),
		(SELECT COUNT(1) FROM tunnels),
		(SELECT COUNT(1) FROM forward_services),
		(SELECT COUNT(1) FROM payment_orders),
		COALESCE((SELECT SUM(amount_cents) FROM payment_orders WHERE status = ?), 0)`
	err := s.db.QueryRowContext(ctx, query, domain.PaymentPaid).Scan(&out.Admins, &out.Users, &out.Nodes, &out.Tunnels, &out.Services, &out.Orders, &out.RevenueCents)
	return out, err
}

func (s *Store) CreateSession(ctx context.Context, actorKind domain.ActorKind, actorID int64, ttl time.Duration) (string, domain.Session, error) {
	token, err := security.RandomToken(32)
	if err != nil {
		return "", domain.Session{}, err
	}
	now := time.Now().UTC()
	session := domain.Session{
		ActorKind: actorKind,
		ActorID:   actorID,
		TokenHash: security.TokenHash(token),
		ExpiresAt: now.Add(ttl),
		CreatedAt: now,
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO sessions(actor_kind, actor_id, token_hash, expires_at, created_at) VALUES(?,?,?,?,?)`,
		session.ActorKind, session.ActorID, session.TokenHash, session.ExpiresAt, session.CreatedAt)
	if err != nil {
		return "", domain.Session{}, err
	}
	return token, session, nil
}

func (s *Store) GetSessionByToken(ctx context.Context, token string) (*domain.Session, error) {
	hash := security.TokenHash(token)
	row := s.db.QueryRowContext(ctx, `SELECT id, actor_kind, actor_id, token_hash, expires_at, created_at FROM sessions WHERE token_hash = ?`, hash)
	var session domain.Session
	err := row.Scan(&session.ID, &session.ActorKind, &session.ActorID, &session.TokenHash, &session.ExpiresAt, &session.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func (s *Store) DeleteSessionByToken(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, security.TokenHash(token))
	return err
}

func (s *Store) GetNodeByID(ctx context.Context, nodeID int64) (*domain.Node, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, host, port, secret, status, last_seen_at, created_at, updated_at FROM nodes WHERE id = ?`, nodeID)
	var node domain.Node
	var lastSeen sql.NullTime
	if err := row.Scan(&node.ID, &node.Name, &node.Host, &node.Port, &node.Secret, &node.Status, &lastSeen, &node.CreatedAt, &node.UpdatedAt); err != nil {
		return nil, err
	}
	if lastSeen.Valid {
		node.LastSeenAt = &lastSeen.Time
	}
	return &node, nil
}

func (s *Store) TouchNode(ctx context.Context, nodeID int64, status domain.NodeStatus) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET status = ?, last_seen_at = ?, updated_at = ? WHERE id = ?`, status, now, now, nodeID)
	return err
}

func (s *Store) recordAudit(ctx context.Context, actorKind domain.ActorKind, actorID int64, action, targetKind string, targetID int64, detail any) {
	payload, _ := json.Marshal(detail)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO audit_logs(actor_kind, actor_id, action, target_kind, target_id, detail_json, created_at) VALUES(?,?,?,?,?,?,?)`,
		actorKind, actorID, action, targetKind, targetID, string(payload), time.Now().UTC())
}
