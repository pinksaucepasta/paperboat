package sync

import (
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/pinksaucepasta/paperboat-server/internal/sync/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestSyncStalledHandshakeReleasesAdmission(t *testing.T) {
	svc, err := NewService(&testAuth{scopes: []string{"machines:read"}}, &testAuthority{})
	if err != nil {
		t.Fatal(err)
	}
	svc.ioTimeout = 20 * time.Millisecond
	listener := bufconn.Listen(1 << 20)
	defer listener.Close()
	server := grpc.NewServer()
	svc.Register(server)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///sync", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(authorized(context.Background()), time.Second)
	defer cancel()
	stream, err := pb.NewSyncServiceClient(conn).Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately never send SyncRequest. The server, not the client deadline,
	// must terminate the stream and release the per-session admission slot.
	started := time.Now()
	_, err = stream.Recv()
	if status.Code(err) != codes.DeadlineExceeded || time.Since(started) > 500*time.Millisecond {
		t.Fatal("stalled handshake was not bounded", err)
	}
	svc.mu.Lock()
	active := svc.streams
	svc.mu.Unlock()
	if active != 0 {
		t.Fatal("stalled stream retained admission", active)
	}
}
