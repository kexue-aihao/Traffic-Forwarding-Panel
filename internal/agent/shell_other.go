//go:build !linux

package agent

import (
	"context"
	"errors"
	"github.com/gorilla/websocket"
)

func runShell(context.Context, *websocket.Conn) error {
	return errors.New("interactive terminal requires Linux")
}
