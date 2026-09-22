package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// hostCommand 调系统命令做 ping / mtr。
//
// **argv 而不是 shell 字符串**：主机名已经过 contract.ParseLookingGlass
// 校验，这里再以独立参数传入 exec，中间没有任何拼接环节。
func hostCommand(ctx context.Context, method, host string) (string, error) {
	var name string
	var args []string
	switch method {
	case "ping":
		// -n 不解析反向域名（输出更干净也更快），-c 4 四次，-W 2 每次两秒上限
		name, args = "ping", []string{"-n", "-c", "4", "-W", "2", host}
	case "mtr":
		// -r 报告模式，-w 宽输出，-c 5 五轮
		name, args = "mtr", []string{"-r", "-w", "-c", "5", host}
	default:
		return "", fmt.Errorf("不支持的诊断方式 %s", method)
	}
	if _, err := exec.LookPath(name); err != nil {
		return "", fmt.Errorf("节点上没有 %s 命令", name)
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		// 命令本身返回非零很常见（比如 ping 有丢包），把已产生的输出一并交回，
		// 让操作方看到真实结果而不是一句「失败了」。
		if out.Len() > 0 {
			return out.String(), nil
		}
		return "", fmt.Errorf("%s 执行失败：%w", name, err)
	}
	return out.String(), nil
}
