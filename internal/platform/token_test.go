package platform

import (
	"strings"
	"testing"
	"time"
)

// userWithToken 建一个普通账号，再让管理员给它发一把凭据 —— 这正是「后台建号
// 之后把密钥发给用户」的操作顺序。
func (f *fixture) userWithToken(name string, body map[string]any) (string, map[string]any) {
	f.t.Helper()
	u := read[map[string]any](f.t, f.req("POST", "/users", map[string]any{"username": name, "role": "user"}, ""), 201)
	id, _ := u["id"].(string)
	if id == "" {
		f.t.Fatalf("账号创建没有返回 id: %v", u)
	}
	secret := read[map[string]any](f.t, f.req("POST", "/users/"+id+"/tokens", body, ""), 201)
	return id, secret
}

func TestAdminIssuesTokenAndSecretIsShownOnce(t *testing.T) {
	f := setup(t)
	id, secret := f.userWithToken("alice", map[string]any{"name": "探针脚本", "permanent": true})
	raw, _ := secret["token"].(string)
	if len(raw) != 64 || secret["permanent"] != true || secret["expires_at"] != nil {
		t.Fatalf("永久凭据的交付内容不对: %v", secret)
	}
	if secret["prefix"] != raw[:8] {
		t.Fatal("前缀与明文不一致")
	}
	page := read[map[string]any](t, f.req("GET", "/users/"+id+"/tokens", nil, ""), 200)
	items := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("凭据列表条数不对: %v", items)
	}
	entry := items[0].(map[string]any)
	if _, leaked := entry["token"]; leaked {
		t.Fatal("列表接口泄露了凭据明文")
	}
	if entry["prefix"] != raw[:8] || entry["scope"] != "owner-resources" {
		t.Fatalf("列表缺少可辨识信息: %v", entry)
	}
	// 明文只在交付那一次出现：拿它调用接口应当被认出来。
	if rr := f.req("GET", "/auth/session", nil, raw); rr.Code != 200 {
		t.Fatalf("凭据不能鉴权: %d %s", rr.Code, rr.Body.String())
	}
}

func TestTokenValidityMustBeExplicit(t *testing.T) {
	f := setup(t)
	id, _ := f.userWithToken("alice", map[string]any{"name": "一次性", "permanent": true})
	cases := []map[string]any{
		{"name": "两个都不给"},
		{"name": "两个都给", "permanent": true, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)},
		{"name": "已经过期", "expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339)},
		{"name": "超过一年", "expires_at": time.Now().Add(400 * 24 * time.Hour).Format(time.RFC3339)},
		{"name": "", "permanent": true},
	}
	for _, body := range cases {
		if rr := f.req("POST", "/users/"+id+"/tokens", body, ""); rr.Code != 400 {
			t.Errorf("无效有效期被接受: %v -> %d", body, rr.Code)
		}
	}
	// 有限期凭据照常发放，并带上到期时间。
	secret := read[map[string]any](t, f.req("POST", "/users/"+id+"/tokens", map[string]any{"name": "限期", "expires_at": time.Now().Add(24 * time.Hour).Format(time.RFC3339)}, ""), 201)
	if secret["permanent"] != false || secret["expires_at"] == nil {
		t.Fatalf("有限期凭据缺少到期时间: %v", secret)
	}
}

func TestTokenResetAndRevokeStopTheOldSecret(t *testing.T) {
	f := setup(t)
	id, secret := f.userWithToken("alice", map[string]any{"name": "脚本", "permanent": true})
	old := secret["token"].(string)
	tid := secret["id"].(string)
	rotated := read[map[string]any](t, f.req("POST", "/users/"+id+"/tokens/"+tid+"/reset", map[string]any{}, ""), 200)
	fresh, _ := rotated["token"].(string)
	if len(fresh) != 64 || fresh == old {
		t.Fatalf("重置没有换出新的明文: %v", rotated)
	}
	if rr := f.req("GET", "/auth/session", nil, old); rr.Code != 401 {
		t.Fatalf("旧密钥在重置后仍然可用: %d", rr.Code)
	}
	if rr := f.req("GET", "/auth/session", nil, fresh); rr.Code != 200 {
		t.Fatalf("新密钥不可用: %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.req("DELETE", "/users/"+id+"/tokens/"+tid, nil, ""); rr.Code != 204 {
		t.Fatalf("撤销失败: %d", rr.Code)
	}
	if rr := f.req("GET", "/auth/session", nil, fresh); rr.Code != 401 {
		t.Fatalf("撤销后密钥仍然可用: %d", rr.Code)
	}
	page := read[map[string]any](t, f.req("GET", "/users/"+id+"/tokens", nil, ""), 200)
	if len(page["items"].([]any)) != 0 {
		t.Fatal("撤销后列表里还有记录")
	}
}

// 机器凭据一律按普通用户处理：即使账号是管理员，也不该让脚本顺带拿到后台权限。
func TestTokenNeverCarriesAdministratorRole(t *testing.T) {
	f := setup(t)
	secret := read[map[string]any](t, f.req("POST", "/auth/tokens", map[string]any{"name": "自用", "permanent": true}, ""), 201)
	raw := secret["token"].(string)
	if rr := f.req("GET", "/users", nil, raw); rr.Code != 403 {
		t.Fatalf("机器凭据拿到了管理员接口: %d", rr.Code)
	}
	if rr := f.req("GET", "/auth/session", nil, raw); rr.Code != 200 {
		t.Fatal("机器凭据无法读取自身会话")
	}
}

func TestUserCannotIssueTokensForOthers(t *testing.T) {
	f := setup(t)
	id, secret := f.userWithToken("alice", map[string]any{"name": "脚本", "permanent": true})
	limited := read[map[string]any](t, f.req("POST", "/auth/tokens", map[string]any{"name": "自己的", "permanent": true}, ""), 201)
	raw := secret["token"].(string)
	if rr := f.req("POST", "/users/"+id+"/tokens", map[string]any{"name": "越权", "permanent": true}, raw); rr.Code != 403 {
		t.Fatalf("普通用户给他人发凭据: %d", rr.Code)
	}
	if rr := f.req("GET", "/users/"+id+"/tokens", nil, limited["token"].(string)); rr.Code != 403 {
		t.Fatalf("普通用户读取他人凭据列表: %d", rr.Code)
	}
	if !strings.HasPrefix(raw, secret["prefix"].(string)) {
		t.Fatal("前缀与明文不匹配")
	}
}

func TestTokenLastUseIsRecorded(t *testing.T) {
	f := setup(t)
	id, secret := f.userWithToken("alice", map[string]any{"name": "脚本", "permanent": true})
	raw := secret["token"].(string)
	before := read[map[string]any](t, f.req("GET", "/users/"+id+"/tokens", nil, ""), 200)
	if used := before["items"].([]any)[0].(map[string]any)["last_used_at"]; used != nil {
		t.Fatalf("尚未使用的凭据带了最近使用时间: %v", used)
	}
	if rr := f.req("GET", "/auth/session", nil, raw); rr.Code != 200 {
		t.Fatal("凭据不可用")
	}
	after := read[map[string]any](t, f.req("GET", "/users/"+id+"/tokens", nil, ""), 200)
	entry := after["items"].([]any)[0].(map[string]any)
	if entry["last_used_at"] == nil {
		t.Fatal("使用之后没有记录最近使用时间")
	}
}
