//go:build !darwin

package daemonrpc

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
)

func defaultSocketAddress() string {
	if runtime.GOOS == "windows" {
		if env := os.Getenv("PAPERBOAT_DAEMON_PIPE"); env != "" {
			return env
		}
		owner, err := user.Current()
		if err != nil {
			return ""
		}
		return DefaultWindowsNamedPipe + "-" + owner.Uid
	}

	if env := os.Getenv("PAPERBOAT_DAEMON_SOCK"); env != "" {
		return env
	}

	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "paperboat", "daemon.sock")
	}

	uid := os.Getuid()
	runUserPath := fmt.Sprintf("/run/user/%d/paperboat/daemon.sock", uid)
	if _, err := os.Stat(runUserPath); err == nil {
		return runUserPath
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("paperboat-%d", uid), "daemon.sock")
}
