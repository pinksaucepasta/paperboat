//go:build windows

package daemonrpc

import (
	"context"
	"errors"
	"net"

	"github.com/pinksaucepasta/paperboat/internal/windows/pipeauth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func daemonPipeAddress(addr string) (string, error) {
	if addr != "" {
		return addr, nil
	}
	addr = DefaultSocketAddress()
	if addr == "" {
		return "", errors.New("resolve default daemon IPC address")
	}
	return addr, nil
}

// Dial connects to the local daemon gRPC IPC over a Windows named pipe.
func Dial(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	addr, err := daemonPipeAddress(addr)
	if err != nil {
		return nil, err
	}
	return grpc.DialContext(
		ctx,
		"passthrough:///"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return pipeauth.DialCurrentUser(ctx, addr)
		}),
		grpc.WithBlock(),
	)
}
