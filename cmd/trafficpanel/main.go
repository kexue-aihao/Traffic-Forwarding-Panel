package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"trafficpanel/internal/agent"
	"trafficpanel/internal/config"
	"trafficpanel/internal/domain"
	"trafficpanel/internal/payment"
	"trafficpanel/internal/service"
	"trafficpanel/internal/store/mysql"
)

type apiServer struct {
	app *service.App
	cfg config.Config
}

type localForwarder struct {
	client        *agent.Client
	cfg           config.Config
	mu            sync.Mutex
	tcpListeners  map[string]net.Listener
	udpForwarders map[string]*udpForwarder
	services      map[string]domain.ForwardService
	bytesIn       map[string]*atomic.Int64
	bytesOut      map[string]*atomic.Int64
	activeConns   map[string]*atomic.Int64
}

type udpForwarder struct {
	service     domain.ForwardService
	listenConn  *net.UDPConn
	targetAddr  *net.UDPAddr
	idleTimeout time.Duration
	bytesIn     *atomic.Int64
	bytesOut    *atomic.Int64
	activeConns *atomic.Int64
	closed      atomic.Bool

	mu       sync.Mutex
	sessions map[string]*udpSession
	done     chan struct{}
	once     sync.Once
}

type udpSession struct {
	clientAddr *net.UDPAddr
	targetConn *net.UDPConn
	lastSeen   atomic.Int64
	once       sync.Once
}

const (
	maxJSONBodyBytes          = 1 << 20
	maxPaymentNotifyBodyBytes = 64 << 10
)

func main() {
	cfg := config.Load()
	if cfg.Mode == "node" {
		runNode(cfg)
		return
	}
	runServer(cfg)
}

func runServer(cfg config.Config) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := mysql.Open(cfg.DatabaseDSN)
	if err != nil {
		log.Fatalf("open mysql: %v", err)
	}
	defer store.Close()
	payments := payment.NewRegistry(
		payment.NewSignedFormProvider("epay", "Epay", cfg.EpayAPIURL, cfg.EpayPID, cfg.EpayKey, cfg.EpayType),
		payment.NewSignedFormProvider("bepusdt", "BEpusdt", cfg.BEpusdtAPIURL, cfg.BEpusdtPID, cfg.BEpusdtKey, cfg.BEpusdtType),
	)
	app := service.New(cfg, store, payments)
	if err := app.EnsureSchema(ctx); err != nil {
		log.Fatalf("ensure schema: %v", err)
	}
	if err := app.Bootstrap(ctx); err != nil {
		log.Fatalf("bootstrap: %v", err)
	}
	server := &apiServer{app: app, cfg: cfg}
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("trafficpanel server listening on %s", cfg.HTTPAddr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}

func runNode(cfg config.Config) {
	if cfg.AgentNodeID <= 0 || strings.TrimSpace(cfg.AgentNodeSecret) == "" {
		log.Fatal("TP_AGENT_NODE_ID and TP_AGENT_NODE_SECRET are required in node mode")
	}
	ctx := context.Background()
	client := agent.New(cfg.AgentServerURL, cfg.AgentNodeID, cfg.AgentNodeSecret)
	forwarder := newLocalForwarder(client, cfg)
	node := domain.Node{
		ID:     cfg.AgentNodeID,
		Name:   cfg.AgentNodeName,
		Host:   cfg.AgentNodeHost,
		Port:   cfg.AgentNodePort,
		Secret: cfg.AgentNodeSecret,
		Status: domain.NodeOnline,
	}
	if err := client.Register(ctx, node); err != nil {
		log.Printf("initial node registration failed: %v", err)
	}
	go forwarder.commandLoop(ctx)
	go forwarder.reportLoop(ctx)
	log.Printf("trafficpanel node started: node_id=%d server=%s", cfg.AgentNodeID, cfg.AgentServerURL)
	select {}
}

