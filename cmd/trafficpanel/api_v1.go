package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"trafficpanel/internal/domain"
)

type v1Response struct {
	Code int    `json:"code"`
	Data any    `json:"data"`
	Msg  string `json:"msg"`
}

func (s *apiServer) handleAPIV1(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	if path == r.URL.Path {
		writeV1Error(w, http.StatusNotFound, 404, "not found")
		return
	}
	if path == "" {
		path = "/"
	}
	switch {
	case path == "/auth/login":
		s.handleV1Login(w, r)
	case path == "/auth/logout":
		s.handleV1Logout(w, r)
	case path == "/system/info":
		s.handleV1SystemInfo(w, r)
	case path == "/system/info/queue":
		s.handleV1QueueInfo(w, r)
	case path == "/system/node/status":
		s.handleV1NodeStatus(w, r)
	case path == "/user/info":
		s.requireV1User(s.handleV1UserInfo)(w, r)
	case path == "/user/statistic":
		s.requireV1User(s.handleV1UserStatistic)(w, r)
	case path == "/user/devicegroup" || strings.HasPrefix(path, "/user/devicegroup/"):
		s.requireV1User(s.handleV1UserDeviceGroups)(w, r)
	case strings.HasPrefix(path, "/user/forward/batch_") || path == "/user/forward/reset_traffic" || path == "/user/forward/search_rules" || path == "/user/forward/folder" || path == "/user/forward/folder/bind":
		s.requireV1User(s.handleV1UserForwardUtility)(w, r)
	case path == "/user/forward" || strings.HasPrefix(path, "/user/forward/"):
		s.requireV1User(s.handleV1UserForward)(w, r)
	case path == "/user/shop/payment_info":
		s.requireV1User(s.handleV1UserShopPaymentInfo)(w, r)
	case path == "/user/shop/plan":
		s.requireV1User(s.handleV1UserShopPlans)(w, r)
	case path == "/user/shop/order" || strings.HasPrefix(path, "/user/shop/order"):
		s.requireV1User(s.handleV1UserShopOrders)(w, r)
	case path == "/user/aff/log" || strings.HasPrefix(path, "/user/aff/log"):
		s.requireV1User(s.handleV1UserAffLogs)(w, r)
	case path == "/user/aff/config":
		s.requireV1User(s.handleV1UserAffConfig)(w, r)
	case path == "/admin/devicegroup" || strings.HasPrefix(path, "/admin/devicegroup/"):
		s.requireV1Admin(s.handleV1AdminDeviceGroups)(w, r)
	case path == "/admin/user" || strings.HasPrefix(path, "/admin/user/"):
		s.requireV1Admin(s.handleV1AdminUsers)(w, r)
	case path == "/admin/statistic":
		s.requireV1Admin(s.handleV1AdminStatistic)(w, r)
	case path == "/admin/usergroup" || strings.HasPrefix(path, "/admin/usergroup/"):
		s.requireV1Admin(s.handleV1AdminUserGroups)(w, r)
	case path == "/admin/shop/plan" || strings.HasPrefix(path, "/admin/shop/plan/"):
		s.requireV1Admin(s.handleV1AdminPlans)(w, r)
	case path == "/admin/shop/order" || strings.HasPrefix(path, "/admin/shop/order/"):
		s.requireV1Admin(s.handleV1AdminOrders)(w, r)
	case path == "/admin/shop/redeem" || strings.HasPrefix(path, "/admin/shop/redeem"):
		s.requireV1Admin(s.handleV1AdminRedeem)(w, r)
	case path == "/admin/aff/log" || strings.HasPrefix(path, "/admin/aff/log"):
		s.requireV1Admin(s.handleV1AdminAffLogs)(w, r)
	case strings.HasPrefix(path, "/admin/kv/"):
		s.requireV1Admin(s.handleV1AdminKV)(w, r)
	case path == "/client/config_v2":
		s.handleV1ClientConfig(w, r)
	case path == "/guest/telegram/webhook":
		s.handleV1TelegramWebhook(w, r)
	default:
		writeV1Error(w, http.StatusNotFound, 404, "unknown api v1 path")
	}
}

