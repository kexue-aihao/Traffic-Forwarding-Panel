package platform

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// offlineGeo 把位置查询关掉：测试不该向任何外部服务发请求。
func (f *fixture) offlineGeo() {
	f.t.Helper()
	settings := read[contract.SiteSettings](f.t, f.req("GET", "/site", nil, ""), 200)
	settings.GeoLookupURL = ""
	read[contract.SiteSettings](f.t, f.req("PUT", "/site", settings, ""), 200)
}

// entitled 让普通用户拥有「有效权益」—— 探针与设备地址都是付费能力。
func (f *fixture) entitled() {
	f.s.opts.ActiveEntitlement = func(context.Context, string) (bool, error) { return true, nil }
}

func (f *fixture) groupWithUser(name, userID string) contract.Group {
	f.t.Helper()
	return read[contract.Group](f.t, f.req("POST", "/groups", map[string]any{"name": name, "user_ids": []string{userID}, "port_min": 20000, "port_max": 21000, "multiplier": "1"}, ""), 201)
}

func (f *fixture) nodeIn(groupID, name string) contract.Registered {
	f.t.Helper()
	en := read[map[string]string](f.t, f.req("POST", "/nodes/enrollment", map[string]any{"name": name, "group_ids": []string{groupID}}, ""), 201)
	return read[contract.Registered](f.t, f.req("POST", "/agent/register", contract.Registration{Token: en["token"], Name: name}, ""), 201)
}

func ptr(v float64) *float64 { return &v }
func ptrS(v string) *string  { return &v }
func ptrU(v uint64) *uint64  { return &v }

