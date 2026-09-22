package integration

import (
	"strconv"
	"strings"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

// 监听地址留空时，面板从设备组允许的范围内分配一个尚未预留的端口。
//
// 这条路径有两个容易写错的地方，所以都钉住：挑出来的端口必须落在组范围内
// （否则会被组策略拒），且不能重复挑到同一个（否则第二次就撞端口预留）。
func TestEmptyListenAllocatesFromGroupRange(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		rule := decode[contract.Rule](t, f.request("POST", "/rules", contract.Rule{
			Name: "auto-listen", NodeID: f.store.Identity().NodeID, GroupID: f.group.ID,
			Network: "tcp", Transport: "direct", Listen: "", Target: "127.0.0.1:8080", Enabled: true,
		}, f.user, 201))
		if rule.Listen == "" {
			t.Fatal("留空监听地址应当被分配一个端口")
		}
		n, e := strconv.Atoi(strings.TrimPrefix(rule.Listen, ":"))
		if e != nil || n < f.group.PortMin || n > f.group.PortMax {
			t.Fatalf("分配到的端口 %q 不在组范围 %d-%d 内", rule.Listen, f.group.PortMin, f.group.PortMax)
		}
		if seen[rule.Listen] {
			t.Fatalf("重复分配了同一个端口 %q", rule.Listen)
		}
		seen[rule.Listen] = true
	}
	// 显式给出地址时不受影响。
	explicit := decode[contract.Rule](t, f.request("POST", "/rules", contract.Rule{
		Name: "explicit-listen", NodeID: f.store.Identity().NodeID, GroupID: f.group.ID,
		Network: "tcp", Transport: "direct", Listen: ":19999", Target: "127.0.0.1:8080", Enabled: true,
	}, f.user, 201))
	if explicit.Listen != ":19999" {
		t.Fatalf("显式监听地址被改写成了 %q", explicit.Listen)
	}
}
