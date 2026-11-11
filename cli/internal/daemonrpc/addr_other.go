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
		// The pipe is named for the SID of the account the process runs as (the listener uses the same call): the
		// enrolled user's own session finds its daemon, and a process elevated as another administrator account
		// addresses that account's pipe (not the enrolled user's), which is the intended isolation; dial then verifies
		// the pipe is exclusive to that account (winpipe.Dial).
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
