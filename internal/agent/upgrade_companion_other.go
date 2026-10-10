//go:build !linux

package agent

func upgradeCompanionActive(string) (bool, error) { return false, nil }
func restartUpgradeCompanion(string, bool) error  { return nil }
