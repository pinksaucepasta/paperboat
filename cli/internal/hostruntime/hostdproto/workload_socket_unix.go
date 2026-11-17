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
	listener, err := s.Listen(path)
	if err != nil {
		// Never wrap a nil *net.UnixListener: callers compare the net.Listener to nil.
		return nil, err
	}
	return listener, nil
}

// DialWorkloads ignores ownerSID, the Windows account of the server: the socket's directory carries its custody.
func DialWorkloads(ctx context.Context, path, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}
func RemoveWorkloadSocket(path string) error { return os.Remove(path) }
