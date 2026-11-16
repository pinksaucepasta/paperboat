//go:build darwin || linux

package localdaemon

import (
	"context"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// daemonLogLimit bounds the macOS daemon log: past it the file moves to .1 (replacing the older one), so at most
// two limits stay on disk. launchd opens StandardOutPath once per start and never rotates it; newsyslog (what the
// root guard's logs use) needs root for a file in the user's Library.
const daemonLogLimit = 4 << 20

// keepDaemonLogBounded checks the log once a minute until ctx ends.
func keepDaemonLogBounded(ctx context.Context, path string) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		_ = rotateDaemonLog(path, daemonLogLimit, 1, 2)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// rotateDaemonLog moves path to path.1 once it exceeds limit and points fds (stdout and stderr) at a fresh file.
// It does nothing unless the last fd already writes to path: a daemon run by hand keeps its terminal.
func rotateDaemonLog(path string, limit int64, fds ...int) error {
	var current unix.Stat_t
	if err := unix.Fstat(fds[len(fds)-1], &current); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	file, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || uint64(file.Dev) != uint64(current.Dev) || file.Ino != current.Ino || info.Size() <= limit {
		return nil
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return err
	}
	fresh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer fresh.Close()
	for _, fd := range fds {
		if err := unix.Dup2(int(fresh.Fd()), fd); err != nil {
			return err
		}
	}
	return nil
}