func (s *apiServer) handleV1Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	role := strings.ToLower(strings.TrimSpace(input.Role))
	var token string
	var err error
	if role == "user" {
		token, err = s.app.LoginUser(r.Context(), input.Username, input.Password)
	} else if role == "admin" {
		token, err = s.app.LoginAdmin(r.Context(), input.Username, input.Password)
	} else {
		token, err = s.app.LoginAdmin(r.Context(), input.Username, input.Password)
		if err != nil {
			token, err = s.app.LoginUser(r.Context(), input.Username, input.Password)
		}
	}
	if err != nil {
		writeV1Error(w, http.StatusUnauthorized, 401, "用户名或密码错误")
		return
	}
	writeV1OK(w, token)
}

func (s *apiServer) handleV1Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	token := authToken(r)
	if token != "" {
		_ = s.app.Logout(r.Context(), token)
	}
	writeV1OK(w, map[string]string{"status": "ok"})
}

func (s *apiServer) handleV1SystemInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	writeV1OK(w, map[string]any{
		"name":           s.cfg.AppName,
		"app_name":       s.cfg.AppName,
		"version":        "dev",
		"time":           time.Now().Unix(),
		"register":       false,
		"captcha":        false,
		"license_expire": 0,
	})
}

func (s *apiServer) handleV1QueueInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	stats, err := s.app.DashboardStats(r.Context())
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeV1OK(w, map[string]any{"pending": 0, "commands": 0, "services": stats.Services})
}

func (s *apiServer) handleV1NodeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	nodes, err := s.app.ListNodes(r.Context())
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		items = append(items, map[string]any{
			"id":           node.ID,
			"name":         node.Name,
			"host":         node.Host,
			"port":         node.Port,
			"status":       node.Status,
			"last_seen_at": node.LastSeenAt,
		})
	}
	writeV1OK(w, items)
}

func (s *apiServer) handleV1UserInfo(w http.ResponseWriter, r *http.Request, session *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	user, err := s.app.GetUserByID(r.Context(), session.ActorID)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	tunnels, _ := s.app.ListTunnelsByUser(r.Context(), session.ActorID)
	writeV1OK(w, v1UserInfo(user, len(tunnels)))
}

func (s *apiServer) handleV1UserStatistic(w http.ResponseWriter, r *http.Request, session *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	user, err := s.app.GetUserByID(r.Context(), session.ActorID)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeV1OK(w, map[string]any{
		"traffic_today":     0,
		"traffic_yesterday": 0,
		"traffic_used":      user.TrafficUsedBytes,
		"traffic_enable":    user.FlowQuotaMB * 1024 * 1024,
	})
}

func (s *apiServer) handleV1UserForwardUtility(w http.ResponseWriter, r *http.Request, session *domain.Session) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch path {
	case "/user/forward/search_rules":
		tunnels, err := s.app.ListTunnelsByUser(r.Context(), session.ActorID)
		if err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		items := make([]map[string]any, 0, len(tunnels))
		for _, tunnel := range tunnels {
			items = append(items, v1ForwardFromTunnel(tunnel))
		}
		writeV1OK(w, items)
	case "/user/forward/folder":
		writeV1OK(w, []any{})
	default:
		if r.Method != http.MethodPost {
			writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
			return
		}
		writeV1OK(w, map[string]string{"status": "ok"})
	}
}

func (s *apiServer) handleV1UserShopPaymentInfo(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	channels, err := s.app.ListPaymentChannels(r.Context())
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeV1OK(w, channels)
}

func (s *apiServer) handleV1UserShopPlans(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	items, err := s.listV1Plans(r)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeV1OK(w, items)
}

func (s *apiServer) handleV1UserShopOrders(w http.ResponseWriter, r *http.Request, session *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	orders, err := s.app.ListPaymentOrdersByUser(r.Context(), session.ActorID)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeV1OK(w, v1OrderList(orders))
}

