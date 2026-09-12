//go:build windows

package hostruntimecmd

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

func checkWindowsBootstrapSSHPort(ctx context.Context, config windowsopenssh.Config) error {
	return checkWindowsBootstrapSSHPortWith(ctx, config, windowsopenssh.CheckPortAvailable, windowsopenssh.CheckLoopbackHealth)
}

func checkWindowsBootstrapSSHPortWith(ctx context.Context, config windowsopenssh.Config, available func(uint16) error, health func(context.Context, windowsopenssh.Config, windowsopenssh.Result) (windowsopenssh.ServiceHealth, error)) error {
	if err := available(config.Port); err != nil {
		// A retry may encounter its own already-installed listener. Only the
		// exact per-SID SCM wrapper and its dual-stack sshd child can retain it.
		result := windowsopenssh.Result{Port: config.Port, SSHDPath: filepath.Join(config.InstallRoot, "sshd.exe")}
		if _, healthErr := health(ctx, config, result); healthErr != nil {
			return errors.Join(err, healthErr)
		}
	}
	return nil
}
