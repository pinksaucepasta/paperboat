//go:build !windows

package daemoncmd

import "path/filepath"

// socketAddress is the coordinator IPC address for a test that owns dir.
func socketAddress(dir string) string { return filepath.Join(dir, "daemon.sock") }
