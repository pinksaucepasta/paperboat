//go:build !windows

package daemoncmd

func enterWindowsConfigService(string, string) (bool, error) { return false, nil }

func resolveWindowsConfigStateRoot(stateRoot, _ string) (string, error) { return stateRoot, nil }