func (s *apiServer) handleV1UserAffLogs(w http.ResponseWriter, r *http.Request, session *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	rows, err := s.app.DB().QueryContext(r.Context(), `SELECT id, user_id, from_user_id, order_id, type, amount_cents, rate, status, COALESCE(CAST(detail_json AS CHAR), ''), created_at, accounted_at FROM aff_logs WHERE user_id = ? ORDER BY id DESC`, session.ActorID)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	defer rows.Close()
	items, err := scanV1AffLogs(rows)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeV1OK(w, map[string]any{"items": items, "total": len(items)})
}

func (s *apiServer) handleV1UserAffConfig(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	writeV1OK(w, map[string]any{"enabled": false, "rate": 0})
}

func (s *apiServer) handleV1AdminUsers(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	switch r.Method {
	case http.MethodGet:
		users, err := s.app.ListUsers(r.Context())
		if err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		items := make([]map[string]any, 0, len(users))
		for _, user := range users {
			items = append(items, v1UserInfo(&user, 0))
		}
		writeV1OK(w, map[string]any{"items": items, "total": len(items)})
	case http.MethodPost:
		var input struct {
			Username    string `json:"username"`
			Password    string `json:"password"`
			FlowQuotaMB int64  `json:"flow_quota_mb"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		id, err := s.app.CreateUser(r.Context(), input.Username, input.Password, input.FlowQuotaMB, nil)
		if err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		writeV1OK(w, map[string]any{"id": id})
	default:
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
	}
}

func (s *apiServer) handleV1AdminStatistic(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	stats, err := s.app.DashboardStats(r.Context())
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeV1OK(w, stats)
}

func (s *apiServer) handleV1AdminUserGroups(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1OK(w, map[string]string{"status": "ok"})
		return
	}
	rows, err := s.app.DB().QueryContext(r.Context(), `SELECT id, name, show_order, (SELECT COUNT(1) FROM users WHERE user_group_id = user_groups.id) FROM user_groups ORDER BY show_order ASC, id ASC`)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		var showOrder int
		var count int64
		if err := rows.Scan(&id, &name, &showOrder, &count); err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "show_order": showOrder, "user_count": count})
	}
	writeV1OK(w, items)
}

func (s *apiServer) handleV1AdminPlans(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1OK(w, map[string]string{"status": "ok"})
		return
	}
	items, err := s.listV1Plans(r)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeV1OK(w, items)
}

func (s *apiServer) handleV1AdminOrders(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1OK(w, map[string]string{"status": "ok"})
		return
	}
	orders, err := s.app.ListPaymentOrders(r.Context())
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeV1OK(w, v1OrderList(orders))
}

func (s *apiServer) handleV1AdminRedeem(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1OK(w, map[string]string{"status": "ok"})
		return
	}
	rows, err := s.app.DB().QueryContext(r.Context(), `SELECT id, code, type, amount_cents, plan_id, traffic_mb, duration_days, max_use, used_count, status, expires_at, created_at FROM redeem_codes ORDER BY id DESC`)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var code, typ, status string
		var amount, traffic int64
		var planID sql.NullInt64
		var duration, maxUse, usedCount int
		var expires sql.NullTime
		var created time.Time
		if err := rows.Scan(&id, &code, &typ, &amount, &planID, &traffic, &duration, &maxUse, &usedCount, &status, &expires, &created); err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		items = append(items, map[string]any{"id": id, "code": code, "type": typ, "amount": float64(amount) / 100, "plan_id": sqlNullInt64Ptr(planID), "traffic": traffic, "duration_days": duration, "max_use": maxUse, "used_count": usedCount, "status": status, "expires_at": sqlTimeUnix(expires), "created_at": created.Unix()})
	}
	writeV1OK(w, map[string]any{"items": items, "total": len(items)})
}

func (s *apiServer) handleV1AdminAffLogs(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	if r.Method != http.MethodGet {
		writeV1OK(w, map[string]string{"status": "ok"})
		return
	}
	rows, err := s.app.DB().QueryContext(r.Context(), `SELECT id, user_id, from_user_id, order_id, type, amount_cents, rate, status, COALESCE(CAST(detail_json AS CHAR), ''), created_at, accounted_at FROM aff_logs ORDER BY id DESC`)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	defer rows.Close()
	items, err := scanV1AffLogs(rows)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeV1OK(w, map[string]any{"items": items, "total": len(items)})
}

func (s *apiServer) handleV1AdminKV(w http.ResponseWriter, r *http.Request, session *domain.Session) {
	key := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/kv/")
	if key == "" {
		writeV1Error(w, http.StatusBadRequest, 400, "missing key")
		return
	}
	switch r.Method {
	case http.MethodGet:
		var valueJSON sql.NullString
		var valueText sql.NullString
		err := s.app.DB().QueryRowContext(r.Context(), `SELECT COALESCE(CAST(value_json AS CHAR), ''), value_text FROM system_kv WHERE key_name = ?`, key).Scan(&valueJSON, &valueText)
		if err == sql.ErrNoRows {
			writeV1OK(w, map[string]any{})
			return
		}
		if err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		if valueJSON.Valid && valueJSON.String != "" {
			var decoded any
			if json.Unmarshal([]byte(valueJSON.String), &decoded) == nil {
				writeV1OK(w, decoded)
				return
			}
		}
		writeV1OK(w, valueText.String)
	case http.MethodPut, http.MethodPost:
		var input any
		if !decodeJSON(w, r, &input) {
			return
		}
		raw, _ := json.Marshal(input)
		now := time.Now().UTC()
		_, err := s.app.DB().ExecContext(r.Context(), `INSERT INTO system_kv(key_name, value_json, scope, updated_by_admin_id, created_at, updated_at) VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE value_json=VALUES(value_json), updated_by_admin_id=VALUES(updated_by_admin_id), updated_at=VALUES(updated_at)`, key, string(raw), "global", session.ActorID, now, now)
		if err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		writeV1OK(w, map[string]string{"status": "ok"})
	default:
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
	}
}

func (s *apiServer) handleV1ClientConfig(w http.ResponseWriter, r *http.Request) {
	writeV1OK(w, map[string]any{"nodes": []any{}, "forwards": []any{}})
}

func (s *apiServer) handleV1TelegramWebhook(w http.ResponseWriter, r *http.Request) {
	writeV1OK(w, map[string]string{"status": "ok"})
}

func (s *apiServer) requireV1User(next func(http.ResponseWriter, *http.Request, *domain.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, err := s.app.RequireSession(r.Context(), authToken(r), domain.ActorUser)
		if err != nil {
			writeV1Error(w, http.StatusUnauthorized, 401, err.Error())
			return
		}
		next(w, r, session)
	}
}

func (s *apiServer) requireV1Admin(next func(http.ResponseWriter, *http.Request, *domain.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, err := s.app.RequireSession(r.Context(), authToken(r), domain.ActorAdmin)
		if err != nil {
			writeV1Error(w, http.StatusUnauthorized, 401, err.Error())
			return
		}
		next(w, r, session)
	}
}

func v1UserInfo(user *domain.User, usedRules int) map[string]any {
	if user == nil {
		return map[string]any{}
	}
	var expire any
	if user.ExpiresAt != nil {
		expire = user.ExpiresAt.Unix()
	}
	return map[string]any{
		"id":               user.ID,
		"username":         user.Username,
		"admin":            false,
		"banned":           user.Status != domain.StatusEnabled,
		"balance":          float64(user.BalanceCents) / 100,
		"balance_cents":    user.BalanceCents,
		"aff_balance":      float64(user.AffBalanceCents) / 100,
		"plan_id":          user.PlanID,
		"plan_name":        "",
		"renew_price":      0,
		"expire":           expire,
		"auto_renew":       user.AutoRenew,
		"group_id":         user.UserGroupID,
		"group_name":       "",
		"max_rules":        user.MaxRules,
		"used_rules":       usedRules,
		"traffic_enable":   user.FlowQuotaMB * 1024 * 1024,
		"traffic_used":     user.TrafficUsedBytes,
		"speed_limit":      0,
		"ip_limit":         0,
		"connection_limit": 0,
		"invite_code":      user.InviteCode,
		"invite_config":    map[string]any{"enabled": false},
		"inviter":          user.InvitedByUserID,
		"telegram_id":      user.TelegramID,
		"allow_device":     user.AllowDevice,
	}
}

func v1OrderList(orders []domain.PaymentOrder) map[string]any {
	items := make([]map[string]any, 0, len(orders))
	for _, order := range orders {
		status := "OrderStatus_Open"
		if order.Status == domain.PaymentPaid {
			status = "OrderStatus_Finished"
		} else if order.Status == domain.PaymentClosed || order.Status == domain.PaymentFailed {
			status = "OrderStatus_Closed"
		}
		var paid any
		if order.PaidAt != nil {
			paid = order.PaidAt.Unix()
		}
		items = append(items, map[string]any{
			"id":        order.ID,
			"type":      "OrderType_DepositToBalance",
			"uid":       order.UserID,
			"amount":    float64(order.AmountCents) / 100,
			"message":   order.Channel,
			"status":    status,
			"order_no":  order.OrderNo,
			"open_time": order.CreatedAt.Unix(),
			"paid_time": paid,
			"username":  "",
		})
	}
	return map[string]any{"items": items, "total": len(items)}
}

func (s *apiServer) listV1Plans(r *http.Request) ([]map[string]any, error) {
	rows, err := s.app.DB().QueryContext(r.Context(), `SELECT id, name, description, price_cents, traffic_mb, duration_days, max_rules, allow_device, user_group_id, status, show_order, COALESCE(CAST(config_json AS CHAR), '') FROM plans WHERE deleted_at IS NULL ORDER BY show_order ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, desc, status, config string
		var price, traffic int64
		var duration, maxRules, showOrder int
		var allowDevice bool
		var groupID sql.NullInt64
		if err := rows.Scan(&id, &name, &desc, &price, &traffic, &duration, &maxRules, &allowDevice, &groupID, &status, &showOrder, &config); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{
			"id":               id,
			"type":             "PlanType_Month",
			"name":             name,
			"desc":             desc,
			"price":            float64(price) / 100,
			"multiple":         duration,
			"show_order":       showOrder,
			"hide":             status != string(domain.StatusEnabled),
			"group_id":         sqlNullInt64Ptr(groupID),
			"max_rules":        maxRules,
			"traffic":          traffic,
			"speed_limit":      0,
			"ip_limit":         0,
			"connection_limit": 0,
			"allow_device":     allowDevice,
			"config":           config,
		})
	}
	return items, rows.Err()
}

