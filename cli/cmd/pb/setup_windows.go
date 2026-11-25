//go:build windows

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/pinksaucepasta/paperboat/cli/internal/windows/elevation"
	"github.com/pinksaucepasta/paperboat/cli/internal/windowsopenssh"
)

func setupPlatformHostPrerequisites(ctx context.Context) (uint16, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, err
	}
	err = runSetupOpenSSH(ctx, func(bounded context.Context) error {
		return elevation.RunOpenSSH(bounded, executable, elevation.ActionOpenSSHSetup)
	})
	if err != nil {
		return 0, err
	}
	return windowsopenssh.DefaultConfig(nil).Port, nil
}

// runSetupOpenSSH fails setup when managed SSH cannot be prepared, never skips it: a skipped step left
// PaperboatSshd missing after an install that reported ok (winpool, 2026-11-21).
func runSetupOpenSSH(ctx context.Context, step func(context.Context) error) error {
	bounded, cancel := context.WithTimeout(ctx, timingsOf(ctx).setupOpenSSH)
	defer cancel()
	if err := step(bounded); err != nil {
		return fmt.Errorf("set up managed SSH: %w; make sure the network allows downloading Microsoft OpenSSH (winget or "+
			"Microsoft's installer from GitHub), then run the command again", err)
	}
	return nil
}
