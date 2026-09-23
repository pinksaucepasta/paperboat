package daemonrpc

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
)

const (
	// DefaultWindowsNamedPipe is the standard IPC named pipe on Windows.
	DefaultWindowsNamedPipe = `\\.\pipe\paperboat-ipc`
)

// DefaultSocketAddress returns the platform-specific default endpoint for daemon IPC.
// On Unix platforms it returns a unix:// socket URI, on Windows a named pipe path.
func DefaultSocketAddress() string {
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
	return filepath.Join(os.TempDir(), fmt.Sprintf("paperboat-%d", uid), "daemon.sock")
}
