package platform

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestInteractiveBridgeProtocolAndRevocation(t *testing.T) {
	f := setup(t)
	_, n := f.node()
	f.req("POST", "/agent/ack", contract.Ack{Version: 1, AppliedVersion: 1, Capabilities: []string{"shell-v1"}}, n.Token)
	// WebSSH 走的就是这条路：不带密码，只声明 scope=shell。
	access := read[map[string]string](t, f.req("POST", "/nodes/"+n.NodeID+"/operation-access", map[string]string{"scope": "shell"}, ""), 201)
	op := read[contract.NodeOperation](t, f.req("POST", "/nodes/"+n.NodeID+"/shell", map[string]string{"access_token": access["token"], "idempotency_key": "interactive-shell"}, ""), 201)
	control := read[contract.Control](t, f.req("POST", "/agent/control", nil, n.Token), 200)
	server := httptest.NewServer(f.m)
	defer server.Close()
	base := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1"
	browser, _, e := websocket.DefaultDialer.Dial(base+"/node-operations/"+op.ID+"/terminal", http.Header{"Cookie": {f.cookie.String()}, "Origin": {server.URL}})
	if e != nil {
		t.Fatal(e)
	}
	defer browser.Close()
	agent, _, e := websocket.DefaultDialer.Dial(base+"/agent/control/"+op.ID+"/terminal", http.Header{"Authorization": {"Bearer " + n.Token}, "X-Operation-Claim": {control.Operation.Claim}})
	if e != nil {
		t.Fatal(e)
	}
	defer agent.Close()
	for _, msg := range []contract.TerminalMessage{{Type: "input", Data: "secret-keystrokes\x03"}, {Type: "input", Data: strings.Repeat("\x1b", 4096)}, {Type: "resize", Cols: 100, Rows: 30}} {
		if e = browser.WriteJSON(msg); e != nil {
			t.Fatal(e)
		}
		agent.SetReadDeadline(time.Now().Add(3 * time.Second))
		var received contract.TerminalMessage
		if e = agent.ReadJSON(&received); e != nil {
			t.Fatal(e)
		}
		if received != msg {
			t.Fatal("input corrupted")
		}
	}
	output := contract.TerminalMessage{Type: "output", Data: base64.StdEncoding.EncodeToString([]byte("中文\r\n"))}
	if e = agent.WriteJSON(output); e != nil {
		t.Fatal(e)
	}
	var received contract.TerminalMessage
	browser.SetReadDeadline(time.Now().Add(3 * time.Second))
	if e = browser.ReadJSON(&received); e != nil || received != output {
		t.Fatal("output corrupted", e)
	}
	commands := read[struct{ Items []any }](t, f.req("GET", "/node-operations/"+op.ID+"/commands", nil, ""), 200)
	if len(commands.Items) != 0 {
		t.Fatal("interactive secrets were audited")
	}
	if r := f.req("POST", "/node-operations/"+op.ID+"/cancel", nil, ""); r.Code != 204 {
		t.Fatal(r.Code)
	}
	if e = browser.ReadJSON(&received); e == nil {
		t.Fatal("cancelled browser remained connected")
	}
	if e = agent.ReadJSON(&received); e == nil {
		t.Fatal("cancelled Agent remained connected")
	}
}