func (s *apiServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/api/v1/", s.handleAPIV1)
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/api/admin/login", s.handleAdminLogin)
	mux.HandleFunc("/api/user/login", s.handleUserLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/admin/summary", s.requireAdmin(s.handleAdminSummary))
	mux.HandleFunc("/api/admin/users", s.requireAdmin(s.handleAdminUsers))
	mux.HandleFunc("/api/admin/nodes", s.requireAdmin(s.handleAdminNodes))
	mux.HandleFunc("/api/admin/tunnels", s.requireAdmin(s.handleAdminTunnels))
	mux.HandleFunc("/api/admin/services", s.requireAdmin(s.handleAdminServices))
	mux.HandleFunc("/api/admin/payments/channels", s.requireAdmin(s.handlePaymentChannels))
	mux.HandleFunc("/api/admin/payments/orders", s.requireAdmin(s.handlePaymentOrders))
	mux.HandleFunc("/api/user/me", s.requireUser(s.handleUserMe))
	mux.HandleFunc("/api/user/tunnels", s.requireUser(s.handleUserTunnels))
	mux.HandleFunc("/api/user/orders", s.requireUser(s.handleUserOrders))
	mux.HandleFunc("/api/user/pay", s.requireUser(s.handleUserPay))
	mux.HandleFunc("/api/pay/", s.handlePayNotify)
	mux.HandleFunc("/api/nodes/register", s.handleNodeRegister)
	mux.HandleFunc("/api/nodes/report", s.handleNodeReport)
	mux.HandleFunc("/api/nodes/commands", s.handleNodeCommands)
	mux.HandleFunc("/api/nodes/commands/ack", s.handleNodeCommandAck)
	return mux
}

func (s *apiServer) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.app.Ready(ctx); err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (s *apiServer) handlePage(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "/app":
		renderTemplate(w, homeTemplate, map[string]any{"AppName": s.cfg.AppName, "RegisterEnabled": s.app.PublicRegisterEnabled()})
	case "/admin":
		renderTemplate(w, adminTemplate, map[string]any{"AppName": s.cfg.AppName})
	case "/user":
		renderTemplate(w, userTemplate, map[string]any{"AppName": s.cfg.AppName})
	default:
		http.NotFound(w, r)
	}
}

func (s *apiServer) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	token, err := s.app.LoginAdmin(r.Context(), input.Username, input.Password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]string{"token": token})
}

func (s *apiServer) handleUserLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	token, err := s.app.LoginUser(r.Context(), input.Username, input.Password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]string{"token": token})
}

