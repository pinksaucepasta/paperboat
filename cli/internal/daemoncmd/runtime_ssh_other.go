//go:build !windows

package daemoncmd

import (
	"context"
	"github.com/pinksaucepasta/paperboat/cli/internal/windowsopenssh"
)

func runWindowsSSHGeneration(context.Context, string, string) error {
	return windowsopenssh.ErrInstallerUnavailable
}
