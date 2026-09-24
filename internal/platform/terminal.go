package platform

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/httporigin"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

type terminalBridge struct {
	browser, agent *websocket.Conn
	ready, done    chan struct{}
	once           sync.Once
}

func (s *Server) closeTerminal(id string) {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	if b := s.terminals[id]; b != nil {
		b.once.Do(func() {
			close(b.done)
			if b.browser != nil {
				b.browser.Close()
			}
			if b.agent != nil {
				b.agent.Close()
			}
		})
		delete(s.terminals, id)
	}
}
func (s *Server) browserTerminal(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	cookie, _ := r.Cookie("tfp_session")
	oid := r.PathValue("id")
	if !s.terminalAuthorized(r.Context(), oid, u.ID, digest(cookie.Value)) {
		fail(w, 403, "terminal authorization expired")
		return
	}
	op, e := s.loadOperation(r.Context(), s.Store.DB, oid)
	if e != nil {
		fail(w, 404, "operation unavailable")
		return
	}
	s.attachTerminal(w, r, false, u.ID, digest(cookie.Value), op.Kind == "shell")
}
func (s *Server) agentTerminal(w http.ResponseWriter, r *http.Request) {
	op, e := s.loadOperation(r.Context(), s.Store.DB, r.PathValue("id"))
	if e != nil || (op.Kind != "terminal" && op.Kind != "shell") || op.NodeID != r.Context().Value(nodeKey{}).(string) || op.Status != "running" || !op.ExpiresAt.After(time.Now()) || op.Claim == "" || subtle.ConstantTimeCompare([]byte(op.Claim), []byte(r.Header.Get("X-Operation-Claim"))) != 1 {
		fail(w, 403, "terminal task unavailable")
		return
	}
	s.attachTerminal(w, r, true, "", "", op.Kind == "shell")
}
func (s *Server) attachTerminal(w http.ResponseWriter, r *http.Request, agentSide bool, user, session string, shell ...bool) {
	oid := r.PathValue("id")
	interactive := len(shell) > 0 && shell[0]
	up := websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096, CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if agentSide {
			return origin == ""
		}
		expected := s.opts.Origin
		if expected == "" {
			expected = httporigin.Scheme(r, s.opts.TrustProxy) + "://" + r.Host
		}
		return origin != "" && strings.TrimRight(origin, "/") == strings.TrimRight(expected, "/")
	}}
	// Reserve a side before upgrading to reject duplicate viewers/executors.
	s.terminalMu.Lock()
	b := s.terminals[oid]
	if b == nil {
		if len(s.terminals) >= 64 {
			s.terminalMu.Unlock()
			fail(w, 503, "terminal capacity reached")
			return
		}
		b = &terminalBridge{ready: make(chan struct{}), done: make(chan struct{})}
		s.terminals[oid] = b
	}
	if agentSide && b.agent != nil || !agentSide && b.browser != nil {
		s.terminalMu.Unlock()
		fail(w, 409, "terminal already attached")
		return
	}
	conn, e := up.Upgrade(w, r, nil)
	if e != nil {
		s.terminalMu.Unlock()
		return
	}
	conn.SetReadLimit(16384)
	if interactive {
		conn.SetReadLimit(32768)
	}
	if agentSide {
		b.agent = conn
	} else {
		b.browser = conn
	}
	if b.agent != nil && b.browser != nil {
		close(b.ready)
	}
	s.terminalMu.Unlock()
	defer s.closeTerminal(oid)
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-b.ready:
	case <-timer.C:
		return
	case <-b.done:
		return
	case <-r.Context().Done():
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Revalidate live cookie sessions, password grants and cancellation every second.
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-b.done:
				return
			case <-tick.C:
				valid := true
				if !agentSide {
					valid = s.terminalAuthorized(ctx, oid, user, session)
				} else {
					op, e := s.loadOperation(ctx, s.Store.DB, oid)
					valid = e == nil && op.Status == "running" && op.ExpiresAt.After(time.Now())
				}
				if !valid {
					s.closeTerminal(oid)
					return
				}
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(3*time.Second))
			}
		}
	}()
	count := 0
	for {
		var msg contract.TerminalMessage
		if e = conn.ReadJSON(&msg); e != nil {
			return
		}
		target := b.browser
		if !agentSide {
			if interactive {
				if !contract.ValidShellInput(msg) || !s.terminalAuthorized(ctx, oid, user, session) {
					return
				}
				target = b.agent
			} else {
				if msg.Type != "command" || !validCommand(msg.Command) || count >= 128 {
					return
				}
				count++
				if !s.terminalAuthorized(ctx, oid, user, session) {
					return
				}
				e = s.Store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
					var status string
					if e := tx.QueryRowContext(ctx, s.q("SELECT status FROM cp_node_operations WHERE id=?"), oid).Scan(&status); e != nil {
						return e
					}
					if status != "running" {
						return errors.New("terminal closed")
					}
					_, e := tx.ExecContext(ctx, s.q("INSERT INTO cp_terminal_commands(id,operation_id,command,created_at) VALUES(?,?,?,?)"), id(), oid, msg.Command, time.Now().Unix())
					return e
				})
				if e != nil {
					return
				}
				msg = contract.TerminalMessage{Type: "command", Command: msg.Command}
				target = b.agent
			}
		} else if msg.Type != "output" && msg.Type != "exit" {
			return
		}
		target.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if e = target.WriteJSON(msg); e != nil {
			return
		}
	}
}
func (s *Server) terminalCommands(w http.ResponseWriter, r *http.Request) {
	rows, e := s.Store.DB.QueryContext(r.Context(), s.q("SELECT command,created_at FROM cp_terminal_commands WHERE operation_id=? ORDER BY created_at,id LIMIT 128"), r.PathValue("id"))
	if e != nil {
		fail(w, 500, "command audit unavailable")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var command string
		var created int64
		if e = rows.Scan(&command, &created); e != nil {
			break
		}
		items = append(items, map[string]any{"command": command, "created_at": time.Unix(created, 0).UTC()})
	}
	if e != nil || rows.Err() != nil {
		fail(w, 500, "command audit unavailable")
		return
	}
	reply(w, 200, map[string]any{"items": items})
}

func (s *Server) RunOperationLoop(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	defer func() {
		s.terminalMu.Lock()
		ids := []string{}
		for id := range s.terminals {
			ids = append(ids, id)
		}
		s.terminalMu.Unlock()
		for _, id := range ids {
			s.closeTerminal(id)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_ = s.Store.Write(ctx, storage.Background, func(tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, s.q("UPDATE cp_node_operations SET status='expired',updated_at=? WHERE status IN ('pending','running') AND expires_at<=? AND NOT (kind='uninstall' AND status='running')"), time.Now().Unix(), time.Now().Unix())
				return e
			})
		}
	}
}
