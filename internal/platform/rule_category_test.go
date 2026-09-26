package platform

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// 规则分类：多选一批规则归到一个分类下，列表按分类筛选，且这一项对 Agent 隐身 ——
// 它不该进 payload，也不该让节点重新应用配置。
func TestRuleCategoriesAreControlPlaneOnly(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	first := read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
	second := ruleFor(g, n)
	second.Listen = "0.0.0.0:20002"
	second.Name = "second"
	second = read[contract.Rule](t, f.req("POST", "/rules", second, ""), 201)

	// 建规则时分类一律为空：它不属于「配置」那一半。
	for _, rule := range []contract.Rule{first, second} {
		if rule.Category != "" {
			t.Fatalf("新规则不该带分类: %v", rule.Category)
		}
	}
	before := read[map[string]any](t, f.req("GET", "/rules", nil, ""), 200)
	if len(before["categories"].([]any)) != 0 {
		t.Fatalf("还没有分类，清单应当是空的: %v", before["categories"])
	}

	// 归类前后比一次节点的期望版本：建规则会 bump，归类不该。
	desiredBefore := read[map[string]any](t, f.req("GET", "/nodes", nil, ""), 200)["items"].([]any)[0].(map[string]any)["desired_version"]
	updated := read[map[string]any](t, f.req("POST", "/rules/category", map[string]any{"ids": []string{first.ID, second.ID}, "category": "日本线路"}, ""), 200)
	if updated["updated"].(float64) != 2 {
		t.Fatalf("应当归类两条: %v", updated)
	}

	page := read[map[string]any](t, f.req("GET", "/rules", nil, ""), 200)
	categories := page["categories"].([]any)
	if len(categories) != 1 || categories[0] != "日本线路" {
		t.Fatalf("分类清单不对: %v", categories)
	}
	for _, item := range page["items"].([]any) {
		rule := item.(map[string]any)
		if rule["category"] != "日本线路" {
			t.Fatalf("列表没有带回分类: %v", rule)
		}
		// 归类不碰版本：payload 没变，界面上的乐观锁不该因此失效。
		if rule["version"].(float64) != 1 {
			t.Fatalf("归类动了规则版本: %v", rule["version"])
		}
	}
	// 筛选：按分类、按未分类。
	scoped := read[map[string]any](t, f.req("GET", "/rules?category=%E6%97%A5%E6%9C%AC%E7%BA%BF%E8%B7%AF", nil, ""), 200)
	if len(scoped["items"].([]any)) != 2 {
		t.Fatalf("按分类筛选丢了规则: %v", scoped)
	}
	empty := read[map[string]any](t, f.req("GET", "/rules?uncategorized=true", nil, ""), 200)
	if len(empty["items"].([]any)) != 0 {
		t.Fatalf("未分类筛选不该有结果: %v", empty["items"])
	}

	// 发给 Agent 的那份配置里不该有分类：它是控制台的展示口径。
	var payload string
	if e := f.s.Store.DB.QueryRowContext(context.Background(), f.s.q("SELECT payload FROM cp_rules WHERE id=?"), first.ID).Scan(&payload); e != nil {
		t.Fatal(e)
	}
	var stored map[string]any
	if e := json.Unmarshal([]byte(payload), &stored); e != nil {
		t.Fatal(e)
	}
	if _, leaked := stored["category"]; leaked {
		t.Fatalf("分类混进了发给 Agent 的配置: %s", payload)
	}
	desired := read[map[string]any](t, f.req("GET", "/nodes", nil, ""), 200)
	if got := desired["items"].([]any)[0].(map[string]any)["desired_version"]; got != desiredBefore {
		t.Fatalf("归类惊动了节点: %v -> %v", desiredBefore, got)
	}

	// 清空分类；越权改别人的规则改不到。
	cleared := read[map[string]any](t, f.req("POST", "/rules/category", map[string]any{"ids": []string{first.ID}, "category": ""}, ""), 200)
	if cleared["updated"].(float64) != 1 {
		t.Fatalf("清空分类失败: %v", cleared)
	}
	_, secret := f.userWithToken("alice", map[string]any{"name": "脚本", "permanent": true})
	other := read[map[string]any](t, f.req("POST", "/rules/category", map[string]any{"ids": []string{second.ID}, "category": "别人的"}, secret["token"].(string)), 200)
	if other["updated"].(float64) != 0 {
		t.Fatalf("普通用户改到了别人的规则: %v", other)
	}
	kept := read[map[string]any](t, f.req("GET", "/rules", nil, ""), 200)
	byID := map[string]string{}
	for _, item := range kept["items"].([]any) {
		rule := item.(map[string]any)
		// 空分类由 omitempty 省掉，读不到就是没分类。
		category, _ := rule["category"].(string)
		byID[rule["id"].(string)] = category
	}
	if byID[first.ID] != "" {
		t.Fatalf("清空分类没有生效: %v", byID)
	}
	if byID[second.ID] != "日本线路" {
		t.Fatalf("越权归类生效了: %v", byID)
	}
}
