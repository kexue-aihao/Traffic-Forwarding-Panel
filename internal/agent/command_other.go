//go:build !linux

package agent

import (
	"context"
	"errors"
	"io"
)

func runCommand(context.Context, string, io.Writer) error {
	return errors.New("terminal requires Linux")
}
