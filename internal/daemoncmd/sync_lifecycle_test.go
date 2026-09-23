package daemoncmd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTerminalSyncFailureWaitsForRuntimeCleanup(t *testing.T) {
	failure := errors.New("authorization revoked")
	cleanup := errors.New("cleanup failed")
	failures := make(chan error, 1)
	failures <- failure
	cleaned := false
	err := runWithSyncFailure(t.Context(), failures, func(ctx context.Context) error {
		<-ctx.Done()
		cleaned = true
		return errors.Join(context.Canceled, cleanup)
	})
	if !cleaned || !errors.Is(err, failure) || !errors.Is(err, cleanup) {
		t.Fatalf("cleaned=%v err=%v", cleaned, err)
	}
}
func TestRuntimeFailurePropagatesWithoutSync(t *testing.T) {
	expected := errors.New("runtime failed")
	if err := runWithSyncFailure(t.Context(), nil, func(context.Context) error { return expected }); !errors.Is(err, expected) {
		t.Fatal(err)
	}
}

type terminalCoordinatorSync struct {
	pbSync.UnimplementedSyncServiceServer
	reject chan struct{}
}

func (s *terminalCoordinatorSync) Sync(stream pbSync.SyncService_SyncServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&pbSync.SyncResponse{Revision: 1, Peers: []*pbSync.PeerUpdate{{PeerId: "device", Approved: true, Online: true, AssignedIp: "127.100.0.2"}}}); err != nil {
		return err
	}
	select {
	case <-s.reject:
		return status.Error(codes.PermissionDenied, "revoked")
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}
func TestCoordinatorForwardsTerminalSyncFailureAfterWithdrawal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	backend := &terminalCoordinatorSync{reject: make(chan struct{})}
	pbSync.RegisterSyncServiceServer(server, backend)
	go server.Serve(listener)
	defer server.Stop()
	dir := t.TempDir()
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c, err := NewCoordinator(CoordinatorConfig{SocketAddress: filepath.Join(dir, "d.sock"), SyncAddress: listener.Addr().String(), DeviceID: "source", Token: func(context.Context) (string, error) { return "fixture", nil }, Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	close(backend.reject)
	select {
	case err = <-c.Errors():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatal(err)
	}
	snapshot, err := c.rpcBackend.GetStatus(ctx)
	if err != nil || snapshot.DaemonState != "disconnected" || len(snapshot.Peers) != 0 {
		t.Fatalf("terminal failure retained readiness: %v %v", snapshot, err)
	}
}
