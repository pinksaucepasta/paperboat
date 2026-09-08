//go:build !windows

package daemoncmd

func enterWindowsConfigService(string) (bool, error) { return false, nil }
func defaultChezmoiPath() string                     { return "/usr/local/bin/chezmoi" }
