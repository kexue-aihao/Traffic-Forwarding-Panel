package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"trafficpanel/internal/config"
	"trafficpanel/internal/domain"
	"trafficpanel/internal/payment"
	"trafficpanel/internal/security"
	"trafficpanel/internal/store/mysql"
)

type App struct {
	cfg      config.Config
	store    *mysql.Store
	payments *payment.Registry
}

func New(cfg config.Config, store *mysql.Store, payments *payment.Registry) *App {
	return &App{cfg: cfg, store: store, payments: payments}
}

func (a *App) EnsureSchema(ctx context.Context) error {
	return a.store.EnsureSchema(ctx)
}

func (a *App) Bootstrap(ctx context.Context) error {
	if err := a.store.BootstrapAdmin(ctx, a.cfg.BootstrapAdminUser, a.cfg.BootstrapAdminPass); err != nil {
		return err
	}
	return a.ensureDefaultPaymentChannels(ctx)
}

func (a *App) ResolveSession(ctx context.Context, token string) (*domain.Session, error) {
	session, err := a.store.GetSessionByToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if time.Now().UTC().After(session.ExpiresAt) {
		_ = a.store.DeleteSessionByToken(ctx, token)
		return nil, errors.New("session expired")
	}
	return session, nil
}

func (a *App) RequireSession(ctx context.Context, token string, kind domain.ActorKind) (*domain.Session, error) {
	session, err := a.ResolveSession(ctx, token)
	if err != nil {
		return nil, err
	}
	if session.ActorKind != kind {
		return nil, errors.New("insufficient permissions")
	}
	return session, nil
}

func (a *App) Logout(ctx context.Context, token string) error {
	return a.store.DeleteSessionByToken(ctx, token)
}

func (a *App) LoginAdmin(ctx context.Context, username, password string) (string, error) {
	row := a.store.DB().QueryRowContext(ctx, `SELECT id, username, password_hash, status, created_at, updated_at FROM admins WHERE username = ?`, username)
	var admin domain.Admin
	if err := row.Scan(&admin.ID, &admin.Username, &admin.PasswordHash, &admin.Status, &admin.CreatedAt, &admin.UpdatedAt); err != nil {
		return "", err
	}
	if admin.Status != domain.StatusEnabled {
		return "", errors.New("admin disabled")
	}
	if !security.VerifyPassword(admin.PasswordHash, password) {
		return "", errors.New("invalid credentials")
	}
	token, _, err := a.store.CreateSession(ctx, domain.ActorAdmin, admin.ID, a.cfg.SessionTTL)
	return token, err
}

func (a *App) LoginUser(ctx context.Context, username, password string) (string, error) {
	row := a.store.DB().QueryRowContext(ctx, `SELECT id, username, password_hash, status, created_at, updated_at FROM users WHERE username = ?`, username)
	var user domain.User
	if err := row.Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Status, &user.CreatedAt, &user.UpdatedAt); err != nil {
		return "", err
	}
	if user.Status != domain.StatusEnabled {
		return "", errors.New("user disabled")
	}
	if !security.VerifyPassword(user.PasswordHash, password) {
		return "", errors.New("invalid credentials")
	}
	token, _, err := a.store.CreateSession(ctx, domain.ActorUser, user.ID, a.cfg.SessionTTL)
	return token, err
}

func (a *App) DashboardStats(ctx context.Context) (mysql.Summary, error) {
	return a.store.Stats(ctx)
}

