//go:build !windows

package windowsopenssh

func RunServiceHost(string, string, string, string) error { return ErrInstallerUnavailable }
