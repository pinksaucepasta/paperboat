package daemonrpc

import (
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"os"
	"path/filepath"
)

func defaultSocketAddress() string {
	if endpoint := os.Getenv("PAPERBOAT_DAEMON_SOCK"); endpoint != "" {
		return endpoint
	}
	// launchd and SSH supply different temporary directories. Share the existing
	// protected per-user runtime namespace with the local API instead.
	paths, err := localapi.CurrentPaths(os.Geteuid())
	if err != nil {
		return ""
	}
	return filepath.Join(paths.RuntimeRoot, "daemon.sock")
}
