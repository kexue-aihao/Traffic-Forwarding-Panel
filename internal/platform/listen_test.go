package platform

import (
	"strconv"
	"strings"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// 端口范围内已无空位时必须明确报错。正常的随机分配与不重复在
// internal/integration/listenalloc_test.go 里走真实控制面验证，这里只补
// 那条端到端用例覆盖不到的边界：范围耗尽。
func TestBlankListenReportsExhaustedRange(t *testing.T) {
	f := setup(t)
	tight := read[contract.Group](t, f.req("POST", "/groups", map[string]any{"name": "窄组", "port_min": 41000, "port_max": 41000, "multiplier": "1"}, ""), 201)
	en := read[map[string]string](t, f.req("POST", "/nodes/enrollment", map[string]any{"name": "tiny", "group_ids": []string{tight.ID}}, ""), 201)
	tiny := read[contract.Registered](t, f.req("POST", "/agent/register", contract.Registration{Token: en["token"], Name: "tiny"}, ""), 201)

	first := bareRule(tight, tiny)
	if got := listenPort(t, read[contract.Rule](t, f.req("POST", "/rules", first, ""), 201).Listen); got != 41000 {
		t.Fatalf("窄组应当只分配到 41000，实际 %d", got)
	}
	second := bareRule(tight, tiny)
	second.Name = "第二个"
	rr := f.req("POST", "/rules", second, "")
	if rr.Code != 409 || !strings.Contains(rr.Body.String(), "端口") {
		t.Fatalf("端口耗尽时没有给出可读的错误: %d %s", rr.Code, rr.Body.String())
	}
}

// 目标地址既可以是 IP，也可以是域名 —— 转发到哪儿由客户自己决定。带 scheme
// 或漏掉端口的写法要在创建时就被拒绝，而不是留给 Agent 去猜。
func TestTargetAcceptsDomainAndAddress(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	for _, target := range []string{"127.0.0.1:8080", "backend.internal:443", "example.com:80", "[2001:db8::1]:8080"} {
		rule := bareRule(g, n)
		rule.Name = target
		rule.Target = target
		read[contract.Rule](t, f.req("POST", "/rules", rule, ""), 201)
	}
	for _, target := range []string{"backend.internal", "127.0.0.1:0", "https://example.com:443", ":8080"} {
		rule := bareRule(g, n)
		rule.Name = target
		rule.Target = target
		if rr := f.req("POST", "/rules", rule, ""); rr.Code == 201 {
			t.Errorf("非法目标地址被接受: %s", target)
		}
	}
}

func bareRule(g contract.Group, n contract.Registered) contract.Rule {
	rule := ruleFor(g, n)
	rule.Listen = ""
	return rule
}

func listenPort(t *testing.T, listen string) int {
	t.Helper()
	_, raw, ok := strings.Cut(listen, ":")
	if !ok {
		t.Fatalf("监听地址格式不对: %s", listen)
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("监听端口不是数字: %s", listen)
	}
	return port
}