// reportProbe 走真正的 Agent 上报路径：在线状态、地址观测与控制面存下来的就是
// 同一份数据，测试不绕开被验证的代码。
func (f *fixture) reportProbe(node contract.Registered, ip, family string) {
	f.t.Helper()
	now := time.Now().UTC()
	probe := contract.Probe{
		NodeID:      node.NodeID,
		SampledAt:   now,
		CPUPercent:  ptr(12.5),
		CPUModel:    ptrS("Test CPU"),
		MemoryUsed:  ptrU(1024),
		MemoryTotal: ptrU(4096),
		PublicIPs:   []contract.IPObservation{{Address: ip, Family: family, Source: "echo", ObservedAt: now}},
	}
	if rr := f.req("POST", "/agent/probe", probe, node.Token); rr.Code != 204 {
		f.t.Fatalf("探针上报失败: %d %s", rr.Code, rr.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var seen int64
		// 心跳是后台优先级的写，会晚于响应落地，所以这里轮询等它；查询同样要走
		// Rebind（Postgres 用 $1），并且把错误报出来 —— 静默忽略只会让失败变成
		// 一句「没有写入心跳时间」，看不出真正的原因。
		if e := f.s.Store.DB.QueryRowContext(context.Background(), f.s.q("SELECT last_seen FROM cp_nodes WHERE id=?"), node.NodeID).Scan(&seen); e != nil {
			f.t.Fatalf("读取心跳时间失败: %v", e)
		}
		if seen > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatal("探针上报后没有写入心跳时间")
}

func TestDeviceIPRequiresEntitlementAndGroup(t *testing.T) {
	f := setup(t)
	f.offlineGeo()
	_, secret := f.userWithToken("alice", map[string]any{"name": "脚本", "permanent": true})
	raw := secret["token"].(string)
	// 没有权益：设备地址与探针一样是付费能力。
	f.s.opts.ActiveEntitlement = func(context.Context, string) (bool, error) { return false, nil }
	for _, path := range []string{"/online/device/ip", "/online/device/ip/list", "/probes"} {
		if rr := f.req("GET", path, nil, raw); rr.Code != 403 {
			t.Fatalf("无权益仍然读到 %s: %d", path, rr.Code)
		}
	}
	f.entitled()
	// 有权益但还没被授权任何设备组。
	page := read[map[string]any](t, f.req("GET", "/online/device/ip/list", nil, raw), 200)
	if len(page["items"].([]any)) != 0 {
		t.Fatal("未授权设备组却看到了设备")
	}
}

func TestDeviceIPSingleDeviceAndList(t *testing.T) {
	f := setup(t)
	f.offlineGeo()
	f.entitled()
	userID, secret := f.userWithToken("alice", map[string]any{"name": "脚本", "permanent": true})
	raw := secret["token"].(string)
	group := f.groupWithUser("香港", userID)
	first := f.nodeIn(group.ID, "hk-1")
	f.reportProbe(first, "198.51.100.7", "ipv4")

	single := read[map[string]any](t, f.req("GET", "/online/device/ip", nil, raw), 200)
	device := single["device"].(map[string]any)
	// 对外的设备名是**设备组名**：机器自报的 ip-172-… 客户认不出来。
	if device["ipv4"] != "198.51.100.7" || device["group_name"] != "香港" {
		t.Fatalf("单台机器的地址不对: %v", device)
	}
	// 一台设备一条记录：只有 IPv4 时不编造一个空的 IPv6 字段。
	if _, invented := device["ipv6"]; invented {
		t.Fatalf("没有 IPv6 却给出了 ipv6 字段: %v", device)
	}

	// 机器被替换：新节点接管同一组，列表里给出的是新节点的新地址。
	second := f.nodeIn(group.ID, "hk-2")
	f.reportProbe(second, "198.51.100.8", "ipv4")
	if rr := f.req("GET", "/online/device/ip", nil, raw); rr.Code != 409 {
		t.Fatalf("多台机器时单台接口没有拒绝: %d %s", rr.Code, rr.Body.String())
	}
	page := read[map[string]any](t, f.req("GET", "/online/device/ip/list", nil, raw), 200)
	items := page["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("列表条数不对: %v", items)
	}
	// 两台机器同属一个组，所以两条记录的名字一样 —— 靠地址区分，不靠名字。
	addresses := []string{}
	for _, item := range items {
		entry := item.(map[string]any)
		if entry["group_name"] != "香港" {
			t.Fatalf("设备名应当是设备组名: %v", entry)
		}
		addresses = append(addresses, entry["ipv4"].(string))
	}
	sort.Strings(addresses)
	if len(addresses) != 2 || addresses[0] != "198.51.100.7" || addresses[1] != "198.51.100.8" {
		t.Fatalf("列表里的地址不对: %v", addresses)
	}
	scoped := read[map[string]any](t, f.req("GET", "/online/device/ip/list?group_id="+group.ID, nil, raw), 200)
	if len(scoped["items"].([]any)) != 2 {
		t.Fatal("按组过滤丢了设备")
	}
}

func TestDeviceIPIsolatedBetweenGroups(t *testing.T) {
	f := setup(t)
	f.offlineGeo()
	f.entitled()
	alice, aliceSecret := f.userWithToken("alice", map[string]any{"name": "a", "permanent": true})
	bob, bobSecret := f.userWithToken("bob", map[string]any{"name": "b", "permanent": true})
	groupA := f.groupWithUser("A", alice)
	f.groupWithUser("B", bob)
	nodeA := f.nodeIn(groupA.ID, "a-1")
	f.reportProbe(nodeA, "198.51.100.11", "ipv4")

	alicePage := read[map[string]any](t, f.req("GET", "/online/device/ip/list", nil, aliceSecret["token"].(string)), 200)
	if len(alicePage["items"].([]any)) != 1 {
		t.Fatal("授权用户看不到自己的设备")
	}
	bobPage := read[map[string]any](t, f.req("GET", "/online/device/ip/list", nil, bobSecret["token"].(string)), 200)
	if len(bobPage["items"].([]any)) != 0 {
		t.Fatal("跨组读到了别人的设备")
	}
	// 拿别人的组号过滤既不会报错也不会越权，只是什么也看不到。
	scoped := read[map[string]any](t, f.req("GET", "/online/device/ip/list?group_id="+groupA.ID, nil, bobSecret["token"].(string)), 200)
	if len(scoped["items"].([]any)) != 0 {
		t.Fatal("按他人组过滤读到了设备")
	}
}

func TestDeviceIPReturnsBothFamilies(t *testing.T) {
	f := setup(t)
	f.offlineGeo()
	f.entitled()
	userID, secret := f.userWithToken("alice", map[string]any{"name": "脚本", "permanent": true})
	group := f.groupWithUser("双栈", userID)
	node := f.nodeIn(group.ID, "dual")
	now := time.Now().UTC()
	probe := contract.Probe{NodeID: node.NodeID, SampledAt: now, CPUPercent: ptr(1), CPUModel: ptrS("cpu"), MemoryUsed: ptrU(1), MemoryTotal: ptrU(2), PublicIPs: []contract.IPObservation{
		{Address: "2001:db8::1", Family: "ipv6", Source: "echo", ObservedAt: now},
		{Address: "198.51.100.20", Family: "ipv4", Source: "echo", ObservedAt: now},
	}}
	if rr := f.req("POST", "/agent/probe", probe, node.Token); rr.Code != 204 {
		f.t.Fatalf("探针上报失败: %d", rr.Code)
	}
	page := read[map[string]any](t, f.req("GET", "/online/device/ip/list", nil, secret["token"].(string)), 200)
	entry := page["items"].([]any)[0].(map[string]any)
	if entry["group_name"] != "双栈" {
		t.Fatalf("设备名应当是设备组名: %v", entry)
	}
	// 双栈机器一条记录里给两个地址，调用方不必先问这台机器是哪一族。
	if entry["ipv4"] != "198.51.100.20" || entry["ipv6"] != "2001:db8::1" {
		t.Fatalf("双栈地址没有一起返回: %v", entry)
	}
}

// 探针页面对普通用户隐藏机器地址，但节点名与所属组照给 —— 那是用来区分设备
// 的，不是地址本身。
func TestProbePageHidesAddressesButKeepsNodeIdentity(t *testing.T) {
	f := setup(t)
	f.offlineGeo()
	f.entitled()
	userID, secret := f.userWithToken("alice", map[string]any{"name": "脚本", "permanent": true})
	group := f.groupWithUser("香港", userID)
	node := f.nodeIn(group.ID, "hk-1")
	now := time.Now().UTC()
	f.s.geo.complete("198.51.100.7", &contract.GeoLocation{CountryCode: "HK", CountryName: "中国香港", Source: "geo"})
	f.s.geo.complete("2001:db8::7", &contract.GeoLocation{CountryCode: "JP", CountryName: "日本", Source: "geo"})
	probe := contract.Probe{
		NodeID: node.NodeID, SampledAt: now, CPUPercent: ptr(12.5), CPUModel: ptrS("Test CPU"),
		MemoryUsed: ptrU(1024), MemoryTotal: ptrU(4096),
		PublicIPs: []contract.IPObservation{
			{Address: "198.51.100.7", Family: "ipv4", Source: "echo", ObservedAt: now},
			{Address: "2001:db8::7", Family: "ipv6", Source: "echo", ObservedAt: now},
		},
	}
	if rr := f.req("POST", "/agent/probe", probe, node.Token); rr.Code != 204 {
		t.Fatalf("探针上报失败: %d %s", rr.Code, rr.Body.String())
	}

	admin := read[map[string]any](t, f.req("GET", "/probes", nil, ""), 200)
	adminEntry := admin["items"].([]any)[0].(map[string]any)
	if adminEntry["node_name"] != "hk-1" || len(adminEntry["public_ips"].([]any)) != 2 {
		t.Fatalf("管理员看不到地址与节点名: %v", adminEntry)
	}
	if adminEntry["ipv4_location"].(map[string]any)["country_code"] != "HK" || adminEntry["ipv6_location"].(map[string]any)["country_code"] != "JP" {
		t.Fatalf("双栈地址归属地没有分别返回: %v", adminEntry)
	}
	if len(adminEntry["group_ids"].([]any)) != 1 {
		t.Fatalf("探针缺少设备组归属: %v", adminEntry)
	}

	raw := secret["token"].(string)
	user := read[map[string]any](t, f.req("GET", "/probes", nil, raw), 200)
	userEntry := user["items"].([]any)[0].(map[string]any)
	if _, leaked := userEntry["public_ips"]; leaked {
		t.Fatalf("普通用户看到了机器地址: %v", userEntry)
	}
	if userEntry["node_name"] != "hk-1" {
		t.Fatalf("普通用户看不到节点名，无法区分设备: %v", userEntry)
	}
	if userEntry["ipv4_location"].(map[string]any)["country_code"] != "HK" || userEntry["ipv6_location"].(map[string]any)["country_code"] != "JP" {
		t.Fatalf("普通用户看不到双栈归属地: %v", userEntry)
	}
	// 按不属于自己的组过滤要被明确拒绝，而不是静默返回空列表。
	other := read[contract.Group](t, f.req("POST", "/groups", map[string]any{"name": "别的组", "port_min": 30000, "port_max": 31000, "multiplier": "1"}, ""), 201)
	if rr := f.req("GET", "/probes?group_id="+other.ID, nil, raw); rr.Code != 403 {
		t.Fatalf("按他人组过滤探针: %d", rr.Code)
	}
	scoped := read[map[string]any](t, f.req("GET", "/probes?group_id="+group.ID, nil, ""), 200)
	if len(scoped["items"].([]any)) != 1 {
		t.Fatal("管理员按组过滤丢了探针")
	}
}

// 探针页面的历史选择器要和探针卡片看到同一批机器：按组收窄时节点列表也得跟着收。
func TestNodeListFollowsProbeGroupScope(t *testing.T) {
	f := setup(t)
	f.offlineGeo()
	f.entitled()
	userID, secret := f.userWithToken("alice", map[string]any{"name": "脚本", "permanent": true})
	raw := secret["token"].(string)
	mine := f.groupWithUser("本组", userID)
	f.nodeIn(mine.ID, "mine-1")
	other := read[contract.Group](t, f.req("POST", "/groups", map[string]any{"name": "别组", "port_min": 30000, "port_max": 31000, "multiplier": "1"}, ""), 201)
	f.nodeIn(other.ID, "other-1")

	all := read[map[string]any](t, f.req("GET", "/nodes", nil, raw), 200)
	if len(all["items"].([]any)) != 1 {
		t.Fatalf("普通用户不应当看到别组的机器: %v", all["items"])
	}
	scoped := read[map[string]any](t, f.req("GET", "/nodes?group_id="+mine.ID, nil, raw), 200)
	if len(scoped["items"].([]any)) != 1 {
		t.Fatalf("按本组过滤丢了机器: %v", scoped["items"])
	}
	if rr := f.req("GET", "/nodes?group_id="+other.ID, nil, raw); rr.Code != 403 {
		t.Fatalf("按他人组过滤节点列表: %d", rr.Code)
	}
	adminScoped := read[map[string]any](t, f.req("GET", "/nodes?group_id="+other.ID, nil, ""), 200)
	if len(adminScoped["items"].([]any)) != 1 {
		t.Fatalf("管理员按组过滤节点列表丢了机器: %v", adminScoped["items"])
	}
}
