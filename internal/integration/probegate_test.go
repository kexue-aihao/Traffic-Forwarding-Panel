package integration

import (
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

// 探针是付费能力：普通用户要有未过期的权益才能看，管理员不受限。
//
// 这里特意把新账号**加进设备组**再断言 —— 否则被拒的原因可能是分组过滤，
// 那就测不到权益这道门槛了。
func TestProbesRequireActiveEntitlement(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	node := f.store.Identity().NodeID

	// fixture 的所有者已经买过套餐，能看到。
	f.request("GET", "/probes", nil, f.user, 200)

	fresh := decode[contract.User](t, f.request("POST", "/users", map[string]string{
		"username": "no-plan", "password": "integration-long-password", "role": "user",
	}, f.admin, 201))
	freshCookie := f.login("no-plan")

	g := f.group
	g.UserIDs = append(append([]string(nil), g.UserIDs...), fresh.ID)
	f.group = decode[contract.Group](t, f.request("PUT", "/groups/"+g.ID, g, f.admin, 200))

	// 在组里、但没有有效权益 —— 列表与历史两个出口都要拒。
	f.request("GET", "/probes", nil, freshCookie, 403)
	f.request("GET", "/probes/"+node+"/history", nil, freshCookie, 403)

	// 管理员不受限：他本来就要排查所有人的节点。
	f.request("GET", "/probes", nil, f.admin, 200)
}
