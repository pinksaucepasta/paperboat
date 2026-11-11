//go:build !windows

package daemonrpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Dial connects to the local daemon gRPC IPC over a Unix domain socket.
func Dial(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	sockPath := strings.TrimPrefix(addr, "unix://")
	if _, err := os.Stat(sockPath); err != nil {
		return nil, fmt.Errorf("socket %s: %w", sockPath, err)
	}
	return grpc.DialContext(
		ctx,
		sockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var dialer net.Dialer
			dialer.Timeout = 2 * time.Second
			conn, err := dialer.DialContext(ctx, "unix", sockPath)
			if err != nil {
				return nil, err
			}
			// A socket path from PAPERBOAT_DAEMON_SOCK or a swapped directory reaches another account's
			// server only if this check passes: the daemon must run as this user or root.
			if err := checkPeer(conn); err != nil {
				return nil, errors.Join(err, conn.Close())
			}
			return conn, nil
		}),
		grpc.WithBlock(),
	)
}
