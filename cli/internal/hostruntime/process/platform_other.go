//go:build !windows

package process

import (
	"io/fs"
	"os"
	"path/filepath"
)

func platformExecutable(_ string, info fs.FileInfo) bool {
	return info.Mode().Perm()&0o111 != 0 && info.Mode().Perm()&0o022 == 0
}

func platformShellArguments(string) []string { return []string{"-l"} }

func platformEnvironmentKey(string) bool { return false }

// BaseEnvironment is the bounded host-owned environment shared by terminals and executions.
func BaseEnvironment(shell string) ([]string, error) {
	values := []string{"PATH=" + os.Getenv("PATH"), "SHELL=" + shell, "TERM=xterm-256color"}
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		values = append(values, "HOME="+home)
	}
	return values, nil
}

func platformShellEnvironment(_ string, environment []string) []string { return environment }