func (s *apiServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	token := bearerToken(r)
	if token != "" {
		_ = s.app.Logout(r.Context(), token)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) handleAdminSummary(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	stats, err := s.app.DashboardStats(r.Context())
	writeResult(w, stats, err)
}

func (s *apiServer) handleAdminUsers(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	switch r.Method {
	case http.MethodGet:
		users, err := s.app.ListUsers(r.Context())
		writeResult(w, users, err)
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
		writeResult(w, map[string]int64{"id": id}, err)
	default:
		methodNotAllowed(w)
	}
}

func (s *apiServer) handleAdminNodes(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	switch r.Method {
	case http.MethodGet:
		nodes, err := s.app.ListNodes(r.Context())
		writeResult(w, nodes, err)
	case http.MethodPost:
		var input struct {
			Name   string `json:"name"`
			Host   string `json:"host"`
			Port   int    `json:"port"`
			Secret string `json:"secret"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		node, err := s.app.CreateNode(r.Context(), input.Name, input.Host, input.Port, input.Secret)
		writeResult(w, node, err)
	default:
		methodNotAllowed(w)
	}
}

func (s *apiServer) handleAdminTunnels(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	switch r.Method {
	case http.MethodGet:
		items, err := s.app.ListTunnels(r.Context())
		writeResult(w, items, err)
	case http.MethodPost:
		var input struct {
			UserID           int64           `json:"user_id"`
			NodeID           int64           `json:"node_id"`
			Name             string          `json:"name"`
			Protocol         domain.Protocol `json:"protocol"`
			ListenAddr       string          `json:"listen_addr"`
			TargetAddr       string          `json:"target_addr"`
			MaxConn          int             `json:"max_conn"`
			SpeedLimitKB     int             `json:"speed_limit_kb"`
			QuotaBytes       int64           `json:"quota_bytes"`
			AutoPauseOnLimit bool            `json:"auto_pause_on_limit"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if input.Protocol == "" {
			input.Protocol = domain.ProtocolTCP
		}
		tunnelID, key, err := s.app.CreateTunnel(r.Context(), input.UserID, input.NodeID, input.Name, input.Protocol, input.ListenAddr, input.TargetAddr, input.MaxConn, input.SpeedLimitKB, input.QuotaBytes, nil, input.AutoPauseOnLimit)
		writeResult(w, map[string]any{"id": tunnelID, "service_key": key}, err)
	default:
		methodNotAllowed(w)
	}
}

func (s *apiServer) handleAdminServices(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	if r.Method == http.MethodGet {
		services, err := s.app.ListForwardServices(r.Context())
		writeResult(w, services, err)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var input struct {
		Action     string `json:"action"`
		ServiceKey string `json:"service_key"`
		Reason     string `json:"reason"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	var err error
	switch input.Action {
	case "pause":
		err = s.app.PauseService(r.Context(), input.ServiceKey, input.Reason)
	case "resume":
		err = s.app.ResumeService(r.Context(), input.ServiceKey)
	case "delete":
		err = s.app.DeleteService(r.Context(), input.ServiceKey)
	default:
		err = errors.New("unknown action")
	}
	writeResult(w, map[string]string{"status": "ok"}, err)
}

func (s *apiServer) handlePaymentChannels(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	channels, err := s.app.ListPaymentChannels(r.Context())
	writeResult(w, channels, err)
}

func (s *apiServer) handlePaymentOrders(w http.ResponseWriter, r *http.Request, _ *domain.Session) {
	orders, err := s.app.ListPaymentOrders(r.Context())
	writeResult(w, orders, err)
}

func (s *apiServer) handleUserMe(w http.ResponseWriter, r *http.Request, session *domain.Session) {
	user, err := s.app.GetUserByID(r.Context(), session.ActorID)
	writeResult(w, user, err)
}

func (s *apiServer) handleUserTunnels(w http.ResponseWriter, r *http.Request, session *domain.Session) {
	tunnels, err := s.app.ListTunnelsByUser(r.Context(), session.ActorID)
	writeResult(w, tunnels, err)
}

func (s *apiServer) handleUserOrders(w http.ResponseWriter, r *http.Request, session *domain.Session) {
	orders, err := s.app.ListPaymentOrdersByUser(r.Context(), session.ActorID)
	writeResult(w, orders, err)
}

func (s *apiServer) handleUserPay(w http.ResponseWriter, r *http.Request, session *domain.Session) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var input struct {
		Channel     string `json:"channel"`
		AmountCents int64  `json:"amount_cents"`
		Subject     string `json:"subject"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Subject == "" {
		input.Subject = "Traffic panel recharge"
	}
	order, err := s.app.CreatePaymentOrder(r.Context(), session.ActorID, input.Channel, input.AmountCents, input.Subject)
	writeResult(w, order, err)
}

func (s *apiServer) handlePayNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/pay/")
	channel := strings.TrimSuffix(path, "/notify")
	body, err := readLimitedBody(r, maxPaymentNotifyBodyBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	form := r.URL.Query()
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/x-www-form-urlencoded") {
		if parsed, err := url.ParseQuery(string(body)); err == nil {
			for key, values := range parsed {
				form[key] = append(form[key], values...)
			}
		}
	}
	order, err := s.app.HandlePaymentNotify(r.Context(), channel, body, form)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, order)
}

func (s *apiServer) handleNodeRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, nodeID, ok := s.readAndAuthNode(w, r)
	if !ok {
		return
	}
	var input domain.Node
	if err := json.Unmarshal(body, &input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err := s.app.SyncNodeRegistration(r.Context(), nodeID, input.Name, input.Host, input.Port)
	writeResult(w, map[string]string{"status": "online"}, err)
}

func (s *apiServer) handleNodeReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, nodeID, ok := s.readAndAuthNode(w, r)
	if !ok {
		return
	}
	var report domain.UsageReport
	if err := json.Unmarshal(body, &report); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	report.NodeID = nodeID
	if report.ReportedAt.IsZero() {
		report.ReportedAt = time.Now().UTC()
	}
	result, err := s.app.SaveUsageReport(r.Context(), report)
	writeResult(w, result, err)
}

func (s *apiServer) handleNodeCommands(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	nodeID, err := strconv.ParseInt(r.Header.Get("X-Node-ID"), 10, 64)
	if err != nil || nodeID <= 0 {
		http.Error(w, "invalid node id", http.StatusUnauthorized)
		return
	}
	if _, err := s.app.AuthenticateNodeRequest(r.Context(), nodeID, []byte(strconv.FormatInt(nodeID, 10)), r.Header.Get("X-Node-Sign")); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	_ = s.app.RegisterNodeHeartbeat(r.Context(), nodeID)
	commands, err := s.app.PendingNodeCommands(r.Context(), nodeID)
	writeResult(w, commands, err)
}

func (s *apiServer) handleNodeCommandAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, nodeID, ok := s.readAndAuthNode(w, r)
	if !ok {
		return
	}
	var input struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err := s.app.AckNodeCommands(r.Context(), nodeID, input.IDs)
	writeResult(w, map[string]string{"status": "acked"}, err)
}

