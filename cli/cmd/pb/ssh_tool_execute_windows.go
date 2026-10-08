//go:build windows

package main

import (
	"context"
	"os/exec"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

func executeManagedSSHTool(ctx context.Context, tool string, arguments, environment []string) error {
	path, err := exec.LookPath(tool)
	if err != nil {
		return managedssh.NativeLaunchError{Err: err}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return managedssh.NativeLaunchError{Err: err}
	}
	return (managedssh.OpenSSHExecutor{}).Execute(ctx, path, arguments, environment)
}
