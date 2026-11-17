//go:build !windows

package daemoncmd

import "github.com/pinksaucepasta/paperboat/cli/internal/windowsopenssh"

func runWindowsSSHService(string) error { return windowsopenssh.ErrInstallerUnavailable }
