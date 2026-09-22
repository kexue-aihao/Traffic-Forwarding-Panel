package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

// agentCall 以节点身份调一次接口 —— 用节点自己的凭据，而不是管理员的 Cookie。
func (f *fixture) agentCall(method, path string, body, out any, status int) {
	f.t.Helper()
	var b bytes.Buffer
	if body != nil {
		if e := json.NewEncoder(&b).Encode(body); e != nil {
			f.t.Fatal(e)
		}
	}
	req, e := http.NewRequest(method, f.http.URL+"/api/v1"+path, &b)
	if e != nil {
		f.t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.store.Identity().Token)
	res, e := f.http.Client().Do(req)
	if e != nil {
		f.t.Fatal(e)
	}
	defer res.Body.Close()
	if res.StatusCode != status {
		data, _ := io.ReadAll(res.Body)
		f.t.Fatalf("%s %s: %d，期望 %d：%s", method, path, res.StatusCode, status, data)
	}
	if out != nil {
		if e := json.NewDecoder(res.Body).Decode(out); e != nil {
			f.t.Fatal(e)
		}
	}
}

// 一条诊断从控制台排到节点、再带回结果，这一整条走通。
func TestLookingGlassRoundTrip(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	node := f.store.Identity().NodeID

	created := decode[contract.LookingGlass](t, f.request("POST", "/nodes/"+node+"/looking-glass", map[string]string{"method": "tcping", "target": "127.0.0.1:9"}, f.admin, 202))
	if created.Status != "pending" || created.Target != "127.0.0.1:9" {
		t.Fatalf("创建结果不对：%+v", created)
	}

	var claim struct {
		Request *contract.LookingGlass `json:"request"`
	}
	f.agentCall("POST", "/agent/looking-glass", nil, &claim, 200)
	if claim.Request == nil || claim.Request.ID != created.ID || claim.Request.Claim == "" {
		t.Fatalf("节点没领到这条诊断：%+v", claim.Request)
	}
	if _, _, err := contract.ParseLookingGlass(claim.Request.Method, claim.Request.Target); err != nil {
		t.Fatalf("下发给节点的目标没通过校验：%v", err)
	}

	claim.Request.Output = "第 1 次：成功（1 ms）\n"
	f.agentCall("POST", "/agent/looking-glass/result", claim.Request, nil, 204)

	got := decode[contract.LookingGlass](t, f.request("GET", "/looking-glass/"+created.ID, nil, f.admin, 200))
	if got.Status != "done" || got.Output == "" {
		t.Fatalf("结果没有落库：%+v", got)
	}
	// 结果只能写一次：重放同一条回传必须被拒。
	f.agentCall("POST", "/agent/looking-glass/result", claim.Request, nil, 409)

	// 非法目标在创建时就被挡下，既不落库也不下发。
	f.request("POST", "/nodes/"+node+"/looking-glass", map[string]string{"method": "ping", "target": "example.com; id"}, f.admin, 400)
	f.request("POST", "/nodes/"+node+"/looking-glass", map[string]string{"method": "nmap", "target": "example.com"}, f.admin, 400)
	// 普通用户不能对节点执行诊断。
	f.request("POST", "/nodes/"+node+"/looking-glass", map[string]string{"method": "ping", "target": "example.com"}, f.user, 403)
}
