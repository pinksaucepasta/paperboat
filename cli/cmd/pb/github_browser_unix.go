//go:build !windows

package main

import (
	"context"
	"os/exec"
	"runtime"
	"time"
)

func openGitHubAuthorizationBrowser(ctx context.Context, target string) error {
	launcher := "xdg-open"
	if runtime.GOOS == "darwin" {
		launcher = "open"
	}
	// Reap the launcher and detect headless-session failures. Its output may
	// contain the one-use URL, so leave stdout/stderr disconnected.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, launcher, target).Run()
}
