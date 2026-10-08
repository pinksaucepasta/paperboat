//go:build !windows

package telemetry

import (
	"io/fs"
	"os"
	"syscall"
)

func openTelemetryDescriptor(path string) (*os.File, error) {
	// Refuse symlink replacement and avoid blocking on a substituted FIFO.
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
}

func secureTelemetryFile(string) error { return nil }

func telemetryFilePrivate(_ string, info fs.FileInfo) bool {
	return info != nil && info.Mode().Perm() == 0o600
}
