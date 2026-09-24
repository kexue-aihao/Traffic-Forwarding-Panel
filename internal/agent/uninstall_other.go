//go:build !linux

package agent

import (
	"context"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func managedInstall(string) error { return errors.New("managed uninstall requires Linux") }
func (a *Agent) stageUninstall(context.Context, contract.NodeOperation) error {
	return managedInstall("")
}
func RunUninstallWorker(context.Context) error { return managedInstall("") }
