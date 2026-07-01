package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"trafficpanel/internal/domain"
)

func (s *apiServer) handleV1AdminDeviceGroups(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	pathID, hasPathID := v1PathID(r.URL.Path, "/api/v1/admin/devicegroup/")
	switch r.Method {
	case http.MethodGet:
		if hasPathID {
			group, err := s.getV1DeviceGroup(r, pathID)
			if err != nil {
				writeV1Error(w, http.StatusBadRequest, 400, err.Error())
				return
			}
			writeV1OK(w, group)
			return
		}
		groups, err := s.listV1DeviceGroups(r)
		if err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		writeV1OK(w, groups)
	case http.MethodPost:
		id, err := s.createV1DeviceGroup(r)
		if err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		writeV1OK(w, map[string]any{"id": id})
	case http.MethodPut, http.MethodPatch:
		if !hasPathID {
			if id, err := parseInt64Param(r, "id"); err == nil {
				pathID = id
				hasPathID = true
			}
		}
		if !hasPathID {
			writeV1Error(w, http.StatusBadRequest, 400, "missing id")
			return
		}
		if err := s.updateV1DeviceGroup(r, pathID); err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		writeV1OK(w, map[string]string{"status": "ok"})
	case http.MethodDelete:
		if !hasPathID {
			if id, err := parseInt64Param(r, "id"); err == nil {
				pathID = id
				hasPathID = true
			}
		}
		if !hasPathID {
			writeV1Error(w, http.StatusBadRequest, 400, "missing id")
			return
		}
		if err := s.deleteV1DeviceGroup(r, pathID); err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		writeV1OK(w, map[string]string{"status": "ok"})
	default:
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
	}
}

func (s *apiServer) handleV1UserDeviceGroups(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	pathID, hasPathID := v1PathID(r.URL.Path, "/api/v1/user/devicegroup/")
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
		return
	}
	if hasPathID {
		group, err := s.getV1DeviceGroup(r, pathID)
		if err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		writeV1OK(w, group)
		return
	}
	groups, err := s.listV1DeviceGroups(r)
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeV1OK(w, groups)
}

func (s *apiServer) handleV1UserForward(w http.ResponseWriter, r *http.Request, session *domain.Session) {
	pathID, hasPathID := v1PathID(r.URL.Path, "/api/v1/user/forward/")
	switch r.Method {
	case http.MethodGet:
		if hasPathID {
			item, err := s.getV1Forward(r, session.ActorID, pathID)
			if err != nil {
				writeV1Error(w, http.StatusBadRequest, 400, err.Error())
				return
			}
			writeV1OK(w, item)
			return
		}
		tunnels, err := s.app.ListTunnelsByUser(r.Context(), session.ActorID)
		if err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		items := make([]map[string]any, 0, len(tunnels))
		for _, tunnel := range tunnels {
			items = append(items, v1ForwardFromTunnel(tunnel))
		}
		writeV1OK(w, map[string]any{"items": items, "total": len(items)})
	case http.MethodPost:
		id, err := s.createV1Forward(r, session.ActorID)
		if err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		writeV1OK(w, id)
	case http.MethodPut, http.MethodPatch:
		if !hasPathID {
			if id, err := parseInt64Param(r, "id"); err == nil {
				pathID = id
				hasPathID = true
			}
		}
		if !hasPathID {
			writeV1Error(w, http.StatusBadRequest, 400, "missing id")
			return
		}
		if err := s.updateV1Forward(r, session.ActorID, pathID); err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		writeV1OK(w, map[string]string{"status": "ok"})
	case http.MethodDelete:
		if !hasPathID {
			if id, err := parseInt64Param(r, "id"); err == nil {
				pathID = id
				hasPathID = true
			}
		}
		if !hasPathID {
			writeV1Error(w, http.StatusBadRequest, 400, "missing id")
			return
		}
		if err := s.deleteV1Forward(r, session.ActorID, pathID); err != nil {
			writeV1Error(w, http.StatusBadRequest, 400, err.Error())
			return
		}
		writeV1OK(w, map[string]string{"status": "ok"})
	default:
		writeV1Error(w, http.StatusMethodNotAllowed, 405, "method not allowed")
	}
}

