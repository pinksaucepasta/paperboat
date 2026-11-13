//go:build !windows

package process

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// platformShellEnvironment gives a terminal that arrives with no locale at all (a Windows client sends none) a UTF-8 one,
// as tmux does (tmux.c: en_US.UTF-8, then C.UTF-8); a locale the client or host chose is left alone.
func platformShellEnvironment(_ string, environment []string) []string {
	for _, entry := range environment {
		if key, value, _ := strings.Cut(entry, "="); value != "" && (key == "LANG" || key == "LC_ALL" || key == "LC_CTYPE") {
			return environment
		}
	}
	if runtime.GOOS == "darwin" {
		return append(environment, "LANG=en_US.UTF-8")
	}
	return append(environment, "LANG=C.UTF-8")
}
