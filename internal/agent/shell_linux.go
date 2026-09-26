package agent

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func runShell(ctx context.Context, conn *websocket.Conn) error {
	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-i")
	// 默认落在 Agent 的配置目录：开 WebSSH 多半就是为了看它的配置、状态和单元
	// 文件，落在 /root 还得自己 cd 过去。目录不在（非官方安装路径）就退回原来的
	// 位置，别让一个探测失败把终端卡在 / 上。
	cmd.Dir = "/etc/tfp-agent"
	if _, e := os.Stat(cmd.Dir); e != nil {
		cmd.Dir = "/"
	}
	home, e := os.UserHomeDir()
	if e == nil && cmd.Dir == "/" {
		cmd.Dir = home
	}
	if e != nil {
		home = "/"
	}
	// Do not expose enrollment/exit tokens from the service environment.
	cmd.Env = []string{"TERM=xterm-256color", "LANG=C.UTF-8", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + home}
	terminal, e := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 30})
	if e != nil {
		return e
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		killShellSession(cmd.Process.Pid)
		_ = terminal.Close()
		_ = conn.Close()
	}()
	defer func() { cancel(); <-done; _ = cmd.Wait() }()
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer cancel()
		for {
			var msg contract.TerminalMessage
			if conn.ReadJSON(&msg) != nil || !contract.ValidShellInput(msg) {
				return
			}
			if msg.Type == "resize" {
				if pty.Setsize(terminal, &pty.Winsize{Cols: msg.Cols, Rows: msg.Rows}) != nil {
					return
				}
			} else if _, e := terminal.Write([]byte(msg.Data)); e != nil {
				return
			}
		}
	}()
	defer func() { cancel(); <-readerDone }()
	buf := make([]byte, 4096)
	for {
		n, err := terminal.Read(buf)
		if n > 0 {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			// Base64 preserves UTF-8 sequences split across PTY reads.
			if e = conn.WriteJSON(contract.TerminalMessage{Type: "output", Data: base64.StdEncoding.EncodeToString(buf[:n])}); e != nil {
				return e
			}
		}
		if err != nil {
			return nil
		}
	}
}

// Interactive shells give foreground jobs separate process groups. Kill all
// members of the PTY session, including those jobs, before reaping the shell.
func killShellSession(session int) {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil {
			continue
		}
		raw, e := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if e != nil {
			continue
		}
		end := strings.LastIndexByte(string(raw), ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(raw[end+1:]))
		if len(fields) < 4 {
			continue
		}
		sid, e := strconv.Atoi(fields[3])
		if e == nil && sid == session {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	_ = syscall.Kill(-session, syscall.SIGKILL)
}