func (s *apiServer) createV1DeviceGroup(r *http.Request) (int64, error) {
	var input struct {
		Name           string  `json:"name"`
		Type           string  `json:"type"`
		NodeID         *int64  `json:"node_id"`
		Ratio          float64 `json:"ratio"`
		ConnectHost    string  `json:"connect_host"`
		PortRangeStart *int    `json:"port_range_start"`
		PortRangeEnd   *int    `json:"port_range_end"`
		Config         any     `json:"config"`
		ShowOrder      int     `json:"show_order"`
		DisplayNum     int     `json:"display_num"`
		HideStatus     bool    `json:"hide_status"`
	}
	if !decodeV1JSON(r, &input) {
		return 0, errors.New("invalid body")
	}
	if strings.TrimSpace(input.Type) == "" {
		input.Type = "node"
	}
	if input.Ratio <= 0 {
		input.Ratio = 1
	}
	configJSON := "{}"
	if input.Config != nil {
		raw, _ := json.Marshal(input.Config)
		configJSON = string(raw)
	}
	now := time.Now().UTC()
	res, err := s.app.DB().ExecContext(r.Context(), `INSERT INTO device_groups(name, type, node_id, ratio, connect_host, port_range_start, port_range_end, config_json, show_order, display_num, hide_status, status, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		strings.TrimSpace(input.Name), input.Type, input.NodeID, input.Ratio, strings.TrimSpace(input.ConnectHost), input.PortRangeStart, input.PortRangeEnd, configJSON, input.ShowOrder, input.DisplayNum, input.HideStatus, domain.StatusEnabled, now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *apiServer) updateV1DeviceGroup(r *http.Request, id int64) error {
	group, err := s.getV1DeviceGroup(r, id)
	if err != nil {
		return err
	}
	var input struct {
		Name           string  `json:"name"`
		Type           string  `json:"type"`
		NodeID         *int64  `json:"node_id"`
		Ratio          float64 `json:"ratio"`
		ConnectHost    string  `json:"connect_host"`
		PortRangeStart *int    `json:"port_range_start"`
		PortRangeEnd   *int    `json:"port_range_end"`
		Config         any     `json:"config"`
		ShowOrder      int     `json:"show_order"`
		DisplayNum     int     `json:"display_num"`
		HideStatus     bool    `json:"hide_status"`
	}
	if !decodeV1JSON(r, &input) {
		return errors.New("invalid body")
	}
	if strings.TrimSpace(input.Type) == "" {
		input.Type = group.Type
	}
	if input.Ratio <= 0 {
		input.Ratio = group.Ratio
	}
	configJSON := group.ConfigJSON
	if input.Config != nil {
		raw, _ := json.Marshal(input.Config)
		configJSON = string(raw)
	}
	now := time.Now().UTC()
	_, err = s.app.DB().ExecContext(r.Context(), `UPDATE device_groups SET name=?, type=?, node_id=?, ratio=?, connect_host=?, port_range_start=?, port_range_end=?, config_json=?, show_order=?, display_num=?, hide_status=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		strings.TrimSpace(firstNonEmpty(input.Name, group.Name)), input.Type, input.NodeID, input.Ratio, strings.TrimSpace(firstNonEmpty(input.ConnectHost, group.ConnectHost)), input.PortRangeStart, input.PortRangeEnd, configJSON, input.ShowOrder, input.DisplayNum, input.HideStatus, now, id)
	return err
}

func (s *apiServer) deleteV1DeviceGroup(r *http.Request, id int64) error {
	now := time.Now().UTC()
	_, err := s.app.DB().ExecContext(r.Context(), `UPDATE device_groups SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`, now, now, id)
	return err
}

