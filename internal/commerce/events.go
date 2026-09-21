package commerce

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

type WebhookSubscription struct {
	Format     string     `json:"format"`
	MutedUntil *time.Time `json:"muted_until"`
	Version    int64      `json:"version"`
	ID         string     `json:"id"`
	URL        string     `json:"url"`
	Events     []string   `json:"events"`
	Enabled    bool       `json:"enabled"`
	CreatedAt  time.Time  `json:"created_at"`
}

type Event struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Payload   string    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) migrateEvents(ctx context.Context, conn *sql.Conn) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS commerce_webhook_subscriptions(id VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,url VARCHAR(2048) NOT NULL,secret VARCHAR(128) NOT NULL,events TEXT NOT NULL,enabled INTEGER NOT NULL,created_at VARCHAR(40) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_events(id VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,kind VARCHAR(100) NOT NULL,payload TEXT NOT NULL,created_at VARCHAR(40) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_event_deliveries(event_id VARCHAR(64) NOT NULL,subscription_id VARCHAR(64) NOT NULL,attempts BIGINT NOT NULL,next_at BIGINT NOT NULL,last_error VARCHAR(500) NOT NULL,delivered_at VARCHAR(40),PRIMARY KEY(event_id,subscription_id))`,
	} {
		if s.Dialect == "mysql" {
			q = strings.ReplaceAll(q, " TEXT", " LONGTEXT")
		}
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for _, idx := range []struct{ table, name, columns string }{
		{"commerce_events", "commerce_events_user", "user_id,created_at,id"},
		{"commerce_event_deliveries", "commerce_delivery_due", "next_at,event_id"},
		{"commerce_webhook_subscriptions", "commerce_webhook_user", "user_id,id"},
	} {
		if err := storage.EnsureIndex(ctx, conn, s.Dialect, idx.table, idx.name, idx.columns, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) emitEventTx(ctx context.Context, tx *sql.Tx, user, kind string, data map[string]any) error {
	if user == "" || strings.TrimSpace(kind) == "" {
		return nil
	}
	eventID := id()
	b, err := json.Marshal(struct {
		ID   string         `json:"id"`
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}{eventID, kind, data})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_events(id,user_id,kind,payload,created_at) VALUES(?,?,?,?,?)"), eventID, user, kind, string(b), stamp(s.Now())); err != nil {
		return err
	}
	query := "SELECT w.id,w.events FROM commerce_webhook_subscriptions w LEFT JOIN commerce_webhook_settings st ON st.subscription_id=w.id LEFT JOIN commerce_notification_formats f ON f.subscription_id=w.id WHERE w.user_id=? AND w.enabled=1 AND COALESCE(st.muted_until,0)<=?"
	if s.Dialect == "postgres" {
		query += " FOR UPDATE OF w"
	} else if s.Dialect == "mysql" {
		query += " FOR UPDATE"
	}
	rows, err := tx.QueryContext(ctx, s.q(query), user, s.Now().UnixMilli())
	if err != nil {
		return err
	}
	type subscription struct{ id, events string }
	var subscriptions []subscription
	for rows.Next() {
		var subID, events string
		if err = rows.Scan(&subID, &events); err != nil {
			rows.Close()
			return err
		}
		subscriptions = append(subscriptions, subscription{id: subID, events: events})
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, subscription := range subscriptions {
		if subscription.events != "*" && !eventAllowed(subscription.events, kind) {
			continue
		}
		q := "INSERT INTO commerce_event_deliveries(event_id,subscription_id,attempts,next_at,last_error) VALUES(?,?,0,0,'') ON CONFLICT(event_id,subscription_id) DO NOTHING"
		if s.Dialect == "mysql" {
			q = "INSERT INTO commerce_event_deliveries(event_id,subscription_id,attempts,next_at,last_error) VALUES(?,?,0,0,'') ON DUPLICATE KEY UPDATE event_id=event_id"
		}
		if _, err = tx.ExecContext(ctx, s.q(q), eventID, subscription.id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func eventAllowed(events, kind string) bool {
	for _, v := range strings.Split(events, ",") {
		if strings.TrimSpace(v) == kind {
			return true
		}
	}
	return false
}

func (s *Service) CreateWebhook(ctx context.Context, user, rawURL string, events []string) (WebhookSubscription, string, error) {
	return s.CreateWebhookScoped(ctx, user, rawURL, events, false)
}
func (s *Service) CreateWebhookScoped(ctx context.Context, user, rawURL string, events []string, adminScope bool) (WebhookSubscription, string, error) {
	return s.CreateNotification(ctx, user, rawURL, events, adminScope, "webhook")
}
func (s *Service) CreateNotification(ctx context.Context, user, rawURL string, events []string, adminScope bool, format string) (WebhookSubscription, string, error) {
	if format == "" {
		format = "webhook"
	}
	if !validNotificationURL(rawURL, format) {
		return WebhookSubscription{}, "", errors.New("invalid notification destination or format")
	}
	if len(rawURL) > 2048 || !validWebhookURL(rawURL) || len(events) == 0 || len(events) > 32 {
		return WebhookSubscription{}, "", errors.New("webhook requires HTTPS URL and events")
	}
	if err := validateWebhookEvents(events); err != nil {
		return WebhookSubscription{}, "", err
	}
	secret := tokenSecret()
	v := WebhookSubscription{Format: format, ID: id(), URL: rawURL, Events: events, Enabled: true, Version: 1, CreatedAt: s.Now().UTC()}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, _, err := s.walletTx(ctx, tx, user); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM commerce_webhook_subscriptions WHERE user_id=?"), user).Scan(&count); err != nil {
			return err
		}
		if count >= 8 {
			return errors.New("webhook subscription limit reached")
		}
		_, err := tx.ExecContext(ctx, s.q("INSERT INTO commerce_webhook_subscriptions(id,user_id,url,secret,events,enabled,created_at) VALUES(?,?,?,?,?,?,?)"), v.ID, user, rawURL, secret, strings.Join(events, ","), 1, stamp(v.CreatedAt))
		if err != nil {
			return err
		}
		scope := 0
		if adminScope {
			scope = 1
		}
		if _, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_notification_formats(subscription_id,format) VALUES(?,?)"), v.ID, format); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_webhook_settings(subscription_id,muted_until,version,admin_scope) VALUES(?,0,1,?)"), v.ID, scope)
		return err
	})
	return v, secret, err
}

func validWebhookURL(raw string) bool {
	u, err := http.NewRequest(http.MethodPost, raw, nil)
	if err != nil || u.URL.Scheme != "https" || u.URL.Hostname() == "" || u.URL.User != nil || u.URL.RawQuery != "" || u.URL.Fragment != "" {
		return false
	}
	if port := u.URL.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if ip, e := netip.ParseAddr(u.URL.Hostname()); e == nil {
		return publicWebhookIP(ip)
	}
	return true
}

func tokenSecret() string {
	return id() + id()
}

func (s *Service) ListWebhooks(ctx context.Context, user string) ([]WebhookSubscription, error) {
	rows, err := s.DB.QueryContext(ctx, s.q("SELECT w.id,w.url,w.events,w.enabled,w.created_at,COALESCE(st.muted_until,0),COALESCE(st.version,1),COALESCE(f.format,'webhook') FROM commerce_webhook_subscriptions w LEFT JOIN commerce_webhook_settings st ON st.subscription_id=w.id LEFT JOIN commerce_notification_formats f ON f.subscription_id=w.id WHERE w.user_id=? ORDER BY w.created_at DESC"), user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookSubscription
	for rows.Next() {
		var v WebhookSubscription
		var events, created string
		var enabled int
		var muted int64
		if err := rows.Scan(&v.ID, &v.URL, &events, &enabled, &created, &muted, &v.Version, &v.Format); err != nil {
			return nil, err
		}
		if muted > 0 {
			until := time.UnixMilli(muted).UTC()
			v.MutedUntil = &until
		}
		v.Events = strings.Split(events, ",")
		v.Enabled = enabled != 0
		v.CreatedAt = parse(created)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Service) DeleteWebhook(ctx context.Context, user, id string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q("DELETE FROM commerce_webhook_subscriptions WHERE id=? AND user_id=?"), id, user)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return sql.ErrNoRows
		}
		if _, err = tx.ExecContext(ctx, s.q("DELETE FROM commerce_notification_formats WHERE subscription_id=?"), id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, s.q("DELETE FROM commerce_webhook_settings WHERE subscription_id=?"), id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.q("DELETE FROM commerce_event_deliveries WHERE subscription_id=?"), id)
		return err
	})
}

func (s *Service) Events(ctx context.Context, user string, limit int) ([]Event, error) {
	return s.events(ctx, user, limit, false)
}
func (s *Service) EventsForActor(ctx context.Context, actor contract.User, limit int) ([]Event, error) {
	return s.events(ctx, actor.ID, limit, actor.Role == "admin")
}
func (s *Service) events(ctx context.Context, user string, limit int, adminScope bool) ([]Event, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, s.q("SELECT id,kind,payload,created_at FROM commerce_events WHERE user_id=? ORDER BY created_at DESC,id DESC LIMIT ?"), user, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var v Event
		var created string
		if err := rows.Scan(&v.ID, &v.Kind, &v.Payload, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = parse(created)
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if s.EventVisible == nil {
		return out, nil
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	visible := []Event{}
	for _, event := range out {
		ok, e := s.EventVisible(ctx, tx, user, event.Kind, event.Payload, adminScope)
		if e != nil {
			return nil, e
		}
		if ok {
			visible = append(visible, event)
		}
	}
	return visible, tx.Commit()
}

func (s *Service) RunWebhookDelivery(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, s.q("SELECT d.event_id,d.subscription_id,d.attempts,e.payload,e.kind,w.url,w.secret,COALESCE(f.format,'webhook') FROM commerce_event_deliveries d JOIN commerce_events e ON e.id=d.event_id JOIN commerce_webhook_subscriptions w ON w.id=d.subscription_id LEFT JOIN commerce_webhook_settings st ON st.subscription_id=w.id LEFT JOIN commerce_notification_formats f ON f.subscription_id=w.id WHERE COALESCE(st.muted_until,0)<=? AND d.delivered_at IS NULL AND d.next_at<=? AND d.attempts<12 AND w.enabled=1 ORDER BY d.next_at,d.event_id LIMIT ?"), s.Now().UnixMilli(), s.Now().UnixMilli(), limit)
	if err != nil {
		return 0, err
	}
	type delivery struct {
		event, sub string
		format     string
		attempts   int64
		payload    string
		kind, url  string
		secret     string
	}
	var list []delivery
	for rows.Next() {
		var d delivery
		if err = rows.Scan(&d.event, &d.sub, &d.attempts, &d.payload, &d.kind, &d.url, &d.secret, &d.format); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	count := 0
	client := s.webhookClient
	if client == nil {
		client = newWebhookClient()
	}
	defer client.CloseIdleConnections()
	for _, d := range list {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		claimed := false
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			var owner string
			var adminScope int
			if e := tx.QueryRowContext(ctx, s.q("SELECT w.user_id,COALESCE(st.admin_scope,0) FROM commerce_webhook_subscriptions w LEFT JOIN commerce_webhook_settings st ON st.subscription_id=w.id WHERE w.id=? AND w.enabled=1 AND COALESCE(st.muted_until,0)<=?"), d.sub, s.Now().UnixMilli()).Scan(&owner, &adminScope); errors.Is(e, sql.ErrNoRows) {
				return nil
			} else if e != nil {
				return e
			}
			if s.CheckAccount != nil {
				if e := s.CheckAccount(ctx, tx, owner); errors.Is(e, ErrAccountDisabled) {
					return nil
				} else if e != nil {
					return e
				}
			}
			if s.EventVisible != nil {
				allowed, e := s.EventVisible(ctx, tx, owner, d.kind, d.payload, adminScope != 0)
				if e != nil {
					return e
				}
				if !allowed {
					_, e = tx.ExecContext(ctx, s.q("UPDATE commerce_event_deliveries SET attempts=12,last_error='access_revoked' WHERE event_id=? AND subscription_id=? AND delivered_at IS NULL"), d.event, d.sub)
					return e
				}
			}
			res, e := tx.ExecContext(ctx, s.q("UPDATE commerce_event_deliveries SET attempts=attempts+1,next_at=? WHERE event_id=? AND subscription_id=? AND delivered_at IS NULL AND next_at<=? AND attempts=?"), s.Now().Add(5*time.Minute).UnixMilli(), d.event, d.sub, s.Now().UnixMilli(), d.attempts)
			if e != nil {
				return e
			}
			n, _ := res.RowsAffected()
			claimed = n == 1
			return nil
		}); err != nil {
			return count, err
		}
		if !claimed {
			continue
		}
		d.attempts++
		body, bodyErr := notificationBody(d.format, d.payload, d.kind)
		if bodyErr != nil {
			return count, bodyErr
		}
		mac := hmac.New(sha256.New, []byte(d.secret))
		mac.Write(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-TFP-Event", d.kind)
			req.Header.Set("X-TFP-Event-ID", d.event)
			req.Header.Set("X-TFP-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		var callErr error
		if err != nil {
			callErr = err
		} else {
			res, e := client.Do(req)
			if e != nil {
				callErr = e
			} else {
				callErr = notificationResponse(d.format, res)
				res.Body.Close()
			}
		}
		if err = s.Write(ctx, func(tx *sql.Tx) error {
			if callErr == nil {
				_, e := tx.ExecContext(ctx, s.q("UPDATE commerce_event_deliveries SET delivered_at=?,last_error='' WHERE event_id=? AND subscription_id=? AND delivered_at IS NULL AND attempts=? AND last_error NOT IN ('suppressed','access_revoked')"), stamp(s.Now()), d.event, d.sub, d.attempts)
				return e
			}
			attempts := d.attempts
			if attempts > 31 {
				attempts = 31
			}
			delay := time.Second * 30
			for n := int64(1); n < attempts && delay < time.Hour; n++ {
				delay *= 2
			}
			if delay > time.Hour {
				delay = time.Hour
			}
			msg := "delivery_failed"
			_, e := tx.ExecContext(ctx, s.q("UPDATE commerce_event_deliveries SET attempts=?,next_at=?,last_error=? WHERE event_id=? AND subscription_id=? AND delivered_at IS NULL AND attempts=? AND last_error NOT IN ('suppressed','access_revoked')"), attempts, s.Now().Add(delay).UnixMilli(), msg, d.event, d.sub, d.attempts)
			return e
		}); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (s *Service) RunWebhookLoop(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.PruneEvents(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "event retention will retry")
		}
		if _, err := s.RunWebhookDelivery(ctx, 20); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "webhook delivery batch will retry")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Resolve on each connection and dial the validated address directly. DNS
// rebinding cannot change the destination between validation and the socket.
func publicWebhookIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"} {
		if netip.MustParsePrefix(cidr).Contains(ip) {
			return false
		}
	}
	return true
}
func newWebhookClient() *http.Client {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 32768}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid webhook host")
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, errors.New("webhook DNS unavailable")
		}
		if len(ips) == 0 {
			return nil, errors.New("webhook DNS empty")
		}
		for _, ip := range ips {
			if !publicWebhookIP(ip) {
				return nil, errors.New("webhook destination denied")
			}
		}
		dialer := net.Dialer{Timeout: 5 * time.Second}
		for _, ip := range ips {
			conn, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if e == nil {
				return conn, nil
			}
		}
		return nil, errors.New("webhook connection failed")
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("webhook redirects forbidden") }}
}
func (s *Service) WebhookDeliveries(ctx context.Context, user string) ([]map[string]any, error) {
	rows, err := s.DB.QueryContext(ctx, s.q("SELECT d.event_id,d.subscription_id,d.attempts,d.next_at,d.last_error,d.delivered_at FROM commerce_event_deliveries d JOIN commerce_webhook_subscriptions w ON w.id=d.subscription_id WHERE w.user_id=? ORDER BY d.next_at DESC,d.event_id DESC LIMIT 100"), user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var event, sub, last string
		var attempts, next int64
		var delivered sql.NullString
		if err = rows.Scan(&event, &sub, &attempts, &next, &last, &delivered); err != nil {
			return nil, err
		}
		status := "pending"
		if delivered.Valid {
			status = "delivered"
		} else if last == "suppressed" || last == "access_revoked" {
			status = last
		} else if attempts >= 12 {
			status = "failed"
		}
		out = append(out, map[string]any{"event_id": event, "subscription_id": sub, "attempts": attempts, "next_at": time.UnixMilli(next).UTC(), "status": status, "last_error": last, "delivered_at": delivered.String})
	}
	return out, rows.Err()
}

// Notification data expires after 30 days; financial ledger facts are separate.
func (s *Service) PruneEvents(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, s.q("SELECT id FROM commerce_events WHERE created_at<? ORDER BY created_at,id LIMIT 100"), stamp(s.Now().Add(-30*24*time.Hour)))
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(ids) == 0 {
		return err
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, s.q("DELETE FROM commerce_event_deliveries WHERE event_id=?"), id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, s.q("DELETE FROM commerce_events WHERE id=?"), id); err != nil {
				return err
			}
		}
		return nil
	})
}
