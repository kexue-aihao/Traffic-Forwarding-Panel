package platform

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// 探针页面按 IP 归属地给每台机器画一个位置图标。地址要问外部服务，所以边界
// 在这里写死：
//
//   - 只有站点设置里配了地址模板才启用，留空就是关闭；
//   - 只查公网单播地址，私网、回环、保留地址一律不外发 —— 「这台机器在哪儿」
//     对它们既没意义，也不该把内网地址送到第三方；
//   - 结果按 IP 缓存 24 小时，查不到的负结果也缓存，免得每次轮询都打一次；
//   - 查询在后台跑，探针推送不等它。位置图标晚一格出现没关系，实时曲线卡住
//     不行。
const (
	geoResultTTL     = 24 * time.Hour
	geoConfigTTL     = time.Minute
	geoMaxEntries    = 4096
	geoMaxConcurrent = 8
	geoTimeout       = 5 * time.Second
	geoBodyLimit     = 64 << 10
)

type geoEntry struct {
	location *contract.GeoLocation
	fetched  time.Time
}

type geoCache struct {
	mu       sync.Mutex
	entries  map[string]geoEntry
	inflight map[string]bool
	url      string
	urlAt    time.Time
	client   *http.Client
}

func newGeoCache() *geoCache {
	return &geoCache{
		entries:  map[string]geoEntry{},
		inflight: map[string]bool{},
		// 重定向一律不跟：查询地址是运营方配的，跳转到哪儿都不该由对方决定。
		client: &http.Client{
			Timeout:       geoTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// configure 更新查询地址模板。站点设置保存后立即生效，不必重启。
func (g *geoCache) configure(url string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.url, g.urlAt = url, time.Now()
}

// lookup 只读缓存，顺便在后台发起缺失的查询。
//
// 它永远不阻塞：探针每 5 秒推一次，这里等一次外部 HTTP 就意味着整条推送
// 被一个第三方服务拖住。
func (g *geoCache) lookup(ctx context.Context, ip, template string) *contract.GeoLocation {
	if ip == "" {
		return nil
	}
	now := time.Now()
	g.mu.Lock()
	if entry, ok := g.entries[ip]; ok && now.Sub(entry.fetched) < geoResultTTL {
		g.mu.Unlock()
		return entry.location
	}
	if g.inflight[ip] || len(g.inflight) >= geoMaxConcurrent {
		g.mu.Unlock()
		return nil
	}
	g.inflight[ip] = true
	g.mu.Unlock()
	if template == "" {
		g.mu.Lock()
		delete(g.inflight, ip)
		g.mu.Unlock()
		return nil
	}
	// 请求上下文到这里就结束了（SSE 一帧写完就返回），查询必须挂在它之外。
	go g.fetch(context.WithoutCancel(ctx), ip, template)
	return nil
}

func (g *geoCache) complete(ip string, location *contract.GeoLocation) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.entries) >= geoMaxEntries {
		// 满了就先清过期项；还满就直接放弃写入 —— 缓存是加速手段，
		// 不值得为它做复杂的淘汰策略。
		now := time.Now()
		for key, entry := range g.entries {
			if now.Sub(entry.fetched) >= geoResultTTL {
				delete(g.entries, key)
			}
		}
		if len(g.entries) >= geoMaxEntries {
			delete(g.inflight, ip)
			return
		}
	}
	g.entries[ip] = geoEntry{location: location, fetched: time.Now()}
	delete(g.inflight, ip)
}

func (g *geoCache) fetch(ctx context.Context, ip, template string) {
	ctx, cancel := context.WithTimeout(ctx, geoTimeout)
	defer cancel()
	endpoint := strings.ReplaceAll(template, "{ip}", ip)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		g.complete(ip, nil)
		return
	}
	req.Header.Set("Accept", "application/json")
	res, err := g.client.Do(req)
	if err != nil {
		g.complete(ip, nil)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		g.complete(ip, nil)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, geoBodyLimit+1))
	if err != nil || len(raw) > geoBodyLimit {
		g.complete(ip, nil)
		return
	}
	g.complete(ip, parseGeo(raw))
}

// parseGeo 从服务商返回的 JSON 里挑出国家代码与名称。
//
// 字段名各家不同（country_code / countryCode / country），所以按候选列表
// 逐个试，而不是绑定某一家。认不出来就返回 nil —— 界面上少一个图标，
// 好过画错一个国家的旗子。
func parseGeo(raw []byte) *contract.GeoLocation {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	if ok, isBool := m["success"].(bool); isBool && !ok {
		return nil
	}
	code := strings.ToUpper(firstString(m, "country_code", "countryCode", "country_code2", "country"))
	name := firstString(m, "country_name", "countryName", "country")
	if len(code) != 2 {
		code = ""
	}
	if code == "" {
		return nil
	}
	location := &contract.GeoLocation{CountryCode: code, CountryName: name, Region: firstString(m, "region", "regionName", "region_name"), City: firstString(m, "city"), Source: "geo"}
	if len(location.CountryName) > 64 || len(location.Region) > 64 || len(location.City) > 64 {
		return nil
	}
	return location
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		v, ok := m[key].(string)
		if !ok {
			continue
		}
		if v = strings.TrimSpace(v); v != "" && len(v) <= 64 {
			return v
		}
	}
	return ""
}

// publicAddress 判断一个地址值不值得外发查询。私网、回环、链路本地、
// 组播和未指定地址一律不外发。
func publicAddress(ip string) bool {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	return parsed != nil && !parsed.IsPrivate() && !parsed.IsLoopback() && !parsed.IsLinkLocalUnicast() && !parsed.IsLinkLocalMulticast() && !parsed.IsMulticast() && !parsed.IsUnspecified()
}

// geoTemplate 取当前生效的查询地址模板，最多每分钟回读一次站点设置。
func (s *Server) geoTemplate(ctx context.Context) string {
	s.geo.mu.Lock()
	url, at := s.geo.url, s.geo.urlAt
	s.geo.mu.Unlock()
	if url != "" && time.Since(at) < geoConfigTTL {
		return url
	}
	settings, err := s.SiteSettings(ctx)
	if err != nil {
		return url
	}
	s.geo.configure(settings.GeoLookupURL)
	return settings.GeoLookupURL
}

// locationWith 用调用方已经取好的查询地址模板取位置。探针推送在持有
// 服务锁的时候调它 —— 模板要提前取，免得在锁里做一次数据库查询。
func (s *Server) locationWith(ctx context.Context, ip, template string) *contract.GeoLocation {
	if !publicAddress(ip) {
		return nil
	}
	return s.geo.lookup(ctx, ip, template)
}

// refreshGeo 在站点设置保存后立刻应用新的查询地址。
func (s *Server) refreshGeo(url string) { s.geo.configure(url) }