func (s *apiServer) getV1DeviceGroup(r *http.Request, id int64) (domain.DeviceGroup, error) {
	rows, err := s.app.DB().QueryContext(r.Context(), `SELECT id, name, type, node_id, ratio, connect_host, port_range_start, port_range_end, COALESCE(CAST(config_json AS CHAR), ''), show_order, display_num, hide_status, status, created_at, updated_at, deleted_at FROM device_groups WHERE id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		return domain.DeviceGroup{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return domain.DeviceGroup{}, sql.ErrNoRows
	}
	var item domain.DeviceGroup
	var nodeID sql.NullInt64
	var start sql.NullInt64
	var end sql.NullInt64
	var deleted sql.NullTime
	if err := rows.Scan(&item.ID, &item.Name, &item.Type, &nodeID, &item.Ratio, &item.ConnectHost, &start, &end, &item.ConfigJSON, &item.ShowOrder, &item.DisplayNum, &item.HideStatus, &item.Status, &item.CreatedAt, &item.UpdatedAt, &deleted); err != nil {
		return domain.DeviceGroup{}, err
	}
	item.NodeID = sqlNullInt64Ptr(nodeID)
	if start.Valid {
		v := int(start.Int64)
		item.PortRangeStart = &v
	}
	if end.Valid {
		v := int(end.Int64)
		item.PortRangeEnd = &v
	}
	if deleted.Valid {
		item.DeletedAt = &deleted.Time
	}
	return item, nil
}

func (s *apiServer) createV1Forward(r *http.Request, userID int64) (int64, error) {
	var input struct {
		Name           string          `json:"name"`
		ListenPort     int             `json:"listen_port"`
		ListenAddr     string          `json:"listen_addr"`
		TargetAddr     string          `json:"target_addr"`
		Dest           []string        `json:"dest"`
		Protocol       domain.Protocol `json:"protocol"`
		DeviceGroupIn  int64           `json:"device_group_in"`
		DeviceGroupOut int64           `json:"device_group_out"`
		MaxConn        int             `json:"max_conn"`
		QuotaBytes     int64           `json:"quota_bytes"`
		Config         any             `json:"config"`
	}
	if !decodeV1JSON(r, &input) {
		return 0, errors.New("invalid body")
	}
	if input.Protocol == "" {
		input.Protocol = domain.ProtocolTCP
	}
	listenAddr := strings.TrimSpace(input.ListenAddr)
	if listenAddr == "" && input.ListenPort > 0 {
		listenAddr = ":" + strconv.Itoa(input.ListenPort)
	}
	targetAddr := strings.TrimSpace(input.TargetAddr)
	if targetAddr == "" && len(input.Dest) > 0 {
		targetAddr = strings.TrimSpace(input.Dest[0])
	}
	nodeID, err := s.nodeIDForDeviceGroup(r, input.DeviceGroupOut, input.DeviceGroupIn)
	if err != nil {
		return 0, err
	}
	tunnelID, _, err := s.app.CreateTunnel(r.Context(), userID, nodeID, input.Name, input.Protocol, listenAddr, targetAddr, input.MaxConn, 0, input.QuotaBytes, nil, true)
	if err != nil {
		return 0, err
	}
	s.patchTunnelV1Fields(r, tunnelID, input.DeviceGroupIn, input.DeviceGroupOut, listenAddr, targetAddr, input.Config)
	return tunnelID, nil
}

func (s *apiServer) updateV1Forward(r *http.Request, userID, tunnelID int64) error {
	current, err := s.getV1Forward(r, userID, tunnelID)
	if err != nil {
		return err
	}
	var input struct {
		Name           string          `json:"name"`
		ListenPort     int             `json:"listen_port"`
		ListenAddr     string          `json:"listen_addr"`
		TargetAddr     string          `json:"target_addr"`
		Dest           []string        `json:"dest"`
		Protocol       domain.Protocol `json:"protocol"`
		DeviceGroupIn  int64           `json:"device_group_in"`
		DeviceGroupOut int64           `json:"device_group_out"`
		MaxConn        int             `json:"max_conn"`
		QuotaBytes     int64           `json:"quota_bytes"`
		Config         any             `json:"config"`
	}
	if !decodeV1JSON(r, &input) {
		return errors.New("invalid body")
	}
	if input.Protocol == "" {
		if currentProtocol, ok := current["protocol"].(domain.Protocol); ok {
			input.Protocol = currentProtocol
		} else if currentProtocol, ok := current["protocol"].(string); ok {
			input.Protocol = domain.Protocol(currentProtocol)
		} else {
			input.Protocol = domain.ProtocolTCP
		}
	}
	listenAddr := strings.TrimSpace(input.ListenAddr)
	if listenAddr == "" && input.ListenPort > 0 {
		listenAddr = ":" + strconv.Itoa(input.ListenPort)
	}
	if listenAddr == "" {
		if v, ok := current["listen_addr"].(string); ok {
			listenAddr = v
		}
	}
	targetAddr := strings.TrimSpace(input.TargetAddr)
	if targetAddr == "" && len(input.Dest) > 0 {
		targetAddr = strings.TrimSpace(input.Dest[0])
	}
	if targetAddr == "" {
		if v, ok := current["target_addr"].(string); ok {
			targetAddr = v
		}
	}
	nodeID := int64(0)
	if input.DeviceGroupOut > 0 || input.DeviceGroupIn > 0 {
		var err error
		nodeID, err = s.nodeIDForDeviceGroup(r, input.DeviceGroupOut, input.DeviceGroupIn)
		if err != nil {
			return err
		}
	}
	if nodeID <= 0 {
		if v, ok := current["node_id"].(int64); ok {
			nodeID = v
		}
	}
	tx, err := s.app.DB().BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	listenHost, listenPort := splitHostPortLoose(listenAddr)
	targetHost, targetPort := splitHostPortLoose(targetAddr)
	configJSON := "{}"
	if input.Config != nil {
		raw, _ := json.Marshal(input.Config)
		configJSON = string(raw)
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE tunnels SET name=?, protocol=?, listen_addr=?, target_addr=?, listen_host=?, listen_port=?, target_host=?, target_port=?, device_group_in_id=?, device_group_out_id=?, config_json=?, node_id=?, max_conn=?, quota_bytes=?, updated_at=? WHERE id=? AND user_id=?`,
		firstNonEmpty(input.Name, current["name"].(string)), input.Protocol, listenAddr, targetAddr, listenHost, listenPort, targetHost, targetPort, nullIfZero(input.DeviceGroupIn), nullIfZero(input.DeviceGroupOut), configJSON, nodeID, input.MaxConn, input.QuotaBytes, time.Now().UTC(), tunnelID, userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE forward_services SET protocol=?, listen_addr=?, target_addr=?, listen_port=?, device_group_in_id=?, device_group_out_id=?, config_json=?, node_id=?, max_conn=?, quota_bytes=?, updated_at=? WHERE tunnel_id=? AND user_id=?`,
		input.Protocol, listenAddr, targetAddr, listenPort, nullIfZero(input.DeviceGroupIn), nullIfZero(input.DeviceGroupOut), configJSON, nodeID, input.MaxConn, input.QuotaBytes, time.Now().UTC(), tunnelID, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *apiServer) deleteV1Forward(r *http.Request, userID, tunnelID int64) error {
	serviceKey, err := s.serviceKeyForUserTunnel(r, userID, tunnelID)
	if err != nil {
		return err
	}
	return s.app.DeleteService(r.Context(), serviceKey)
}

func (s *apiServer) getV1Forward(r *http.Request, userID, tunnelID int64) (map[string]any, error) {
	rows, err := s.app.DB().QueryContext(r.Context(), `SELECT id, user_id, node_id, name, protocol, listen_addr, target_addr, listen_host, listen_port, target_host, target_port, device_group_in_id, device_group_out_id, COALESCE(CAST(config_json AS CHAR), ''), folder, show_order, max_conn, speed_limit_kb, quota_bytes, used_bytes, expires_at, auto_pause_on_limit, status, created_at, updated_at FROM tunnels WHERE id = ? AND user_id = ?`, tunnelID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	var tunnel domain.Tunnel
	var expires sql.NullTime
	var autoPause bool
	var listenPort sql.NullInt64
	var targetPort sql.NullInt64
	var deviceGroupIn sql.NullInt64
	var deviceGroupOut sql.NullInt64
	if err := rows.Scan(&tunnel.ID, &tunnel.UserID, &tunnel.NodeID, &tunnel.Name, &tunnel.Protocol, &tunnel.ListenAddr, &tunnel.TargetAddr, &tunnel.ListenHost, &listenPort, &tunnel.TargetHost, &targetPort, &deviceGroupIn, &deviceGroupOut, &tunnel.ConfigJSON, &tunnel.Folder, &tunnel.ShowOrder, &tunnel.MaxConn, &tunnel.SpeedLimitKB, &tunnel.QuotaBytes, &tunnel.UsedBytes, &expires, &autoPause, &tunnel.Status, &tunnel.CreatedAt, &tunnel.UpdatedAt); err != nil {
		return nil, err
	}
	tunnel.ListenPort = sqlNullIntPtr(listenPort)
	tunnel.TargetPort = sqlNullIntPtr(targetPort)
	tunnel.DeviceGroupInID = sqlNullInt64Ptr(deviceGroupIn)
	tunnel.DeviceGroupOutID = sqlNullInt64Ptr(deviceGroupOut)
	tunnel.AutoPauseOnLimit = autoPause
	if expires.Valid {
		tunnel.ExpiresAt = &expires.Time
	}
	return v1ForwardFromTunnel(tunnel), nil
}

func decodeV1JSON(r *http.Request, out any) bool {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxJSONBodyBytes+1))
	return decoder.Decode(out) == nil
}

func sqlNullIntPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	value := int(v.Int64)
	return &value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func nullIfZero(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

func v1PathID(path, prefix string) (int64, bool) {
	if !strings.HasPrefix(path, prefix) {
		return 0, false
	}
	value := strings.TrimPrefix(path, prefix)
	value = strings.Trim(value, "/")
	if value == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func (s *apiServer) listV1DeviceGroups(r *http.Request) ([]domain.DeviceGroup, error) {
	rows, err := s.app.DB().QueryContext(r.Context(), `SELECT id, name, type, node_id, ratio, connect_host, port_range_start, port_range_end, COALESCE(CAST(config_json AS CHAR), ''), show_order, display_num, hide_status, status, created_at, updated_at, deleted_at FROM device_groups WHERE deleted_at IS NULL ORDER BY show_order ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []domain.DeviceGroup
	for rows.Next() {
		var item domain.DeviceGroup
		var nodeID sql.NullInt64
		var start sql.NullInt64
		var end sql.NullInt64
		var deleted sql.NullTime
		if err := rows.Scan(&item.ID, &item.Name, &item.Type, &nodeID, &item.Ratio, &item.ConnectHost, &start, &end, &item.ConfigJSON, &item.ShowOrder, &item.DisplayNum, &item.HideStatus, &item.Status, &item.CreatedAt, &item.UpdatedAt, &deleted); err != nil {
			return nil, err
		}
		item.NodeID = sqlNullInt64Ptr(nodeID)
		if start.Valid {
			v := int(start.Int64)
			item.PortRangeStart = &v
		}
		if end.Valid {
			v := int(end.Int64)
			item.PortRangeEnd = &v
		}
		if deleted.Valid {
			item.DeletedAt = &deleted.Time
		}
		groups = append(groups, item)
	}
	return groups, rows.Err()
}

func (s *apiServer) nodeIDForDeviceGroup(r *http.Request, ids ...int64) (int64, error) {
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		var nodeID sql.NullInt64
		err := s.app.DB().QueryRowContext(r.Context(), `SELECT node_id FROM device_groups WHERE id = ? AND deleted_at IS NULL`, id).Scan(&nodeID)
		if err != nil {
			return 0, err
		}
		if nodeID.Valid && nodeID.Int64 > 0 {
			return nodeID.Int64, nil
		}
	}
	var fallback int64
	err := s.app.DB().QueryRowContext(r.Context(), `SELECT id FROM nodes ORDER BY id ASC LIMIT 1`).Scan(&fallback)
	return fallback, err
}

