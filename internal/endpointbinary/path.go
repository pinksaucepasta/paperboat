// Package endpointbinary resolves the installed Paperboat executable without
// consulting PATH or launching an arbitrary command from the working directory.
package endpointbinary

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

func Daemon(executable string) (string, error) { return installed(executable) }
func CLI(executable string) (string, error)    { return installed(executable) }

// DaemonPathForRemoval resolves the executable path recorded in an existing
// service definition. The daemon and CLI share this executable.
func DaemonPathForRemoval(executable string) (string, error) {
	return resolvedPath(executable)
}

func resolvedPath(executable string) (string, error) {
	if !filepath.IsAbs(executable) {
		return "", fmt.Errorf("Paperboat executable path must be absolute")
	}
	resolved, err := resolveExecutablePath(executable)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func installed(executable string) (string, error) {
	target, err := resolvedPath(executable)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("Paperboat executable is unavailable: %w", err)
	}
	if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("Paperboat executable is not an executable file; reinstall pb")
	}
	return target, nil
}
