package integration

import (
	"regexp"
	"strings"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

var joinKeyShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

// 设备组的接入密钥是运营方要反复复制的那一串东西，所以它必须能再次读出、
// 并且能重复使用 —— 这正是一次性接入令牌做不到的两件事。
func TestGroupJoinKeyOnboardsRepeatably(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	path := "/groups/" + f.group.ID + "/join-key"

	first := decode[map[string]string](t, f.request("GET", path, nil, f.admin, 200))
	key := first["join_key"]
	if !joinKeyShape.MatchString(key) {
		t.Fatalf("接入密钥形状不对：%q", key)
	}
	// 再读一次必须拿到同一把：命令要能被反复复制，密钥就不能一次一变。
	again := decode[map[string]string](t, f.request("GET", path, nil, f.admin, 200))
	if again["join_key"] != key {
		t.Fatalf("重复读取拿到了不同的密钥：%q -> %q", key, again["join_key"])
	}

	// 同一把密钥连续接入两台设备，这是它与一次性令牌的根本区别。
	for _, name := range []string{"edge-01", "edge-02"} {
		registered := decode[contract.Registered](t, f.request("POST", "/agent/register", contract.Registration{
			Token: key, Name: name, Version: "test", OS: "linux", Arch: "amd64",
		}, nil, 201))
		if registered.NodeID == "" || registered.Token == "" {
			t.Fatalf("%s 注册没有返回节点身份", name)
		}
	}

	type nodePage struct {
		Items []contract.Node `json:"items"`
	}
	page := decode[nodePage](t, f.request("GET", "/nodes?page_size=100", nil, f.admin, 200))
	// 按名字数自己接入的两台：fixture 自身也往这个组里放过节点，数总数没意义。
	joined := map[string]bool{}
	for _, node := range page.Items {
		if node.Name != "edge-01" && node.Name != "edge-02" {
			continue
		}
		for _, gid := range node.GroupIDs {
			if gid == f.group.ID {
				joined[node.Name] = true
			}
		}
	}
	if len(joined) != 2 {
		t.Fatalf("用接入密钥注册并进入本组的节点 = %v，期望 edge-01 与 edge-02", joined)
	}

	// 密钥只该出现在按组取的那个端点上。列表接口普通用户也调得到，
	// 一旦把密钥带进去就是全站泄露。
	listing := string(f.request("GET", "/groups", nil, f.admin, 200))
	if strings.Contains(listing, key) {
		t.Fatal("接入密钥出现在了 GET /groups 的响应里")
	}
	// 普通用户更不该拿到它。
	f.request("GET", path, nil, f.user, 403)

	// 轮换：旧密钥立即失效，新密钥可用。这是撤销已分发命令的唯一手段。
	rotated := decode[map[string]string](t, f.request("POST", path, map[string]any{}, f.admin, 200))
	if rotated["join_key"] == key || !joinKeyShape.MatchString(rotated["join_key"]) {
		t.Fatalf("轮换没有换出新密钥：%q", rotated["join_key"])
	}
	f.request("POST", "/agent/register", contract.Registration{
		Token: key, Name: "edge-stale", Version: "test", OS: "linux", Arch: "amd64",
	}, nil, 401)
	f.request("POST", "/agent/register", contract.Registration{
		Token: rotated["join_key"], Name: "edge-03", Version: "test", OS: "linux", Arch: "amd64",
	}, nil, 201)
}

// 空名字会在控制台上建出一个认不出来的节点，注册必须拒绝。
func TestGroupJoinKeyRejectsEmptyNodeName(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	key := decode[map[string]string](t, f.request("GET", "/groups/"+f.group.ID+"/join-key", nil, f.admin, 200))["join_key"]
	f.request("POST", "/agent/register", contract.Registration{
		Token: key, Name: "   ", Version: "test", OS: "linux", Arch: "amd64",
	}, nil, 401)
}