func (s *apiServer) patchTunnelV1Fields(r *http.Request, tunnelID, groupIn, groupOut int64, listenAddr, targetAddr string, config any) {
	listenHost, listenPort := splitHostPortLoose(listenAddr)
	targetHost, targetPort := splitHostPortLoose(targetAddr)
	configJSON := "{}"
	if config != nil {
		raw, _ := json.Marshal(config)
		configJSON = string(raw)
	} else if targetAddr != "" {
		raw, _ := json.Marshal(map[string]any{"dest": []string{targetAddr}})
		configJSON = string(raw)
	}
	var in any
	if groupIn > 0 {
		in = groupIn
	}
	var out any
	if groupOut > 0 {
		out = groupOut
	}
	_, _ = s.app.DB().ExecContext(r.Context(), `UPDATE tunnels SET listen_host=?, listen_port=?, target_host=?, target_port=?, device_group_in_id=?, device_group_out_id=?, config_json=? WHERE id=?`, listenHost, listenPort, targetHost, targetPort, in, out, configJSON, tunnelID)
	_, _ = s.app.DB().ExecContext(r.Context(), `UPDATE forward_services SET listen_port=?, device_group_in_id=?, device_group_out_id=?, config_json=? WHERE tunnel_id=?`, listenPort, in, out, configJSON, tunnelID)
}

