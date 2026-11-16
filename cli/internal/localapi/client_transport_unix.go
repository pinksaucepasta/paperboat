//go:build darwin || linux

package localapi

import (
	"context"
	"net"
	"path/filepath"
	"time"
)

func NewClient(socketPath string, timeout time.Duration) (*Client, error) {
	if !filepath.IsAbs(socketPath) || len(socketPath) > maxUnixSocketPath || timeout <= 0 || timeout > time.Minute {
		return nil, ErrInvalidConfig
	}
	return newClient(socketPath, timeout), nil
}

// NewOwnerClient is NewClient that also refuses a listener not run by uid (its peer credentials), for
// the vault custody handoff, which gives key material to whatever answers the socket.
func NewOwnerClient(socketPath string, timeout time.Duration, uid int) (*Client, error) {
	if !filepath.IsAbs(socketPath) || len(socketPath) > maxUnixSocketPath || timeout <= 0 || timeout > time.Minute || uid < 0 {
		return nil, ErrInvalidConfig
	}
	return newClientWithDial(socketPath, timeout, func(ctx context.Context) (net.Conn, error) {
		connection, err := dialLocal(ctx, socketPath, timeout)
		if err != nil {
			return nil, err
		}
		if peer, err := peerIdentity(connection); err != nil || peer.UID != uid {
			_ = connection.Close()
			return nil, ErrPermission
		}
		return connection, nil
	}), nil
}

func dialLocal(ctx context.Context, socketPath string, timeout time.Duration) (net.Conn, error) {
	return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", socketPath)
}
