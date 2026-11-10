//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
	if tool == "rsync" {
		if arguments, err = convertDriveOperands(arguments, cygpathUnder(filepath.Dir(path))); err != nil {
			return managedssh.NativeLaunchError{Err: err}
		}
	}
	return (managedssh.OpenSSHExecutor{}).Execute(ctx, path, arguments, environment)
}

// cygpathUnder converts with the cygpath beside rsync.exe; a native rsync without one keeps the path.
func cygpathUnder(dir string) func(string) (string, error) {
	return func(value string) (string, error) {
		tool := filepath.Join(dir, "cygpath.exe")
		if _, err := os.Stat(tool); err != nil {
			return value, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, tool, "-u", value).Output()
		if err != nil {
			return "", fmt.Errorf("convert local path %q for rsync with cygpath (is the MSYS2/Cygwin install intact?): %w", value, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
}
