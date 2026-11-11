//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// A Paperboat remote shell need not inherit a desktop's user-bus variables.
// Discover only the current UID's protected native runtime and socket, and
// supply them to this read-only query without changing the shell environment.
func prepareConfigServiceQuery(query *exec.Cmd) {
	root := filepath.Join("/run/user", strconv.Itoa(os.Geteuid()))
	if refusal := configServiceRuntimeRefusal(root, os.Geteuid()); refusal != "" {
		// Windows returns an error here; the query still runs, with the caller's own environment, and says why it did not
		// get the user bus.
		diagnosticlog.TryWarn("config service query runs without the user bus", "reason", refusal)
		return
	}
	query.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+root, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(root, "bus"))
}

// configServiceRuntimeRefusal says why root (the user's /run/user/<uid>) and its bus socket may not be handed to the
// query, or "" when both are this user's own and protected.
func configServiceRuntimeRefusal(root string, uid int) string {
	for _, path := range []string{root, filepath.Join(root, "bus")} {
		info, err := os.Lstat(path)
		if err != nil {
			return filepath.Base(path) + " is not readable"
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return filepath.Base(path) + " is a link"
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(owner.Uid) != uid {
			return filepath.Base(path) + " is not owned by this user"
		}
		if path == root && (!info.IsDir() || info.Mode().Perm()&0077 != 0) {
			return "the runtime directory is not private"
		}
		if path != root && info.Mode()&os.ModeSocket == 0 {
			return "the bus is not a socket"
		}
	}
	return ""
}