func (s *apiServer) readAndAuthNode(w http.ResponseWriter, r *http.Request) ([]byte, int64, bool) {
	nodeID, err := strconv.ParseInt(r.Header.Get("X-Node-ID"), 10, 64)
	if err != nil || nodeID <= 0 {
		http.Error(w, "invalid node id", http.StatusUnauthorized)
		return nil, 0, false
	}
	body, err := readLimitedBody(r, maxJSONBodyBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, 0, false
	}
	if _, err := s.app.AuthenticateNodeRequest(r.Context(), nodeID, body, r.Header.Get("X-Node-Sign")); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return nil, 0, false
	}
	return body, nodeID, true
}

func (s *apiServer) requireAdmin(next func(http.ResponseWriter, *http.Request, *domain.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, err := s.app.RequireSession(r.Context(), bearerToken(r), domain.ActorAdmin)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next(w, r, session)
	}
}

func (s *apiServer) requireUser(next func(http.ResponseWriter, *http.Request, *domain.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, err := s.app.RequireSession(r.Context(), bearerToken(r), domain.ActorUser)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next(w, r, session)
	}
}

func newLocalForwarder(client *agent.Client, cfg config.Config) *localForwarder {
	return &localForwarder{
		client:        client,
		cfg:           cfg,
		tcpListeners:  make(map[string]net.Listener),
		udpForwarders: make(map[string]*udpForwarder),
		services:      make(map[string]domain.ForwardService),
		bytesIn:       make(map[string]*atomic.Int64),
		bytesOut:      make(map[string]*atomic.Int64),
		activeConns:   make(map[string]*atomic.Int64),
	}
}

func (f *localForwarder) commandLoop(ctx context.Context) {
	ticker := time.NewTicker(f.cfg.NodePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			commands, err := f.client.FetchCommands(ctx)
			if err != nil {
				log.Printf("fetch commands failed: %v", err)
				continue
			}
			var ack []int64
			for _, command := range commands {
				if err := f.applyCommand(command); err != nil {
					log.Printf("apply command %d failed: %v", command.ID, err)
					continue
				}
				ack = append(ack, command.ID)
			}
			if len(ack) > 0 {
				if err := f.client.AcknowledgeCommands(ctx, ack); err != nil {
					log.Printf("ack commands failed: %v", err)
				}
			}
		}
	}
}

