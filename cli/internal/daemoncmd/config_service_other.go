//go:build !windows

package daemoncmd

func enterWindowsConfigService(string, string) (bool, error) { return false, nil }
func defaultChezmoiPath() string                             { return "/usr/local/bin/chezmoi" }

func resolveWindowsConfigStateRoot(stateRoot, _ string) (string, error) { return stateRoot, nil }