func (s *apiServer) serviceKeyForUserTunnel(r *http.Request, userID, tunnelID int64) (string, error) {
	var key string
	err := s.app.DB().QueryRowContext(r.Context(), `SELECT service_key FROM forward_services WHERE tunnel_id = ? AND user_id = ?`, tunnelID, userID).Scan(&key)
	return key, err
}

func v1ForwardFromTunnel(tunnel domain.Tunnel) map[string]any {
	listenPort := 0
	if tunnel.ListenPort != nil {
		listenPort = *tunnel.ListenPort
	} else if _, port := splitHostPortLoose(tunnel.ListenAddr); port != nil {
		listenPort = *port
	}
	config := tunnel.ConfigJSON
	if config == "" {
		raw, _ := json.Marshal(map[string]any{"dest": []string{tunnel.TargetAddr}})
		config = string(raw)
	}
	return map[string]any{
		"id":                 tunnel.ID,
		"name":               tunnel.Name,
		"uid":                tunnel.UserID,
		"listen_port":        listenPort,
		"device_group_in":    tunnel.DeviceGroupInID,
		"device_group_out":   tunnel.DeviceGroupOutID,
		"traffic_used":       tunnel.UsedBytes,
		"config":             config,
		"status":             tunnel.Status,
		"display_updated_at": tunnel.UpdatedAt.Format("2006-01-02 15:04:05"),
		"node_id":            tunnel.NodeID,
		"listen_addr":        tunnel.ListenAddr,
		"target_addr":        tunnel.TargetAddr,
		"protocol":           tunnel.Protocol,
	}
}

func splitHostPortLoose(addr string) (string, *int) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", nil
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		if strings.HasPrefix(addr, ":") {
			port, parseErr := strconv.Atoi(strings.TrimPrefix(addr, ":"))
			if parseErr == nil {
				return "", &port
			}
		}
		return addr, nil
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return host, nil
	}
	return host, &port
}