func scanV1AffLogs(rows *sql.Rows) ([]map[string]any, error) {
	items := []map[string]any{}
	for rows.Next() {
		var id, userID int64
		var fromUser sql.NullInt64
		var orderID sql.NullInt64
		var typ, status, detail string
		var amount int64
		var rate float64
		var created time.Time
		var accounted sql.NullTime
		if err := rows.Scan(&id, &userID, &fromUser, &orderID, &typ, &amount, &rate, &status, &detail, &created, &accounted); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"id": id, "uid": userID, "from_user_id": sqlNullInt64Ptr(fromUser), "order_id": sqlNullInt64Ptr(orderID), "type": typ, "amount": float64(amount) / 100, "rate": rate, "status": status, "detail": detail, "created_at": created.Unix(), "accounted_at": sqlTimeUnix(accounted)})
	}
	return items, rows.Err()
}

func sqlTimeUnix(v sql.NullTime) any {
	if !v.Valid {
		return nil
	}
	return v.Time.Unix()
}

func authToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	if header != "" {
		return header
	}
	if cookie, err := r.Cookie("tp_token"); err == nil {
		return cookie.Value
	}
	return ""
}

func writeV1OK(w http.ResponseWriter, data any) {
	writeJSON(w, v1Response{Code: 0, Data: data, Msg: ""})
}

func writeV1Error(w http.ResponseWriter, status int, code int, msg string) {
	if status <= 0 {
		status = http.StatusBadRequest
	}
	if code == 0 {
		code = status
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v1Response{Code: code, Data: nil, Msg: msg})
}

func parseInt64Param(r *http.Request, name string) (int64, error) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return 0, errors.New("missing " + name)
	}
	return strconv.ParseInt(value, 10, 64)
}

func sqlNullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}
