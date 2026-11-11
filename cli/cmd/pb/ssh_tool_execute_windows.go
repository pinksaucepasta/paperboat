//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/processlaunch"
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
		convert := cygpathUnder(filepath.Dir(path))
		if arguments, err = convertDriveOperands(arguments, convert); err != nil {
			return managedssh.NativeLaunchError{Err: err}
		}
		// rsync runs the "ssh" first on its own PATH: MSYS2's or cwRsync's Cygwin ssh reads another ~/.ssh and never
		// sees Paperboat's managed configuration. The native OpenSSH that reads it is named, as on Linux and macOS
		// where rsync's ssh is that OpenSSH.
		if !rsyncRemoteShellChosen(arguments, environment) {
			capabilities, err := managedssh.ProbeOpenSSH(ctx, "ssh.exe", 5*time.Second)
			if err != nil || !capabilities.Ready() {
				return managedssh.NativeLaunchError{Err: errors.Join(managedssh.ErrOpenSSHUnavailable, err)}
			}
			shell, err := convert(filepath.ToSlash(capabilities.Executable))
			if err != nil {
				return managedssh.NativeLaunchError{Err: err}
			}
			arguments = append([]string{rsyncRemoteShellArgument(shell)}, arguments...)
		}
		// MSYS2 rewrites POSIX-looking arguments when it starts a native program: the remote "rsync --server ... /tmp/x"
		// rsync hands to ssh.exe would reach the target as "C:/msys64/tmp/x". Cygwin does not rewrite.
		environment = withDefaultEnvironment(environment, "MSYS2_ARG_CONV_EXCL", "*")
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
		command := exec.CommandContext(ctx, tool, "-u", value)
		processlaunch.ConfigureBackground(command)
		out, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("convert local path %q for rsync with cygpath (is the MSYS2/Cygwin install intact?): %w", value, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
}
