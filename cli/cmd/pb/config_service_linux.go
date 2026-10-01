//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// A Paperboat remote shell need not inherit a desktop's user-bus variables.
// Discover only the current UID's protected native runtime and socket, and
// supply them to this read-only query without changing the shell environment.
func prepareConfigServiceQuery(query *exec.Cmd) {
	root := filepath.Join("/run/user", strconv.Itoa(os.Geteuid()))
	for _, path := range []string{root, filepath.Join(root, "bus")} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(owner.Uid) != os.Geteuid() {
			return
		}
		if path == root && (!info.IsDir() || info.Mode().Perm()&0077 != 0) {
			return
		}
		if path != root && info.Mode()&os.ModeSocket == 0 {
			return
		}
	}
	query.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+root, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(root, "bus"))
}
