// Package agentdist 提供节点侧 Agent 的产物与接入脚本，让运营方复制一条
// 命令就能把一台设备接进某个设备组。
//
// 二进制**不**内嵌：它是为各目标平台单独构建的程序，面板镜像已经把它放在
// 面板可执行文件旁边。接入脚本**内嵌** —— 它必须永远在，否则交给运营方的
// 那条命令会指向一个不存在的东西。
//
// 两个路由都是**免登录**的：接入命令在目标设备上执行，那里没有会话。
// 交给它们的是可执行文件与安装脚本，都不含密钥；真正的凭据是一次性接入
// 令牌，由命令本身携带、15 分钟内有效、用完即废。
package agentdist

import (
	_ "embed"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// 脚本随面板编译进二进制，因此任何部署都自带一个可用的接入入口。
//
//go:embed agent-install.sh
var installer string

// 平台片段只允许小写字母与数字：路径拼接前先把它锁死，
// 后面那次 filepath 包含关系检查是第二道防线，不是唯一一道。
var platformSegment = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// Register 挂载两个公开路由：
//
//	GET /download/agent-install.sh      接入脚本
//	GET /download/agent/{os}/{arch}     指定平台的 Agent
//	GET /download/agent                 面板自身平台的 Agent
//
// dir 是 Agent 产物所在目录，空值表示面板可执行文件所在目录 —— 容器镜像
// 正是把 /agent 放在 /panel 旁边。
func Register(mux *http.ServeMux, dir string) {
	mux.HandleFunc("GET /download/agent-install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Header().Set("Content-Disposition", `inline; filename="agent-install.sh"`)
		// 内容内嵌在二进制里，没有有意义的修改时间；零值让 ServeContent
		// 省掉 Last-Modified，不去伪造一个时间戳。
		http.ServeContent(w, r, "agent-install.sh", time.Time{}, strings.NewReader(installer))
	})

	serve := func(w http.ResponseWriter, r *http.Request) {
		goos := r.PathValue("os")
		goarch := r.PathValue("arch")
		if goos == "" || goarch == "" {
			goos, goarch = runtime.GOOS, runtime.GOARCH
		}
		if !platformSegment.MatchString(goos) || !platformSegment.MatchString(goarch) {
			notFound(w, "unknown platform")
			return
		}
		path, ok := Lookup(dir, goos, goarch)
		if !ok {
			notFound(w, "this panel does not carry an Agent for "+goos+"/"+goarch)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			notFound(w, "agent binary unavailable")
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || info.IsDir() {
			notFound(w, "agent binary unavailable")
			return
		}
		// 显式声明类型与文件名：Go 内建表在 Windows 上会被注册表覆盖，
		// 而这个响应的唯一消费者是一个 `curl -o` 管道。
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="tfp-agent"`)
		http.ServeContent(w, r, "tfp-agent", info.ModTime(), f)
	}
	mux.HandleFunc("GET /download/agent", serve)
	mux.HandleFunc("GET /download/agent/{os}/{arch}", serve)
}

// Lookup 返回某平台的 Agent 产物路径。优先取带上平台后缀的文件，退回同目录
// 下不带后缀的 agent —— 后者是镜像里的默认布局，只为面板自身平台服务。
func Lookup(dir, goos, goarch string) (string, bool) {
	root := dir
	if root == "" {
		if exe, err := os.Executable(); err == nil {
			root = filepath.Dir(exe)
		}
	}
	if root == "" {
		return "", false
	}
	candidates := []string{"agent-" + goos + "-" + goarch}
	if goos == runtime.GOOS && goarch == runtime.GOARCH {
		candidates = append(candidates, "agent", "agent.exe")
	}
	for _, name := range candidates {
		path := filepath.Join(root, name)
		// 目录穿越的第二道防线：拼接结果必须仍在 root 之内。
		if rel, err := filepath.Rel(root, path); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

func notFound(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(reason + "\n"))
}
