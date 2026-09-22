//go:build !linux

package agent

import (
	"context"
	"errors"
)

func hostCommand(context.Context, string, string) (string, error) {
	return "", errors.New("ping 与 mtr 需要 Linux 节点")
}
