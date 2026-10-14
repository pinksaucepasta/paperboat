//go:build darwin || linux

package hostdproto

import (
	"context"
	"net"
	"os"
)

// ListenWorkloads reuses the lifecycle socket's owner-only filesystem policy.
// Workload payloads have their own typed protocol and never enter lifecycle frames.
func ListenWorkloads(path string) (net.Listener, error) {
	s := &Server{config: SocketConfig{SocketPath: path, UID: os.Geteuid(), GID: os.Getegid()}}
	return s.listen()
}
func DialWorkloads(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}
func RemoveWorkloadSocket(path string) error { return os.Remove(path) }
