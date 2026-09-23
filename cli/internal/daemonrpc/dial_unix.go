//go:build !windows

package daemonrpc

import (
	"context"
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
			return dialer.DialContext(ctx, "unix", sockPath)
		}),
		grpc.WithBlock(),
	)
}
