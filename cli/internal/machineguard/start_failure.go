package machineguard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const startFailureFile = "start-failure.txt"

// The guard's service log is root-only and pb's error printer hides untyped errors, so a guard that cannot
// start leaves its reason in the public control directory for `pb doctor` (any user may read it).
func startFailurePath(socket string) string {
	return filepath.Join(filepath.Dir(socket), startFailureFile)
}

func recordStartFailure(socket string, cause error) {
	if runtime.GOOS == "windows" || socket == "" || cause == nil {
		return
	}
	// Service umasks make both 0700; the public readers are why this file exists, as for the control directory.
	dir, path := filepath.Dir(socket), startFailurePath(socket)
	if os.MkdirAll(dir, 0755) == nil && os.Chmod(dir, 0755) == nil && os.WriteFile(path, []byte(cause.Error()+"\n"), 0644) == nil {
		_ = os.Chmod(path, 0644)
	}
}

func clearStartFailure(socket string) {
	if runtime.GOOS != "windows" && socket != "" {
		_ = os.Remove(startFailurePath(socket))
	}
}

// StartFailure is why the machine guard last stopped before serving, or "" when it did not record one.
func StartFailure() string { return readStartFailure(DefaultSocket) }

func readStartFailure(socket string) string {
	if runtime.GOOS == "windows" || socket == "" {
		return ""
	}
	data, err := os.ReadFile(startFailurePath(socket))
	if err != nil {
		return ""
	}
	if len(data) > 2048 {
		data = data[:2048]
	}
	return strings.TrimSpace(string(data))
}
