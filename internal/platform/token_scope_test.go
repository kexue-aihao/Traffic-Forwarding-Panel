package platform

import (
	"testing"
)

// 凭据可以只覆盖账号有权访问的一部分设备组。范围只可能变窄 —— 它挡的是越权，
// 不是授权：范围外的设备组对这个凭据来说等于不存在。
func TestTokenGroupScopeNarrowsAccess(t *testing.T) {
	f := setup(t)
	f.entitled()
	alice, unscoped := f.userWithToken("alice", map[string]any{"name": "全量", "permanent": true})
	japan := f.groupWithUser("日本", alice)
	hongkong := f.groupWithUser("香港", alice)
	// 探针列表只列「上报过」的机器，所以两台都上报一次。
	f.reportProbe(f.nodeIn(japan.ID, "jp-1"), "198.51.100.31", "ipv4")
	f.reportProbe(f.nodeIn(hongkong.ID, "hk-1"), "198.51.100.32", "ipv4")

	scoped := read[map[string]any](t, f.req("POST", "/users/"+alice+"/tokens", map[string]any{
		"name": "只看日本", "permanent": true, "group_ids": []string{japan.ID},
	}, ""), 201)
	scopedRaw := scoped["token"].(string)
	if len(scoped["group_ids"].([]any)) != 1 {
		t.Fatalf("签发响应没有带回范围: %v", scoped)
	}
	unscopedRaw := unscoped["token"].(string)

	// 设备地址：范围外的组整个不出现。
	devices := read[map[string]any](t, f.req("GET", "/online/device/ip/list", nil, scopedRaw), 200)
	items := devices["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["group_name"] != "日本" {
		t.Fatalf("带范围的凭据看到了范围外的设备: %v", items)
	}
	both := read[map[string]any](t, f.req("GET", "/online/device/ip/list", nil, unscopedRaw), 200)
	if len(both["items"].([]any)) != 2 {
		t.Fatalf("不带范围的凭据应当照旧看到全部: %v", both["items"])
	}

	// 节点、设备组、探针三处列表同样收窄。
	nodes := read[map[string]any](t, f.req("GET", "/nodes", nil, scopedRaw), 200)
	if len(nodes["items"].([]any)) != 1 {
		t.Fatalf("带范围的凭据看到了范围外的机器: %v", nodes["items"])
	}
	groups := read[map[string]any](t, f.req("GET", "/groups", nil, scopedRaw), 200)
	if len(groups["items"].([]any)) != 1 {
		t.Fatalf("带范围的凭据看到了范围外的设备组: %v", groups["items"])
	}
	probes := read[map[string]any](t, f.req("GET", "/probes", nil, scopedRaw), 200)
	if len(probes["items"].([]any)) != 1 {
		t.Fatalf("带范围的凭据看到了范围外的探针: %v", probes["items"])
	}
	if locked := read[map[string]any](t, f.req("GET", "/probes", nil, unscopedRaw), 200); len(locked["items"].([]any)) != 2 {
		t.Fatalf("不带范围的凭据应当照旧看到全部探针: %v", locked["items"])
	}

	// 指名要范围外的组：按「不在你的授权范围内」拒掉，而不是悄悄返回空。
	if rr := f.req("GET", "/nodes?group_id="+hongkong.ID, nil, scopedRaw); rr.Code != 403 {
		t.Fatalf("按范围外的组筛选没有被拒: %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.req("GET", "/probes?group_id="+hongkong.ID, nil, scopedRaw); rr.Code != 403 {
		t.Fatalf("探针按范围外的组筛选没有被拒: %d", rr.Code)
	}

	// 范围不能超出账号自己的授权：那不是收窄，是另发一把更宽的钥匙。
	if rr := f.req("POST", "/users/"+alice+"/tokens", map[string]any{
		"name": "越权", "permanent": true, "group_ids": []string{"not-a-group"},
	}, ""); rr.Code != 400 {
		t.Fatalf("超出账号授权的范围被接受了: %d", rr.Code)
	}
}
