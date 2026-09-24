package agent

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestInteractiveShellRetainsDirectoryResizesAndInterrupts(t *testing.T) {
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if e != nil {
			done <- e
			return
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		done <- runShell(ctx, conn)
	}))
	defer server.Close()
	conn, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	readUntil := func(want string) {
		t.Helper()
		var out strings.Builder
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for !strings.Contains(out.String(), want) {
			var msg contract.TerminalMessage
			if e := conn.ReadJSON(&msg); e != nil {
				t.Fatalf("waiting for %q: %v (%s)", want, e, out.String())
			}
			b, e := base64.StdEncoding.DecodeString(msg.Data)
			if e != nil {
				t.Fatal(e)
			}
			out.Write(b)
		}
	}
	write := func(data string) {
		t.Helper()
		if e := conn.WriteJSON(contract.TerminalMessage{Type: "input", Data: data}); e != nil {
			t.Fatal(e)
		}
	}
	write("stty -echo; cd /tmp; printf 'REA''DY\\n'\n")
	readUntil("READY")
	write("pwd\n")
	readUntil("/tmp")
	if e = conn.WriteJSON(contract.TerminalMessage{Type: "resize", Cols: 92, Rows: 27}); e != nil {
		t.Fatal(e)
	}
	write("stty size\n")
	readUntil("27 92")
	write("printf 'BEFORE\\n'; sleep 60; printf 'AFTER\\n'\n")
	readUntil("BEFORE")
	write("\x03")
	write("printf 'INTERRUPTED\\n'\n")
	readUntil("INTERRUPTED")
	write("printf '中文\\n'\n")
	readUntil("中文")
	conn.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shell did not close")
	}
}
