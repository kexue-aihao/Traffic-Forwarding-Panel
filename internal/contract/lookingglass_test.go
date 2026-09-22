package contract

import "testing"

// 目标是操作方输入的，最终会变成 argv 里的一项。这个函数是那条边界，
// 所以这里既验「合法的不被拦」，也验「能拼出别的东西的一律拒绝」。
func TestParseLookingGlass(t *testing.T) {
	ok := []struct{ method, target, host, port string }{
		{"ping", "10.20.0.11", "10.20.0.11", ""},
		{"ping", "db.internal", "db.internal", ""},
		{"mtr", "2001:db8::1", "2001:db8::1", ""},
		{"tcping", "10.20.0.11:27015", "10.20.0.11", "27015"},
		{"tcping", "[2001:db8::1]:443", "2001:db8::1", "443"},
		{"ping", "  example.com  ", "example.com", ""},
	}
	for _, c := range ok {
		host, port, err := ParseLookingGlass(c.method, c.target)
		if err != nil || host != c.host || port != c.port {
			t.Errorf("%s %q => %q,%q,%v，期望 %q,%q", c.method, c.target, host, port, err, c.host, c.port)
		}
	}

	bad := []struct{ method, target string }{
		{"ping", ""},
		{"ping", "a b"},                     // 空格
		{"ping", "example.com; id"},         // 分号
		{"ping", "example.com|id"},          // 管道
		{"ping", "$(id)"},                   // 命令替换
		{"ping", "`id`"},                    // 反引号
		{"ping", "example.com\nid"},         // 换行
		{"ping", "-oProxyCommand=x"},        // 前导连字符（会被当成选项）
		{"ping", "10.20.0.11:80"},           // ping 不接受端口
		{"mtr", "10.20.0.11:80"},            // 同上
		{"tcping", "10.20.0.11"},            // tcping 必须有端口
		{"tcping", "10.20.0.11:0"},          // 端口越界
		{"tcping", "10.20.0.11:70000"},      // 端口越界
		{"tcping", "10.20.0.11:abc"},        // 端口不是数字
		{"nmap", "10.20.0.11"},              // 方法不在白名单
		{"ping", "example.com:80"},          // 方法不允许端口
		{"ping", string(make([]byte, 400))}, // 超长
	}
	for _, c := range bad {
		if _, _, err := ParseLookingGlass(c.method, c.target); err == nil {
			t.Errorf("%s %q 应当被拒绝，但没有", c.method, c.target)
		}
	}
}