func (a *App) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := a.store.DB().QueryContext(ctx, `SELECT id, username, password_hash, status, balance_cents, flow_quota_mb, traffic_used_bytes, expires_at, created_at, updated_at FROM users ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		var item domain.User
		var expires sql.NullTime
		if err := rows.Scan(&item.ID, &item.Username, &item.PasswordHash, &item.Status, &item.BalanceCents, &item.FlowQuotaMB, &item.TrafficUsedBytes, &expires, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if expires.Valid {
			item.ExpiresAt = &expires.Time
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (a *App) ListNodes(ctx context.Context) ([]domain.Node, error) {
	rows, err := a.store.DB().QueryContext(ctx, `SELECT id, name, host, port, secret, status, last_seen_at, created_at, updated_at FROM nodes ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Node
	for rows.Next() {
		var item domain.Node
		var lastSeen sql.NullTime
		if err := rows.Scan(&item.ID, &item.Name, &item.Host, &item.Port, &item.Secret, &item.Status, &lastSeen, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			item.LastSeenAt = &lastSeen.Time
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (a *App) ListTunnels(ctx context.Context) ([]domain.Tunnel, error) {
	rows, err := a.store.DB().QueryContext(ctx, `SELECT id, user_id, node_id, name, protocol, listen_addr, target_addr, max_conn, speed_limit_kb, quota_bytes, used_bytes, expires_at, auto_pause_on_limit, status, created_at, updated_at FROM tunnels ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Tunnel
	for rows.Next() {
		var item domain.Tunnel
		var expires sql.NullTime
		var autoPause bool
		if err := rows.Scan(&item.ID, &item.UserID, &item.NodeID, &item.Name, &item.Protocol, &item.ListenAddr, &item.TargetAddr, &item.MaxConn, &item.SpeedLimitKB, &item.QuotaBytes, &item.UsedBytes, &expires, &autoPause, &item.Status, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.AutoPauseOnLimit = autoPause
		if expires.Valid {
			item.ExpiresAt = &expires.Time
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (a *App) ListTunnelsByUser(ctx context.Context, userID int64) ([]domain.Tunnel, error) {
	rows, err := a.store.DB().QueryContext(ctx, `SELECT id, user_id, node_id, name, protocol, listen_addr, target_addr, max_conn, speed_limit_kb, quota_bytes, used_bytes, expires_at, auto_pause_on_limit, status, created_at, updated_at FROM tunnels WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Tunnel
	for rows.Next() {
		var item domain.Tunnel
		var expires sql.NullTime
		var autoPause bool
		if err := rows.Scan(&item.ID, &item.UserID, &item.NodeID, &item.Name, &item.Protocol, &item.ListenAddr, &item.TargetAddr, &item.MaxConn, &item.SpeedLimitKB, &item.QuotaBytes, &item.UsedBytes, &expires, &autoPause, &item.Status, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.AutoPauseOnLimit = autoPause
		if expires.Valid {
			item.ExpiresAt = &expires.Time
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (a *App) ListPaymentChannels(ctx context.Context) ([]domain.PaymentChannel, error) {
	rows, err := a.store.DB().QueryContext(ctx, `SELECT id, code, name, enabled, provider, config_json, created_at, updated_at FROM payment_channels ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PaymentChannel
	for rows.Next() {
		var item domain.PaymentChannel
		if err := rows.Scan(&item.ID, &item.Code, &item.Name, &item.Enabled, &item.Provider, &item.ConfigJSON, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (a *App) ListPaymentOrders(ctx context.Context) ([]domain.PaymentOrder, error) {
	rows, err := a.store.DB().QueryContext(ctx, `SELECT id, order_no, user_id, channel, amount_cents, status, pay_url, trade_no, raw_request, raw_notify, paid_at, created_at, updated_at FROM payment_orders ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PaymentOrder
	for rows.Next() {
		var item domain.PaymentOrder
		var paidAt sql.NullTime
		if err := rows.Scan(&item.ID, &item.OrderNo, &item.UserID, &item.Channel, &item.AmountCents, &item.Status, &item.PayURL, &item.TradeNo, &item.RawRequest, &item.RawNotify, &paidAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if paidAt.Valid {
			item.PaidAt = &paidAt.Time
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (a *App) ListPaymentOrdersByUser(ctx context.Context, userID int64) ([]domain.PaymentOrder, error) {
	rows, err := a.store.DB().QueryContext(ctx, `SELECT id, order_no, user_id, channel, amount_cents, status, pay_url, trade_no, raw_request, raw_notify, paid_at, created_at, updated_at FROM payment_orders WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PaymentOrder
	for rows.Next() {
		var item domain.PaymentOrder
		var paidAt sql.NullTime
		if err := rows.Scan(&item.ID, &item.OrderNo, &item.UserID, &item.Channel, &item.AmountCents, &item.Status, &item.PayURL, &item.TradeNo, &item.RawRequest, &item.RawNotify, &paidAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if paidAt.Valid {
			item.PaidAt = &paidAt.Time
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (a *App) CreateUser(ctx context.Context, username, password string, quotaMB int64, expiresAt *time.Time) (int64, error) {
	hash, err := security.HashPassword(password)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	res, err := a.store.DB().ExecContext(ctx, `INSERT INTO users(username, password_hash, status, balance_cents, flow_quota_mb, traffic_used_bytes, expires_at, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		username, hash, domain.StatusEnabled, 0, quotaMB, 0, expiresAt, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (a *App) CreateNode(ctx context.Context, name, host string, port int, secret string) (*domain.Node, error) {
	if strings.TrimSpace(secret) == "" {
		token, err := security.RandomToken(24)
		if err != nil {
			return nil, err
		}
		secret = token
	}
	now := time.Now().UTC()
	res, err := a.store.DB().ExecContext(ctx, `INSERT INTO nodes(name, host, port, secret, status, last_seen_at, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		name, host, port, secret, domain.NodeOffline, nil, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &domain.Node{
		ID:        id,
		Name:      name,
		Host:      host,
		Port:      port,
		Secret:    secret,
		Status:    domain.NodeOffline,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func (a *App) CreateTunnel(ctx context.Context, userID, nodeID int64, name string, protocol domain.Protocol, listenAddr, targetAddr string, maxConn, speedLimitKB int, quotaBytes int64, expiresAt *time.Time, autoPause bool) (int64, string, error) {
	now := time.Now().UTC()
	tx, err := a.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, "", err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	serviceKey, keyErr := serviceKey(userID, nodeID, now)
	if keyErr != nil {
		return 0, "", keyErr
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO tunnels(user_id, node_id, name, protocol, listen_addr, target_addr, max_conn, speed_limit_kb, quota_bytes, used_bytes, expires_at, auto_pause_on_limit, status, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, userID, nodeID, name, protocol, listenAddr, targetAddr, maxConn, speedLimitKB, quotaBytes, 0, expiresAt, autoPause, domain.ServiceActive, now, now)
	if err != nil {
		return 0, "", err
	}
	tunnelID, err := res.LastInsertId()
	if err != nil {
		return 0, "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO forward_services(tunnel_id, user_id, node_id, service_key, protocol, listen_addr, target_addr, status, max_conn, speed_limit_kb, quota_bytes, used_bytes, bytes_in, bytes_out, active_conn, paused_reason, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, tunnelID, userID, nodeID, serviceKey, protocol, listenAddr, targetAddr, domain.ServiceActive, maxConn, speedLimitKB, quotaBytes, 0, 0, 0, 0, "", now, now); err != nil {
		return 0, "", err
	}
	if err = tx.Commit(); err != nil {
		return 0, "", err
	}
	payload := domain.NodeCommandPayload{
		Service: domain.ForwardService{
			TunnelID:     tunnelID,
			UserID:       userID,
			NodeID:       nodeID,
			ServiceKey:   serviceKey,
			Protocol:     protocol,
			ListenAddr:   listenAddr,
			TargetAddr:   targetAddr,
			Status:       domain.ServiceActive,
			MaxConn:      maxConn,
			SpeedLimitKB: speedLimitKB,
			QuotaBytes:   quotaBytes,
		},
	}
	if _, err = a.store.EnqueueNodeCommand(ctx, nodeID, domain.CommandUpsertService, payload, now); err != nil {
		return tunnelID, serviceKey, err
	}
	return tunnelID, serviceKey, nil
}

func (a *App) ListForwardServices(ctx context.Context) ([]domain.ForwardService, error) {
	rows, err := a.store.DB().QueryContext(ctx, `SELECT id, tunnel_id, user_id, node_id, service_key, protocol, listen_addr, target_addr, status, max_conn, speed_limit_kb, quota_bytes, used_bytes, bytes_in, bytes_out, active_conn, paused_reason, created_at, updated_at FROM forward_services ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ForwardService
	for rows.Next() {
		var item domain.ForwardService
		if err := rows.Scan(&item.ID, &item.TunnelID, &item.UserID, &item.NodeID, &item.ServiceKey, &item.Protocol, &item.ListenAddr, &item.TargetAddr, &item.Status, &item.MaxConn, &item.SpeedLimitKB, &item.QuotaBytes, &item.UsedBytes, &item.BytesIn, &item.BytesOut, &item.ActiveConn, &item.PausedReason, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (a *App) UpsertNode(ctx context.Context, node domain.Node) (int64, error) {
	now := time.Now().UTC()
	res, err := a.store.DB().ExecContext(ctx, `INSERT INTO nodes(name, host, port, secret, status, last_seen_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE host=VALUES(host), port=VALUES(port), secret=VALUES(secret), status=VALUES(status), last_seen_at=VALUES(last_seen_at), updated_at=VALUES(updated_at)`,
		node.Name, node.Host, node.Port, node.Secret, node.Status, node.LastSeenAt, now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil || id == 0 {
		row := a.store.DB().QueryRowContext(ctx, `SELECT id FROM nodes WHERE name = ?`, node.Name)
		if scanErr := row.Scan(&id); scanErr != nil {
			return 0, scanErr
		}
	}
	return id, nil
}

func (a *App) AuthenticateNodeRequest(ctx context.Context, nodeID int64, payload []byte, signature string) (*domain.Node, error) {
	node, err := a.store.GetNodeByID(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if !security.VerifyMessage(node.Secret, payload, signature) {
		return nil, errors.New("invalid node signature")
	}
	return node, nil
}

func (a *App) RegisterNodeHeartbeat(ctx context.Context, nodeID int64) error {
	return a.store.TouchNode(ctx, nodeID, domain.NodeOnline)
}

func (a *App) SyncNodeRegistration(ctx context.Context, nodeID int64, name, host string, port int) error {
	now := time.Now().UTC()
	_, err := a.store.DB().ExecContext(ctx, `UPDATE nodes SET name = ?, host = ?, port = ?, status = ?, last_seen_at = ?, updated_at = ? WHERE id = ?`,
		name, host, port, domain.NodeOnline, now, now, nodeID)
	return err
}

func (a *App) PendingNodeCommands(ctx context.Context, nodeID int64) ([]domain.NodeCommand, error) {
	return a.store.PendingNodeCommands(ctx, nodeID, 100)
}

func (a *App) AckNodeCommands(ctx context.Context, nodeID int64, ids []int64) error {
	return a.store.ConsumeNodeCommands(ctx, nodeID, ids)
}

func (a *App) SaveUsageReport(ctx context.Context, report domain.UsageReport) (*mysql.UsageResult, error) {
	tx, err := a.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var service domain.ForwardService
	var tunnel domain.Tunnel
	var user domain.User
	row := tx.QueryRowContext(ctx, `SELECT id, tunnel_id, user_id, node_id, service_key, protocol, listen_addr, target_addr, status, max_conn, speed_limit_kb, quota_bytes, used_bytes, bytes_in, bytes_out, active_conn, paused_reason, created_at, updated_at FROM forward_services WHERE service_key = ? AND node_id = ?`, report.ServiceKey, report.NodeID)
	if scanErr := row.Scan(&service.ID, &service.TunnelID, &service.UserID, &service.NodeID, &service.ServiceKey, &service.Protocol, &service.ListenAddr, &service.TargetAddr, &service.Status, &service.MaxConn, &service.SpeedLimitKB, &service.QuotaBytes, &service.UsedBytes, &service.BytesIn, &service.BytesOut, &service.ActiveConn, &service.PausedReason, &service.CreatedAt, &service.UpdatedAt); scanErr != nil {
		return nil, scanErr
	}
	service.BytesIn = report.BytesIn
	service.BytesOut = report.BytesOut
	service.ActiveConn = report.ActiveConn
	total := report.BytesIn + report.BytesOut
	service.UsedBytes += total
	row = tx.QueryRowContext(ctx, `SELECT id, user_id, node_id, name, protocol, listen_addr, target_addr, max_conn, speed_limit_kb, quota_bytes, used_bytes, expires_at, auto_pause_on_limit, status, created_at, updated_at FROM tunnels WHERE id = ?`, service.TunnelID)
	var tunnelExpires sql.NullTime
	if scanErr := row.Scan(&tunnel.ID, &tunnel.UserID, &tunnel.NodeID, &tunnel.Name, &tunnel.Protocol, &tunnel.ListenAddr, &tunnel.TargetAddr, &tunnel.MaxConn, &tunnel.SpeedLimitKB, &tunnel.QuotaBytes, &tunnel.UsedBytes, &tunnelExpires, &tunnel.AutoPauseOnLimit, &tunnel.Status, &tunnel.CreatedAt, &tunnel.UpdatedAt); scanErr != nil {
		return nil, scanErr
	}
	if tunnelExpires.Valid {
		tunnel.ExpiresAt = &tunnelExpires.Time
	}
	row = tx.QueryRowContext(ctx, `SELECT id, username, password_hash, status, balance_cents, flow_quota_mb, traffic_used_bytes, expires_at, created_at, updated_at FROM users WHERE id = ?`, service.UserID)
	var userExpires sql.NullTime
	if scanErr := row.Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Status, &user.BalanceCents, &user.FlowQuotaMB, &user.TrafficUsedBytes, &userExpires, &user.CreatedAt, &user.UpdatedAt); scanErr != nil {
		return nil, scanErr
	}
	if userExpires.Valid {
		user.ExpiresAt = &userExpires.Time
	}
	newTunnelUsed := tunnel.UsedBytes + total
	newUserUsed := user.TrafficUsedBytes + total
	paused, reason := false, ""
	now := time.Now().UTC()
	if tunnel.QuotaBytes > 0 && newTunnelUsed >= tunnel.QuotaBytes {
		paused = true
		reason = "tunnel quota exceeded"
	}
	if user.FlowQuotaMB > 0 && newUserUsed >= user.FlowQuotaMB*1024*1024 {
		paused = true
		reason = "user quota exceeded"
	}
	if tunnel.ExpiresAt != nil && tunnel.ExpiresAt.Before(now) {
		paused = true
		reason = "tunnel expired"
	}
	if user.ExpiresAt != nil && user.ExpiresAt.Before(now) {
		paused = true
		reason = "user expired"
	}
	newStatus := domain.ServiceActive
	if paused && tunnel.AutoPauseOnLimit {
		newStatus = domain.ServicePaused
	}
	if _, err = tx.ExecContext(ctx, `UPDATE forward_services SET bytes_in = bytes_in + ?, bytes_out = bytes_out + ?, used_bytes = ?, active_conn = ?, status = ?, paused_reason = ?, updated_at = ? WHERE id = ?`,
		report.BytesIn, report.BytesOut, newTunnelUsed, report.ActiveConn, newStatus, reason, now, service.ID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tunnels SET used_bytes = ?, status = ?, updated_at = ? WHERE id = ?`, newTunnelUsed, newStatus, now, tunnel.ID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET traffic_used_bytes = ?, updated_at = ? WHERE id = ?`, newUserUsed, now, user.ID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO usage_reports(node_id, service_key, bytes_in, bytes_out, active_conn, reported_at, created_at) VALUES(?,?,?,?,?,?,?)`,
		report.NodeID, report.ServiceKey, report.BytesIn, report.BytesOut, report.ActiveConn, report.ReportedAt, now); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if paused && tunnel.AutoPauseOnLimit {
		_, _ = a.store.EnqueueNodeCommand(ctx, service.NodeID, domain.CommandPauseService, domain.NodeCommandPayload{
			Service: domain.ForwardService{ServiceKey: service.ServiceKey},
			Reason:  reason,
		}, now)
	}
	return &mysql.UsageResult{Service: service, Tunnel: tunnel, User: user, Paused: paused, Reason: reason}, nil
}

func (a *App) CreatePaymentOrder(ctx context.Context, userID int64, channelCode string, amountCents int64, subject string) (*domain.PaymentOrder, error) {
	provider, ok := a.payments.Get(channelCode)
	if !ok {
		return nil, fmt.Errorf("unknown payment channel: %s", channelCode)
	}
	now := time.Now().UTC()
	orderNo, err := security.RandomToken(20)
	if err != nil {
		return nil, err
	}
	input := payment.PaymentOrderInput{
		OrderNo:     strings.ToUpper(strings.ReplaceAll(orderNo, "-", "")),
		AmountCents: amountCents,
		Subject:     subject,
		NotifyURL:   strings.TrimRight(a.cfg.BaseURL, "/") + "/api/pay/" + provider.Code() + "/notify",
		ReturnURL:   strings.TrimRight(a.cfg.BaseURL, "/") + "/app/billing",
		Extra:       map[string]string{"channel": provider.Code()},
	}
	result, err := provider.CreateOrder(ctx, input)
	if err != nil {
		return nil, err
	}
	rawReq, _ := json.Marshal(input)
	rawRes, _ := json.Marshal(result)
	_, err = a.store.DB().ExecContext(ctx, `INSERT INTO payment_orders(order_no, user_id, channel, amount_cents, status, pay_url, trade_no, raw_request, raw_notify, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, input.OrderNo, userID, provider.Code(), amountCents, domain.PaymentPending, result.PayURL, result.TradeNo, string(rawReq), string(rawRes), now, now)
	if err != nil {
		return nil, err
	}
	return &domain.PaymentOrder{
		OrderNo:     input.OrderNo,
		UserID:      userID,
		Channel:     provider.Code(),
		AmountCents: amountCents,
		Status:      domain.PaymentPending,
		PayURL:      result.PayURL,
		TradeNo:     result.TradeNo,
		RawRequest:  string(rawReq),
		RawNotify:   string(rawRes),
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func (a *App) PauseService(ctx context.Context, serviceKey string, reason string) error {
	now := time.Now().UTC()
	tx, err := a.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var service domain.ForwardService
	if err = tx.QueryRowContext(ctx, `SELECT id, tunnel_id, user_id, node_id, service_key, protocol, listen_addr, target_addr, status, max_conn, speed_limit_kb, quota_bytes, used_bytes, bytes_in, bytes_out, active_conn, paused_reason, created_at, updated_at FROM forward_services WHERE service_key = ?`, serviceKey).
		Scan(&service.ID, &service.TunnelID, &service.UserID, &service.NodeID, &service.ServiceKey, &service.Protocol, &service.ListenAddr, &service.TargetAddr, &service.Status, &service.MaxConn, &service.SpeedLimitKB, &service.QuotaBytes, &service.UsedBytes, &service.BytesIn, &service.BytesOut, &service.ActiveConn, &service.PausedReason, &service.CreatedAt, &service.UpdatedAt); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE forward_services SET status = ?, paused_reason = ?, updated_at = ? WHERE id = ?`, domain.ServicePaused, reason, now, service.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tunnels SET status = ?, updated_at = ? WHERE id = ?`, domain.ServicePaused, now, service.TunnelID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO node_commands(node_id, type, payload_json, status, available_at, created_at) VALUES(?,?,?,?,?,?)`,
		service.NodeID, domain.CommandPauseService, fmt.Sprintf(`{"service":{"service_key":"%s","tunnel_id":%d,"user_id":%d,"node_id":%d,"protocol":"%s","listen_addr":"%s","target_addr":"%s","status":"%s","max_conn":%d,"speed_limit_kb":%d,"quota_bytes":%d},"reason":%q}`,
			service.ServiceKey, service.TunnelID, service.UserID, service.NodeID, service.Protocol, service.ListenAddr, service.TargetAddr, domain.ServicePaused, service.MaxConn, service.SpeedLimitKB, service.QuotaBytes, reason), "pending", now, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) ResumeService(ctx context.Context, serviceKey string) error {
	now := time.Now().UTC()
	tx, err := a.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var service domain.ForwardService
	if err = tx.QueryRowContext(ctx, `SELECT id, tunnel_id, user_id, node_id, service_key, protocol, listen_addr, target_addr, status, max_conn, speed_limit_kb, quota_bytes, used_bytes, bytes_in, bytes_out, active_conn, paused_reason, created_at, updated_at FROM forward_services WHERE service_key = ?`, serviceKey).
		Scan(&service.ID, &service.TunnelID, &service.UserID, &service.NodeID, &service.ServiceKey, &service.Protocol, &service.ListenAddr, &service.TargetAddr, &service.Status, &service.MaxConn, &service.SpeedLimitKB, &service.QuotaBytes, &service.UsedBytes, &service.BytesIn, &service.BytesOut, &service.ActiveConn, &service.PausedReason, &service.CreatedAt, &service.UpdatedAt); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE forward_services SET status = ?, paused_reason = '', updated_at = ? WHERE id = ?`, domain.ServiceActive, now, service.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tunnels SET status = ?, updated_at = ? WHERE id = ?`, domain.ServiceActive, now, service.TunnelID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO node_commands(node_id, type, payload_json, status, available_at, created_at) VALUES(?,?,?,?,?,?)`,
		service.NodeID, domain.CommandResumeService, fmt.Sprintf(`{"service":{"service_key":"%s","tunnel_id":%d,"user_id":%d,"node_id":%d,"protocol":"%s","listen_addr":"%s","target_addr":"%s","status":"%s","max_conn":%d,"speed_limit_kb":%d,"quota_bytes":%d}}`,
			service.ServiceKey, service.TunnelID, service.UserID, service.NodeID, service.Protocol, service.ListenAddr, service.TargetAddr, domain.ServiceActive, service.MaxConn, service.SpeedLimitKB, service.QuotaBytes), "pending", now, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) DeleteService(ctx context.Context, serviceKey string) error {
	now := time.Now().UTC()
	tx, err := a.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var service domain.ForwardService
	if err = tx.QueryRowContext(ctx, `SELECT id, tunnel_id, user_id, node_id, service_key, protocol, listen_addr, target_addr, status, max_conn, speed_limit_kb, quota_bytes, used_bytes, bytes_in, bytes_out, active_conn, paused_reason, created_at, updated_at FROM forward_services WHERE service_key = ?`, serviceKey).
		Scan(&service.ID, &service.TunnelID, &service.UserID, &service.NodeID, &service.ServiceKey, &service.Protocol, &service.ListenAddr, &service.TargetAddr, &service.Status, &service.MaxConn, &service.SpeedLimitKB, &service.QuotaBytes, &service.UsedBytes, &service.BytesIn, &service.BytesOut, &service.ActiveConn, &service.PausedReason, &service.CreatedAt, &service.UpdatedAt); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE forward_services SET status = ?, updated_at = ? WHERE id = ?`, domain.ServiceClosed, now, service.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tunnels SET status = ?, updated_at = ? WHERE id = ?`, domain.ServiceClosed, now, service.TunnelID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO node_commands(node_id, type, payload_json, status, available_at, created_at) VALUES(?,?,?,?,?,?)`,
		service.NodeID, domain.CommandDeleteService, fmt.Sprintf(`{"service":{"service_key":"%s","tunnel_id":%d,"user_id":%d,"node_id":%d,"protocol":"%s","listen_addr":"%s","target_addr":"%s","status":"%s","max_conn":%d,"speed_limit_kb":%d,"quota_bytes":%d}}`,
			service.ServiceKey, service.TunnelID, service.UserID, service.NodeID, service.Protocol, service.ListenAddr, service.TargetAddr, domain.ServiceClosed, service.MaxConn, service.SpeedLimitKB, service.QuotaBytes), "pending", now, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) ensureDefaultPaymentChannels(ctx context.Context) error {
	now := time.Now().UTC()
	channels := []domain.PaymentChannel{
		{Code: "epay", Name: "Epay", Enabled: true, Provider: "epay"},
		{Code: "bepusdt", Name: "BEpusdt", Enabled: true, Provider: "bepusdt"},
	}
	for _, channel := range channels {
		payload, _ := json.Marshal(map[string]any{"provider": channel.Provider, "mode": "plugin"})
		_, err := a.store.DB().ExecContext(ctx, `INSERT INTO payment_channels(code, name, enabled, provider, config_json, created_at, updated_at)
			VALUES(?,?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE name = VALUES(name), enabled = VALUES(enabled), provider = VALUES(provider), config_json = VALUES(config_json), updated_at = VALUES(updated_at)`,
			channel.Code, channel.Name, channel.Enabled, channel.Provider, string(payload), now, now)
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *App) HandlePaymentNotify(ctx context.Context, channelCode string, body []byte, form url.Values) (*domain.PaymentOrder, error) {
	provider, ok := a.payments.Get(channelCode)
	if !ok {
		return nil, fmt.Errorf("unknown payment channel: %s", channelCode)
	}
	notify, err := provider.ParseNotify(ctx, body, form)
	if err != nil {
		return nil, err
	}
	if notify.OrderNo == "" {
		return nil, errors.New("missing order number")
	}
	tx, err := a.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var order domain.PaymentOrder
	row := tx.QueryRowContext(ctx, `SELECT id, order_no, user_id, channel, amount_cents, status, pay_url, trade_no, raw_request, raw_notify, paid_at, created_at, updated_at FROM payment_orders WHERE order_no = ?`, notify.OrderNo)
	var paidAtValue sql.NullTime
	if scanErr := row.Scan(&order.ID, &order.OrderNo, &order.UserID, &order.Channel, &order.AmountCents, &order.Status, &order.PayURL, &order.TradeNo, &order.RawRequest, &order.RawNotify, &paidAtValue, &order.CreatedAt, &order.UpdatedAt); scanErr != nil {
		return nil, scanErr
	}
	if paidAtValue.Valid {
		order.PaidAt = &paidAtValue.Time
	}
	now := time.Now().UTC()
	paidAt := now
	if notify.Status == domain.PaymentPaid {
		if _, err = tx.ExecContext(ctx, `UPDATE payment_orders SET status = ?, trade_no = COALESCE(NULLIF(?, ''), trade_no), raw_notify = ?, paid_at = ?, updated_at = ? WHERE order_no = ?`,
			domain.PaymentPaid, notify.TradeNo, notify.Raw, paidAt, now, notify.OrderNo); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE users SET balance_cents = balance_cents + ? , updated_at = ? WHERE id = ?`, order.AmountCents, now, order.UserID); err != nil {
			return nil, err
		}
		order.Status = domain.PaymentPaid
		order.TradeNo = notify.TradeNo
		order.RawNotify = notify.Raw
		order.PaidAt = &paidAt
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &order, nil
}

func (a *App) GetUserByID(ctx context.Context, userID int64) (*domain.User, error) {
	row := a.store.DB().QueryRowContext(ctx, `SELECT id, username, password_hash, status, balance_cents, flow_quota_mb, traffic_used_bytes, expires_at, created_at, updated_at FROM users WHERE id = ?`, userID)
	var user domain.User
	var expires sql.NullTime
	if err := row.Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Status, &user.BalanceCents, &user.FlowQuotaMB, &user.TrafficUsedBytes, &expires, &user.CreatedAt, &user.UpdatedAt); err != nil {
		return nil, err
	}
	if expires.Valid {
		user.ExpiresAt = &expires.Time
	}
	return &user, nil
}

func serviceKey(userID, nodeID int64, now time.Time) (string, error) {
	token, err := security.RandomToken(12)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d_%d_%d_%s", userID, nodeID, now.Unix(), strings.ToLower(token[:8])), nil
}
