package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// Version is injected at release build time using -ldflags -X.
var Version = "0.1.0-beta.2"

func (a *Agent) capabilities() []string {
	result := capabilities()
	if a.EnableTerminal && runtime.GOOS == "linux" {
		result = append(result, "terminal-v1")
	}
	if a.Upgrader != nil && runtime.GOOS == "linux" {
		result = append(result, "upgrade-v1")
	}
	return result
}

func (a *Agent) runControl(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var current string
	var cancel context.CancelFunc
	var result *contract.OperationResult
	finished := make(chan contract.OperationResult, 1)
	defer func() {
		if cancel != nil {
			cancel()
			<-finished
		}
	}()
	for {
		if a.Upgrader != nil {
			_ = a.Upgrader.Report(ctx, a)
		}
		if result != nil {
			staged := result.Status == "staged"
			if a.request(ctx, "POST", "/agent/control/result", result, nil) == nil {
				result = nil
				if staged {
					a.Upgrader.Restart()
					return
				}
			}
		}
		var control contract.Control
		requestCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		err := a.request(requestCtx, "POST", "/agent/control", nil, &control)
		stop()
		if err == nil {
			if current != "" {
				active := false
				for _, id := range control.Active {
					active = active || id == current
				}
				if !active && cancel != nil {
					cancel()
				}
			}
			if op := control.Operation; op != nil {
				if current != "" || result != nil {
					_ = a.request(ctx, "POST", "/agent/control/result", contract.OperationResult{ID: op.ID, Claim: op.Claim, Status: "failed", Error: "busy"}, nil)
				} else {
					taskCtx, taskCancel := context.WithDeadline(ctx, op.ExpiresAt)
					cancel = taskCancel
					current = op.ID
					go func() {
						status := "succeeded"
						switch op.Kind {
						case "terminal":
							if !a.EnableTerminal || a.runTerminal(taskCtx, *op) != nil {
								status = "failed"
							}
						case "upgrade":
							if a.Upgrader == nil || op.Upgrade == nil {
								status = "failed"
							} else if a.Upgrader.Stage(taskCtx, *op) != nil {
								status = "failed"
							} else {
								status = "staged"
							}
						default:
							status = "failed"
						}
						finished <- contract.OperationResult{ID: op.ID, Claim: op.Claim, Status: status}
					}()
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case r := <-finished:
			cancel()
			cancel = nil
			current = ""
			if r.Status == "staged" {
				// Complete the durable operation before asking the supervisor to
				// restart. A temporary panel failure is retried on the next poll.
				result = &r
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
				continue
			}
			result = &r
		case <-tick.C:
		}
	}
}

func (a *Agent) runTerminal(ctx context.Context, op contract.NodeOperation) error {
	u, e := url.Parse(strings.TrimRight(a.URL, "/") + "/api/v1/agent/control/" + url.PathEscape(op.ID) + "/terminal")
	if e != nil {
		return e
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	if tr, ok := a.HTTP.Transport.(*http.Transport); ok {
		dialer.TLSClientConfig = tr.TLSClientConfig
	}
	headers := http.Header{"Authorization": []string{"Bearer " + a.Store.Identity().Token}, "X-Operation-Claim": []string{op.Claim}}
	conn, res, e := dialer.DialContext(ctx, u.String(), headers)
	if res != nil && res.Body != nil {
		res.Body.Close()
	}
	if e != nil {
		return e
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetReadLimit(8192)
	commands := make(chan string, 1)
	go func() {
		defer cancel()
		for {
			var msg contract.TerminalMessage
			if conn.ReadJSON(&msg) != nil {
				return
			}
			if msg.Type != "command" || len(msg.Command) == 0 || len(msg.Command) > 4096 || strings.ContainsRune(msg.Command, 0) {
				return
			}
			select {
			case commands <- msg.Command:
			case <-ctx.Done():
				return
			default:
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case command := <-commands:
			commandCtx, stopCommand := context.WithTimeout(ctx, 60*time.Second)
			writer := &terminalWriter{conn: conn, cancel: stopCommand}
			err := runCommand(commandCtx, command, writer)
			stopCommand()
			code := 0
			if err != nil {
				code = 1
			}
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if e = conn.WriteJSON(contract.TerminalMessage{Type: "exit", Code: code}); e != nil {
				return e
			}
		}
	}
}

type terminalWriter struct {
	mu      sync.Mutex
	conn    *websocket.Conn
	written int
	cancel  context.CancelFunc
}

func (w *terminalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.written+len(p) > 1<<20 {
		w.cancel()
		return 0, errors.New("command output limit reached")
	}
	n := len(p)
	for len(p) > 0 {
		size := min(len(p), 4096)
		w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if e := w.conn.WriteJSON(contract.TerminalMessage{Type: "output", Data: strings.ToValidUTF8(string(p[:size]), "�")}); e != nil {
			w.cancel()
			return 0, e
		}
		p = p[size:]
	}
	w.written += n
	return n, nil
}

var _ io.Writer = (*terminalWriter)(nil)
