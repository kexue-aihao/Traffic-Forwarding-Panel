package contract

import (
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LookingGlass 是一次在节点上执行的网络诊断。
//
// 与节点运维里的远程终端是两件事。终端是一个**不受限的 shell**：默认关闭、
// 要求节点声明 terminal-v1、每次还要二次授权。这里只做三件事 —— ping、
// tcping、mtr —— 参数是结构化的（方法 + 目标），面板校验后由 agent 按 argv
// 直接 exec，**不经过 shell**。所以它既能默认可用，也不会顺带把任意命令执行
// 的能力暴露出去。
type LookingGlass struct {
	ID        string    `json:"id"`
	NodeID    string    `json:"node_id"`
	Method    string    `json:"method"`
	Target    string    `json:"target"`
	Status    string    `json:"status"`
	Claim     string    `json:"claim,omitempty"`
	Output    string    `json:"output"`
	Error     string    `json:"error"`
	CreatedAt time.Time `json:"created_at"`
}

// 主机名：字母数字开头结尾，中间允许点、下划线、连字符。
var lookingHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,251}[A-Za-z0-9])?$`)

// ParseLookingGlass 校验方法并拆出主机与端口。
//
// 返回值会被当作 argv 的独立元素交给 exec，全程没有 shell；这里仍然严格限定
// 字符集 —— 「不经过 shell」是当前实现的性质，而校验应当是它自己的保证，
// 不能依赖调用方恰好没走 shell。
func ParseLookingGlass(method, target string) (host, port string, err error) {
	switch method {
	case "ping", "mtr", "tcping":
	default:
		return "", "", errors.New("不支持的诊断方式")
	}
	target = strings.TrimSpace(target)
	if target == "" || len(target) > 300 {
		return "", "", errors.New("目标不合法")
	}
	host, port = target, ""
	switch {
	case strings.HasPrefix(target, "["):
		// [v6]:port
		if i := strings.LastIndex(target, "]"); i > 0 {
			host = target[1:i]
			if rest := target[i+1:]; strings.HasPrefix(rest, ":") {
				port = rest[1:]
			}
		}
	case strings.Count(target, ":") == 1:
		host, port, _ = strings.Cut(target, ":")
	}
	if host == "" || net.ParseIP(host) == nil && !lookingHost.MatchString(host) {
		return "", "", errors.New("主机名不合法")
	}
	if port == "" {
		if method == "tcping" {
			return "", "", errors.New("tcping 需要填写 主机:端口")
		}
		return host, "", nil
	}
	if method != "tcping" {
		return "", "", errors.New("只有 tcping 接受端口")
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 {
		return "", "", errors.New("端口不合法")
	}
	return host, port, nil
}
