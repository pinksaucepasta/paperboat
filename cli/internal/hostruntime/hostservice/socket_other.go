//go:build !windows

package hostservice

import "runtime"

func DefaultSocketPath() string { return SocketPath(runtime.GOOS) }

// SocketPath is the root host service's control socket. On macOS it is not under /var/run: it sits in the run root the
// installer makes once (service.darwinRunRoot), which purge removes with the last installation.
func SocketPath(platform string) string {
	if platform == "darwin" {
		return "/Library/Application Support/Paperboat/run/paperboat-host/host-service.sock"
	}
	return "/var/run/paperboat/host-service.sock"
}
