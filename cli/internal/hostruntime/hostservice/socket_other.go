//go:build !windows

package hostservice

import (
	"os"
	"runtime"
	"strconv"
)

// DefaultSocketPath is the calling OS user's host service socket (hostd and pb doctor run as that user).
func DefaultSocketPath() string { return SocketPath(runtime.GOOS, os.Getuid()) }

// SocketPath is the root host service's control socket of one OS user. Each user's privileged runtime runs its own
// host service, and the server replaces any socket at its path, so a shared path let the last user's service take
// the other's over (pbmac C032: admin's availability was never applied). On macOS it sits in the run root the
// installer makes once (service.darwinRunRoot), under the per-user name purge removes with that user; on Linux it
// is the unit's own RuntimeDirectory (paperboat-<instance>).
func SocketPath(platform string, uid int) string {
	instance := "u" + strconv.Itoa(uid)
	if platform == "darwin" {
		return "/Library/Application Support/Paperboat/run/paperboat-host-" + instance + "/host-service.sock"
	}
	return "/var/run/paperboat-" + instance + "/host-service.sock"
}
