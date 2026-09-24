package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
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
	var pending string
	readUntil := func(want string) {
		t.Helper()
		if e := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); e != nil {
			t.Fatal(e)
		}
		for {
			if i := strings.Index(pending, want); i >= 0 {
				// A PTY frame can contain both command output and the next prompt.
				pending = pending[i+len(want):]
				return
			}
			var msg contract.TerminalMessage
			if e := conn.ReadJSON(&msg); e != nil {
				t.Fatalf("waiting for %q: %v (%q)", want, e, pending)
			}
			b, e := base64.StdEncoding.DecodeString(msg.Data)
			if e != nil {
				t.Fatal(e)
			}
			pending += string(b)
		}
	}
	write := func(data string) {
		t.Helper()
		if e := conn.WriteJSON(contract.TerminalMessage{Type: "input", Data: data}); e != nil {
			t.Fatal(e)
		}
	}
	const prompt = "__TFP_PROMPT__ "
	// Split the markers so the initial command echo cannot satisfy readiness.
	write("PS1='__TFP_''PROMPT__ '; stty -echo; cd /tmp; printf '__TFP_''BOOTED__\\n'\n")
	readUntil("__TFP_BOOTED__\r\n")
	readUntil(prompt)
	write("pwd\n")
	readUntil("/tmp\r\n")
	readUntil(prompt)
	if e = conn.WriteJSON(contract.TerminalMessage{Type: "resize", Cols: 92, Rows: 27}); e != nil {
		t.Fatal(e)
	}
	write("stty size\n")
	readUntil("27 92\r\n")
	readUntil(prompt)
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	// The foreground child announces readiness after installing its SIGINT
	// handler. A marker printed by the parent before launching a job is too early.
	write("TFP_TEST_SHELL_FOREGROUND=1 '" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "' -test.run='^TestInteractiveShellForegroundHelper$'\n")
	readUntil("__TFP_FOREGROUND_READY__\r\n")
	write("\x03")
	readUntil("__TFP_SIGINT_RECEIVED__\r\n")
	readUntil(prompt)
	write("printf 'INTERRUPTED\\n'\n")
	readUntil("INTERRUPTED\r\n")
	readUntil(prompt)
	write("printf '中文\\n'\n")
	readUntil("中文\r\n")
	readUntil(prompt)
	conn.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shell did not close")
	}
}

// Executed as a foreground job inside the PTY, using the current test binary.
func TestInteractiveShellForegroundHelper(t *testing.T) {
	if os.Getenv("TFP_TEST_SHELL_FOREGROUND") != "1" {
		return
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	fmt.Println("__TFP_FOREGROUND_READY__")
	<-interrupt
	fmt.Println("__TFP_SIGINT_RECEIVED__")
	os.Exit(0)
}