func (f *localForwarder) reportLoop(ctx context.Context) {
	ticker := time.NewTicker(f.cfg.NodeReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.mu.Lock()
			keys := make([]string, 0, len(f.services))
			for key := range f.services {
				keys = append(keys, key)
			}
			f.mu.Unlock()
			for _, key := range keys {
				in, out, active := f.counters(key)
				if in == nil || out == nil || active == nil {
					continue
				}
				report := domain.UsageReport{
					NodeID:     f.cfg.AgentNodeID,
					ServiceKey: key,
					BytesIn:    in.Swap(0),
					BytesOut:   out.Swap(0),
					ActiveConn: int(active.Load()),
					ReportedAt: time.Now().UTC(),
				}
				if report.BytesIn == 0 && report.BytesOut == 0 && report.ActiveConn == 0 {
					continue
				}
				if err := f.client.ReportUsage(ctx, report); err != nil {
					log.Printf("report usage failed: %v", err)
				}
			}
		}
	}
}

func (f *localForwarder) applyCommand(command domain.NodeCommand) error {
	var payload domain.NodeCommandPayload
	if command.PayloadJSON != "" {
		if err := json.Unmarshal([]byte(command.PayloadJSON), &payload); err != nil {
			return err
		}
	}
	switch command.Type {
	case domain.CommandUpsertService, domain.CommandResumeService:
		return f.startService(payload.Service)
	case domain.CommandPauseService, domain.CommandDeleteService:
		f.stopService(payload.Service.ServiceKey)
		return nil
	case domain.CommandSyncState:
		return nil
	default:
		return fmt.Errorf("unknown command type: %s", command.Type)
	}
}

func (f *localForwarder) startService(service domain.ForwardService) error {
	if service.ServiceKey == "" || service.ListenAddr == "" || service.TargetAddr == "" {
		return errors.New("missing service key/listen/target")
	}
	f.stopService(service.ServiceKey)
	switch service.Protocol {
	case domain.ProtocolTCP:
		return f.startTCPService(service)
	case domain.ProtocolUDP:
		return f.startUDPService(service)
	default:
		return fmt.Errorf("unsupported protocol: %s", service.Protocol)
	}
}

func (f *localForwarder) startTCPService(service domain.ForwardService) error {
	ln, err := net.Listen("tcp", service.ListenAddr)
	if err != nil {
		return err
	}
	bytesIn := &atomic.Int64{}
	bytesOut := &atomic.Int64{}
	activeConns := &atomic.Int64{}
	f.mu.Lock()
	f.tcpListeners[service.ServiceKey] = ln
	f.services[service.ServiceKey] = service
	f.bytesIn[service.ServiceKey] = bytesIn
	f.bytesOut[service.ServiceKey] = bytesOut
	f.activeConns[service.ServiceKey] = activeConns
	f.mu.Unlock()
	go f.acceptLoop(service, ln)
	log.Printf("tcp service %s listening on %s -> %s", service.ServiceKey, service.ListenAddr, service.TargetAddr)
	return nil
}

func (f *localForwarder) startUDPService(service domain.ForwardService) error {
	listenAddr, err := net.ResolveUDPAddr("udp", service.ListenAddr)
	if err != nil {
		return err
	}
	targetAddr, err := net.ResolveUDPAddr("udp", service.TargetAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return err
	}
	bytesIn := &atomic.Int64{}
	bytesOut := &atomic.Int64{}
	activeConns := &atomic.Int64{}
	forwarder := newUDPForwarder(service, conn, targetAddr, f.cfg.AgentUDPIdleTimeout, bytesIn, bytesOut, activeConns)
	f.mu.Lock()
	f.udpForwarders[service.ServiceKey] = forwarder
	f.services[service.ServiceKey] = service
	f.bytesIn[service.ServiceKey] = bytesIn
	f.bytesOut[service.ServiceKey] = bytesOut
	f.activeConns[service.ServiceKey] = activeConns
	f.mu.Unlock()
	go forwarder.run()
	log.Printf("udp service %s listening on %s -> %s", service.ServiceKey, service.ListenAddr, service.TargetAddr)
	return nil
}

