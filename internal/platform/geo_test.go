package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestPublicAddressRejectsNonRoutableTargets(t *testing.T) {
	for _, allowed := range []string{"198.51.100.1", "8.8.8.8", "2001:db8::1"} {
		if !publicAddress(allowed) {
			t.Errorf("公网地址被拒绝: %s", allowed)
		}
	}
	for _, denied := range []string{"", "10.0.0.1", "192.168.1.1", "172.16.0.1", "127.0.0.1", "::1", "169.254.1.1", "fd00::1", "not-an-ip"} {
		if publicAddress(denied) {
			t.Errorf("非公网地址被外发: %q", denied)
		}
	}
}

func TestParseGeoToleratesProviderFieldNames(t *testing.T) {
	cases := []struct {
		raw  string
		code string
		name string
	}{
		{`{"country_code":"hk","country":"香港","region":"HK","city":"Central"}`, "HK", "香港"},
		{`{"countryCode":"JP","countryName":"Japan"}`, "JP", "Japan"},
		{`{"success":true,"country_code":"US","country":"United States"}`, "US", "United States"},
		{`{"country":"Germany"}`, "", "Germany"},
		{`{"success":false,"message":"reserved range"}`, "", ""},
		{`not json`, "", ""},
		{`{"country_code":"USA"}`, "", ""},
	}
	for _, c := range cases {
		location := parseGeo([]byte(c.raw))
		if c.code == "" {
			if location != nil && location.CountryCode != "" {
				t.Errorf("不该解析出国家码: %s -> %v", c.raw, location)
			}
			continue
		}
		if location == nil || location.CountryCode != c.code || location.CountryName != c.name {
			t.Errorf("解析结果不对: %s -> %v", c.raw, location)
		}
	}
}

// 查询地址模板的校验：必须是 HTTPS 且带 {ip}，留空表示关闭。
func TestGeoLookupURLValidation(t *testing.T) {
	for _, ok := range []string{"", "https://ipwho.is/{ip}", "https://geo.example.com/v1/{ip}?fields=country"} {
		if !contract.ValidGeoLookupURL(ok) {
			t.Errorf("合法模板被拒绝: %s", ok)
		}
	}
	for _, bad := range []string{"http://ipwho.is/{ip}", "https://ipwho.is/", "https://user:pw@ipwho.is/{ip}", "https://ipwho.is/{ip} extra"} {
		if contract.ValidGeoLookupURL(bad) {
			t.Errorf("非法模板被接受: %s", bad)
		}
	}
}

func TestGeoCacheServesFreshAndCachesNegativeResults(t *testing.T) {
	// 计数在服务端 goroutine 里自增、在测试 goroutine 里读，用原子量而不是
	// 裸 int —— 否则 -race 会把测试自己的竞态报出来。
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"country_code":"SG","country":"Singapore"}`))
	}))
	defer server.Close()
	cache := newGeoCache()
	cache.client = server.Client()
	cache.client.Timeout = 2 * time.Second
	ctx := context.Background()

	// 第一次只发起后台查询，当前这一帧不阻塞。
	if location := cache.lookup(ctx, "198.51.100.5", server.URL+"/{ip}"); location != nil {
		t.Fatal("首次查询不该阻塞在外部请求上")
	}
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if location := cache.lookup(ctx, "198.51.100.5", server.URL+"/{ip}"); location != nil {
			if location.CountryCode != "SG" || location.Source != "geo" {
				t.Fatalf("位置内容不对: %v", location)
			}
			if calls.Load() != 1 {
				t.Fatalf("缓存没有生效，查询了 %d 次", calls.Load())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("查询结果没有落进缓存")
}

func TestGeoCacheDisabledTemplateNeverCallsOut(t *testing.T) {
	cache := newGeoCache()
	if location := cache.lookup(context.Background(), "198.51.100.6", ""); location != nil {
		t.Fatal("关闭状态下返回了位置")
	}
	if len(cache.inflight) != 0 {
		t.Fatal("关闭状态下留下了在途标记")
	}
}

func TestProbeLocationsSeparateIPv4AndIPv6(t *testing.T) {
	cache := newGeoCache()
	cache.complete("198.51.100.20", &contract.GeoLocation{CountryCode: "US", Source: "geo"})
	cache.complete("2001:db8::20", &contract.GeoLocation{CountryCode: "JP", Source: "geo"})
	server := &Server{geo: cache}
	p := contract.Probe{PublicIPs: []contract.IPObservation{
		{Address: "2001:db8::20", Family: "ipv6"},
		{Address: "198.51.100.20", Family: "ipv4"},
	}}
	v4, v6 := server.probeLocations(context.Background(), p, "")
	if v4 == nil || v4.CountryCode != "US" {
		t.Fatalf("IPv4 归属地不正确: %v", v4)
	}
	if v6 == nil || v6.CountryCode != "JP" {
		t.Fatalf("IPv6 归属地不正确: %v", v6)
	}
	if probeAddressForFamily(p, "ipv4") != "198.51.100.20" || probeAddressForFamily(p, "ipv6") != "2001:db8::20" {
		t.Fatalf("地址族没有各自选择对应地址")
	}
}
