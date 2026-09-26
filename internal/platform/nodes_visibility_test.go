package platform

import (
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// 一台机器可以同时属于多个设备组。普通用户看节点列表时，group_ids 只该出现他
// 有权访问的那些 —— 整份照发等于告诉他「这台机器还属于别家」。这里原来直接把
// group_ids 置空，虽然同样挡住了越权，但界面上「入口服务器 = 某台机器 + 它所属
// 的入口组」这条组合就凑不出来，普通用户新建转发规则时选不到入口。
func TestNodeListScopesGroupIDsToAuthorizedGroups(t *testing.T) {
	f := setup(t)
	alice, secret := f.userWithToken("alice", map[string]any{"name": "脚本", "permanent": true})
	raw := secret["token"].(string)
	mine := f.groupWithUser("本组", alice)
	other := read[contract.Group](t, f.req("POST", "/groups", map[string]any{"name": "别组", "port_min": 30000, "port_max": 31000, "multiplier": "1"}, ""), 201)
	enrollment := read[map[string]string](t, f.req("POST", "/nodes/enrollment", map[string]any{"name": "shared", "group_ids": []string{mine.ID, other.ID}}, ""), 201)
	read[contract.Registered](t, f.req("POST", "/agent/register", contract.Registration{Token: enrollment["token"], Name: "shared"}, ""), 201)

	page := read[map[string]any](t, f.req("GET", "/nodes", nil, raw), 200)
	items := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("普通用户看到的机器数不对: %v", items)
	}
	groups := items[0].(map[string]any)["group_ids"].([]any)
	if len(groups) != 1 || groups[0] != mine.ID {
		t.Fatalf("普通用户看到了无权访问的组归属: %v", groups)
	}
	admin := read[map[string]any](t, f.req("GET", "/nodes", nil, ""), 200)
	adminGroups := admin["items"].([]any)[0].(map[string]any)["group_ids"].([]any)
	if len(adminGroups) != 2 {
		t.Fatalf("管理员应当看到全部组归属: %v", adminGroups)
	}
}