func (f *localForwarder) stopService(serviceKey string) {
	f.mu.Lock()
	ln := f.tcpListeners[serviceKey]
	udp := f.udpForwarders[serviceKey]
	delete(f.tcpListeners, serviceKey)
	delete(f.udpForwarders, serviceKey)
	delete(f.services, serviceKey)
	delete(f.bytesIn, serviceKey)
	delete(f.bytesOut, serviceKey)
	delete(f.activeConns, serviceKey)
	f.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if udp != nil {
		udp.close()
	}
}

func (f *localForwarder) counters(serviceKey string) (*atomic.Int64, *atomic.Int64, *atomic.Int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bytesIn[serviceKey], f.bytesOut[serviceKey], f.activeConns[serviceKey]
}

func (f *localForwarder) acceptLoop(service domain.ForwardService, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go f.handleConn(service, conn)
	}
}

func (f *localForwarder) handleConn(service domain.ForwardService, inbound net.Conn) {
	defer inbound.Close()
	_, _, counter := f.counters(service.ServiceKey)
	if counter != nil {
		current := counter.Add(1)
		if service.MaxConn > 0 && int(current) > service.MaxConn {
			counter.Add(-1)
			log.Printf("tcp service %s max connections reached: %d", service.ServiceKey, service.MaxConn)
			return
		}
		defer counter.Add(-1)
	}
	outbound, err := net.DialTimeout("tcp", service.TargetAddr, 10*time.Second)
	if err != nil {
		log.Printf("dial target failed: %v", err)
		return
	}
	defer outbound.Close()
	bytesIn, bytesOut, _ := f.counters(service.ServiceKey)
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(outbound, inbound)
		if bytesIn != nil {
			bytesIn.Add(n)
		}
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(inbound, outbound)
		if bytesOut != nil {
			bytesOut.Add(n)
		}
		done <- struct{}{}
	}()
	<-done
}

func newUDPForwarder(service domain.ForwardService, listenConn *net.UDPConn, targetAddr *net.UDPAddr, idleTimeout time.Duration, bytesIn, bytesOut, activeConns *atomic.Int64) *udpForwarder {
	if idleTimeout <= 0 {
		idleTimeout = 2 * time.Minute
	}
	return &udpForwarder{
		service:     service,
		listenConn:  listenConn,
		targetAddr:  targetAddr,
		idleTimeout: idleTimeout,
		bytesIn:     bytesIn,
		bytesOut:    bytesOut,
		activeConns: activeConns,
		sessions:    make(map[string]*udpSession),
		done:        make(chan struct{}),
	}
}

func (u *udpForwarder) run() {
	go u.cleanupLoop()
	buf := make([]byte, 64*1024)
	for {
		n, clientAddr, err := u.listenConn.ReadFromUDP(buf)
		if err != nil {
			u.close()
			return
		}
		session, err := u.getSession(clientAddr)
		if err != nil {
			log.Printf("udp service %s session failed: %v", u.service.ServiceKey, err)
			continue
		}
		session.lastSeen.Store(time.Now().UnixNano())
		written, err := session.targetConn.Write(buf[:n])
		if err != nil {
			log.Printf("udp service %s target write failed: %v", u.service.ServiceKey, err)
			u.removeSession(clientAddr.String(), session)
			continue
		}
		if u.bytesIn != nil {
			u.bytesIn.Add(int64(written))
		}
	}
}

