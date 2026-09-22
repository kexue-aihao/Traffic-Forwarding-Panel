package agent

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// 一次诊断最多跑这么久 —— 节点上的网络命令不能把 agent 挂住。
const lookingGlassTimeout = 30 * time.Second

// 回传输出的硬上限。ping/mtr 的输出本来很小，但这是从节点流向控制面的数据，
// 给它一个天花板，免得把面板和数据库撑爆。
const lookingGlassLimit = 32 << 10

// runLookingGlass 每几秒领一次待执行的诊断。与诊断（diagnostics）同一节奏，
// 保持轮询频率一致，免得两条队列的延迟表现不一样。
func (a *Agent) runLookingGlass(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		a.pollLookingGlass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// pollLookingGlass 领一条待执行的诊断，跑完回传。
//
// 命令按 argv 直接 exec，不经过 shell：目标经 contract.ParseLookingGlass
// 校验后作为独立参数传入。
func (a *Agent) pollLookingGlass(ctx context.Context) {
	var out struct {
		Request *contract.LookingGlass `json:"request"`
	}
	if a.request(ctx, "POST", "/agent/looking-glass", nil, &out) != nil || out.Request == nil {
		return
	}
	req := out.Request
	run, cancel := context.WithTimeout(ctx, lookingGlassTimeout)
	defer cancel()
	text, err := executeLookingGlass(run, *req)
	if len(text) > lookingGlassLimit {
		text = text[:lookingGlassLimit] + "\n…（输出已截断）"
	}
	req.Output = text
	req.Error = ""
	if err != nil {
		req.Error = err.Error()
	}
	// 回传不跟着 run 的取消走：超时本身就是一个结果，得送回去。
	_ = a.request(context.WithoutCancel(run), "POST", "/agent/looking-glass/result", req, nil)
}

// executeLookingGlass 分派三种方法。tcping 自己实现，不依赖设备上装了什么；
// ping 与 mtr 调系统命令，缺命令时明确说是缺命令，而不是笼统地失败。
func executeLookingGlass(ctx context.Context, req contract.LookingGlass) (string, error) {
	host, port, err := contract.ParseLookingGlass(req.Method, req.Target)
	if err != nil {
		return "", err
	}
	if req.Method == "tcping" {
		return tcping(ctx, net.JoinHostPort(host, port))
	}
	return hostCommand(ctx, req.Method, host)
}

// tcping 自己实现：连一次、计时、关掉，重复四次。
//
// 不调外部 tcping 命令是有意的 —— 那个二进制在多数发行版上默认不装，而
// 「某个端口通不通」只需要一次 TCP 握手，标准库就够了。
func tcping(ctx context.Context, address string) (string, error) {
	var b strings.Builder
	var ok int
	for i := 1; i <= 4; i++ {
		start := time.Now()
		d := net.Dialer{Timeout: 3 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", address)
		took := time.Since(start).Milliseconds()
		if err != nil {
			fmt.Fprintf(&b, "第 %d 次：失败（%d ms）%v\n", i, took, err)
		} else {
			ok++
			conn.Close()
			fmt.Fprintf(&b, "第 %d 次：成功（%d ms）\n", i, took)
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(&b, "已取消\n")
			return b.String(), nil
		case <-time.After(300 * time.Millisecond):
		}
	}
	fmt.Fprintf(&b, "\n4 次里成功 %d 次\n", ok)
	if ok == 0 {
		return b.String(), fmt.Errorf("无法连接到 %s", address)
	}
	return b.String(), nil
}
