//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pinksaucepasta/paperboat/cli/internal/windows/elevation"
	"github.com/pinksaucepasta/paperboat/cli/internal/windowsopenssh"
)

// setupNoticeWriter receives the notice that setup went on without managed SSH.
var setupNoticeWriter io.Writer = os.Stderr

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

// runSetupOpenSSH skips what only means "this machine cannot supply OpenSSH in time" (the managed SSH target stays not
// ready; `pb doctor --repair` prepares it later, as after an install). A refusal or the caller's own expiry still fails.
func runSetupOpenSSH(ctx context.Context, step func(context.Context) error) error {
	bounded, cancel := context.WithTimeout(ctx, timingsOf(ctx).setupOpenSSH)
	defer cancel()
	err := step(bounded)
	if err == nil || ctx.Err() != nil {
		return err
	}
	var remote *elevation.RemoteError
	switch {
	case errors.As(err, &remote) && remote.Code == elevation.CodeOpenSSHUnobtainable:
	case bounded.Err() != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, elevation.ErrElevationTimedOut)):
	default:
		return err
	}
	fmt.Fprintf(setupNoticeWriter, "Managed SSH was not prepared (%v); setup continues without it. Run `pb doctor --repair` to prepare it.\n", err)
	return nil
}