func (u *udpForwarder) getSession(clientAddr *net.UDPAddr) (*udpSession, error) {
	if u.closed.Load() {
		return nil, net.ErrClosed
	}
	key := clientAddr.String()
	u.mu.Lock()
	if session := u.sessions[key]; session != nil {
		u.mu.Unlock()
		return session, nil
	}
	if u.service.MaxConn > 0 && len(u.sessions) >= u.service.MaxConn {
		u.mu.Unlock()
		return nil, fmt.Errorf("udp max sessions reached: %d", u.service.MaxConn)
	}
	u.mu.Unlock()

	targetConn, err := net.DialUDP("udp", nil, u.targetAddr)
	if err != nil {
		return nil, err
	}
	if u.closed.Load() {
		_ = targetConn.Close()
		return nil, net.ErrClosed
	}
	session := &udpSession{
		clientAddr: cloneUDPAddr(clientAddr),
		targetConn: targetConn,
	}
	session.lastSeen.Store(time.Now().UnixNano())

	u.mu.Lock()
	if u.closed.Load() {
		u.mu.Unlock()
		_ = targetConn.Close()
		return nil, net.ErrClosed
	}
	if existing := u.sessions[key]; existing != nil {
		u.mu.Unlock()
		_ = targetConn.Close()
		return existing, nil
	}
	if u.service.MaxConn > 0 && len(u.sessions) >= u.service.MaxConn {
		u.mu.Unlock()
		_ = targetConn.Close()
		return nil, fmt.Errorf("udp max sessions reached: %d", u.service.MaxConn)
	}
	u.sessions[key] = session
	if u.activeConns != nil {
		u.activeConns.Add(1)
	}
	u.mu.Unlock()

	go u.readTarget(key, session)
	return session, nil
}

func (u *udpForwarder) readTarget(key string, session *udpSession) {
	buf := make([]byte, 64*1024)
	for {
		n, err := session.targetConn.Read(buf)
		if err != nil {
			u.removeSession(key, session)
			return
		}
		written, err := u.listenConn.WriteToUDP(buf[:n], session.clientAddr)
		if err != nil {
			u.removeSession(key, session)
			return
		}
		session.lastSeen.Store(time.Now().UnixNano())
		if u.bytesOut != nil {
			u.bytesOut.Add(int64(written))
		}
	}
}

func (u *udpForwarder) cleanupLoop() {
	interval := u.idleTimeout / 2
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-u.done:
			return
		case <-ticker.C:
			u.closeIdleSessions(time.Now())
		}
	}
}

func (u *udpForwarder) closeIdleSessions(now time.Time) {
	cutoff := now.Add(-u.idleTimeout).UnixNano()
	u.mu.Lock()
	candidates := make(map[string]*udpSession)
	for key, session := range u.sessions {
		if session.lastSeen.Load() < cutoff {
			candidates[key] = session
		}
	}
	u.mu.Unlock()
	for key, session := range candidates {
		u.removeSession(key, session)
	}
}

func (u *udpForwarder) removeSession(key string, session *udpSession) {
	removed := false
	u.mu.Lock()
	if existing := u.sessions[key]; existing == session {
		delete(u.sessions, key)
		removed = true
	}
	u.mu.Unlock()
	if !removed {
		return
	}
	session.close()
	if u.activeConns != nil {
		u.activeConns.Add(-1)
	}
}

func (u *udpForwarder) close() {
	u.once.Do(func() {
		u.closed.Store(true)
		close(u.done)
		_ = u.listenConn.Close()
		u.mu.Lock()
		sessions := u.sessions
		u.sessions = make(map[string]*udpSession)
		u.mu.Unlock()
		for _, session := range sessions {
			session.close()
			if u.activeConns != nil {
				u.activeConns.Add(-1)
			}
		}
	})
}

func (s *udpSession) close() {
	s.once.Do(func() {
		_ = s.targetConn.Close()
	})
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}
	ip := make(net.IP, len(addr.IP))
	copy(ip, addr.IP)
	return &net.UDPAddr{IP: ip, Port: addr.Port, Zone: addr.Zone}
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	if cookie, err := r.Cookie("tp_token"); err == nil {
		return cookie.Value
	}
	return ""
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	if err := decoder.Decode(out); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func readLimitedBody(r *http.Request, maxBytes int64) ([]byte, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("request body too large")
	}
	return body, nil
}

func writeResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, value)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func renderTemplate(w http.ResponseWriter, body string, data any) {
	tpl := template.Must(template.New("page").Parse(body))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
